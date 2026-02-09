package kalshi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/caesar-terminal/caesar/internal/adapter"
	"github.com/gorilla/websocket"
)

// generateTestKey creates an RSA key pair and returns the PEM-encoded private key.
func generateTestKey(t *testing.T) ([]byte, *rsa.PublicKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	return pemBytes, &priv.PublicKey
}

func TestAuthHeaders(t *testing.T) {
	pemKey, _ := generateTestKey(t)

	headers, err := AuthHeaders("test-api-key", pemKey)
	if err != nil {
		t.Fatalf("AuthHeaders: %v", err)
	}

	if headers.Get("KALSHI-ACCESS-KEY") != "test-api-key" {
		t.Fatalf("expected API key 'test-api-key', got %q", headers.Get("KALSHI-ACCESS-KEY"))
	}
	if headers.Get("KALSHI-ACCESS-TIMESTAMP") == "" {
		t.Fatal("missing KALSHI-ACCESS-TIMESTAMP")
	}
	if headers.Get("KALSHI-ACCESS-SIGNATURE") == "" {
		t.Fatal("missing KALSHI-ACCESS-SIGNATURE")
	}
}

// captureServer upgrades to WS and captures the first client message.
func captureServer(t *testing.T) (*httptest.Server, <-chan []byte) {
	t.Helper()
	captured := make(chan []byte, 1)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, msg, err := c.ReadMessage()
		if err != nil {
			return
		}
		captured <- msg
		select {}
	}))
	return srv, captured
}

func wsURL(s *httptest.Server) string {
	return "ws" + strings.TrimPrefix(s.URL, "http")
}

func TestKalshiAdapter_SubscriptionMessage(t *testing.T) {
	srv, captured := captureServer(t)
	defer srv.Close()

	cfg := adapter.DefaultWSConfig(wsURL(srv))
	cfg.HeartbeatTimeout = 5 * time.Second
	ws := adapter.NewWSClient(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := ws.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer ws.Close()

	ka := New(ws)
	ka.Subscribe("FED-23DEC-T3.00")

	select {
	case raw := <-captured:
		var cmd command
		if err := json.Unmarshal(raw, &cmd); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if cmd.Cmd != "subscribe" {
			t.Fatalf("expected cmd 'subscribe', got %q", cmd.Cmd)
		}
		if len(cmd.Params.Channels) != 1 || cmd.Params.Channels[0] != "orderbook_delta" {
			t.Fatalf("expected channels ['orderbook_delta'], got %v", cmd.Params.Channels)
		}
		if cmd.Params.MarketTicker != "FED-23DEC-T3.00" {
			t.Fatalf("expected ticker 'FED-23DEC-T3.00', got %q", cmd.Params.MarketTicker)
		}
		if cmd.ID != 1 {
			t.Fatalf("expected id 1, got %d", cmd.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for subscription message")
	}
}

func TestKalshiAdapter_ParseSnapshot(t *testing.T) {
	snapshotJSON := `{
		"type": "orderbook_snapshot",
		"sid": 2,
		"seq": 1,
		"msg": {
			"market_ticker": "FED-23DEC-T3.00",
			"market_id": "9b0f6b43-5b68-4f9f-9f02-9a2d1b8ac1a1",
			"yes": [[48, 300], [52, 150]],
			"no": [[54, 200], [60, 100]]
		}
	}`

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.WriteMessage(websocket.TextMessage, []byte(snapshotJSON))
		select {}
	}))
	defer srv.Close()

	cfg := adapter.DefaultWSConfig(wsURL(srv))
	cfg.HeartbeatTimeout = 5 * time.Second
	ws := adapter.NewWSClient(cfg)

	// Create adapter before Connect so the fan-out subscriber is
	// registered before any messages arrive from the server.
	ka := New(ws)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := ws.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer ws.Close()

	go ka.Run(ctx)

	select {
	case update := <-ka.Updates():
		if update.Exchange != adapter.ExchangeKalshi {
			t.Fatalf("expected exchange kalshi, got %s", update.Exchange)
		}
		if update.MarketID != "9b0f6b43-5b68-4f9f-9f02-9a2d1b8ac1a1" {
			t.Fatalf("wrong market ID: %s", update.MarketID)
		}
		if update.AssetID != "FED-23DEC-T3.00" {
			t.Fatalf("wrong asset ID: %s", update.AssetID)
		}

		// Verify bids (YES side), normalised from cents to 0-1.
		sort.Slice(update.Bids, func(i, j int) bool {
			return update.Bids[i].Price < update.Bids[j].Price
		})
		if len(update.Bids) != 2 {
			t.Fatalf("expected 2 bids, got %d", len(update.Bids))
		}
		assertLevel(t, "bid[0]", update.Bids[0], 0.48, 300)
		assertLevel(t, "bid[1]", update.Bids[1], 0.52, 150)

		// Verify asks (NO side), normalised from cents to 0-1.
		sort.Slice(update.Asks, func(i, j int) bool {
			return update.Asks[i].Price < update.Asks[j].Price
		})
		if len(update.Asks) != 2 {
			t.Fatalf("expected 2 asks, got %d", len(update.Asks))
		}
		assertLevel(t, "ask[0]", update.Asks[0], 0.54, 200)
		assertLevel(t, "ask[1]", update.Asks[1], 0.60, 100)

	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for BookUpdate")
	}
}

