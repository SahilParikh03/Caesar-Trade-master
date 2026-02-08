package signer

import (
	"context"
	"encoding/hex"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"

	signerv1 "github.com/caesar-terminal/caesar/internal/gen/signer/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Handler implements the SignerServiceServer interface.
type Handler struct {
	signerv1.UnimplementedSignerServiceServer
	session *SessionManager
}

// NewHandler creates a Handler wired to the given SessionManager.
func NewHandler(session *SessionManager) *Handler {
	return &Handler{session: session}
}

// SignOrder signs a Polymarket order using EIP-712 typed data.
// Delegates to the SessionManager which enforces TTL and value limits.
func (h *Handler) SignOrder(_ context.Context, req *signerv1.SignOrderRequest) (*signerv1.SignOrderResponse, error) {
	if req.Order == nil {
		return nil, status.Errorf(codes.InvalidArgument, "order is required")
	}
	if req.Domain == nil {
		return nil, status.Errorf(codes.InvalidArgument, "domain is required")
	}

	// Parse the maker amount as the order value for limit tracking.
	orderValue := new(big.Int)
	if _, ok := orderValue.SetString(req.Order.MakerAmount, 10); !ok {
		return nil, status.Errorf(codes.InvalidArgument, "invalid maker_amount: %s", req.Order.MakerAmount)
	}

	domain := &DomainData{
		Name:              req.Domain.Name,
		Version:           req.Domain.Version,
		ChainID:           big.NewInt(req.Domain.ChainId),
		VerifyingContract: common.HexToAddress(req.Domain.VerifyingContract),
	}

	tokenID := new(big.Int)
	tokenID.SetString(req.Order.TokenId, 10)

	makerAmt := new(big.Int)
	makerAmt.SetString(req.Order.MakerAmount, 10)

	takerAmt := new(big.Int)
	takerAmt.SetString(req.Order.TakerAmount, 10)

	order := &OrderData{
		Salt:          new(big.Int), // Salt can be set to 0 or passed via request extension.
		Maker:         common.HexToAddress(req.Order.Maker),
		Signer:        common.HexToAddress(req.Order.Maker), // Signer defaults to maker.
		Taker:         common.HexToAddress(req.Order.Taker),
		TokenID:       tokenID,
		MakerAmount:   makerAmt,
		TakerAmount:   takerAmt,
		Expiration:    new(big.Int).SetUint64(req.Order.Expiration),
		Nonce:         new(big.Int).SetUint64(req.Order.Nonce),
		FeeRateBps:    new(big.Int).SetUint64(uint64(req.Order.FeeRateBps)),
		Side:          protoSideToUint8(req.Order.Side),
		SignatureType: protoSigTypeToUint8(req.Order.SignatureType),
	}

	sig, err := h.session.Sign(orderValue, domain, order)
	if err != nil {
		switch err {
		case ErrNoActiveSession:
			return nil, status.Errorf(codes.FailedPrecondition, "no active session")
		case ErrSessionExpired:
			return nil, status.Errorf(codes.FailedPrecondition, "session expired")
		case ErrValueLimitExceeded:
			return nil, status.Errorf(codes.ResourceExhausted, "cumulative value limit exceeded")
		default:
			return nil, status.Errorf(codes.Internal, "signing failed: %v", err)
		}
	}

	_, _, _, _, addr := h.session.Status()

	return &signerv1.SignOrderResponse{
		Signature:     "0x" + hex.EncodeToString(sig),
		SignerAddress: addr,
		SignedAt:      time.Now().UnixNano(),
	}, nil
}

// GetSessionStatus returns the current session key status.
func (h *Handler) GetSessionStatus(_ context.Context, _ *signerv1.GetSessionStatusRequest) (*signerv1.GetSessionStatusResponse, error) {
	active, ttl, maxLimit, used, addr := h.session.Status()

	return &signerv1.GetSessionStatusResponse{
		Active:         active,
		TtlSeconds:     ttl,
		MaxValueLimit:  maxLimit,
		ValueUsed:      used,
		SessionAddress: addr,
	}, nil
}

// protoSideToUint8 maps the proto OrderSide enum to Polymarket's uint8 convention.
// Polymarket: BUY=0, SELL=1.
func protoSideToUint8(s signerv1.OrderSide) uint8 {
	switch s {
	case signerv1.OrderSide_ORDER_SIDE_BUY:
		return 0
	case signerv1.OrderSide_ORDER_SIDE_SELL:
		return 1
	default:
		return 0
	}
}

// protoSigTypeToUint8 maps the proto SignatureType enum to Polymarket's uint8 convention.
func protoSigTypeToUint8(s signerv1.SignatureType) uint8 {
	switch s {
	case signerv1.SignatureType_SIGNATURE_TYPE_EOA:
		return 0
	case signerv1.SignatureType_SIGNATURE_TYPE_POLY_PROXY:
		return 1
	case signerv1.SignatureType_SIGNATURE_TYPE_POLY_GNOSIS_SAFE:
		return 2
	default:
		return 0
	}
}
