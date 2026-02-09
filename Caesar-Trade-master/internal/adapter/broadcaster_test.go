package adapter

import (
	"context"
	"testing"
	"time"
)

// mockProvider is a simple UpdatesProvider backed by a plain channel.
type mockProvider struct {
	ch chan BookUpdate
}

func newMockProvider() *mockProvider {
	return &mockProvider{ch: make(chan BookUpdate, 64)}
}

func (m *mockProvider) Updates() <-chan BookUpdate { return m.ch }

func (m *mockProvider) send(update BookUpdate) { m.ch <- update }

func TestBroadcaster_MultipleAdapters(t *testing.T) {
	poly := newMockProvider()
	kalshi := newMockProvider()

	bc := NewBroadcaster()
	bc.Register(poly)
	bc.Register(kalshi)

	all := bc.SubscribeAll()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go bc.Run(ctx)

	// Send one update from each adapter.
	poly.send(BookUpdate{Exchange: ExchangePolymarket, MarketID: "mkt-poly-1"})
	kalshi.send(BookUpdate{Exchange: ExchangeKalshi, MarketID: "mkt-kalshi-1"})

	received := map[Exchange]bool{}
	for i := 0; i < 2; i++ {
		select {
		case u := <-all:
			received[u.Exchange] = true
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for update %d", i+1)
		}
	}

	if !received[ExchangePolymarket] {
		t.Fatal("missing polymarket update on unified stream")
	}
	if !received[ExchangeKalshi] {
		t.Fatal("missing kalshi update on unified stream")
	}
}

func TestBroadcaster_FilteredSubscribers(t *testing.T) {
	poly := newMockProvider()

	bc := NewBroadcaster()
	bc.Register(poly)

	subA := bc.Subscribe(ExchangePolymarket, "mkt-A")
	subB := bc.Subscribe(ExchangePolymarket, "mkt-B")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go bc.Run(ctx)

	// Send updates for two different markets.
	poly.send(BookUpdate{Exchange: ExchangePolymarket, MarketID: "mkt-A", AssetID: "a1"})
	poly.send(BookUpdate{Exchange: ExchangePolymarket, MarketID: "mkt-B", AssetID: "b1"})

	// subA should only receive mkt-A.
	select {
	case u := <-subA:
		if u.MarketID != "mkt-A" {
			t.Fatalf("subA got wrong market: %s", u.MarketID)
		}
	case <-time.After(time.Second):
		t.Fatal("subA: timed out")
	}

	// subB should only receive mkt-B.
	select {
	case u := <-subB:
		if u.MarketID != "mkt-B" {
			t.Fatalf("subB got wrong market: %s", u.MarketID)
		}
	case <-time.After(time.Second):
		t.Fatal("subB: timed out")
	}

	// Neither channel should have extra messages.
	select {
	case u := <-subA:
		t.Fatalf("subA received unexpected extra update: %+v", u)
	case u := <-subB:
		t.Fatalf("subB received unexpected extra update: %+v", u)
	case <-time.After(100 * time.Millisecond):
		// Good — no stray messages.
	}
}

func TestBroadcaster_SlowSubscriber(t *testing.T) {
	poly := newMockProvider()

	bc := NewBroadcaster()
	bc.Register(poly)

	// slowSub has a tiny buffer that will fill up immediately.
	slowKey := subKey{Exchange: ExchangePolymarket, MarketID: "mkt-slow"}
	slowCh := make(chan BookUpdate, 1)
	bc.mu.Lock()
	bc.subs[slowKey] = append(bc.subs[slowKey], slowCh)
	bc.mu.Unlock()

	// fastSub has a normal buffer.
	fastSub := bc.Subscribe(ExchangePolymarket, "mkt-fast")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go bc.Run(ctx)

	// Fill the slow subscriber's buffer.
	poly.send(BookUpdate{Exchange: ExchangePolymarket, MarketID: "mkt-slow", AssetID: "s1"})
	time.Sleep(50 * time.Millisecond)

	// Now send updates for both. The slow channel is full — it should drop
	// without blocking the fast subscriber.
	poly.send(BookUpdate{Exchange: ExchangePolymarket, MarketID: "mkt-slow", AssetID: "s2"})
	poly.send(BookUpdate{Exchange: ExchangePolymarket, MarketID: "mkt-fast", AssetID: "f1"})

	select {
	case u := <-fastSub:
		if u.AssetID != "f1" {
			t.Fatalf("fast subscriber got wrong update: %s", u.AssetID)
		}
	case <-time.After(time.Second):
		t.Fatal("fast subscriber was blocked by slow subscriber")
	}
}
