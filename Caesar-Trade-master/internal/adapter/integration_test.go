package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// controlledServer is a WS server that lets the test push messages at will.
type controlledServer struct {
	srv    *httptest.Server
	connMu sync.Mutex
	conn   *websocket.Conn
	ready  chan struct{}
}

func newControlledServer(t *testing.T) *controlledServer {
	t.Helper()
	cs := &controlledServer{ready: make(chan struct{})}
	upgrader := websocket.Upgrader{}
	cs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		cs.connMu.Lock()
		cs.conn = c
		cs.connMu.Unlock()
		close(cs.ready)
		// Hold connection open until server is closed.
		select {}
	}))
	return cs
}

func (cs *controlledServer) URL() string {
	return "ws" + strings.TrimPrefix(cs.srv.URL, "http")
}

func (cs *controlledServer) Send(t *testing.T, msg string) {
	t.Helper()
	cs.connMu.Lock()
	c := cs.conn
	cs.connMu.Unlock()
	if c == nil {
		t.Fatal("controlledServer: no client connected")
	}
	if err := c.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("controlledServer.Send: %v", err)
	}
}

func (cs *controlledServer) Close() { cs.srv.Close() }

// polyBookJSON builds a Polymarket book event with the given prices.
func polyBookJSON(marketID, assetID string, bidPrice, askPrice float64, tsMs int64) string {
	return fmt.Sprintf(`{
		"event_type": "book",
		"asset_id": "%s",
		"market": "%s",
		"bids": [{"price": "%.2f", "size": "100"}],
		"asks": [{"price": "%.2f", "size": "100"}],
		"timestamp": "%d",
		"hash": "0xintegration"
	}`, assetID, marketID, bidPrice, askPrice, tsMs)
}

// mockRedisForIntegration records HSet calls.
type mockRedisForIntegration struct {
	mu    sync.Mutex
	calls []map[string]string // key → field values
}

func (m *mockRedisForIntegration) HSet(_ context.Context, key string, values ...any) error {
	fields := make(map[string]string)
	fields["_key"] = key
	for i := 0; i+1 < len(values); i += 2 {
		k, _ := values[i].(string)
		v, _ := values[i+1].(string)
		fields[k] = v
	}
	m.mu.Lock()
	m.calls = append(m.calls, fields)
	m.mu.Unlock()
	return nil
}

func (m *mockRedisForIntegration) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func (m *mockRedisForIntegration) last() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		return nil
	}
	return m.calls[len(m.calls)-1]
}

// polyAdapter is a minimal in-package adapter that parses Polymarket book
// events the same way the poly package does, but lives in the adapter
// package so we can use it in integration tests without an import cycle.
type polyAdapter struct {
	ws      *WSClient
	raw     <-chan []byte
	updates chan BookUpdate
}

func newPolyAdapter(ws *WSClient) *polyAdapter {
	return &polyAdapter{
		ws:      ws,
		raw:     ws.Subscribe(),
		updates: make(chan BookUpdate, 1024),
	}
}

func (pa *polyAdapter) Updates() <-chan BookUpdate { return pa.updates }

func (pa *polyAdapter) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case raw, ok := <-pa.raw:
			if !ok {
				return
			}
			pa.handle(raw)
		}
	}
}

func (pa *polyAdapter) handle(raw []byte) {
	// Minimal inline parser — avoids importing the poly package.
	type level struct {
		Price string `json:"price"`
		Size  string `json:"size"`
	}
	type book struct {
		EventType string  `json:"event_type"`
		AssetID   string  `json:"asset_id"`
		Market    string  `json:"market"`
		Bids      []level `json:"bids"`
		Asks      []level `json:"asks"`
		Timestamp string  `json:"timestamp"`
		Hash      string  `json:"hash"`
	}

	import_json := func(s string) float64 {
		var f float64
		fmt.Sscanf(s, "%f", &f)
		return f
	}
	import_ts := func(s string) time.Time {
		var ms int64
		fmt.Sscanf(s, "%d", &ms)
		return time.UnixMilli(ms)
	}

	var b book
	if err := json.Unmarshal(raw, &b); err != nil || b.EventType != "book" {
		return
	}

	bids := make([]PriceLevel, len(b.Bids))
	for i, l := range b.Bids {
		bids[i] = PriceLevel{Price: import_json(l.Price), Size: import_json(l.Size)}
	}
	asks := make([]PriceLevel, len(b.Asks))
	for i, l := range b.Asks {
		asks[i] = PriceLevel{Price: import_json(l.Price), Size: import_json(l.Size)}
	}

	select {
	case pa.updates <- BookUpdate{
		Exchange:  ExchangePolymarket,
		MarketID:  b.Market,
		AssetID:   b.AssetID,
		Bids:      bids,
		Asks:      asks,
		Timestamp: import_ts(b.Timestamp),
		Hash:      b.Hash,
	}:
	default:
	}
}

// ---------------------------------------------------------------------------
// Integration Test
// ---------------------------------------------------------------------------

