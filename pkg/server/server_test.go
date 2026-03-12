package server_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloud-exit/argocd-cluster-proxy/pkg/agent"
	"github.com/cloud-exit/argocd-cluster-proxy/pkg/server"
)

// startHTTPEchoTarget starts a plain HTTP server that echoes the request path
// and reports whether an Authorization header was received.
func startHTTPEchoTarget(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Echo", "true")
			// Report the Authorization header so tests can verify credential flow.
			if auth := r.Header.Get("Authorization"); auth != "" {
				w.Header().Set("X-Got-Auth", auth)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("path=" + r.URL.Path))
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), func() {
		_ = srv.Close()
	}
}

func waitForAgent(t *testing.T, reg *server.Registry, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(reg.Connected()) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("agent did not connect in time")
}

func TestEndToEndHTTPProxy(t *testing.T) {
	targetAddr, targetCleanup := startHTTPEchoTarget(t)
	defer targetCleanup()

	clusters := []server.ClusterConfig{
		{ID: "test-cluster", Token: "secret-token", TargetAddr: targetAddr},
	}
	reg := server.NewRegistry(clusters)
	srv := server.New(reg, nil) // no proxy token for test simplicity

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/connect"
	a, err := agent.New(agent.Config{
		ServerURL:           wsURL,
		Token:               "secret-token",
		TargetAddr:          targetAddr,
		PlainTarget:         true,
		AllowInsecureServer: true,
	}, nil)
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	agentCtx, agentCancel := context.WithCancel(ctx)
	defer agentCancel()
	go func() { _ = a.Run(agentCtx) }()

	waitForAgent(t, reg, 5*time.Second)

	resp, err := http.Get(ts.URL + "/tunnel/test-cluster/api/v1/pods")
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "path=/api/v1/pods") {
		t.Errorf("body: got %q, want to contain path=/api/v1/pods", string(body))
	}
}

// TestAuthorizationStrippedBeforeTunnel verifies that any Authorization header
// from the caller is NOT forwarded through the tunnel to the target. The agent
// should be the only one injecting credentials.
func TestAuthorizationStrippedBeforeTunnel(t *testing.T) {
	targetAddr, targetCleanup := startHTTPEchoTarget(t)
	defer targetCleanup()

	clusters := []server.ClusterConfig{
		{ID: "c1", Token: "agent-token", TargetAddr: targetAddr},
	}
	reg := server.NewRegistry(clusters)
	srv := server.New(reg, nil)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/connect"
	a, _ := agent.New(agent.Config{
		ServerURL:           wsURL,
		Token:               "agent-token",
		TargetAddr:          targetAddr,
		PlainTarget:         true,
		AllowInsecureServer: true,
	}, nil)
	go func() { _ = a.Run(ctx) }()

	waitForAgent(t, reg, 5*time.Second)

	// Send request with an Authorization header. The echo server reports
	// the auth header it received via X-Got-Auth. It should be empty
	// because the server must strip Authorization before tunneling, and
	// in tests there is no SA token file for the agent to inject.
	req, _ := http.NewRequest("GET", ts.URL+"/tunnel/c1/check-auth", nil)
	req.Header.Set("Authorization", "Bearer some-caller-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	// The echo target should NOT have received the caller's token.
	gotAuth := resp.Header.Get("X-Got-Auth")
	if strings.Contains(gotAuth, "some-caller-token") {
		t.Errorf("caller token leaked through tunnel: X-Got-Auth=%q", gotAuth)
	}
}

// TestAgentInjectsSAToken verifies that when an SA token file exists, the
// agent injects it as the Authorization header to the upstream target.
func TestAgentInjectsSAToken(t *testing.T) {
	targetAddr, targetCleanup := startHTTPEchoTarget(t)
	defer targetCleanup()

	// Create a temporary SA token file.
	tokenFile := t.TempDir() + "/token"
	if err := writeFile(tokenFile, "test-sa-token-12345"); err != nil {
		t.Fatal(err)
	}

	clusters := []server.ClusterConfig{
		{ID: "c1", Token: "agent-token", TargetAddr: targetAddr},
	}
	reg := server.NewRegistry(clusters)
	srv := server.New(reg, nil)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/connect"
	a, _ := agent.New(agent.Config{
		ServerURL:           wsURL,
		Token:               "agent-token",
		TargetAddr:          targetAddr,
		SATokenPath:         tokenFile,
		PlainTarget:         true,
		AllowInsecureServer: true,
	}, nil)
	go func() { _ = a.Run(ctx) }()

	waitForAgent(t, reg, 5*time.Second)

	resp, err := http.Get(ts.URL + "/tunnel/c1/check-auth")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	// The echo target should have received the SA token injected by the agent.
	gotAuth := resp.Header.Get("X-Got-Auth")
	if gotAuth != "Bearer test-sa-token-12345" {
		t.Errorf("expected SA token injection: X-Got-Auth=%q, want 'Bearer test-sa-token-12345'", gotAuth)
	}
}

func TestAuthRejection(t *testing.T) {
	clusters := []server.ClusterConfig{
		{ID: "c1", Token: "valid-token"},
	}
	reg := server.NewRegistry(clusters)
	srv := server.New(reg, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/connect"
	a, err := agent.New(agent.Config{
		ServerURL:           wsURL,
		Token:               "wrong-token",
		PlainTarget:         true,
		AllowInsecureServer: true,
	}, nil)
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx) }()

	time.Sleep(500 * time.Millisecond)
	if len(reg.Connected()) != 0 {
		t.Error("agent with wrong token should not be connected")
	}
	cancel()
}

