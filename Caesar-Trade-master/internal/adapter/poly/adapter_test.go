package poly

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caesar-terminal/caesar/internal/adapter"
	"github.com/gorilla/websocket"
)

// captureServer upgrades to WS and captures the first message sent by the
// client, making it available via the returned channel.
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
		// Keep connection alive so client doesn't reconnect.
		select {}
	}))
	return srv, captured
}

func wsURL(s *httptest.Server) string {
	return "ws" + strings.TrimPrefix(s.URL, "http")
}

func TestPolyAdapter_SubscriptionMessage(t *testing.T) {
	srv, captured := captureServer(t)
	defer srv.Close()

	cfg := adapter.DefaultWSConfig(wsURL(srv))
	cfg.HeartbeatTimeout = 5 * time.Second // don't timeout during test
	ws := adapter.NewWSClient(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := ws.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer ws.Close()

	pa := New(ws)
	pa.Subscribe("71321045679252212594626385532706912750332728571942532289631379312455583992563")

	select {
	case raw := <-captured:
		var msg subscribeMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("unmarshal subscription: %v", err)
		}
		if msg.Type != "market" {
			t.Fatalf("expected type 'market', got %q", msg.Type)
		}
		if len(msg.AssetsIDs) != 1 {
			t.Fatalf("expected 1 asset ID, got %d", len(msg.AssetsIDs))
		}
		if msg.AssetsIDs[0] != "71321045679252212594626385532706912750332728571942532289631379312455583992563" {
			t.Fatalf("wrong asset ID: %s", msg.AssetsIDs[0])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for subscription message")
	}
}

func TestPolyAdapter_ParseBookEvent(t *testing.T) {
	// Spin up an echo-like server that sends a canned book event.
	bookJSON := `{
		"event_type": "book",
		"asset_id": "65818619657568813474341868652308942079804919287380422192892211131408793125422",
		"market": "0xbd31dc8a20211944f6b70f31557f1001557b59905b7738480ca09bd4532f84af",
		"bids": [
			{"price": ".48", "size": "30"},
			{"price": ".49", "size": "20"}
		],
		"asks": [
			{"price": ".52", "size": "25"},
			{"price": ".53", "size": "60"}
		],
		"timestamp": "1700000000000",
		"hash": "0xabc123"
	}`

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.WriteMessage(websocket.TextMessage, []byte(bookJSON))
		// Keep alive.
		select {}
	}))
	defer srv.Close()

	cfg := adapter.DefaultWSConfig(wsURL(srv))
	cfg.HeartbeatTimeout = 5 * time.Second
	ws := adapter.NewWSClient(cfg)

	// Create adapter before Connect so the fan-out subscriber is
	// registered before any messages arrive from the server.
	pa := New(ws)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := ws.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer ws.Close()

	go pa.Run(ctx)

	select {
	case update := <-pa.Updates():
		if update.Exchange != adapter.ExchangePolymarket {
			t.Fatalf("expected exchange polymarket, got %s", update.Exchange)
		}
		if update.MarketID != "0xbd31dc8a20211944f6b70f31557f1001557b59905b7738480ca09bd4532f84af" {
			t.Fatalf("wrong market ID: %s", update.MarketID)
		}
		if update.AssetID != "65818619657568813474341868652308942079804919287380422192892211131408793125422" {
			t.Fatalf("wrong asset ID: %s", update.AssetID)
		}
		if update.Hash != "0xabc123" {
			t.Fatalf("wrong hash: %s", update.Hash)
		}

		// Verify bids.
		if len(update.Bids) != 2 {
			t.Fatalf("expected 2 bids, got %d", len(update.Bids))
		}
		assertLevel(t, "bid[0]", update.Bids[0], 0.48, 30)
		assertLevel(t, "bid[1]", update.Bids[1], 0.49, 20)

		// Verify asks.
		if len(update.Asks) != 2 {
			t.Fatalf("expected 2 asks, got %d", len(update.Asks))
		}
		assertLevel(t, "ask[0]", update.Asks[0], 0.52, 25)
		assertLevel(t, "ask[1]", update.Asks[1], 0.53, 60)

		// Verify timestamp.
		expected := time.UnixMilli(1700000000000)
		if !update.Timestamp.Equal(expected) {
			t.Fatalf("expected timestamp %v, got %v", expected, update.Timestamp)
		}

	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for BookUpdate")
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
