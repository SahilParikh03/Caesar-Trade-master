package signer

import (
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/awnumar/memguard"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

var (
	ErrNoActiveSession    = errors.New("no active session")
	ErrSessionExpired     = errors.New("session expired")
	ErrValueLimitExceeded = errors.New("cumulative value limit exceeded")
)

// EIP-712 type hashes (pre-computed keccak256 of the type strings).
var (
	// keccak256("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)")
	eip712DomainTypeHash = crypto.Keccak256Hash([]byte(
		"EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)",
	))

	// keccak256("Order(uint256 salt,address maker,address signer,address taker,uint256 tokenId,uint256 makerAmount,uint256 takerAmount,uint256 expiration,uint256 nonce,uint256 feeRateBps,uint8 side,uint8 signatureType)")
	orderTypeHash = crypto.Keccak256Hash([]byte(
		"Order(uint256 salt,address maker,address signer,address taker,uint256 tokenId,uint256 makerAmount,uint256 takerAmount,uint256 expiration,uint256 nonce,uint256 feeRateBps,uint8 side,uint8 signatureType)",
	))
)

// OrderData holds the fields needed to produce an EIP-712 Order struct hash.
type OrderData struct {
	Salt          *big.Int
	Maker         common.Address
	Signer        common.Address
	Taker         common.Address
	TokenID       *big.Int
	MakerAmount   *big.Int
	TakerAmount   *big.Int
	Expiration    *big.Int
	Nonce         *big.Int
	FeeRateBps    *big.Int
	Side          uint8
	SignatureType uint8
}

// DomainData holds the EIP-712 domain separator fields.
type DomainData struct {
	Name              string
	Version           string
	ChainID           *big.Int
	VerifyingContract common.Address
}

// SessionManager holds a decrypted session key in locked memory with TTL
// and cumulative value-limit enforcement. The key is encrypted at rest via
// memguard.Enclave and only opened momentarily during Sign.
type SessionManager struct {
	mu            sync.RWMutex
	enclave       *memguard.Enclave // encrypted-at-rest key buffer
	address       string            // derived signer address (hex)
	expiresAt     time.Time
	maxValueLimit *big.Int // USDC atomic units (6 decimals)
	valueUsed     *big.Int // cumulative USDC signed
	ttl           time.Duration
}

// NewSessionManager creates a manager with the given default TTL.
// No session is active until Activate is called.
func NewSessionManager(ttl time.Duration) *SessionManager {
	return &SessionManager{
		ttl:       ttl,
		valueUsed: new(big.Int),
	}
}

// Activate seals keyBytes into a memguard Enclave, derives the Ethereum
// address from the private key, sets expiry, and resets counters.
// The caller MUST zero their copy of keyBytes after calling this.
func (sm *SessionManager) Activate(keyBytes []byte, maxValueLimit *big.Int) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Derive address before sealing the key.
	privKey, err := crypto.ToECDSA(keyBytes)
	if err != nil {
		return fmt.Errorf("invalid private key: %w", err)
	}
	addr := crypto.PubkeyToAddress(privKey.PublicKey)

	// Clear any previous session.
	sm.enclave = nil

	sm.enclave = memguard.NewEnclave(keyBytes)
	sm.expiresAt = time.Now().Add(sm.ttl)
	sm.maxValueLimit = new(big.Int).Set(maxValueLimit)
	sm.valueUsed = new(big.Int)
	sm.address = addr.Hex()

	return nil
}

