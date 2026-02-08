package signer_test

import (
	"context"
	"encoding/hex"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	signerv1 "github.com/caesar-terminal/caesar/internal/gen/signer/v1"
	"github.com/caesar-terminal/caesar/internal/signer"
)

// TestIntegration_SignOrder starts a real gRPC server on a temporary Unix
// domain socket, activates a session with a test key, signs a mock order,
// and verifies the signature is a valid 65-byte ECDSA output.
func TestIntegration_SignOrder(t *testing.T) {
	// Create a temporary directory for the UDS socket.
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test-signer.sock")

	// Set up SessionManager with a generous TTL.
	sm := signer.NewSessionManager(10 * time.Minute)

	// Generate a test private key.
	privKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	keyBytes := crypto.FromECDSA(privKey)
	expectedAddr := crypto.PubkeyToAddress(privKey.PublicKey).Hex()

	// Activate the session.
	maxValue := new(big.Int).Mul(big.NewInt(1_000_000), big.NewInt(1e6)) // 1M USDC
	if err := sm.Activate(keyBytes, maxValue); err != nil {
		t.Fatalf("activate: %v", err)
	}

	// Start the gRPC server.
	srv, err := signer.New(socketPath, sm)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	go func() {
		if err := srv.Serve(); err != nil {
			// Server stopped — expected during cleanup.
		}
	}()
	t.Cleanup(func() {
		srv.GracefulStop()
	})

	// Wait briefly for the server to start listening.
	waitForSocket(t, socketPath)

	// Dial the server via UDS.
	conn, err := grpc.NewClient(
		"unix:"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	client := signerv1.NewSignerServiceClient(conn)

	// Verify session status first.
	statusResp, err := client.GetSessionStatus(context.Background(), &signerv1.GetSessionStatusRequest{})
	if err != nil {
		t.Fatalf("get session status: %v", err)
	}
	if !statusResp.Active {
		t.Fatal("expected session to be active")
	}
	if statusResp.SessionAddress != expectedAddr {
		t.Errorf("address mismatch: got %s, want %s", statusResp.SessionAddress, expectedAddr)
	}

	// Sign a mock order.
	req := &signerv1.SignOrderRequest{
		Domain: &signerv1.EIP712Domain{
			Name:              "ClobExchange",
			Version:           "1",
			ChainId:           137,
			VerifyingContract: "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E",
		},
		Order: &signerv1.PolymarketOrder{
			Maker:         "0x1234567890abcdef1234567890abcdef12345678",
			Taker:         "0x0000000000000000000000000000000000000000",
			TokenId:       "12345",
			ConditionId:   "0xabcdef",
			Side:          signerv1.OrderSide_ORDER_SIDE_BUY,
			MakerAmount:   "100000000",
			TakerAmount:   "50000000",
			Expiration:    1700000000,
			Nonce:         1,
			FeeRateBps:    100,
			SignatureType: signerv1.SignatureType_SIGNATURE_TYPE_EOA,
		},
	}

	resp, err := client.SignOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("sign order: %v", err)
	}

	// Verify the signature is hex-encoded and 65 bytes.
	sigHex := resp.Signature
	if len(sigHex) < 2 || sigHex[:2] != "0x" {
		t.Fatalf("signature should start with 0x, got: %s", sigHex[:10])
	}
	sigBytes, err := hex.DecodeString(sigHex[2:])
	if err != nil {
		t.Fatalf("decode signature hex: %v", err)
	}
	if len(sigBytes) != 65 {
		t.Fatalf("expected 65-byte signature, got %d bytes", len(sigBytes))
	}

	// Verify v value is 27 or 28.
	v := sigBytes[64]
	if v != 27 && v != 28 {
		t.Errorf("expected v=27 or v=28, got v=%d", v)
	}

	// Verify the signer address matches.
	if resp.SignerAddress != expectedAddr {
		t.Errorf("signer address mismatch: got %s, want %s", resp.SignerAddress, expectedAddr)
	}

	// Verify signed_at is recent.
	if resp.SignedAt == 0 {
		t.Error("signed_at should be non-zero")
	}

	t.Logf("signature: %s", sigHex)
	t.Logf("signer: %s", resp.SignerAddress)
	t.Logf("signed_at: %d", resp.SignedAt)
}

// TestIntegration_ValueLimit verifies the cumulative value limit is enforced
// across multiple sign operations.
func TestIntegration_ValueLimit(t *testing.T) {
	sm := signer.NewSessionManager(10 * time.Minute)

	privKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	keyBytes := crypto.FromECDSA(privKey)

	// Set a small limit: 200 USDC (200_000_000 atomic units).
	maxValue := big.NewInt(200_000_000)
	if err := sm.Activate(keyBytes, maxValue); err != nil {
		t.Fatalf("activate: %v", err)
	}

	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test-limit.sock")
	srv, err := signer.New(socketPath, sm)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.GracefulStop() })
	waitForSocket(t, socketPath)

	conn, err := grpc.NewClient(
		"unix:"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	client := signerv1.NewSignerServiceClient(conn)

	makeReq := func(amount string) *signerv1.SignOrderRequest {
		return &signerv1.SignOrderRequest{
			Domain: &signerv1.EIP712Domain{
				Name:              "ClobExchange",
				Version:           "1",
				ChainId:           137,
				VerifyingContract: "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E",
			},
			Order: &signerv1.PolymarketOrder{
				Maker:       "0x1234567890abcdef1234567890abcdef12345678",
				Taker:       "0x0000000000000000000000000000000000000000",
				TokenId:     "12345",
				MakerAmount: amount,
				TakerAmount: "50000000",
				Nonce:       1,
				FeeRateBps:  100,
			},
		}
	}

	// First order: 100 USDC — should succeed.
	if _, err := client.SignOrder(context.Background(), makeReq("100000000")); err != nil {
		t.Fatalf("first sign should succeed: %v", err)
	}

	// Second order: 100 USDC — should succeed (total: 200 USDC = limit).
	if _, err := client.SignOrder(context.Background(), makeReq("100000000")); err != nil {
		t.Fatalf("second sign should succeed: %v", err)
	}

	// Third order: 1 USDC — should fail (would exceed limit).
	_, err = client.SignOrder(context.Background(), makeReq("1000000"))
	if err == nil {
		t.Fatal("third sign should fail due to value limit")
	}
	t.Logf("correctly rejected: %v", err)
}

// waitForSocket polls until the socket file appears or the timeout elapses.
func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			// Also try connecting to make sure it's listening.
			conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
			if err == nil {
				conn.Close()
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("socket %s did not become available", path)
}
