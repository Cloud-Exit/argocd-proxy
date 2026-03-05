package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cloud-exit/argocd-cluster-proxy/pkg/metrics"
	"github.com/cloud-exit/argocd-cluster-proxy/pkg/tunnel"
	"github.com/gorilla/websocket"
)

// Config holds the agent configuration.
type Config struct {
	// ServerURL is the WebSocket URL of the proxy server (e.g. wss://proxy:8080/connect).
	ServerURL string

	// Token is the pre-shared authentication token.
	Token string

	// TargetAddr is the local Kubernetes API address to proxy to
	// (default: kubernetes.default.svc:443).
	TargetAddr string

	// SATokenPath is the path to the Kubernetes ServiceAccount token that
	// the agent injects into every proxied request. The file is re-read on
	// each request to support automatic token rotation.
	// Defaults to /var/run/secrets/kubernetes.io/serviceaccount/token.
	// Set to empty string to disable credential injection (testing only).
	SATokenPath string

	// CACertPath is the path to the CA certificate for the local K8s API.
	// Defaults to /var/run/secrets/kubernetes.io/serviceaccount/ca.crt.
	CACertPath string

	// Insecure skips TLS verification to the local K8s API.
	Insecure bool

	// PlainTarget dials the target without TLS (useful for testing).
	PlainTarget bool

	// AllowInsecureServer permits connecting to the proxy server over
	// plaintext ws:// instead of wss://. Must be set explicitly.
	AllowInsecureServer bool

	// MaxRetryInterval caps the exponential backoff between reconnection
	// attempts (default: 60s).
	MaxRetryInterval time.Duration
}

func (c *Config) defaults() {
	if c.TargetAddr == "" {
		c.TargetAddr = "kubernetes.default.svc:443"
	}
	if c.SATokenPath == "" {
		c.SATokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	}
	if c.CACertPath == "" {
		c.CACertPath = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	}
	if c.MaxRetryInterval == 0 {
		c.MaxRetryInterval = 60 * time.Second
	}
}

// Agent connects to the proxy server and tunnels Kubernetes API requests to
// the local cluster, injecting its own ServiceAccount credentials so that no
// customer cluster credentials need to exist on the management side.
type Agent struct {
	cfg       Config
	tlsConfig *tls.Config
	log       *slog.Logger
	connected atomic.Bool
}

// IsConnected returns whether the agent is currently connected to the server.
func (a *Agent) IsConnected() bool { return a.connected.Load() }

// New creates a new agent.
func New(cfg Config, logger *slog.Logger) (*Agent, error) {
	cfg.defaults()
	if logger == nil {
		logger = slog.Default()
	}

	// [C3 fix] Reject plaintext server connections unless explicitly allowed.
	if strings.HasPrefix(cfg.ServerURL, "ws://") && !cfg.AllowInsecureServer {
		return nil, fmt.Errorf("refusing plaintext ws:// server URL (use wss:// or set AllowInsecureServer)")
	}

	a := &Agent{cfg: cfg, log: logger}

	if !cfg.PlainTarget {
		tc, err := a.buildTLSConfig()
		if err != nil {
			return nil, fmt.Errorf("tls config: %w", err)
		}
		a.tlsConfig = tc
	}

	return a, nil
}

// Run connects to the proxy server and reconnects on failure. It blocks until
// the context is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	var attempt int
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		start := time.Now()
		err := a.connectOnce(ctx)
		elapsed := time.Since(start)

		if ctx.Err() != nil {
			return ctx.Err()
		}

		a.log.Warn("disconnected", "err", err, "duration", elapsed)

		// Reset backoff if the connection lasted a reasonable time.
		if elapsed > 30*time.Second {
			attempt = 0
		}

		delay := a.backoff(attempt)
		attempt++
		metrics.AgentReconnectsTotal.Inc()
		a.log.Info("reconnecting", "delay", delay)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

func (a *Agent) connectOnce(ctx context.Context) error {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+a.cfg.Token)

	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		TLSClientConfig:  &tls.Config{MinVersion: tls.VersionTLS12},
	}

	a.log.Info("connecting", "server", a.cfg.ServerURL)
	ws, _, err := dialer.DialContext(ctx, a.cfg.ServerURL, header)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	// [H2 fix] Limit inbound WebSocket messages.
	ws.SetReadLimit(16 << 20)

	sess := tunnel.NewSession(ws, a.log)
	sess.OnConnect = a.handleConnect

	a.connected.Store(true)
	metrics.AgentConnected.Set(1)
	a.log.Info("connected")

	err = sess.Serve(ctx)

	a.connected.Store(false)
	metrics.AgentConnected.Set(0)
	return err
}