func TestClusterNotConnected(t *testing.T) {
	clusters := []server.ClusterConfig{
		{ID: "c1", Token: "tok"},
	}
	reg := server.NewRegistry(clusters)
	srv := server.New(reg, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/tunnel/c1/api/v1/pods")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status: got %d, want 502", resp.StatusCode)
	}
	// [H7] Verify error message does not contain cluster ID.
	if strings.Contains(string(body), "c1") {
		t.Errorf("error message should not contain cluster ID: %q", string(body))
	}
}

func TestHealthEndpoints(t *testing.T) {
	reg := server.NewRegistry(nil)
	srv := server.New(reg, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, _ := http.Get(ts.URL + "/healthz")
	if resp.StatusCode != 200 {
		t.Errorf("healthz: got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp, _ = http.Get(ts.URL + "/readyz")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("readyz: got %d, want 503", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestAgentSSRFBlocked(t *testing.T) {
	targetAddr, targetCleanup := startHTTPEchoTarget(t)
	defer targetCleanup()

	// Start a second HTTP server that the agent should NOT be able to reach.
	evilLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	evilAddr := evilLn.Addr().String()
	evilSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("SSRF_SUCCESS"))
	})}
	go func() { _ = evilSrv.Serve(evilLn) }()
	defer func() { _ = evilSrv.Close() }()

	// Configure server with evil address as the target (but agent only
	// allows its own configured targetAddr).
	clusters := []server.ClusterConfig{
		{ID: "victim", Token: "tok", TargetAddr: evilAddr},
	}
	reg := server.NewRegistry(clusters)
	proxyServer := server.New(reg, nil)
	ts := httptest.NewServer(proxyServer.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Agent is configured with targetAddr != evilAddr.
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/connect"
	a, _ := agent.New(agent.Config{
		ServerURL:           wsURL,
		Token:               "tok",
		TargetAddr:          targetAddr, // different from evilAddr
		PlainTarget:         true,
		AllowInsecureServer: true,
	}, nil)
	go func() { _ = a.Run(ctx) }()

	waitForAgent(t, reg, 5*time.Second)

	// The server will try to tunnel to evilAddr, but the agent should
	// refuse because it doesn't match the agent's configured targetAddr.
	resp, err := http.Get(ts.URL + "/tunnel/victim/test")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusOK && strings.Contains(string(body), "SSRF_SUCCESS") {
		t.Fatal("SSRF succeeded: agent connected to non-target address")
	}
	// Should get a 502 because the agent refused to dial.
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status: got %d, want 502", resp.StatusCode)
	}
}

