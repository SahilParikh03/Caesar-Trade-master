package engine

import (
	"time"

	"github.com/caesar-terminal/caesar/internal/adapter"
)

// Side represents the direction of an order.
type Side uint8

const (
	Buy  Side = iota + 1
	Sell
)

func (s Side) String() string {
	switch s {
	case Buy:
		return "buy"
	case Sell:
		return "sell"
	default:
		return "unknown"
	}
}

// OrderType distinguishes execution semantics.
type OrderType uint8

const (
	Limit    OrderType = iota + 1
	Market
	StopLoss
)

func (t OrderType) String() string {
	switch t {
	case Limit:
		return "limit"
	case Market:
		return "market"
	case StopLoss:
		return "stop-loss"
	default:
		return "unknown"
	}
}

// Status tracks the lifecycle of an order.
type Status uint8

const (
	StatusNew       Status = iota + 1
	StatusValidated
	StatusPending
	StatusFilled
	StatusCancelled
	StatusRejected
)

func (s Status) String() string {
	switch s {
	case StatusNew:
		return "new"
	case StatusValidated:
		return "validated"
	case StatusPending:
		return "pending"
	case StatusFilled:
		return "filled"
	case StatusCancelled:
		return "cancelled"
	case StatusRejected:
		return "rejected"
	default:
		return "unknown"
	}
}

// Order is the unified order representation for both Polymarket and Kalshi.
type Order struct {
	OrderID   string
	UserID    string
	Exchange  adapter.Exchange
	MarketID  string
	AssetID   string
	Side      Side
	Type      OrderType
	Price     float64
	Quantity  float64
	Status    Status
	CreatedAt time.Time
}
