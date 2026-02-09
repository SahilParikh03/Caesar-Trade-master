package adapter

import "time"

// Exchange identifies the source of market data.
type Exchange string

const (
	ExchangePolymarket Exchange = "polymarket"
	ExchangeKalshi     Exchange = "kalshi"
)

// PriceLevel represents a single bid or ask at a given price.
type PriceLevel struct {
	Price float64
	Size  float64
}

// BookUpdate is the unified order book snapshot used across all exchange
// adapters. Downstream consumers (engine, UI) operate on this type
// regardless of origin.
type BookUpdate struct {
	Exchange  Exchange
	MarketID  string
	AssetID   string
	Bids      []PriceLevel
	Asks      []PriceLevel
	Timestamp time.Time
	Hash      string
}
