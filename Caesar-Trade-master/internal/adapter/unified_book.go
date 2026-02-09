package adapter

import (
	"context"
	"sync"
	"time"
)

// MarketPair links the same real-world event across two exchanges.
type MarketPair struct {
	Name          string // human-readable label, e.g. "BTC > $100k"
	PolyMarketID  string // Polymarket market / condition ID
	KalshiMarketID string // Kalshi market ID
}

// ArbitrageEvent is emitted when a crossed-book opportunity is detected.
type ArbitrageEvent struct {
	Pair      MarketPair
	Direction ArbitrageDirection
	BidExchange Exchange // exchange with the higher bid
	AskExchange Exchange // exchange with the lower ask
	Bid       float64   // best bid on the bid exchange
	Ask       float64   // best ask on the ask exchange
	Spread    float64   // bid − ask (positive = opportunity)
	Timestamp time.Time
}

// ArbitrageDirection indicates which exchange is cheap vs expensive.
type ArbitrageDirection int

const (
	// ArbPolyBidKalshiAsk means Polymarket bid > Kalshi ask.
	ArbPolyBidKalshiAsk ArbitrageDirection = iota
	// ArbKalshiBidPolyAsk means Kalshi bid > Polymarket ask.
	ArbKalshiBidPolyAsk
)

// side holds the latest best bid/ask snapshot for one exchange.
type side struct {
	BestBid float64
	BestAsk float64
	Updated time.Time
}

// pairState is the merged view for a single market pair.
type pairState struct {
	Pair  MarketPair
	Poly  side
	Kalshi side
}

// UnifiedBook merges order book data from two exchanges for paired markets
// and detects arbitrage opportunities in real time.
type UnifiedBook struct {
	bc        *Broadcaster
	threshold float64 // minimum spread to emit an event

	mu     sync.RWMutex
	states map[string]*pairState // keyed by MarketPair.Name

	events chan ArbitrageEvent
}

// NewUnifiedBook creates a UnifiedBook. The threshold is the minimum
// positive spread (bid − ask) required before an ArbitrageEvent is emitted.
// Set to 0 to emit on any crossed book.
func NewUnifiedBook(bc *Broadcaster, threshold float64) *UnifiedBook {
	return &UnifiedBook{
		bc:        bc,
		threshold: threshold,
		states:    make(map[string]*pairState),
		events:    make(chan ArbitrageEvent, 256),
	}
}

// Events returns the channel of detected arbitrage opportunities.
func (ub *UnifiedBook) Events() <-chan ArbitrageEvent {
	return ub.events
}

// AddPair registers a market pair and subscribes to both exchanges on the
// Broadcaster. Must be called before Run.
func (ub *UnifiedBook) AddPair(pair MarketPair) {
	ub.mu.Lock()
	ub.states[pair.Name] = &pairState{Pair: pair}
	ub.mu.Unlock()
}

// Snapshot returns the current merged state for a pair, or false if not found.
func (ub *UnifiedBook) Snapshot(pairName string) (pairState, bool) {
	ub.mu.RLock()
	defer ub.mu.RUnlock()
	ps, ok := ub.states[pairName]
	if !ok {
		return pairState{}, false
	}
	return *ps, true
}

// Run subscribes to both sides of every registered pair and processes
// updates. It blocks until ctx is cancelled.
func (ub *UnifiedBook) Run(ctx context.Context) {
	ub.mu.RLock()
	pairs := make([]MarketPair, 0, len(ub.states))
	for _, ps := range ub.states {
		pairs = append(pairs, ps.Pair)
	}
	ub.mu.RUnlock()

	var wg sync.WaitGroup

	for _, pair := range pairs {
		polyCh := ub.bc.Subscribe(ExchangePolymarket, pair.PolyMarketID)
		kalshiCh := ub.bc.Subscribe(ExchangeKalshi, pair.KalshiMarketID)

		wg.Add(2)
		go func(p MarketPair, ch <-chan BookUpdate) {
			defer wg.Done()
			ub.consumeSide(ctx, p, ExchangePolymarket, ch)
		}(pair, polyCh)

		go func(p MarketPair, ch <-chan BookUpdate) {
			defer wg.Done()
			ub.consumeSide(ctx, p, ExchangeKalshi, ch)
		}(pair, kalshiCh)
	}

	wg.Wait()
}

func (ub *UnifiedBook) consumeSide(ctx context.Context, pair MarketPair, exchange Exchange, ch <-chan BookUpdate) {
	for {
		select {
		case <-ctx.Done():
			return
		case update, ok := <-ch:
			if !ok {
				return
			}
			ub.applyUpdate(pair, exchange, update)
		}
	}
}

func (ub *UnifiedBook) applyUpdate(pair MarketPair, exchange Exchange, update BookUpdate) {
	bestBid := bestHigh(update.Bids)
	bestAsk := bestLow(update.Asks)

	ub.mu.Lock()
	ps := ub.states[pair.Name]
	switch exchange {
	case ExchangePolymarket:
		ps.Poly = side{BestBid: bestBid, BestAsk: bestAsk, Updated: update.Timestamp}
	case ExchangeKalshi:
		ps.Kalshi = side{BestBid: bestBid, BestAsk: bestAsk, Updated: update.Timestamp}
	}
	poly := ps.Poly
	kalshi := ps.Kalshi
	ub.mu.Unlock()

	ub.checkArbitrage(pair, poly, kalshi)
}

func (ub *UnifiedBook) checkArbitrage(pair MarketPair, poly, kalshi side) {
	// Direction 1: Poly bid > Kalshi ask
	if kalshi.BestAsk > 0 {
		spread := poly.BestBid - kalshi.BestAsk
		if spread > ub.threshold {
			ub.emit(ArbitrageEvent{
				Pair:        pair,
				Direction:   ArbPolyBidKalshiAsk,
				BidExchange: ExchangePolymarket,
				AskExchange: ExchangeKalshi,
				Bid:         poly.BestBid,
				Ask:         kalshi.BestAsk,
				Spread:      spread,
				Timestamp:   time.Now(),
			})
		}
	}

	// Direction 2: Kalshi bid > Poly ask
	if poly.BestAsk > 0 {
		spread := kalshi.BestBid - poly.BestAsk
		if spread > ub.threshold {
			ub.emit(ArbitrageEvent{
				Pair:        pair,
				Direction:   ArbKalshiBidPolyAsk,
				BidExchange: ExchangeKalshi,
				AskExchange: ExchangePolymarket,
				Bid:         kalshi.BestBid,
				Ask:         poly.BestAsk,
				Spread:      spread,
				Timestamp:   time.Now(),
			})
		}
	}
}

func (ub *UnifiedBook) emit(ev ArbitrageEvent) {
	select {
	case ub.events <- ev:
	default:
		// Events channel full — drop to avoid blocking the hot path.
	}
}

// bestHigh returns the highest price from a set of bids.
func bestHigh(levels []PriceLevel) float64 {
	if len(levels) == 0 {
		return 0
	}
	best := levels[0].Price
	for _, l := range levels[1:] {
		if l.Price > best {
			best = l.Price
		}
	}
	return best
}

// bestLow returns the lowest price from a set of asks.
func bestLow(levels []PriceLevel) float64 {
	if len(levels) == 0 {
		return 0
	}
	best := levels[0].Price
	for _, l := range levels[1:] {
		if l.Price < best {
			best = l.Price
		}
	}
	return best
}
