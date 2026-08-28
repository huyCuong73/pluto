package tx

import (
	"crypto/subtle"
	"math/big"

	"github.com/huyCuong73/pluto/internal/pqc"
)

// HybridValidator enforces the Pluto phase-one policy:
//
//	valid Ethereum ECDSA transaction
//	AND registered ML-DSA-65 public key for the recovered sender
//	AND valid ML-DSA-65 signature over canonical hybrid sign bytes.
type HybridValidator struct {
	chainID           *big.Int
	ethereumValidator TransactionValidator
	pqScheme          pqc.Scheme
	keyResolver       PQKeyResolver
}

func NewHybridValidator(
	chainID int64,
	ethereumValidator TransactionValidator,
	pqScheme pqc.Scheme,
	keyResolver PQKeyResolver,
) (*HybridValidator, error) {
	if chainID <= 0 {
		return nil, ErrInvalidChainID
	}
	if ethereumValidator == nil {
		return nil, ErrNilEthereumValidator
	}
	if pqScheme == nil {
		return nil, ErrNilPQScheme
	}
	if keyResolver == nil {
		return nil, ErrNilPQKeyResolver
	}
	return &HybridValidator{
		chainID:           big.NewInt(chainID),
		ethereumValidator: ethereumValidator,
		pqScheme:          pqScheme,
		keyResolver:       keyResolver,
	}, nil
}

func (v *HybridValidator) Validate(raw []byte) (*ValidatedTransaction, error) {
	if v == nil {
		return nil, newValidationError(FailurePQC, "hybrid validator is nil")
	}

	envelope, err := DecodeHybridEnvelopeV1(raw)
	if err != nil {
		return nil, newValidationError(FailureEncoding, "decode hybrid transaction: %w", err)
	}

	validated, err := v.ethereumValidator.Validate(envelope.EthereumTx)
	if err != nil {
		// Preserve the stable failure category produced by the selected Ethereum
		// validator rather than translating it into a PQC failure.
		return nil, err
	}
	if validated == nil || validated.Message == nil {
		return nil, newValidationError(FailureAuthentication, "Ethereum validator returned an empty transaction")
	}

	expectedHash, found, err := v.keyResolver.ExpectedKeyHash(validated.Sender)
	if err != nil {
		return nil, newValidationError(FailurePQC, "resolve post-quantum key for %s: %w", validated.Sender.Hex(), err)
	}
	if !found {
		return nil, newValidationError(FailurePQC, "%w: %s", ErrPQKeyNotRegistered, validated.Sender.Hex())
	}
	actualHash := HashPQPublicKey(envelope.PQPublicKey)
	if subtle.ConstantTimeCompare(actualHash[:], expectedHash[:]) != 1 {
		return nil, newValidationError(FailurePQC, "%w: %s", ErrPQKeyBindingMismatch, validated.Sender.Hex())
	}

	signBytes, err := BuildHybridSignBytesV1(envelope, v.chainID)
	if err != nil {
		return nil, newValidationError(FailureEncoding, "build hybrid sign bytes: %w", err)
	}
	if err := v.pqScheme.Verify(
		envelope.PQPublicKey,
		signBytes,
		[]byte(MLDSA65TransactionContext),
		envelope.PQSignature,
	); err != nil {
		return nil, newValidationError(FailurePQC, "verify ML-DSA-65 transaction signature: %w", err)
	}

	return validated, nil
}
