package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/cloud-exit/argocd-cluster-proxy/pkg/metrics"
	"github.com/cloud-exit/argocd-cluster-proxy/pkg/tunnel"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/time/rate"
)

// Server is the management-cluster component that accepts agent tunnels and
// proxies ArgoCD requests through them.
type Server struct {
	registry *Registry
	upgrader websocket.Upgrader
	log      *slog.Logger

	// ProxyToken is a bearer token that must be presented on /tunnel/
	// requests. If empty, proxy auth is disabled (NOT recommended).
	ProxyToken string

	// limiter rate-limits proxy requests. Nil means unlimited.
	limiter *rate.Limiter
}

// Option configures the server.
type Option func(*Server)

// WithProxyToken sets a bearer token required on /tunnel/ requests.
// [C2 fix: authenticate the proxy endpoint]
func WithProxyToken(token string) Option {
	return func(s *Server) { s.ProxyToken = token }
}

// WithRateLimit sets a per-server rate limit in requests per second.
// A value of 0 means unlimited (default).
func WithRateLimit(rps float64) Option {
	return func(s *Server) {
		if rps > 0 {
			s.limiter = rate.NewLimiter(rate.Limit(rps), int(rps)+1)
		}
	}
}

// New creates a server backed by the given cluster registry.
func New(reg *Registry, logger *slog.Logger, opts ...Option) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		registry: reg,
		upgrader: websocket.Upgrader{
			// [H4 fix] Only allow non-browser clients (no Origin header).
			CheckOrigin: func(r *http.Request) bool {
				return r.Header.Get("Origin") == ""
			},
		},
		log: logger,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Handler returns an http.Handler that serves both the agent tunnel endpoint
// and the ArgoCD proxy endpoint.
//
//	GET /connect          — agent WebSocket tunnel
//	ANY /tunnel/{id}/...  — reverse-proxied to the remote cluster (requires ProxyToken)
//	GET /healthz          — liveness probe
//	GET /readyz           — readiness (at least one cluster connected)
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/connect", s.handleConnect)
	mux.HandleFunc("/tunnel/", s.handleProxy)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if len(s.registry.Connected()) > 0 {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("no clusters connected"))
		}
	})
	mux.Handle("/metrics", promhttp.Handler())
	return mux
}

// ---------- agent tunnel endpoint ----------

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	// Authenticate.
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	clusterID := s.registry.Authenticate(token)
	if clusterID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Error("websocket upgrade", "err", err)
		return
	}

	// [H2 fix] Limit WebSocket message size to 16 MiB.
	ws.SetReadLimit(16 << 20)

	log := s.log.With("cluster", clusterID)
	log.Info("agent connected")
	metrics.AgentsConnected.Inc()

	sess := tunnel.NewSession(ws, log)
	sess.OnConnect = s.makeAgentConnectHandler(clusterID)

	if !s.registry.Attach(clusterID, sess) {
		log.Warn("unknown cluster id after auth")
		metrics.AgentsConnected.Dec()
		_ = ws.Close()
		return
	}

	// Serve blocks until the WebSocket closes.
	if err := sess.Serve(r.Context()); err != nil {
		log.Info("agent disconnected", "err", err)
	}
	sess.Drain(5 * time.Second)
	s.registry.Detach(clusterID)
	metrics.AgentsConnected.Dec()
	log.Info("agent session ended")
}

// makeAgentConnectHandler returns a no-op handler for the server side. The
// server never receives MsgConnect — it only sends them. This is here only to
// satisfy the interface; agents are the ones that handle MsgConnect.
func (s *Server) makeAgentConnectHandler(_ string) tunnel.ConnectHandler {
	return func(_ *tunnel.Session, _ uint32, _ string) {}
}

// ---------- proxy endpoint ----------

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	// Rate limit before any processing.
	if s.limiter != nil {
		if err := s.limiter.Wait(r.Context()); err != nil {
			metrics.ProxyRateLimitedTotal.Inc()
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
	}

	// [C2 fix] Authenticate the proxy request.
	if s.ProxyToken != "" {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.ProxyToken)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Remove the proxy auth header so it doesn't leak to the target
		// cluster. ArgoCD sends its own cluster-specific token which is
		// forwarded separately via the X-Forwarded-Authorization header
		// or by the caller re-adding it after proxy auth.
	}

	clusterID, remainingPath := parseClusterPath(r.URL.Path)
	if clusterID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	cluster := s.registry.Get(clusterID)
	if cluster == nil {
		// [H7 fix] Generic error — do not disclose whether the cluster ID
		// exists or is merely disconnected.
		http.Error(w, "cluster not available", http.StatusBadGateway)
		return
	}

	start := time.Now()
	metrics.TunnelActiveConnections.WithLabelValues(clusterID).Inc()
	defer metrics.TunnelActiveConnections.WithLabelValues(clusterID).Dec()

	if isUpgradeRequest(r) {
		s.handleUpgrade(w, r, cluster, remainingPath)
		return
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http" // tunnel carries post-TLS plaintext
			req.URL.Host = cluster.TargetAddr
			req.URL.Path = remainingPath
			req.URL.RawQuery = r.URL.RawQuery
			req.Host = cluster.TargetAddr
			// Strip hop-by-hop headers that should not transit the proxy.
			stripHopByHopHeaders(req.Header)
			// Strip Authorization so no management-side credentials travel
			// through the tunnel. The agent injects its own SA token.
			req.Header.Del("Authorization")
		},
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return cluster.session.Dial(r.Context(), cluster.TargetAddr)
			},
			DisableKeepAlives: true, // one tunnel conn per request, simplifies lifecycle
		},
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, err error) {
			// [H6 fix] Do not leak internal error details to the client.
			s.log.Error("proxy error", "cluster", clusterID, "err", err)
			rw.WriteHeader(http.StatusBadGateway)
			_, _ = rw.Write([]byte("bad gateway"))
		},
	}

	rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
	proxy.ServeHTTP(rec, r)
	metrics.ProxyRequestsTotal.WithLabelValues(clusterID, normalizeMethod(r.Method), fmt.Sprintf("%d", rec.code)).Inc()
	metrics.ProxyRequestDuration.WithLabelValues(clusterID).Observe(time.Since(start).Seconds())
}

