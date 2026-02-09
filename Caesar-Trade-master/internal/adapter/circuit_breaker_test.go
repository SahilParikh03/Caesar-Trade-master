package adapter

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeClock provides a controllable time source for tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t} }

func (fc *fakeClock) Now() time.Time {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.now
}

func (fc *fakeClock) Advance(d time.Duration) {
	fc.mu.Lock()
	fc.now = fc.now.Add(d)
	fc.mu.Unlock()
}

func newTestBreaker(clock *fakeClock) (*CircuitBreaker, chan BookUpdate) {
	feed := make(chan BookUpdate, 64)
	cfg := CircuitBreakerConfig{
		StaleThreshold: 1000 * time.Millisecond,
		CoolOff:        2 * time.Second,
		PollInterval:   50 * time.Millisecond,
	}
	cb := NewCircuitBreaker(cfg, feed)
	cb.nowFunc = clock.Now
	return cb, feed
}

func TestCircuitBreaker_HeartbeatFailure(t *testing.T) {
	clock := newFakeClock(time.Now())
	cb, feed := newTestBreaker(clock)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go cb.Run(ctx)

	// Create a WSClient with its circuit forced open.
	ws := &WSClient{}
	ws.circuit.Store(int32(CircuitOpen))
	cb.WatchConnection(ExchangePolymarket, ws)

	// Send a fresh update so the market exists.
	feed <- BookUpdate{
		Exchange: ExchangePolymarket,
		MarketID: "mkt-1",
		Timestamp: clock.Now(),
	}
	time.Sleep(50 * time.Millisecond)

	// CanTrade should be false because the WSClient circuit is Open.
	if cb.CanTrade(ExchangePolymarket, "mkt-1") {
		t.Fatal("expected CanTrade=false when WSClient circuit is Open")
	}

	// Restore the connection.
	ws.circuit.Store(int32(CircuitClosed))

	// Still blocked by cool-off (market was unhealthy → healthy transition).
	// The initial update triggered a recovery timestamp. We haven't advanced
	// past cool-off yet.
	if cb.CanTrade(ExchangePolymarket, "mkt-1") {
		t.Fatal("expected CanTrade=false during cool-off")
	}

	// Advance past cool-off.
	clock.Advance(3 * time.Second)

	// Send a fresh update at the new time.
	feed <- BookUpdate{
		Exchange:  ExchangePolymarket,
		MarketID:  "mkt-1",
		Timestamp: clock.Now(),
	}
	time.Sleep(50 * time.Millisecond)

	if !cb.CanTrade(ExchangePolymarket, "mkt-1") {
		t.Fatal("expected CanTrade=true after connection restored + cool-off + fresh data")
	}
}

func TestCircuitBreaker_StaleData(t *testing.T) {
	clock := newFakeClock(time.Now())
	cb, feed := newTestBreaker(clock)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go cb.Run(ctx)

	// Send a fresh update.
	feed <- BookUpdate{
		Exchange:  ExchangeKalshi,
		MarketID:  "FED-DEC",
		Timestamp: clock.Now(),
	}
	time.Sleep(50 * time.Millisecond)

	// Advance past cool-off so that doesn't interfere.
	clock.Advance(3 * time.Second)

	// Send another fresh update at the new time.
	feed <- BookUpdate{
		Exchange:  ExchangeKalshi,
		MarketID:  "FED-DEC",
		Timestamp: clock.Now(),
	}
	time.Sleep(50 * time.Millisecond)

	// Data is fresh — should be tradeable.
	if !cb.CanTrade(ExchangeKalshi, "FED-DEC") {
		t.Fatal("expected CanTrade=true for fresh data")
	}

	// Advance time past the stale threshold without sending new data.
	clock.Advance(1500 * time.Millisecond)

	if cb.CanTrade(ExchangeKalshi, "FED-DEC") {
		t.Fatal("expected CanTrade=false for stale data (1500ms since last update)")
	}
}

func TestCircuitBreaker_CoolOff(t *testing.T) {
	clock := newFakeClock(time.Now())
	cb, feed := newTestBreaker(clock)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go cb.Run(ctx)

	// Mark the market as stale, then send a fresh update to trigger recovery.
	feed <- BookUpdate{
		Exchange:  ExchangePolymarket,
		MarketID:  "mkt-cool",
		Timestamp: clock.Now(),
	}
	time.Sleep(50 * time.Millisecond)

	// Force the market unhealthy.
	cb.MarkStale(ExchangePolymarket, "mkt-cool")

	// Advance a bit, then send a new update → triggers recovery.
	clock.Advance(100 * time.Millisecond)
	feed <- BookUpdate{
		Exchange:  ExchangePolymarket,
		MarketID:  "mkt-cool",
		Timestamp: clock.Now(),
	}
	time.Sleep(50 * time.Millisecond)

	// Data is fresh but cool-off hasn't elapsed.
	if cb.CanTrade(ExchangePolymarket, "mkt-cool") {
		t.Fatal("expected CanTrade=false during cool-off period")
	}

	// Advance past the 2s cool-off.
	clock.Advance(2100 * time.Millisecond)

	// Send another fresh update.
	feed <- BookUpdate{
		Exchange:  ExchangePolymarket,
		MarketID:  "mkt-cool",
		Timestamp: clock.Now(),
	}
	time.Sleep(50 * time.Millisecond)

	if !cb.CanTrade(ExchangePolymarket, "mkt-cool") {
		t.Fatal("expected CanTrade=true after cool-off elapsed + fresh data")
	}
}

func TestCircuitBreaker_ManualHalt(t *testing.T) {
	clock := newFakeClock(time.Now())
	cb, feed := newTestBreaker(clock)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go cb.Run(ctx)

	// Establish a healthy market.
	feed <- BookUpdate{
		Exchange:  ExchangePolymarket,
		MarketID:  "mkt-halt",
		Timestamp: clock.Now(),
	}
	time.Sleep(50 * time.Millisecond)

	// Advance past cool-off.
	clock.Advance(3 * time.Second)
	feed <- BookUpdate{
		Exchange:  ExchangePolymarket,
		MarketID:  "mkt-halt",
		Timestamp: clock.Now(),
	}
	time.Sleep(50 * time.Millisecond)

	if !cb.CanTrade(ExchangePolymarket, "mkt-halt") {
		t.Fatal("expected CanTrade=true before halt")
	}

	cb.ManualHalt()
	if cb.CanTrade(ExchangePolymarket, "mkt-halt") {
		t.Fatal("expected CanTrade=false after ManualHalt")
	}

	cb.Resume()
	if !cb.CanTrade(ExchangePolymarket, "mkt-halt") {
		t.Fatal("expected CanTrade=true after Resume")
	}
}
