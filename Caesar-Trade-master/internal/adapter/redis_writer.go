package adapter

import (
	"context"
	"fmt"
	"strconv"
	"sync"
)

// RedisClient abstracts the Redis operations used by RedisWriter.
// In production this is satisfied by *redis.Client; in tests by a mock.
type RedisClient interface {
	HSet(ctx context.Context, key string, values ...any) error
}

// bookSnapshot holds the last-written best bid/ask for a market so we can
// skip duplicate writes.
type bookSnapshot struct {
	Bid string
	Ask string
}

// RedisWriter subscribes to a Broadcaster's unified stream and persists
// the best bid/ask for every market into Redis using the schema:
//
//	Key:    book:{exchange}:{market_id}
//	Fields: bid, ask, ts
//
// Writes are non-blocking: updates are buffered in an internal channel and
// flushed by a dedicated goroutine. Duplicate prices are suppressed.
type RedisWriter struct {
	client RedisClient
	feed   <-chan BookUpdate
	buf    chan BookUpdate

	mu   sync.Mutex
	last map[string]bookSnapshot // keyed by Redis key
}

// NewRedisWriter creates a RedisWriter that reads from the Broadcaster's
// SubscribeAll channel and writes to the given Redis client.
func NewRedisWriter(client RedisClient, feed <-chan BookUpdate) *RedisWriter {
	return &RedisWriter{
		client: client,
		feed:   feed,
		buf:    make(chan BookUpdate, 1024),
		last:   make(map[string]bookSnapshot),
	}
}

// Run starts two goroutines: one to drain the Broadcaster feed into an
// internal buffer, and one to flush buffered updates to Redis. It blocks
// until ctx is cancelled.
func (rw *RedisWriter) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)

	// Ingestion: drain the Broadcaster feed into the internal buffer
	// so we never block the Broadcaster.
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case update, ok := <-rw.feed:
				if !ok {
					return
				}
				select {
				case rw.buf <- update:
				default:
					// Buffer full — drop oldest-unsent to keep up.
				}
			}
		}
	}()

	// Flusher: write buffered updates to Redis.
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case update, ok := <-rw.buf:
				if !ok {
					return
				}
				rw.write(ctx, update)
			}
		}
	}()

	wg.Wait()
}

// write extracts best bid/ask, checks for duplicates, and issues an HSET.
func (rw *RedisWriter) write(ctx context.Context, update BookUpdate) {
	bestBid := bestPrice(update.Bids, true)
	bestAsk := bestPrice(update.Asks, false)

	key := fmt.Sprintf("book:%s:%s", update.Exchange, update.MarketID)

	rw.mu.Lock()
	prev, exists := rw.last[key]
	if exists && prev.Bid == bestBid && prev.Ask == bestAsk {
		rw.mu.Unlock()
		return
	}
	rw.last[key] = bookSnapshot{Bid: bestBid, Ask: bestAsk}
	rw.mu.Unlock()

	ts := strconv.FormatInt(update.Timestamp.UnixMilli(), 10)
	rw.client.HSet(ctx, key, "bid", bestBid, "ask", bestAsk, "ts", ts)
}

// bestPrice returns the best (highest bid or lowest ask) price as a string.
// For bids, "best" is the highest price; for asks, the lowest.
func bestPrice(levels []PriceLevel, isBid bool) string {
	if len(levels) == 0 {
		return "0"
	}
	best := levels[0].Price
	for _, l := range levels[1:] {
		if isBid && l.Price > best {
			best = l.Price
		}
		if !isBid && l.Price < best {
			best = l.Price
		}
	}
	return strconv.FormatFloat(best, 'f', -1, 64)
}