func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request, cluster *Cluster, path string) {
	tunnelConn, err := cluster.session.Dial(r.Context(), cluster.TargetAddr)
	if err != nil {
		// [H6 fix] Generic error.
		s.log.Error("tunnel dial failed", "cluster", cluster.ID, "err", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer func() { _ = tunnelConn.Close() }()

	// Rewrite and forward the original request through the tunnel.
	outReq := r.Clone(r.Context())
	outReq.URL.Path = path
	outReq.URL.Host = cluster.TargetAddr
	outReq.Host = cluster.TargetAddr
	outReq.RequestURI = outReq.URL.RequestURI()
	// Preserve upgrade headers across hop-by-hop stripping.
	upgrade := outReq.Header.Get("Upgrade")
	stripHopByHopHeaders(outReq.Header)
	outReq.Header.Set("Connection", "Upgrade")
	outReq.Header.Set("Upgrade", upgrade)
	// Strip Authorization — the agent injects its own SA token.
	outReq.Header.Del("Authorization")
	if err := outReq.Write(tunnelConn); err != nil {
		s.log.Error("write upgrade request", "err", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	clientConn, clientBuf, err := hijacker.Hijack()
	if err != nil {
		s.log.Error("hijack", "err", err)
		return
	}
	defer func() { _ = clientConn.Close() }()

	// Flush any buffered client data.
	if clientBuf != nil && clientBuf.Reader.Buffered() > 0 {
		buf := make([]byte, clientBuf.Reader.Buffered())
		n, _ := clientBuf.Read(buf)
		if n > 0 {
			_, _ = tunnelConn.Write(buf[:n])
		}
	}

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(tunnelConn, clientConn)
		close(done)
	}()
	_, _ = io.Copy(clientConn, tunnelConn)
	<-done
}

// ---------- helpers ----------

// parseClusterPath extracts the cluster ID and remaining path from a URL path
// of the form /tunnel/{id}/... .
func parseClusterPath(path string) (clusterID, remaining string) {
	// Strip leading /tunnel/
	path = strings.TrimPrefix(path, "/tunnel/")
	idx := strings.IndexByte(path, '/')
	if idx < 0 {
		return path, "/"
	}
	return path[:idx], path[idx:]
}

// normalizeMethod returns the HTTP method if it is a known standard method,
// or "OTHER" to prevent unbounded Prometheus label cardinality.
func normalizeMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace:
		return m
	default:
		return "OTHER"
	}
}

func isUpgradeRequest(r *http.Request) bool {
	for _, v := range r.Header["Connection"] {
		if strings.EqualFold(v, "upgrade") {
			return true
		}
	}
	return false
}

// stripHopByHopHeaders removes headers that must not transit a proxy per
// RFC 7230 Section 6.1, including any custom hop-by-hop headers declared in
// the Connection header.
func stripHopByHopHeaders(h http.Header) {
	// Parse the Connection header for dynamically-declared hop-by-hop headers.
	for _, v := range h["Connection"] {
		for _, key := range strings.Split(v, ",") {
			if k := strings.TrimSpace(key); k != "" {
				h.Del(k)
			}
		}
	}
	for _, key := range []string{
		"Connection",
		"Keep-Alive",
		"Proxy-Authorization",
		"Proxy-Connection",
		"Te",
		"Trailer",
		"Transfer-Encoding",
	} {
		h.Del(key)
	}
}

// statusRecorder wraps http.ResponseWriter to capture the status code.
type statusRecorder struct {
	http.ResponseWriter
	code        int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.code = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher for streaming response support (e.g. kubectl logs -f).
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ListenAndServe starts the proxy server. It blocks until the context is
// cancelled, then gracefully shuts down.
func ListenAndServe(ctx context.Context, addr string, handler http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}
