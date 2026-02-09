package kalshi

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/caesar-terminal/caesar/internal/adapter"
)

const wsPath = "/trade-api/ws/v2"

// command is the Kalshi WebSocket command envelope.
type command struct {
	ID     int           `json:"id"`
	Cmd    string        `json:"cmd"`
	Params commandParams `json:"params"`
}

type commandParams struct {
	Channels     []string `json:"channels"`
	MarketTicker string   `json:"market_ticker"`
}

// --- Raw wire types ---

type rawEnvelope struct {
	Type string `json:"type"`
}

type rawSnapshot struct {
	Type string `json:"type"`
	SID  int    `json:"sid"`
	Seq  int    `json:"seq"`
	Msg  struct {
		MarketTicker string      `json:"market_ticker"`
		MarketID     string      `json:"market_id"`
		Yes          [][2]int    `json:"yes"`
		No           [][2]int    `json:"no"`
	} `json:"msg"`
}

type rawDelta struct {
	Type string `json:"type"`
	SID  int    `json:"sid"`
	Seq  int    `json:"seq"`
	Msg  struct {
		MarketTicker string `json:"market_ticker"`
		MarketID     string `json:"market_id"`
		Price        int    `json:"price"`
		Delta        int    `json:"delta"`
		Side         string `json:"side"`
		Ts           string `json:"ts"`
	} `json:"msg"`
}

// orderBook is the internal order book state for a single market.
type orderBook struct {
	MarketTicker string
	MarketID     string
	Yes          map[int]int // price (cents) → quantity
	No           map[int]int
}

// KalshiAdapter connects to the Kalshi WebSocket and normalises order book
// data into unified BookUpdate values.
type KalshiAdapter struct {
	ws *adapter.WSClient

	raw     <-chan []byte
	updates chan adapter.BookUpdate

	mu    sync.RWMutex
	books map[string]*orderBook // keyed by market_ticker

	levelPool sync.Pool
	cmdID     int
}

// AuthHeaders computes the RSA-PSS authentication headers required for the
// Kalshi WebSocket upgrade request.
func AuthHeaders(apiKey string, privateKeyPEM []byte) (http.Header, error) {
	block, _ := pem.Decode(privateKeyPEM)
	if block == nil {
		return nil, fmt.Errorf("kalshi: failed to decode PEM block")
	}

	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("kalshi: parse private key: %w", err)
	}

	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("kalshi: key is not RSA")
	}

	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	msg := ts + "GET" + wsPath

	h := sha256.Sum256([]byte(msg))
	sig, err := rsa.SignPSS(rand.Reader, rsaKey, crypto.SHA256, h[:], &rsa.PSSOptions{
		SaltLength: rsa.PSSSaltLengthEqualsHash,
	})
	if err != nil {
		return nil, fmt.Errorf("kalshi: sign: %w", err)
	}

	headers := http.Header{}
	headers.Set("KALSHI-ACCESS-KEY", apiKey)
	headers.Set("KALSHI-ACCESS-TIMESTAMP", ts)
	headers.Set("KALSHI-ACCESS-SIGNATURE", base64.StdEncoding.EncodeToString(sig))

	return headers, nil
}

// New creates a KalshiAdapter backed by the given WSClient.
// It immediately subscribes to the WSClient fan-out so no messages are missed.
func New(ws *adapter.WSClient) *KalshiAdapter {
	return &KalshiAdapter{
		ws:      ws,
		raw:     ws.Subscribe(),
		updates: make(chan adapter.BookUpdate, 1024),
		books:   make(map[string]*orderBook),
		levelPool: sync.Pool{
			New: func() any {
				s := make([]adapter.PriceLevel, 0, 32)
				return &s
			},
		},
	}
}

// Updates returns the channel of normalised book updates.
func (ka *KalshiAdapter) Updates() <-chan adapter.BookUpdate {
	return ka.updates
}

// Subscribe sends a Kalshi orderbook_delta subscription for the given ticker.
func (ka *KalshiAdapter) Subscribe(ticker string) {
	ka.cmdID++
	msg, _ := json.Marshal(command{
		ID:  ka.cmdID,
		Cmd: "subscribe",
		Params: commandParams{
			Channels:     []string{"orderbook_delta"},
			MarketTicker: ticker,
		},
	})
	ka.ws.Send(msg)
}

