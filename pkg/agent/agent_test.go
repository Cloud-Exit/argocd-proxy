package agent_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloud-exit/argocd-cluster-proxy/pkg/agent"
	"github.com/cloud-exit/argocd-cluster-proxy/pkg/server"
	"github.com/cloud-exit/argocd-cluster-proxy/pkg/tunnel"
	"github.com/gorilla/websocket"
)

func TestAgentRejectsPlaintextByDefault(t *testing.T) {
	_, err := agent.New(agent.Config{
		ServerURL:   "ws://localhost:8080/connect",
		Token:       "tok",
		PlainTarget: true,
	}, nil)
	if err == nil {
		t.Fatal("expected error for plaintext ws:// URL without AllowInsecureServer")
	}
	if !strings.Contains(err.Error(), "plaintext") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestAgentReconnect(t *testing.T) {
	var connectCount int
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token != "test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connectCount++
		if connectCount == 1 {
			ws.Close()
			return
		}
		sess := tunnel.NewSession(ws, nil)
		sess.Serve(r.Context())
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a, err := agent.New(agent.Config{
		ServerURL:           "ws" + strings.TrimPrefix(srv.URL, "http") + "/connect",
		Token:               "test-token",
		PlainTarget:         true,
		AllowInsecureServer: true,
		MaxRetryInterval:    1 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	go a.Run(ctx)

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if connectCount >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if connectCount < 2 {
		t.Errorf("expected at least 2 connections, got %d", connectCount)
	}
	cancel()
}

func TestAgentDialsLocalTarget(t *testing.T) {
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()

	go func() {
		for {
			conn, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				n, _ := c.Read(buf)
				_, _ = c.Write(buf[:n])
			}(conn)
		}
	}()

	clusters := []server.ClusterConfig{
		{ID: "test", Token: "tok", TargetAddr: echoLn.Addr().String()},
	}
	reg := server.NewRegistry(clusters)
	proxyServer := server.New(reg, nil)
	ts := httptest.NewServer(proxyServer.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a, _ := agent.New(agent.Config{
		ServerURL:           "ws" + strings.TrimPrefix(ts.URL, "http") + "/connect",
		Token:               "tok",
		TargetAddr:          echoLn.Addr().String(),
		PlainTarget:         true,
		AllowInsecureServer: true,
	}, nil)

	go a.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(reg.Connected()) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	cluster := reg.Get("test")
	if cluster == nil {
		t.Fatal("cluster not connected")
	}

	cancel()
}
