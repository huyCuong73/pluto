package pqc

import (
	"errors"
	"fmt"
	"io"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
)

const (
	// ML-DSA contexts are limited to 255 bytes by FIPS 204.
	MLDSA65MaxContextSize = 255

	MLDSA65PublicKeySize  = mldsa65.PublicKeySize
	MLDSA65PrivateKeySize = mldsa65.PrivateKeySize
	MLDSA65SignatureSize  = mldsa65.SignatureSize
)

var (
	ErrInvalidPublicKeySize  = errors.New("invalid ML-DSA-65 public key size")
	ErrInvalidPrivateKeySize = errors.New("invalid ML-DSA-65 private key size")
	ErrInvalidSignatureSize  = errors.New("invalid ML-DSA-65 signature size")
	ErrContextTooLong        = errors.New("ML-DSA-65 context exceeds 255 bytes")
	ErrVerificationFailed    = errors.New("ML-DSA-65 signature verification failed")
)

// MLDSA65 adapts CIRCL's FIPS 204 ML-DSA-65 implementation to Scheme.
// It has no mutable state, so one value can safely be shared by callers.
type MLDSA65 struct{}

var _ Scheme = MLDSA65{}

func NewMLDSA65() MLDSA65 {
	return MLDSA65{}
}

// GenerateKey creates a packed ML-DSA-65 public/private key pair. CIRCL uses
// crypto/rand.Reader when random is nil; accepting a reader also enables
// deterministic test vectors and explicit entropy-failure tests.
func (MLDSA65) GenerateKey(random io.Reader) ([]byte, []byte, error) {
	publicKey, privateKey, err := mldsa65.GenerateKey(random)
	if err != nil {
		return nil, nil, fmt.Errorf("generate ML-DSA-65 key pair: %w", err)
	}
	return publicKey.Bytes(), privateKey.Bytes(), nil
}

// Sign signs message with the supplied FIPS 204 context. SignTo is called in
// deterministic mode. The output buffer is allocated at the exact required
// size because CIRCL's low-level API panics for a short destination buffer.
func (MLDSA65) Sign(privateKey, message, context []byte) ([]byte, error) {
	if len(privateKey) != MLDSA65PrivateKeySize {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrInvalidPrivateKeySize, len(privateKey), MLDSA65PrivateKeySize)
	}
	if len(context) > MLDSA65MaxContextSize {
		return nil, fmt.Errorf("%w: got %d", ErrContextTooLong, len(context))
	}

	key := new(mldsa65.PrivateKey)
	if err := key.UnmarshalBinary(privateKey); err != nil {
		return nil, fmt.Errorf("decode ML-DSA-65 private key: %w", err)
	}

	signature := make([]byte, MLDSA65SignatureSize)
	if err := mldsa65.SignTo(key, message, context, false, signature); err != nil {
		return nil, fmt.Errorf("sign with ML-DSA-65: %w", err)
	}
	return signature, nil
}

// Verify checks a packed public key and signature against message and context.
// All public-input sizes are rejected before parsing or cryptographic work.
func (MLDSA65) Verify(publicKey, message, context, signature []byte) error {
	if len(publicKey) != MLDSA65PublicKeySize {
		return fmt.Errorf("%w: got %d, want %d", ErrInvalidPublicKeySize, len(publicKey), MLDSA65PublicKeySize)
	}
	if len(signature) != MLDSA65SignatureSize {
		return fmt.Errorf("%w: got %d, want %d", ErrInvalidSignatureSize, len(signature), MLDSA65SignatureSize)
	}
	if len(context) > MLDSA65MaxContextSize {
		return fmt.Errorf("%w: got %d", ErrContextTooLong, len(context))
	}

	key := new(mldsa65.PublicKey)
	if err := key.UnmarshalBinary(publicKey); err != nil {
		return fmt.Errorf("decode ML-DSA-65 public key: %w", err)
	}
	if !mldsa65.Verify(key, message, context, signature) {
		return ErrVerificationFailed
	}
	return nil
}
