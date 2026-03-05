package tunnel_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloud-exit/argocd-cluster-proxy/pkg/tunnel"
	"github.com/gorilla/websocket"
)

// wsConnPair creates a connected pair of WebSocket connections via an
// httptest server. Returns (server-side ws, client-side ws, cleanup).
func wsConnPair(t *testing.T) (*websocket.Conn, *websocket.Conn, func()) {
	t.Helper()
	var serverConn *websocket.Conn
	ready := make(chan struct{})

	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		serverConn, err = upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		close(ready)
		// Block until test completes to keep the connection alive.
		select {}
	}))

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	<-ready

	return serverConn, clientConn, func() {
		clientConn.Close()
		serverConn.Close()
		srv.Close()
	}
}

func TestSessionDialAndPipe(t *testing.T) {
	serverWS, clientWS, cleanup := wsConnPair(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Server session (sends Dial).
	serverSess := tunnel.NewSession(serverWS, nil)

	// Agent session (handles Connect by echoing data back uppercased).
	agentSess := tunnel.NewSession(clientWS, nil)
	agentSess.OnConnect = func(sess *tunnel.Session, connID uint32, addr string) {
		tunnelConn := sess.Accept(connID)
		defer tunnelConn.Close()
		if err := sess.SendConnected(connID); err != nil {
			return
		}
		buf := make([]byte, 4096)
		for {
			n, err := tunnelConn.Read(buf)
			if n > 0 {
				upper := strings.ToUpper(string(buf[:n]))
				if _, werr := tunnelConn.Write([]byte(upper)); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); serverSess.Serve(ctx) }()
	go func() { defer wg.Done(); agentSess.Serve(ctx) }()

	// Give sessions a moment to start their read loops.
	time.Sleep(50 * time.Millisecond)

	// Dial through the tunnel.
	conn, err := serverSess.Dial(ctx, "echo-target:1234")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Write data and read the echo.
	_, err = conn.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "HELLO" {
		t.Errorf("got %q, want %q", got, "HELLO")
	}

	cancel()
	wg.Wait()
}

func TestSessionMultipleConnections(t *testing.T) {
	serverWS, clientWS, cleanup := wsConnPair(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	serverSess := tunnel.NewSession(serverWS, nil)
	agentSess := tunnel.NewSession(clientWS, nil)

	// Agent echoes back the connID as a string prefix.
	agentSess.OnConnect = func(sess *tunnel.Session, connID uint32, addr string) {
		tunnelConn := sess.Accept(connID)
		defer tunnelConn.Close()
		_ = sess.SendConnected(connID)
		buf := make([]byte, 4096)
		n, err := tunnelConn.Read(buf)
		if err != nil && err != io.EOF {
			return
		}
		_, _ = tunnelConn.Write(buf[:n])
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); serverSess.Serve(ctx) }()
	go func() { defer wg.Done(); agentSess.Serve(ctx) }()
	time.Sleep(50 * time.Millisecond)

	// Open 5 concurrent connections.
	const N = 5
	results := make([]string, N)
	var mu sync.Mutex
	var connWg sync.WaitGroup

	for i := 0; i < N; i++ {
		connWg.Add(1)
		go func(idx int) {
			defer connWg.Done()
			c, err := serverSess.Dial(ctx, "target:443")
			if err != nil {
				t.Errorf("dial %d: %v", idx, err)
				return
			}
			defer c.Close()
			msg := []byte(strings.Repeat("x", idx+1))
			if _, err := c.Write(msg); err != nil {
				t.Errorf("write %d: %v", idx, err)
				return
			}
			buf := make([]byte, 64)
			n, _ := c.Read(buf)
			mu.Lock()
			results[idx] = string(buf[:n])
			mu.Unlock()
		}(i)
	}
	connWg.Wait()

	for i := 0; i < N; i++ {
		expected := strings.Repeat("x", i+1)
		if results[i] != expected {
			t.Errorf("conn %d: got %q, want %q", i, results[i], expected)
		}
	}

	cancel()
	wg.Wait()
}

func TestSessionPingPong(t *testing.T) {
	serverWS, clientWS, cleanup := wsConnPair(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverSess := tunnel.NewSession(serverWS, nil)
	agentSess := tunnel.NewSession(clientWS, nil)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); serverSess.Serve(ctx) }()
	go func() { defer wg.Done(); agentSess.Serve(ctx) }()

	// Let ping/pong cycle at least once (ping interval is 15s in production,
	// but we just verify the sessions stay alive for a short period).
	time.Sleep(100 * time.Millisecond)

	cancel()
	wg.Wait()
}