func TestUpgradeRequestProxy(t *testing.T) {
	// Start an HTTP target that handles upgrade requests: it responds with
	// 101 Switching Protocols and then echoes data bidirectionally.
	upgradeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	upgradeAddr := upgradeLn.Addr().String()
	upgradeSrv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.EqualFold(r.Header.Get("Connection"), "upgrade") {
				http.Error(w, "expected upgrade", http.StatusBadRequest)
				return
			}
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "hijack not supported", http.StatusInternalServerError)
				return
			}
			conn, buf, herr := hj.Hijack()
			if herr != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			// Write 101 response.
			_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: SPDY/3.1\r\n\r\n")
			_ = buf.Flush()
			// Echo loop.
			tmp := make([]byte, 4096)
			for {
				n, readErr := conn.Read(tmp)
				if n > 0 {
					_, _ = conn.Write(tmp[:n])
				}
				if readErr != nil {
					return
				}
			}
		}),
	}
	go func() { _ = upgradeSrv.Serve(upgradeLn) }()
	defer func() { _ = upgradeSrv.Close() }()

	clusters := []server.ClusterConfig{
		{ID: "upgrade-test", Token: "tok", TargetAddr: upgradeAddr},
	}
	reg := server.NewRegistry(clusters)
	srv := server.New(reg, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/connect"
	a, aerr := agent.New(agent.Config{
		ServerURL:           wsURL,
		Token:               "tok",
		TargetAddr:          upgradeAddr,
		PlainTarget:         true,
		AllowInsecureServer: true,
	}, nil)
	if aerr != nil {
		t.Fatalf("new agent: %v", aerr)
	}
	go func() { _ = a.Run(ctx) }()
	waitForAgent(t, reg, 5*time.Second)

	// Send an upgrade request through the proxy.
	proxyURL := strings.TrimPrefix(ts.URL, "http://")
	conn, dialErr := net.DialTimeout("tcp", proxyURL, 5*time.Second)
	if dialErr != nil {
		t.Fatalf("dial proxy: %v", dialErr)
	}
	defer func() { _ = conn.Close() }()

	// Write raw HTTP upgrade request.
	reqStr := "GET /tunnel/upgrade-test/stream HTTP/1.1\r\n" +
		"Host: " + proxyURL + "\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: SPDY/3.1\r\n" +
		"\r\n"
	if _, werr := conn.Write([]byte(reqStr)); werr != nil {
		t.Fatalf("write upgrade request: %v", werr)
	}

	// Read the response — should contain 101.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	respBuf := make([]byte, 4096)
	n, rerr := conn.Read(respBuf)
	if rerr != nil {
		t.Fatalf("read upgrade response: %v", rerr)
	}
	resp := string(respBuf[:n])
	if !strings.Contains(resp, "101") {
		t.Fatalf("expected 101 response, got: %q", resp)
	}

	// Send data and verify echo.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	payload := []byte("hello-upgrade-test")
	if _, werr := conn.Write(payload); werr != nil {
		t.Fatalf("write payload: %v", werr)
	}
	echo := make([]byte, len(payload))
	if _, rerr = io.ReadFull(conn, echo); rerr != nil {
		t.Fatalf("read echo: %v", rerr)
	}
	if string(echo) != string(payload) {
		t.Errorf("echo mismatch: got %q, want %q", string(echo), string(payload))
	}
}

// TestPublicHandlerDoesNotServeTunnel verifies that PublicHandler does not
// expose /tunnel/ or /metrics endpoints.
func TestPublicHandlerDoesNotServeTunnel(t *testing.T) {
	reg := server.NewRegistry(nil)
	srv := server.New(reg, nil)
	ts := httptest.NewServer(srv.PublicHandler())
	defer ts.Close()

	for _, path := range []string{"/tunnel/c1/api/v1/pods", "/metrics", "/readyz"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("request %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("PublicHandler %s: got %d, want 404", path, resp.StatusCode)
		}
	}

	// /healthz should still work.
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("PublicHandler /healthz: got %d, want 200", resp.StatusCode)
	}
}

// TestInternalHandlerDoesNotServeConnect verifies that InternalHandler does
// not expose the /connect WebSocket endpoint.
func TestInternalHandlerDoesNotServeConnect(t *testing.T) {
	reg := server.NewRegistry(nil)
	srv := server.New(reg, nil)
	ts := httptest.NewServer(srv.InternalHandler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/connect")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("InternalHandler /connect: got %d, want 404", resp.StatusCode)
	}

	// /healthz, /readyz, /metrics should work on internal handler.
	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("request %s: %v", path, err)
		}
		_ = resp.Body.Close()
		// 200 or 503 (readyz with no clusters) — just not 404.
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("InternalHandler %s: got 404, expected it to be served", path)
		}
	}
}

