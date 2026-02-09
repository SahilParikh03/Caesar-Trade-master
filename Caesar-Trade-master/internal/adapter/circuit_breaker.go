package adapter

import (
	"context"
	"sync"
	"time"
)

// CircuitBreakerConfig holds tunable parameters for the CircuitBreaker.
type CircuitBreakerConfig struct {
	// StaleThreshold is the maximum age of a BookUpdate before the market
	// is considered stale. Default: 1000ms.
	StaleThreshold time.Duration

	// CoolOff is the duration of continuous healthy data required after a
	// reconnection before trading is re-enabled. Default: 2s.
	CoolOff time.Duration

	// PollInterval is how frequently the breaker checks connection and
	// staleness state. Default: 100ms.
	PollInterval time.Duration
}

// DefaultCircuitBreakerConfig returns production-tuned defaults.
func DefaultCircuitBreakerConfig() CircuitBreakerConfig {
	return CircuitBreakerConfig{
		StaleThreshold: 1000 * time.Millisecond,
		CoolOff:        2 * time.Second,
		PollInterval:   100 * time.Millisecond,
	}
}

// marketState tracks health for a single (exchange, market) pair.
type marketState struct {
	LastUpdate time.Time
	// recoveredAt is set when a market transitions from unhealthy→healthy.
	// Trading is blocked until time.Since(recoveredAt) >= CoolOff.
	RecoveredAt time.Time
	Healthy     bool
}

// CircuitBreaker monitors WebSocket connections and data freshness, gating
// all trade execution behind CanTrade(). It enforces:
//   - Connection health via WSClient.Circuit()
//   - Data staleness via BookUpdate timestamps
//   - Cool-off period after recovery
//   - Manual emergency halt
type CircuitBreaker struct {
	cfg  CircuitBreakerConfig
	feed <-chan BookUpdate

	// connections tracked for heartbeat monitoring.
	connMu sync.RWMutex
	conns  map[Exchange]*WSClient

	// Per-market health state.
	mu      sync.RWMutex
	markets map[subKey]*marketState

	// Global manual halt.
	haltMu sync.RWMutex
	halted bool

	nowFunc func() time.Time // injectable clock for testing
}

// NewCircuitBreaker creates a CircuitBreaker that monitors the given
// Broadcaster feed for staleness. WSClients are registered separately
// via WatchConnection.
func NewCircuitBreaker(cfg CircuitBreakerConfig, feed <-chan BookUpdate) *CircuitBreaker {
	return &CircuitBreaker{
		cfg:     cfg,
		feed:    feed,
		conns:   make(map[Exchange]*WSClient),
		markets: make(map[subKey]*marketState),
		nowFunc: time.Now,
	}
}

// WatchConnection registers a WSClient so its Circuit() state is monitored.
func (cb *CircuitBreaker) WatchConnection(exchange Exchange, ws *WSClient) {
	cb.connMu.Lock()
	cb.conns[exchange] = ws
	cb.connMu.Unlock()
}

// ManualHalt forces all markets into a halted state. Trading is blocked
// until Resume is called.
func (cb *CircuitBreaker) ManualHalt() {
	cb.haltMu.Lock()
	cb.halted = true
	cb.haltMu.Unlock()
}

// Resume clears the manual halt. Markets still need to pass staleness and
// cool-off checks before CanTrade returns true.
func (cb *CircuitBreaker) Resume() {
	cb.haltMu.Lock()
	cb.halted = false
	cb.haltMu.Unlock()
}

// CanTrade returns true only if ALL of the following hold:
//  1. No manual halt is active.
//  2. The exchange's WSClient circuit is Closed (healthy).
//  3. The last BookUpdate for this market is within StaleThreshold.
//  4. The cool-off period has elapsed since recovery.
func (cb *CircuitBreaker) CanTrade(exchange Exchange, marketID string) bool {
	// Check manual halt.
	cb.haltMu.RLock()
	if cb.halted {
		cb.haltMu.RUnlock()
		return false
	}
	cb.haltMu.RUnlock()

	// Check connection health.
	cb.connMu.RLock()
	ws, ok := cb.conns[exchange]
	cb.connMu.RUnlock()
	if ok && ws.Circuit() == CircuitOpen {
		return false
	}

	// Check market staleness and cool-off.
	key := subKey{Exchange: exchange, MarketID: marketID}
	now := cb.nowFunc()

	cb.mu.RLock()
	ms, exists := cb.markets[key]
	cb.mu.RUnlock()

	if !exists {
		return false // no data received yet
	}

	if now.Sub(ms.LastUpdate) > cb.cfg.StaleThreshold {
		return false
	}

	if !ms.RecoveredAt.IsZero() && now.Sub(ms.RecoveredAt) < cb.cfg.CoolOff {
		return false
	}

	return true
}

// Run consumes the Broadcaster feed, updating per-market timestamps and
// health state. It blocks until ctx is cancelled.
func (cb *CircuitBreaker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case update, ok := <-cb.feed:
			if !ok {
				return
			}
			cb.recordUpdate(update)
		}
	}
}

func (cb *CircuitBreaker) recordUpdate(update BookUpdate) {
	key := subKey{Exchange: update.Exchange, MarketID: update.MarketID}
	now := cb.nowFunc()

	cb.mu.Lock()
	ms, exists := cb.markets[key]
	if !exists {
		ms = &marketState{}
		cb.markets[key] = ms
	}

	wasHealthy := ms.Healthy
	ms.LastUpdate = now

	// Determine current health: data is fresh.
	ms.Healthy = true

	// If transitioning from unhealthy to healthy, start cool-off.
	if !wasHealthy && ms.Healthy {
		ms.RecoveredAt = now
	}

	cb.mu.Unlock()
}

// MarkStale can be called externally (e.g. by the heartbeat monitor) to
// force a market into an unhealthy state.
func (cb *CircuitBreaker) MarkStale(exchange Exchange, marketID string) {
	key := subKey{Exchange: exchange, MarketID: marketID}

	cb.mu.Lock()
	ms, exists := cb.markets[key]
	if exists {
		ms.Healthy = false
	}
	cb.mu.Unlock()
}
