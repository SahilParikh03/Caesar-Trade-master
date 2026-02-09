package adapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newTestServer returns an httptest.Server that upgrades to WebSocket and
// echoes every message back to the client.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	}))
}

func wsURL(s *httptest.Server) string {
	return "ws" + strings.TrimPrefix(s.URL, "http")
}

func TestWSClient_Connect(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	cfg := DefaultWSConfig(wsURL(srv))
	client := NewWSClient(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()

	if client.Circuit() != CircuitClosed {
		t.Fatalf("expected CircuitClosed after connect, got %d", client.Circuit())
	}

	// Verify round-trip: subscribe, send, receive.
	sub := client.Subscribe()
	client.Send([]byte("hello"))

	select {
	case msg := <-sub:
		if string(msg) != "hello" {
			t.Fatalf("expected 'hello', got %q", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for echoed message")
	}
}

func TestWSClient_Reconnect(t *testing.T) {
	srv := newTestServer(t)

	cfg := DefaultWSConfig(wsURL(srv))
	cfg.HeartbeatTimeout = 200 * time.Millisecond
	cfg.BackoffInitial = 50 * time.Millisecond

	var reconnects atomic.Int32
	client := NewWSClient(cfg)
	client.onReconnect = func() { reconnects.Add(1) }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()

	// Kill the server to break the connection.
	srv.Close()

	// Wait for the client to detect the drop and open the circuit.
	time.Sleep(400 * time.Millisecond)
	if client.Circuit() != CircuitOpen {
		t.Fatal("expected CircuitOpen after server close")
	}

	// Start a new server on the same address so reconnect succeeds.
	// Since we can't reuse the port exactly, start a new server and
	// update the URL.
	srv2 := newTestServer(t)
	defer srv2.Close()

	client.mu.Lock()
	client.cfg.URL = wsURL(srv2)
	client.mu.Unlock()

	// Wait for reconnect to succeed.
	deadline := time.After(3 * time.Second)
	for {
		if reconnects.Load() > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for reconnect")
		case <-time.After(50 * time.Millisecond):
		}
	}

	if client.Circuit() != CircuitClosed {
		t.Fatal("expected CircuitClosed after reconnect")
	}
}

func TestWSClient_HeartbeatTimeout(t *testing.T) {
	// Create a server that accepts the connection but never sends anything.
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		// Block forever — never send data.
		select {}
	}))
	defer srv.Close()

	cfg := DefaultWSConfig(wsURL(srv))
	cfg.HeartbeatTimeout = 200 * time.Millisecond
	cfg.BackoffInitial = 50 * time.Millisecond

	client := NewWSClient(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()

	// The silent server should trigger a heartbeat timeout and reconnect.
	// The circuit should transition to Open.
	deadline := time.After(2 * time.Second)
	for {
		if client.Circuit() == CircuitOpen {
			break
		}
		select {
		case <-deadline:
			t.Fatal("heartbeat timeout did not trigger circuit open")
		case <-time.After(50 * time.Millisecond):
		}
	}
}
