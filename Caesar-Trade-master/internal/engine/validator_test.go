package engine

import (
	"errors"
	"testing"

	"github.com/caesar-terminal/caesar/internal/adapter"
)

// mockGate implements TradingGate for testing.
type mockGate struct {
	canTrade bool
}

func (m *mockGate) CanTrade(adapter.Exchange, string) bool { return m.canTrade }

func validOrder() *Order {
	return &Order{
		UserID:   "user-1",
		Exchange: adapter.ExchangePolymarket,
		MarketID: "BTC-100K",
		AssetID:  "token-abc",
		Side:     Buy,
		Type:     Limit,
		Price:    0.55,
		Quantity: 10,
		Status:   StatusNew,
	}
}

func TestValidate_Success(t *testing.T) {
	v := NewValidator(&mockGate{canTrade: true})
	order := validOrder()

	if err := v.Validate(order); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if order.Status != StatusValidated {
		t.Fatalf("expected StatusValidated, got %s", order.Status)
	}
}

func TestValidate_InvalidSide(t *testing.T) {
	v := NewValidator(&mockGate{canTrade: true})
	order := validOrder()
	order.Side = 0

	err := v.Validate(order)
	if !errors.Is(err, ErrInvalidSide) {
		t.Fatalf("expected ErrInvalidSide, got %v", err)
	}
	if order.Status != StatusRejected {
		t.Fatalf("expected StatusRejected, got %s", order.Status)
	}
}

func TestValidate_InvalidType(t *testing.T) {
	v := NewValidator(&mockGate{canTrade: true})
	order := validOrder()
	order.Type = 0

	err := v.Validate(order)
	if !errors.Is(err, ErrInvalidType) {
		t.Fatalf("expected ErrInvalidType, got %v", err)
	}
}

func TestValidate_PriceTooLow(t *testing.T) {
	v := NewValidator(&mockGate{canTrade: true})
	order := validOrder()
	order.Price = 0.0

	err := v.Validate(order)
	if !errors.Is(err, ErrPriceOutOfRange) {
		t.Fatalf("expected ErrPriceOutOfRange, got %v", err)
	}
}

func TestValidate_PriceTooHigh(t *testing.T) {
	v := NewValidator(&mockGate{canTrade: true})
	order := validOrder()
	order.Price = 1.0

	err := v.Validate(order)
	if !errors.Is(err, ErrPriceOutOfRange) {
		t.Fatalf("expected ErrPriceOutOfRange, got %v", err)
	}
}

func TestValidate_PriceAtBoundaries(t *testing.T) {
	v := NewValidator(&mockGate{canTrade: true})

	// Just above 0 — should pass.
	order := validOrder()
	order.Price = 0.01
	if err := v.Validate(order); err != nil {
		t.Fatalf("price 0.01 should be valid, got %v", err)
	}

	// Just below 1 — should pass.
	order = validOrder()
	order.Price = 0.99
	if err := v.Validate(order); err != nil {
		t.Fatalf("price 0.99 should be valid, got %v", err)
	}
}

func TestValidate_MarketOrderSkipsPriceCheck(t *testing.T) {
	v := NewValidator(&mockGate{canTrade: true})
	order := validOrder()
	order.Type = Market
	order.Price = 0 // no user price for market orders

	if err := v.Validate(order); err != nil {
		t.Fatalf("market order with zero price should be valid, got %v", err)
	}
}

func TestValidate_QuantityTooLow(t *testing.T) {
	v := NewValidator(&mockGate{canTrade: true})
	order := validOrder()
	order.Quantity = 0.5

	err := v.Validate(order)
	if !errors.Is(err, ErrQuantityTooLow) {
		t.Fatalf("expected ErrQuantityTooLow, got %v", err)
	}
}

func TestValidate_CircuitBreakerOpen(t *testing.T) {
	v := NewValidator(&mockGate{canTrade: false})
	order := validOrder()

	err := v.Validate(order)
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got %v", err)
	}
}

func TestValidate_KalshiExchange(t *testing.T) {
	v := NewValidator(&mockGate{canTrade: true})
	order := validOrder()
	order.Exchange = adapter.ExchangeKalshi
	order.Price = 0.45

	if err := v.Validate(order); err != nil {
		t.Fatalf("kalshi order should be valid, got %v", err)
	}
}

func TestValidate_UnknownExchange(t *testing.T) {
	v := NewValidator(&mockGate{canTrade: true})
	order := validOrder()
	order.Exchange = "binance"

	err := v.Validate(order)
	if err == nil {
		t.Fatal("expected error for unknown exchange")
	}
}

func TestValidate_StopLossOrder(t *testing.T) {
	v := NewValidator(&mockGate{canTrade: true})
	order := validOrder()
	order.Type = StopLoss
	order.Price = 0.30

	if err := v.Validate(order); err != nil {
		t.Fatalf("stop-loss order should be valid, got %v", err)
	}
}

func TestValidate_SellSide(t *testing.T) {
	v := NewValidator(&mockGate{canTrade: true})
	order := validOrder()
	order.Side = Sell

	if err := v.Validate(order); err != nil {
		t.Fatalf("sell order should be valid, got %v", err)
	}
}