func TestKalshiAdapter_ParseDelta(t *testing.T) {
	// Server sends a snapshot then a delta.
	snapshotJSON := `{
		"type": "orderbook_snapshot",
		"sid": 2, "seq": 1,
		"msg": {
			"market_ticker": "FED-23DEC-T3.00",
			"market_id": "9b0f6b43",
			"yes": [[48, 300]],
			"no": [[54, 200]]
		}
	}`
	deltaJSON := `{
		"type": "orderbook_delta",
		"sid": 2, "seq": 2,
		"msg": {
			"market_ticker": "FED-23DEC-T3.00",
			"market_id": "9b0f6b43",
			"price": 48,
			"delta": -100,
			"side": "yes",
			"ts": "2024-01-15T10:30:00Z"
		}
	}`

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.WriteMessage(websocket.TextMessage, []byte(snapshotJSON))
		time.Sleep(50 * time.Millisecond)
		c.WriteMessage(websocket.TextMessage, []byte(deltaJSON))
		select {}
	}))
	defer srv.Close()

	cfg := adapter.DefaultWSConfig(wsURL(srv))
	cfg.HeartbeatTimeout = 5 * time.Second
	ws := adapter.NewWSClient(cfg)

	ka := New(ws)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := ws.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer ws.Close()

	go ka.Run(ctx)

	// First update is the snapshot.
	<-ka.Updates()

	// Second update is after the delta is applied.
	select {
	case update := <-ka.Updates():
		if len(update.Bids) != 1 {
			t.Fatalf("expected 1 bid after delta, got %d", len(update.Bids))
		}
		// 300 - 100 = 200 remaining at 48 cents.
		assertLevel(t, "bid[0]", update.Bids[0], 0.48, 200)

		// Asks (NO side) unchanged.
		if len(update.Asks) != 1 {
			t.Fatalf("expected 1 ask, got %d", len(update.Asks))
		}
		assertLevel(t, "ask[0]", update.Asks[0], 0.54, 200)

	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delta BookUpdate")
	}
}

func assertLevel(t *testing.T, name string, got adapter.PriceLevel, wantPrice, wantSize float64) {
	t.Helper()
	if math.Abs(got.Price-wantPrice) > 1e-9 {
		t.Errorf("%s price: want %f, got %f", name, wantPrice, got.Price)
	}
	if math.Abs(got.Size-wantSize) > 1e-9 {
		t.Errorf("%s size: want %f, got %f", name, wantSize, got.Size)
	}
}