// handleConnect is invoked when the server asks us to open a connection to a
// target address. Instead of raw TCP piping, we parse the HTTP request from
// the tunnel, inject our ServiceAccount token, and forward to the local K8s
// API. This ensures no customer credentials exist on the management side.
func (a *Agent) handleConnect(sess *tunnel.Session, connID uint32, addr string) {
	log := a.log.With("connID", connID, "addr", addr)

	// [H3 fix] Only allow dialling the configured target address to prevent
	// SSRF into the customer's internal network.
	if addr != a.cfg.TargetAddr {
		log.Warn("refusing to dial non-target address", "target", a.cfg.TargetAddr)
		_ = sess.SendError(connID, "address not allowed")
		return
	}

	var upstream net.Conn
	var dialErr error

	if a.cfg.PlainTarget {
		upstream, dialErr = net.DialTimeout("tcp", addr, 10*time.Second)
	} else {
		upstream, dialErr = tls.DialWithDialer(
			&net.Dialer{Timeout: 10 * time.Second},
			"tcp", addr, a.tlsConfig,
		)
	}
	if dialErr != nil {
		log.Error("dial target", "err", dialErr)
		_ = sess.SendError(connID, "dial failed")
		return
	}
	defer upstream.Close()

	tunnelConn := sess.Accept(connID)
	defer tunnelConn.Close()

	_ = sess.SendConnected(connID)

	metrics.AgentUpstreamRequestsTotal.Inc()

	// Read the HTTP request coming through the tunnel.
	// Use a 64 KB buffer so oversized headers are rejected cleanly by ReadRequest.
	bufReader := bufio.NewReaderSize(tunnelConn, 64<<10)
	req, err := http.ReadRequest(bufReader)
	if err != nil {
		metrics.AgentUpstreamErrorsTotal.Inc()
		log.Error("read request from tunnel", "err", err)
		return
	}

	// Inject the agent's ServiceAccount token so that no customer cluster
	// credentials need to exist on the management cluster.
	if token, tokenErr := a.readSAToken(); tokenErr == nil && token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else if tokenErr != nil {
		log.Debug("SA token not available, forwarding without credentials", "err", tokenErr)
	}

	// Write the modified request to the upstream K8s API.
	if err := req.Write(upstream); err != nil {
		metrics.AgentUpstreamErrorsTotal.Inc()
		log.Error("write request to upstream", "err", err)
		return
	}
	// Close the request body to avoid leaking the reference.
	if req.Body != nil {
		_ = req.Body.Close()
	}

	// Pipe remaining data bidirectionally. For normal requests the response
	// flows back; for upgrade requests (SPDY/WebSocket) additional frames
	// are exchanged after the initial handshake.
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(upstream, bufReader) // tunnel → upstream (via bufReader)
		close(done)
	}()
	_, _ = io.Copy(tunnelConn, upstream) // upstream → tunnel
	<-done

	log.Debug("connection closed")
}

// readSAToken reads the Kubernetes ServiceAccount token from disk. It re-reads
// on every call to support automatic token rotation (projected volumes).
func (a *Agent) readSAToken() (string, error) {
	data, err := os.ReadFile(a.cfg.SATokenPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func (a *Agent) buildTLSConfig() (*tls.Config, error) {
	if a.cfg.Insecure {
		return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, nil //nolint:gosec // user-requested
	}

	caCert, err := os.ReadFile(a.cfg.CACertPath)
	if err != nil {
		return nil, fmt.Errorf("read ca cert %s: %w", a.cfg.CACertPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to parse ca cert %s", a.cfg.CACertPath)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

func (a *Agent) backoff(attempt int) time.Duration {
	base := float64(500 * time.Millisecond)
	delay := time.Duration(base * math.Pow(1.5, float64(attempt)))
	if delay > a.cfg.MaxRetryInterval {
		delay = a.cfg.MaxRetryInterval
	}
	return delay
}
