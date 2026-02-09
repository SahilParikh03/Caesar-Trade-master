package adapter

import (
	"context"
	"sync"
	"testing"
	"time"
)

// mockRedis records every HSet call for assertion.
type mockRedis struct {
	mu    sync.Mutex
	calls []hsetCall
}

type hsetCall struct {
	Key    string
	Fields map[string]string
}

func (m *mockRedis) HSet(_ context.Context, key string, values ...any) error {
	fields := make(map[string]string)
	for i := 0; i+1 < len(values); i += 2 {
		k, _ := values[i].(string)
		v, _ := values[i+1].(string)
		fields[k] = v
	}
	m.mu.Lock()
	m.calls = append(m.calls, hsetCall{Key: key, Fields: fields})
	m.mu.Unlock()
	return nil
}

func (m *mockRedis) getCalls() []hsetCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]hsetCall, len(m.calls))
	copy(out, m.calls)
	return out
}

func TestRedisWriter_HSetCommand(t *testing.T) {
	mock := &mockRedis{}
	feed := make(chan BookUpdate, 8)

	rw := NewRedisWriter(mock, feed)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go rw.Run(ctx)

	ts := time.UnixMilli(1700000000000)
	feed <- BookUpdate{
		Exchange: ExchangePolymarket,
		MarketID: "0xabc",
		Bids: []PriceLevel{
			{Price: 0.48, Size: 30},
			{Price: 0.52, Size: 10},
		},
		Asks: []PriceLevel{
			{Price: 0.55, Size: 25},
			{Price: 0.60, Size: 15},
		},
		Timestamp: ts,
	}

	// Wait for the write to propagate.
	deadline := time.After(time.Second)
	for {
		calls := mock.getCalls()
		if len(calls) > 0 {
			c := calls[0]
			if c.Key != "book:polymarket:0xabc" {
				t.Fatalf("wrong key: %s", c.Key)
			}
			// Best bid is highest: 0.52
			if c.Fields["bid"] != "0.52" {
				t.Fatalf("expected bid '0.52', got %q", c.Fields["bid"])
			}
			// Best ask is lowest: 0.55
			if c.Fields["ask"] != "0.55" {
				t.Fatalf("expected ask '0.55', got %q", c.Fields["ask"])
			}
			if c.Fields["ts"] != "1700000000000" {
				t.Fatalf("expected ts '1700000000000', got %q", c.Fields["ts"])
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for HSET call")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestRedisWriter_DuplicateSuppression(t *testing.T) {
	mock := &mockRedis{}
	feed := make(chan BookUpdate, 8)

	rw := NewRedisWriter(mock, feed)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go rw.Run(ctx)

	base := BookUpdate{
		Exchange: ExchangeKalshi,
		MarketID: "FED-DEC",
		Bids:     []PriceLevel{{Price: 0.48, Size: 300}},
		Asks:     []PriceLevel{{Price: 0.54, Size: 200}},
		Timestamp: time.UnixMilli(1000),
	}

	// Send the same prices three times.
	feed <- base

	dup := base
	dup.Timestamp = time.UnixMilli(2000)
	feed <- dup

	dup2 := base
	dup2.Timestamp = time.UnixMilli(3000)
	feed <- dup2

	// Wait for processing.
	time.Sleep(200 * time.Millisecond)

	calls := mock.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 HSET call (duplicates suppressed), got %d", len(calls))
	}

	// Now send a different price — should trigger a second write.
	changed := base
	changed.Bids = []PriceLevel{{Price: 0.50, Size: 100}}
	changed.Timestamp = time.UnixMilli(4000)
	feed <- changed

	time.Sleep(200 * time.Millisecond)

	calls = mock.getCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 HSET calls after price change, got %d", len(calls))
	}
	if calls[1].Fields["bid"] != "0.5" {
		t.Fatalf("expected updated bid '0.5', got %q", calls[1].Fields["bid"])
	}
}
