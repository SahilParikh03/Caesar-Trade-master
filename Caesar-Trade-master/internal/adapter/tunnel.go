package adapter

import (
	"context"
	"fmt"
	"net/http"
	"sync"
)

// Tunnel is a private, authenticated WebSocket session for a single user
// on a single exchange. All credentials are held in memory only.
type Tunnel struct {
	UserID   string
	Exchange Exchange
	ws       *WSClient
	cancel   context.CancelFunc
	msgs     <-chan []byte
}

// Messages returns the channel of raw inbound messages for this tunnel.
func (t *Tunnel) Messages() <-chan []byte { return t.msgs }

// TunnelConfig holds the parameters needed to open a private tunnel.
type TunnelConfig struct {
	UserID   string
	Exchange Exchange
	URL      string
	Headers  http.Header // auth headers (RSA-PSS for Kalshi, EIP-712 for Poly)
}

// IPPool is a placeholder for future IP rotation to avoid exchange
// rate-limiting. Implementations will provide a rotating set of local
// addresses or proxy endpoints.
type IPPool interface {
	// Next returns the next outbound IP address or proxy URL to use.
	Next() string
}

// TunnelManager manages private, authenticated WebSocket sessions keyed by
// (UserID, Exchange). It guarantees data isolation: User A's messages are
// never visible to User B.
type TunnelManager struct {
	mu      sync.Mutex
	tunnels map[tunnelKey]*Tunnel
	pool    IPPool // nil until IP rotation is configured
}

type tunnelKey struct {
	UserID   string
	Exchange Exchange
}

// NewTunnelManager creates a TunnelManager ready for use.
func NewTunnelManager() *TunnelManager {
	return &TunnelManager{
		tunnels: make(map[tunnelKey]*Tunnel),
	}
}

// SetIPPool configures the IP rotation pool. Pass nil to disable.
func (tm *TunnelManager) SetIPPool(pool IPPool) {
	tm.mu.Lock()
	tm.pool = pool
	tm.mu.Unlock()
}

// Open creates a new private tunnel for the given user and exchange.
// If a tunnel already exists for this (user, exchange) pair, it is closed
// first. The caller receives authenticated messages via Tunnel.Messages().
func (tm *TunnelManager) Open(ctx context.Context, cfg TunnelConfig) (*Tunnel, error) {
	key := tunnelKey{UserID: cfg.UserID, Exchange: cfg.Exchange}

	tm.mu.Lock()
	if existing, ok := tm.tunnels[key]; ok {
		existing.close()
		delete(tm.tunnels, key)
	}
	tm.mu.Unlock()

	wsCfg := DefaultWSConfig(cfg.URL)
	wsCfg.Headers = cfg.Headers

	ws := NewWSClient(wsCfg)
	msgs := ws.Subscribe()

	tunnelCtx, cancel := context.WithCancel(ctx)
	if err := ws.Connect(tunnelCtx); err != nil {
		cancel()
		return nil, fmt.Errorf("tunnel: connect %s for user %s: %w", cfg.Exchange, cfg.UserID, err)
	}

	t := &Tunnel{
		UserID:   cfg.UserID,
		Exchange: cfg.Exchange,
		ws:       ws,
		cancel:   cancel,
		msgs:     msgs,
	}

	tm.mu.Lock()
	tm.tunnels[key] = t
	tm.mu.Unlock()

	return t, nil
}

// Close tears down the private tunnel for the given user and exchange,
// closing the underlying WebSocket and removing all in-memory state.
func (tm *TunnelManager) Close(userID string, exchange Exchange) {
	key := tunnelKey{UserID: userID, Exchange: exchange}

	tm.mu.Lock()
	t, ok := tm.tunnels[key]
	if ok {
		delete(tm.tunnels, key)
	}
	tm.mu.Unlock()

	if ok {
		t.close()
	}
}

// CloseAll tears down every active tunnel.
func (tm *TunnelManager) CloseAll() {
	tm.mu.Lock()
	tunnels := tm.tunnels
	tm.tunnels = make(map[tunnelKey]*Tunnel)
	tm.mu.Unlock()

	for _, t := range tunnels {
		t.close()
	}
}

// Get returns the active tunnel for the given user and exchange, or nil.
func (tm *TunnelManager) Get(userID string, exchange Exchange) *Tunnel {
	key := tunnelKey{UserID: userID, Exchange: exchange}
	tm.mu.Lock()
	t := tm.tunnels[key]
	tm.mu.Unlock()
	return t
}

// Send enqueues a message on the user's private tunnel.
func (tm *TunnelManager) Send(userID string, exchange Exchange, data []byte) error {
	t := tm.Get(userID, exchange)
	if t == nil {
		return fmt.Errorf("tunnel: no active session for user %s on %s", userID, exchange)
	}
	t.ws.Send(data)
	return nil
}

func (t *Tunnel) close() {
	t.cancel()
	t.ws.Close()
}