// Sign opens the enclave momentarily, computes the EIP-712 digest, signs it
// with ECDSA, and returns a 65-byte signature (r || s || v). It enforces
// session active, TTL, and cumulative value limit checks.
func (sm *SessionManager) Sign(orderValue *big.Int, domain *DomainData, order *OrderData) ([]byte, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.enclave == nil {
		return nil, ErrNoActiveSession
	}

	if sm.isExpired() {
		sm.destroyLocked()
		return nil, ErrSessionExpired
	}

	// Check cumulative value limit.
	newTotal := new(big.Int).Add(sm.valueUsed, orderValue)
	if newTotal.Cmp(sm.maxValueLimit) > 0 {
		return nil, ErrValueLimitExceeded
	}

	// Compute the EIP-712 digest.
	domainHash := hashDomain(domain)
	orderHash := hashOrder(order)
	digest := eip712Digest(domainHash, orderHash)

	// Open the enclave into a LockedBuffer for signing.
	buf, err := sm.enclave.Open()
	if err != nil {
		return nil, fmt.Errorf("open enclave: %w", err)
	}

	privKey, err := crypto.ToECDSA(buf.Bytes())
	buf.Destroy()
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	sig, err := crypto.Sign(digest[:], privKey)
	if err != nil {
		return nil, fmt.Errorf("ecdsa sign: %w", err)
	}

	// Adjust v value for Ethereum compatibility (0/1 → 27/28).
	sig[64] += 27

	// Commit value usage only after successful signing.
	sm.valueUsed.Set(newTotal)

	return sig, nil
}

// Status returns a read-only snapshot of the current session state.
// Monetary values are returned as decimal strings.
func (sm *SessionManager) Status() (active bool, ttlRemaining int64, maxLimit string, used string, address string) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if sm.enclave == nil {
		return false, 0, "0", "0", ""
	}

	if sm.isExpired() {
		return false, 0, "0", "0", ""
	}

	remaining := time.Until(sm.expiresAt).Seconds()
	if remaining < 0 {
		remaining = 0
	}

	return true, int64(remaining), sm.maxValueLimit.String(), sm.valueUsed.String(), sm.address
}

// Destroy zeroes and destroys the enclave, resetting all session state.
func (sm *SessionManager) Destroy() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.destroyLocked()
}

// destroyLocked performs the actual cleanup. Caller must hold sm.mu.
func (sm *SessionManager) destroyLocked() {
	sm.enclave = nil
	sm.address = ""
	sm.valueUsed = new(big.Int)
	sm.maxValueLimit = nil
}

// isExpired checks whether the session TTL has elapsed. Caller must hold sm.mu.
func (sm *SessionManager) isExpired() bool {
	return time.Now().After(sm.expiresAt)
}

// hashDomain computes the EIP-712 domain separator hash.
func hashDomain(d *DomainData) common.Hash {
	return crypto.Keccak256Hash(
		eip712DomainTypeHash.Bytes(),
		crypto.Keccak256([]byte(d.Name)),
		crypto.Keccak256([]byte(d.Version)),
		common.LeftPadBytes(d.ChainID.Bytes(), 32),
		common.LeftPadBytes(d.VerifyingContract.Bytes(), 32),
	)
}

// hashOrder computes the EIP-712 struct hash for a Polymarket Order.
func hashOrder(o *OrderData) common.Hash {
	return crypto.Keccak256Hash(
		orderTypeHash.Bytes(),
		common.LeftPadBytes(o.Salt.Bytes(), 32),
		common.LeftPadBytes(o.Maker.Bytes(), 32),
		common.LeftPadBytes(o.Signer.Bytes(), 32),
		common.LeftPadBytes(o.Taker.Bytes(), 32),
		common.LeftPadBytes(o.TokenID.Bytes(), 32),
		common.LeftPadBytes(o.MakerAmount.Bytes(), 32),
		common.LeftPadBytes(o.TakerAmount.Bytes(), 32),
		common.LeftPadBytes(o.Expiration.Bytes(), 32),
		common.LeftPadBytes(o.Nonce.Bytes(), 32),
		common.LeftPadBytes(o.FeeRateBps.Bytes(), 32),
		common.LeftPadBytes(big.NewInt(int64(o.Side)).Bytes(), 32),
		common.LeftPadBytes(big.NewInt(int64(o.SignatureType)).Bytes(), 32),
	)
}

// eip712Digest computes the final EIP-712 signing digest:
// keccak256("\x19\x01" || domainSeparator || structHash)
func eip712Digest(domainHash, structHash common.Hash) common.Hash {
	return crypto.Keccak256Hash(
		[]byte{0x19, 0x01},
		domainHash.Bytes(),
		structHash.Bytes(),
	)
}
