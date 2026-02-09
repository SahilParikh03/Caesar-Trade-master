package adapter

import (
	"context"
	"testing"
	"time"
)

func setupUnifiedBook(t *testing.T, threshold float64, pair MarketPair) (*UnifiedBook, *mockProvider, *mockProvider, context.CancelFunc) {
	t.Helper()

	poly := newMockProvider()
	kalshi := newMockProvider()

	bc := NewBroadcaster()
	bc.Register(poly)
	bc.Register(kalshi)

	ub := NewUnifiedBook(bc, threshold)
	ub.AddPair(pair)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)

	go bc.Run(ctx)
	go ub.Run(ctx)

	// Let goroutines start.
	time.Sleep(20 * time.Millisecond)

	return ub, poly, kalshi, cancel
}

var testPair = MarketPair{
	Name:           "BTC > $100k",
	PolyMarketID:   "0xbtc100k",
	KalshiMarketID: "BTC-100K",
}

func TestUnifiedBook_MergedSnapshot(t *testing.T) {
	ub, poly, kalshi, cancel := setupUnifiedBook(t, 0, testPair)
	defer cancel()

	poly.send(BookUpdate{
		Exchange: ExchangePolymarket,
		MarketID: "0xbtc100k",
		Bids:     []PriceLevel{{Price: 0.55, Size: 100}},
		Asks:     []PriceLevel{{Price: 0.58, Size: 50}},
		Timestamp: time.Now(),
	})

	kalshi.send(BookUpdate{
		Exchange: ExchangeKalshi,
		MarketID: "BTC-100K",
		Bids:     []PriceLevel{{Price: 0.52, Size: 200}},
		Asks:     []PriceLevel{{Price: 0.56, Size: 80}},
		Timestamp: time.Now(),
	})

	// Wait for both updates to propagate.
	deadline := time.After(time.Second)
	for {
		snap, ok := ub.Snapshot("BTC > $100k")
		if ok && snap.Poly.BestBid > 0 && snap.Kalshi.BestBid > 0 {
			if snap.Poly.BestBid != 0.55 {
				t.Fatalf("poly best bid: want 0.55, got %f", snap.Poly.BestBid)
			}
			if snap.Poly.BestAsk != 0.58 {
				t.Fatalf("poly best ask: want 0.58, got %f", snap.Poly.BestAsk)
			}
			if snap.Kalshi.BestBid != 0.52 {
				t.Fatalf("kalshi best bid: want 0.52, got %f", snap.Kalshi.BestBid)
			}
			if snap.Kalshi.BestAsk != 0.56 {
				t.Fatalf("kalshi best ask: want 0.56, got %f", snap.Kalshi.BestAsk)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for merged snapshot")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestUnifiedBook_ArbitrageDetected(t *testing.T) {
	ub, poly, kalshi, cancel := setupUnifiedBook(t, 0, testPair)
	defer cancel()

	// Create a crossed book: Poly bid 0.60 > Kalshi ask 0.52
	poly.send(BookUpdate{
		Exchange: ExchangePolymarket,
		MarketID: "0xbtc100k",
		Bids:     []PriceLevel{{Price: 0.60, Size: 100}},
		Asks:     []PriceLevel{{Price: 0.65, Size: 50}},
		Timestamp: time.Now(),
	})

	kalshi.send(BookUpdate{
		Exchange: ExchangeKalshi,
		MarketID: "BTC-100K",
		Bids:     []PriceLevel{{Price: 0.48, Size: 200}},
		Asks:     []PriceLevel{{Price: 0.52, Size: 80}},
		Timestamp: time.Now(),
	})

	select {
	case ev := <-ub.Events():
		if ev.Direction != ArbPolyBidKalshiAsk {
			t.Fatalf("expected ArbPolyBidKalshiAsk, got %d", ev.Direction)
		}
		if ev.BidExchange != ExchangePolymarket {
			t.Fatalf("expected bid exchange polymarket, got %s", ev.BidExchange)
		}
		if ev.AskExchange != ExchangeKalshi {
			t.Fatalf("expected ask exchange kalshi, got %s", ev.AskExchange)
		}
		if ev.Bid != 0.60 {
			t.Fatalf("expected bid 0.60, got %f", ev.Bid)
		}
		if ev.Ask != 0.52 {
			t.Fatalf("expected ask 0.52, got %f", ev.Ask)
		}
		expectedSpread := 0.08
		if ev.Spread < expectedSpread-0.001 || ev.Spread > expectedSpread+0.001 {
			t.Fatalf("expected spread ~0.08, got %f", ev.Spread)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for arbitrage event")
	}
}

func TestUnifiedBook_NoArbitrageBelowThreshold(t *testing.T) {
	// Threshold of 0.10 — a 0.03 spread should NOT trigger.
	ub, poly, kalshi, cancel := setupUnifiedBook(t, 0.10, testPair)
	defer cancel()

	// Poly bid 0.55 > Kalshi ask 0.52 → spread 0.03 (below 0.10 threshold)
	poly.send(BookUpdate{
		Exchange: ExchangePolymarket,
		MarketID: "0xbtc100k",
		Bids:     []PriceLevel{{Price: 0.55, Size: 100}},
		Asks:     []PriceLevel{{Price: 0.60, Size: 50}},
		Timestamp: time.Now(),
	})

	kalshi.send(BookUpdate{
		Exchange: ExchangeKalshi,
		MarketID: "BTC-100K",
		Bids:     []PriceLevel{{Price: 0.48, Size: 200}},
		Asks:     []PriceLevel{{Price: 0.52, Size: 80}},
		Timestamp: time.Now(),
	})

	// Wait enough for processing, then verify no event was emitted.
	time.Sleep(200 * time.Millisecond)

	select {
	case ev := <-ub.Events():
		t.Fatalf("should not have emitted arbitrage event (spread %f below threshold 0.10)", ev.Spread)
	default:
		// Good — no event.
	}
}

func TestUnifiedBook_NegativeSpreadNoEvent(t *testing.T) {
	// No crossed book: Poly bid < Kalshi ask
	ub, poly, kalshi, cancel := setupUnifiedBook(t, 0, testPair)
	defer cancel()

	poly.send(BookUpdate{
		Exchange: ExchangePolymarket,
		MarketID: "0xbtc100k",
		Bids:     []PriceLevel{{Price: 0.48, Size: 100}},
		Asks:     []PriceLevel{{Price: 0.55, Size: 50}},
		Timestamp: time.Now(),
	})

	kalshi.send(BookUpdate{
		Exchange: ExchangeKalshi,
		MarketID: "BTC-100K",
		Bids:     []PriceLevel{{Price: 0.46, Size: 200}},
		Asks:     []PriceLevel{{Price: 0.52, Size: 80}},
		Timestamp: time.Now(),
	})

	time.Sleep(200 * time.Millisecond)

	select {
	case ev := <-ub.Events():
		t.Fatalf("should not have emitted event for negative spread: %+v", ev)
	default:
		// Good — no event.
	}
}