func TestIntegration_Phase2_SuccessAndFailure(t *testing.T) {
	// ---------------------------------------------------------------
	// 1. Setup: mock WS server + full pipeline
	// ---------------------------------------------------------------
	server := newControlledServer(t)
	defer server.Close()

	cfg := DefaultWSConfig(server.URL())
	cfg.HeartbeatTimeout = 5 * time.Second // long timeout so we control staleness via clock
	ws := NewWSClient(cfg)

	// Create adapter before connecting (subscribe to fan-out early).
	polyAd := newPolyAdapter(ws)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := ws.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer ws.Close()

	// Wait for server to accept the connection.
	select {
	case <-server.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("server never accepted connection")
	}

	// Broadcaster: register the poly adapter.
	bc := NewBroadcaster()
	bc.Register(polyAd)

	// Kalshi mock provider (inject directly for the unified book's other leg).
	kalshiMock := newMockProvider()
	bc.Register(kalshiMock)

	// RedisWriter: mock Redis.
	redis := &mockRedisForIntegration{}
	redisFeed := bc.SubscribeAll()
	rw := NewRedisWriter(redis, redisFeed)

	// UnifiedBook: pair poly and kalshi markets.
	pair := MarketPair{
		Name:           "BTC > $100k",
		PolyMarketID:   "0xbtc100k",
		KalshiMarketID: "BTC-100K",
	}
	ub := NewUnifiedBook(bc, 0)
	ub.AddPair(pair)

	// CircuitBreaker: use a fake clock for deterministic staleness.
	clock := newFakeClock(time.Now())
	cbCfg := CircuitBreakerConfig{
		StaleThreshold: 1 * time.Second,
		CoolOff:        500 * time.Millisecond, // short for test
	}
	cbFeed := bc.SubscribeAll()
	cbr := NewCircuitBreaker(cbCfg, cbFeed)
	cbr.nowFunc = clock.Now
	cbr.WatchConnection(ExchangePolymarket, ws)

	// Start all goroutines.
	go polyAd.Run(ctx)
	go bc.Run(ctx)
	go rw.Run(ctx)
	go ub.Run(ctx)
	go cbr.Run(ctx)

	// Allow goroutines to initialise.
	time.Sleep(50 * time.Millisecond)

	// ---------------------------------------------------------------
	// 2. SUCCESS SCENARIO
	// ---------------------------------------------------------------
	t.Run("Success", func(t *testing.T) {
		nowMs := clock.Now().UnixMilli()

		// 2a. Push a Polymarket book event via the WS server.
		server.Send(t, polyBookJSON("0xbtc100k", "asset-btc", 0.55, 0.58, nowMs))

		// 2b. Push a Kalshi update directly (simulating the Kalshi adapter).
		kalshiMock.send(BookUpdate{
			Exchange:  ExchangeKalshi,
			MarketID:  "BTC-100K",
			Bids:      []PriceLevel{{Price: 0.52, Size: 200}},
			Asks:      []PriceLevel{{Price: 0.50, Size: 80}},
			Timestamp: clock.Now(),
		})

		// Wait for data to propagate through the pipeline.
		time.Sleep(200 * time.Millisecond)

		// 2c. Verify RedisWriter received the update.
		if redis.count() == 0 {
			t.Fatal("RedisWriter: no HSET calls recorded")
		}
		lastWrite := redis.last()
		if lastWrite["_key"] != "book:polymarket:0xbtc100k" &&
			lastWrite["_key"] != "book:kalshi:BTC-100K" {
			t.Fatalf("RedisWriter: unexpected key %q", lastWrite["_key"])
		}

		// 2d. Verify UnifiedBook has merged both sides.
		deadline := time.After(time.Second)
		for {
			snap, ok := ub.Snapshot("BTC > $100k")
			if ok && snap.Poly.BestBid > 0 && snap.Kalshi.BestBid > 0 {
				if snap.Poly.BestBid != 0.55 {
					t.Fatalf("unified poly bid: want 0.55, got %f", snap.Poly.BestBid)
				}
				if snap.Kalshi.BestAsk != 0.50 {
					t.Fatalf("unified kalshi ask: want 0.50, got %f", snap.Kalshi.BestAsk)
				}
				break
			}
			select {
			case <-deadline:
				t.Fatal("timed out waiting for unified book snapshot")
			case <-time.After(20 * time.Millisecond):
			}
		}

		// 2e. Verify ArbitrageEvent fired (Poly bid 0.55 > Kalshi ask 0.50).
		select {
		case ev := <-ub.Events():
			if ev.Spread < 0.04 || ev.Spread > 0.06 {
				t.Fatalf("expected spread ~0.05, got %f", ev.Spread)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for arbitrage event")
		}

		// 2f. Advance past cool-off, send fresh data, verify CanTrade = true.
		clock.Advance(1 * time.Second)

		server.Send(t, polyBookJSON("0xbtc100k", "asset-btc", 0.55, 0.58, clock.Now().UnixMilli()))
		time.Sleep(200 * time.Millisecond)

		if !cbr.CanTrade(ExchangePolymarket, "0xbtc100k") {
			t.Fatal("expected CanTrade=true for Polymarket after cool-off + fresh data")
		}
	})

	// ---------------------------------------------------------------
	// 3. FAILURE SCENARIO
	// ---------------------------------------------------------------
	t.Run("Failure_StaleData", func(t *testing.T) {
		// Advance the clock past the stale threshold without sending data.
		clock.Advance(2 * time.Second)

		if cbr.CanTrade(ExchangePolymarket, "0xbtc100k") {
			t.Fatal("expected CanTrade=false after stale threshold exceeded")
		}
	})
}