// Run reads from the WSClient fan-out, processes snapshots and deltas, and
// emits BookUpdate values. It blocks until ctx is cancelled.
func (ka *KalshiAdapter) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case raw, ok := <-ka.raw:
			if !ok {
				return
			}
			ka.handleMessage(raw)
		}
	}
}

func (ka *KalshiAdapter) handleMessage(raw []byte) {
	var env rawEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		log.Printf("kalshi: invalid JSON: %v", err)
		return
	}

	switch env.Type {
	case "orderbook_snapshot":
		ka.handleSnapshot(raw)
	case "orderbook_delta":
		ka.handleDelta(raw)
	case "error":
		log.Printf("kalshi: exchange error: %s", raw)
	default:
		// Other message types ignored.
	}
}

func (ka *KalshiAdapter) handleSnapshot(raw []byte) {
	var snap rawSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		log.Printf("kalshi: failed to parse snapshot: %v", err)
		return
	}

	book := &orderBook{
		MarketTicker: snap.Msg.MarketTicker,
		MarketID:     snap.Msg.MarketID,
		Yes:          make(map[int]int, len(snap.Msg.Yes)),
		No:           make(map[int]int, len(snap.Msg.No)),
	}
	for _, level := range snap.Msg.Yes {
		book.Yes[level[0]] = level[1]
	}
	for _, level := range snap.Msg.No {
		book.No[level[0]] = level[1]
	}

	ka.mu.Lock()
	ka.books[snap.Msg.MarketTicker] = book
	ka.mu.Unlock()

	ka.emitUpdate(book)
}

func (ka *KalshiAdapter) handleDelta(raw []byte) {
	var delta rawDelta
	if err := json.Unmarshal(raw, &delta); err != nil {
		log.Printf("kalshi: failed to parse delta: %v", err)
		return
	}

	ka.mu.Lock()
	book, ok := ka.books[delta.Msg.MarketTicker]
	if !ok {
		ka.mu.Unlock()
		return
	}

	side := book.Yes
	if delta.Msg.Side == "no" {
		side = book.No
	}

	newQty := side[delta.Msg.Price] + delta.Msg.Delta
	if newQty <= 0 {
		delete(side, delta.Msg.Price)
	} else {
		side[delta.Msg.Price] = newQty
	}
	ka.mu.Unlock()

	ka.emitUpdate(book)
}

// emitUpdate converts the internal book state into a BookUpdate and sends it.
// YES bids → BookUpdate.Bids, NO bids → BookUpdate.Asks.
// Prices are normalised from cents (0-99) to a 0.0-1.0 scale.
func (ka *KalshiAdapter) emitUpdate(book *orderBook) {
	ka.mu.RLock()
	bids := ka.centsToLevels(book.Yes)
	asks := ka.centsToLevels(book.No)
	ka.mu.RUnlock()

	update := adapter.BookUpdate{
		Exchange:  adapter.ExchangeKalshi,
		MarketID:  book.MarketID,
		AssetID:   book.MarketTicker,
		Bids:      bids,
		Asks:      asks,
		Timestamp: time.Now(),
	}

	select {
	case ka.updates <- update:
	default:
		log.Printf("kalshi: updates channel full, dropping book update for %s", book.MarketTicker)
	}
}

// centsToLevels converts a map of cents→quantity into a PriceLevel slice,
// normalising prices from cents (0-99) to a 0.0-1.0 scale.
func (ka *KalshiAdapter) centsToLevels(m map[int]int) []adapter.PriceLevel {
	pooled := ka.levelPool.Get().(*[]adapter.PriceLevel)
	levels := (*pooled)[:0]

	for price, qty := range m {
		levels = append(levels, adapter.PriceLevel{
			Price: float64(price) / 100.0,
			Size:  float64(qty),
		})
	}

	out := make([]adapter.PriceLevel, len(levels))
	copy(out, levels)
	ka.levelPool.Put(pooled)

	return out
}