func writeFile(path, content string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	_, err = f.WriteString(content)
	if err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// generateCA creates a self-signed CA certificate and key.
func generateCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	return cert, key
}

// generateCert creates a certificate signed by the given CA.
func generateCert(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, isServer bool) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:     []string{"localhost"},
	}
	if isServer {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}, parsed
}

// writePEM writes a PEM-encoded file.
func writePEM(t *testing.T, dir, name, pemType string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	if err := pem.Encode(f, &pem.Block{Type: pemType, Bytes: data}); err != nil {
		_ = f.Close()
		t.Fatalf("encode %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
	return path
}

// writeKeyPEM writes an EC private key as PEM.
func writeKeyPEM(t *testing.T, dir, name string, key *ecdsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return writePEM(t, dir, name, "EC PRIVATE KEY", der)
}

func TestMTLSEndToEnd(t *testing.T) {
	// --- Generate CA, server cert, client cert ---
	caCert, caKey := generateCA(t)
	serverTLS, _ := generateCert(t, caCert, caKey, "server", true)
	clientTLS, _ := generateCert(t, caCert, caKey, "client", false)

	dir := t.TempDir()
	caCertPath := writePEM(t, dir, "ca.crt", "CERTIFICATE", caCert.Raw)
	serverCertPath := writePEM(t, dir, "server.crt", "CERTIFICATE", serverTLS.Certificate[0])
	serverKeyPath := writeKeyPEM(t, dir, "server.key", serverTLS.PrivateKey.(*ecdsa.PrivateKey))
	clientCertPath := writePEM(t, dir, "client.crt", "CERTIFICATE", clientTLS.Certificate[0])
	clientKeyPath := writeKeyPEM(t, dir, "client.key", clientTLS.PrivateKey.(*ecdsa.PrivateKey))

	// --- Start echo target ---
	targetAddr, targetCleanup := startHTTPEchoTarget(t)
	defer targetCleanup()

	// --- Start server with mTLS ---
	clusters := []server.ClusterConfig{
		{ID: "mtls-cluster", Token: "mtls-token", TargetAddr: targetAddr},
	}
	reg := server.NewRegistry(clusters)
	srv := server.New(reg, nil)

	tc := server.TLSConfig{
		CertFile: serverCertPath,
		KeyFile:  serverKeyPath,
		CAFile:   caCertPath,
	}

	// Pick a free port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srvAddr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServeTLS(ctx, srvAddr, srv.Handler(), tc) }()

	// Wait for server to be ready.
	time.Sleep(200 * time.Millisecond)

	// --- Connect agent with client cert ---
	wsURL := "wss://" + srvAddr + "/connect"
	a, err := agent.New(agent.Config{
		ServerURL:        wsURL,
		Token:            "mtls-token",
		TargetAddr:       targetAddr,
		PlainTarget:      true,
		ServerCACertPath: caCertPath,
		ClientCertPath:   clientCertPath,
		ClientKeyPath:    clientKeyPath,
	}, nil)
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	agentCtx, agentCancel := context.WithCancel(ctx)
	defer agentCancel()
	go func() { _ = a.Run(agentCtx) }()

	waitForAgent(t, reg, 5*time.Second)

	// --- Proxy request through mTLS server ---
	caPool := x509.NewCertPool()
	caPool.AddCert(caCert)
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:      caPool,
				Certificates: []tls.Certificate{clientTLS},
				MinVersion:   tls.VersionTLS12,
			},
		},
	}

	resp, err := httpClient.Get("https://" + srvAddr + "/tunnel/mtls-cluster/api/v1/pods")
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want 200; body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "path=/api/v1/pods") {
		t.Errorf("body: got %q, want to contain path=/api/v1/pods", string(body))
	}

	// --- Verify connection without client cert is rejected ---
	noClientCertHTTP := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    caPool,
				MinVersion: tls.VersionTLS12,
			},
		},
	}

	_, err = noClientCertHTTP.Get("https://" + srvAddr + "/tunnel/mtls-cluster/api/v1/pods")
	if err == nil {
		t.Fatal("expected TLS handshake error without client cert, got nil")
	}
	// The error should be a TLS-related error (remote error: tls: certificate required
	// or similar). Any connection error here is acceptable.
	t.Logf("correctly rejected without client cert: %v", err)
}
