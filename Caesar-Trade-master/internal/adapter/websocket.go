package adapter

import (
	"context"
	"log"
	"math"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// CircuitState represents the health of the WebSocket connection for circuit
// breaker integration. Consumers (e.g. UI) can read this to decide whether
// trading actions should be allowed.
type CircuitState int32

const (
	CircuitClosed   CircuitState = iota // healthy
	CircuitOpen                         // unhealthy — disable trading
)

// WSConfig holds tunable parameters for a WSClient.
type WSConfig struct {
	URL string

	// Buffer sizes for the underlying TCP connection.
	ReadBufferSize  int
	WriteBufferSize int

	// HeartbeatTimeout is the maximum duration of silence before the client
	// considers the connection dead and triggers a reconnect.
	HeartbeatTimeout time.Duration

	// Backoff parameters for reconnection.
	BackoffInitial time.Duration
	BackoffMax     time.Duration
	BackoffFactor  float64

	// Headers sent during the WebSocket handshake.
	Headers http.Header
}

// DefaultWSConfig returns sensible defaults tuned for low-latency market data.
func DefaultWSConfig(url string) WSConfig {
	return WSConfig{
		URL:              url,
		ReadBufferSize:   4096,
		WriteBufferSize:  4096,
		HeartbeatTimeout: 500 * time.Millisecond,
		BackoffInitial:   50 * time.Millisecond,
		BackoffMax:       5 * time.Second,
		BackoffFactor:    2.0,
	}
}

// WSClient is a resilient, low-latency WebSocket connection manager.
// It automatically reconnects with exponential backoff, monitors heartbeats,
// and fans out incoming messages to subscribers.
type WSClient struct {
	cfg WSConfig

	// circuit exposes connection health for the circuit breaker (Ticket 2.8).
	circuit atomic.Int32

	mu   sync.RWMutex
	conn *websocket.Conn

	// subscribers receive copies of every inbound message.
	subMu sync.RWMutex
	subs  []chan []byte

	// outbox for sending messages through the connection.
	outbox chan []byte

	cancel context.CancelFunc
	done   chan struct{}

	// onReconnect is called after each successful reconnection (testing hook).
	onReconnect func()
}

// NewWSClient creates a new WebSocket client. Call Connect to start.
func NewWSClient(cfg WSConfig) *WSClient {
	return &WSClient{
		cfg:    cfg,
		outbox: make(chan []byte, 256),
		done:   make(chan struct{}),
	}
}

// Circuit returns the current circuit breaker state.
func (ws *WSClient) Circuit() CircuitState {
	return CircuitState(ws.circuit.Load())
}

// Subscribe returns a channel that receives copies of every inbound message.
// The caller must drain the channel to avoid blocking other subscribers.
func (ws *WSClient) Subscribe() <-chan []byte {
	ch := make(chan []byte, 512)
	ws.subMu.Lock()
	ws.subs = append(ws.subs, ch)
	ws.subMu.Unlock()
	return ch
}

// Send enqueues a message for delivery over the WebSocket connection.
func (ws *WSClient) Send(data []byte) {
	select {
	case ws.outbox <- data:
	default:
		log.Printf("ws: outbox full, dropping message (%d bytes)", len(data))
	}
}

// Connect dials the WebSocket endpoint and starts the read/write/heartbeat
// loops. It blocks until the initial connection succeeds or ctx is cancelled.
func (ws *WSClient) Connect(ctx context.Context) error {
	ctx, ws.cancel = context.WithCancel(ctx)

	if err := ws.dial(ctx); err != nil {
		return err
	}
	ws.circuit.Store(int32(CircuitClosed))

	go ws.readLoop(ctx)
	go ws.writeLoop(ctx)

	return nil
}

// Close shuts down the client, closing the underlying connection and all
// subscriber channels.
func (ws *WSClient) Close() {
	if ws.cancel != nil {
		ws.cancel()
	}
	ws.mu.Lock()
	if ws.conn != nil {
		ws.conn.Close()
	}
	ws.mu.Unlock()

	ws.subMu.RLock()
	for _, ch := range ws.subs {
		close(ch)
	}
	ws.subMu.RUnlock()

	close(ws.done)
}

// Done returns a channel that is closed when the client has fully shut down.
func (ws *WSClient) Done() <-chan struct{} {
	return ws.done
}

// dial establishes the WebSocket connection with TCP_NODELAY enabled.
func (ws *WSClient) dial(ctx context.Context) error {
	dialer := websocket.Dialer{
		ReadBufferSize:  ws.cfg.ReadBufferSize,
		WriteBufferSize: ws.cfg.WriteBufferSize,
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := net.Dialer{}
			conn, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if tc, ok := conn.(*net.TCPConn); ok {
				tc.SetNoDelay(true)
			}
			return conn, nil
		},
	}

	conn, _, err := dialer.DialContext(ctx, ws.cfg.URL, ws.cfg.Headers)
	if err != nil {
		return err
	}

	ws.mu.Lock()
	ws.conn = conn
	ws.mu.Unlock()
	return nil
}

// reconnect loops with exponential backoff until a connection is re-established
// or the context is cancelled.
func (ws *WSClient) reconnect(ctx context.Context) bool {
	ws.circuit.Store(int32(CircuitOpen))

	delay := ws.cfg.BackoffInitial
	for {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(delay):
		}

		if err := ws.dial(ctx); err != nil {
			log.Printf("ws: reconnect failed: %v (retry in %v)", err, delay)
			delay = time.Duration(math.Min(
				float64(delay)*ws.cfg.BackoffFactor,
				float64(ws.cfg.BackoffMax),
			))
			continue
		}

		ws.circuit.Store(int32(CircuitClosed))
		if ws.onReconnect != nil {
			ws.onReconnect()
		}
		return true
	}
}

// readLoop reads messages and fans them out to subscribers. It also acts as the
// heartbeat monitor: if no message arrives within HeartbeatTimeout, it triggers
// a reconnect.
func (ws *WSClient) readLoop(ctx context.Context) {
	for {
		ws.mu.RLock()
		c := ws.conn
		ws.mu.RUnlock()

		c.SetReadDeadline(time.Now().Add(ws.cfg.HeartbeatTimeout))
		_, msg, err := c.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("ws: read error (triggering reconnect): %v", err)
			c.Close()
			if !ws.reconnect(ctx) {
				return
			}
			continue
		}

		ws.fanOut(msg)
	}
}

// writeLoop drains the outbox and writes messages to the connection.
func (ws *WSClient) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case data := <-ws.outbox:
			ws.mu.RLock()
			c := ws.conn
			ws.mu.RUnlock()
			if err := c.WriteMessage(websocket.TextMessage, data); err != nil {
				log.Printf("ws: write error: %v", err)
			}
		}
	}
}

// fanOut delivers msg to every subscriber without blocking.
func (ws *WSClient) fanOut(msg []byte) {
	ws.subMu.RLock()
	defer ws.subMu.RUnlock()

	for _, ch := range ws.subs {
		select {
		case ch <- msg:
		default:
			// Slow consumer — drop to avoid head-of-line blocking.
		}
	}
}
