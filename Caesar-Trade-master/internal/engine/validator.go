package engine

import (
	"errors"
	"fmt"

	"github.com/caesar-terminal/caesar/internal/adapter"
)

// Sentinel errors returned by Validate.
var (
	ErrInvalidSide     = errors.New("invalid order side")
	ErrInvalidType     = errors.New("invalid order type")
	ErrPriceOutOfRange = errors.New("price out of valid range")
	ErrPriceMissing    = errors.New("limit order requires a price")
	ErrQuantityTooLow  = errors.New("quantity below minimum lot size")
	ErrCircuitOpen     = errors.New("circuit breaker: trading disabled for market")
	ErrSlippageCap     = errors.New("market order exceeds slippage cap")
)

// ExchangeConstraints defines per-exchange validation limits.
type ExchangeConstraints struct {
	MinPrice   float64
	MaxPrice   float64
	MinLotSize float64
}

// DefaultConstraints maps each exchange to its validation rules.
var DefaultConstraints = map[adapter.Exchange]ExchangeConstraints{
	adapter.ExchangePolymarket: {
		MinPrice:   0.0,
		MaxPrice:   1.0,
		MinLotSize: 1.0,
	},
	adapter.ExchangeKalshi: {
		MinPrice:   0.0,
		MaxPrice:   1.0,
		MinLotSize: 1.0,
	},
}

// TradingGate is the interface for checking whether trading is allowed.
// Satisfied by adapter.CircuitBreaker.
type TradingGate interface {
	CanTrade(exchange adapter.Exchange, marketID string) bool
}

// SlippageCapBps is the maximum allowed slippage for market orders,
// expressed in basis points (0.1% = 10 bps).
const SlippageCapBps = 10

// Validator performs pre-flight checks on orders before they enter the
// execution pipeline. It fails fast: the first failing check returns
// an error and the order is rejected.
type Validator struct {
	gate        TradingGate
	constraints map[adapter.Exchange]ExchangeConstraints
}

// NewValidator creates a Validator with the given circuit breaker gate
// and default exchange constraints.
func NewValidator(gate TradingGate) *Validator {
	return &Validator{
		gate:        gate,
		constraints: DefaultConstraints,
	}
}

// Validate runs all pre-flight checks on the order. On success the order
// status is advanced to StatusValidated. On failure an error is returned
// and the status is set to StatusRejected.
func (v *Validator) Validate(order *Order) error {
	if err := v.validate(order); err != nil {
		order.Status = StatusRejected
		return err
	}
	order.Status = StatusValidated
	return nil
}

func (v *Validator) validate(order *Order) error {
	// 1. Basic field checks.
	if order.Side != Buy && order.Side != Sell {
		return ErrInvalidSide
	}
	if order.Type != Limit && order.Type != Market && order.Type != StopLoss {
		return ErrInvalidType
	}

	// 2. Exchange constraints.
	ec, ok := v.constraints[order.Exchange]
	if !ok {
		return fmt.Errorf("unknown exchange: %s", order.Exchange)
	}

	// 3. Price check.
	if order.Type == Limit || order.Type == StopLoss {
		if order.Price <= ec.MinPrice || order.Price >= ec.MaxPrice {
			return fmt.Errorf("%w: %.4f not in (%.1f, %.1f)",
				ErrPriceOutOfRange, order.Price, ec.MinPrice, ec.MaxPrice)
		}
	}
	if order.Type == Market && order.Price != 0 {
		// Market orders should not carry a user-specified price.
		// Price is determined at execution time from the book.
	}

	// 4. Quantity check.
	if order.Quantity < ec.MinLotSize {
		return fmt.Errorf("%w: %.4f < minimum %.1f",
			ErrQuantityTooLow, order.Quantity, ec.MinLotSize)
	}

	// 5. Circuit breaker check.
	if !v.gate.CanTrade(order.Exchange, order.MarketID) {
		return ErrCircuitOpen
	}

	// 6. Slippage guard for market orders.
	// The actual slippage calculation requires the current best price from
	// Redis, which is done in the execution pipeline (Ticket 3.2). Here we
	// enforce that the order is flagged for the slippage cap so the pipeline
	// can reject it if the cap is breached at execution time.
	// This is a structural placeholder: market orders are accepted here but
	// the pipeline MUST apply the SlippageCapBps check before submission.

	return nil
}
