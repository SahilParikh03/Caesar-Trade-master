package poly

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/caesar-terminal/caesar/internal/adapter"
)

// Polymarket market-channel subscription message.
type subscribeMsg struct {
	Type      string   `json:"type"`
	AssetsIDs []string `json:"assets_ids"`
}

// Raw Polymarket book event as received over the wire.
type rawBookEvent struct {
	EventType string          `json:"event_type"`
	AssetID   string          `json:"asset_id"`
	Market    string          `json:"market"`
	Bids      []rawPriceLevel `json:"bids"`
	Asks      []rawPriceLevel `json:"asks"`
	Timestamp string          `json:"timestamp"`
	Hash      string          `json:"hash"`
}

type rawPriceLevel struct {
	Price string `json:"price"`
	Size  string `json:"size"`
}

// rawEnvelope is used for fast event-type detection before full parsing.
type rawEnvelope struct {
	EventType string `json:"event_type"`
}

// PolyAdapter connects to the Polymarket CLOB WebSocket and normalises
// incoming book snapshots into unified BookUpdate values.
type PolyAdapter struct {
	ws *adapter.WSClient

	// updates receives normalised book updates for downstream consumers.
	updates chan adapter.BookUpdate

	// pool reduces GC pressure from high-frequency PriceLevel allocations.
	levelPool sync.Pool
}

// New creates a PolyAdapter backed by the given WSClient.
// The caller must have already called ws.Connect.
func New(ws *adapter.WSClient) *PolyAdapter {
	return &PolyAdapter{
		ws:      ws,
		updates: make(chan adapter.BookUpdate, 1024),
		levelPool: sync.Pool{
			New: func() any {
				s := make([]adapter.PriceLevel, 0, 32)
				return &s
			},
		},
	}
}

// Updates returns the channel of normalised book updates.
func (pa *PolyAdapter) Updates() <-chan adapter.BookUpdate {
	return pa.updates
}

// Subscribe sends a Polymarket market-channel subscription for the given
// token ID. Multiple token IDs can be subscribed by calling this repeatedly.
func (pa *PolyAdapter) Subscribe(tokenID string) {
	msg, _ := json.Marshal(subscribeMsg{
		Type:      "market",
		AssetsIDs: []string{tokenID},
	})
	pa.ws.Send(msg)
}

// Run reads from the WSClient fan-out channel, parses book events, and
// pushes BookUpdate values to the updates channel. It blocks until ctx
// is cancelled.
func (pa *PolyAdapter) Run(ctx context.Context) {
	sub := pa.ws.Subscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case raw, ok := <-sub:
			if !ok {
				return
			}
			pa.handleMessage(raw)
		}
	}
}

func (pa *PolyAdapter) handleMessage(raw []byte) {
	var env rawEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		log.Printf("poly: invalid JSON: %v", err)
		return
	}

	switch env.EventType {
	case "book":
		pa.handleBook(raw)
	case "error":
		log.Printf("poly: exchange error: %s", raw)
	default:
		// price_change, tick_size_change, last_trade_price — ignored for now.
	}
}

func (pa *PolyAdapter) handleBook(raw []byte) {
	var ev rawBookEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		log.Printf("poly: failed to parse book event: %v", err)
		return
	}

	bids := pa.parseLevels(ev.Bids)
	asks := pa.parseLevels(ev.Asks)

	ts := parseTimestamp(ev.Timestamp)

	update := adapter.BookUpdate{
		Exchange:  adapter.ExchangePolymarket,
		MarketID:  ev.Market,
		AssetID:   ev.AssetID,
		Bids:      bids,
		Asks:      asks,
		Timestamp: ts,
		Hash:      ev.Hash,
	}

	select {
	case pa.updates <- update:
	default:
		log.Printf("poly: updates channel full, dropping book update for %s", ev.AssetID)
	}
}

// parseLevels converts raw string price/size pairs into PriceLevel slices.
// It borrows a slice from the pool to reduce allocations under load.
func (pa *PolyAdapter) parseLevels(raw []rawPriceLevel) []adapter.PriceLevel {
	pooled := pa.levelPool.Get().(*[]adapter.PriceLevel)
	levels := (*pooled)[:0]

	for _, r := range raw {
		p, err := strconv.ParseFloat(r.Price, 64)
		if err != nil {
			continue
		}
		s, err := strconv.ParseFloat(r.Size, 64)
		if err != nil {
			continue
		}
		levels = append(levels, adapter.PriceLevel{Price: p, Size: s})
	}

	// Copy out so the pooled slice can be returned.
	out := make([]adapter.PriceLevel, len(levels))
	copy(out, levels)
	pa.levelPool.Put(pooled)

	return out
}

// parseTimestamp converts a Unix-millisecond string to time.Time.
func parseTimestamp(s string) time.Time {
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
