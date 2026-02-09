package adapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// echoServer upgrades to WS and echoes every message back.
func echoServer(t *testing.T) *httptest.Server {
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

func toWS(s *httptest.Server) string {
	return "ws" + strings.TrimPrefix(s.URL, "http")
}

func TestTunnelManager_OpenAndGet(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()

	tm := NewTunnelManager()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	tunnel, err := tm.Open(ctx, TunnelConfig{
		UserID:   "user-1",
		Exchange: ExchangePolymarket,
		URL:      toWS(srv),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Verify we can retrieve the tunnel.
	got := tm.Get("user-1", ExchangePolymarket)
	if got == nil {
		t.Fatal("Get returned nil for active tunnel")
	}
	if got.UserID != "user-1" || got.Exchange != ExchangePolymarket {
		t.Fatalf("tunnel identity mismatch: %s / %s", got.UserID, got.Exchange)
	}

	// Verify round-trip through the private tunnel.
	tunnel.ws.Send([]byte("private-hello"))
	select {
	case msg := <-tunnel.Messages():
		if string(msg) != "private-hello" {
			t.Fatalf("expected 'private-hello', got %q", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for echo")
	}

	// Other users shouldn't see this tunnel.
	if tm.Get("user-2", ExchangePolymarket) != nil {
		t.Fatal("user-2 should not have a tunnel")
	}

	tm.CloseAll()
}

func TestTunnelManager_CloseCleanup(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()

	tm := NewTunnelManager()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	tunnel, err := tm.Open(ctx, TunnelConfig{
		UserID:   "user-1",
		Exchange: ExchangeKalshi,
		URL:      toWS(srv),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	msgs := tunnel.Messages()

	// Close the tunnel.
	tm.Close("user-1", ExchangeKalshi)

	// Get should return nil after close.
	if tm.Get("user-1", ExchangeKalshi) != nil {
		t.Fatal("tunnel still present after Close")
	}

	// The messages channel should be closed by the WSClient teardown.
	select {
	case _, ok := <-msgs:
		if ok {
			// Draining residual is fine; we just need it to close eventually.
		}
	case <-time.After(time.Second):
		t.Fatal("messages channel not closed after tunnel shutdown")
	}

	// Send should fail for a closed tunnel.
	if err := tm.Send("user-1", ExchangeKalshi, []byte("test")); err == nil {
		t.Fatal("expected error sending to closed tunnel")
	}
}

func TestTunnelManager_DataIsolation(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()

	tm := NewTunnelManager()
	defer tm.CloseAll()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	t1, err := tm.Open(ctx, TunnelConfig{
		UserID:   "alice",
		Exchange: ExchangePolymarket,
		URL:      toWS(srv),
	})
	if err != nil {
		t.Fatalf("Open alice: %v", err)
	}

	t2, err := tm.Open(ctx, TunnelConfig{
		UserID:   "bob",
		Exchange: ExchangePolymarket,
		URL:      toWS(srv),
	})
	if err != nil {
		t.Fatalf("Open bob: %v", err)
	}

	// Send on alice's tunnel.
	t1.ws.Send([]byte("alice-secret"))

	// Alice should receive her echo.
	select {
	case msg := <-t1.Messages():
		if string(msg) != "alice-secret" {
			t.Fatalf("alice got wrong message: %q", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("alice timed out")
	}

	// Bob should NOT receive alice's message.
	select {
	case msg := <-t2.Messages():
		t.Fatalf("bob received alice's message: %q", msg)
	case <-time.After(200 * time.Millisecond):
		// Good — isolated.
	}
}
