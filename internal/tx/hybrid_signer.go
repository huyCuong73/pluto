package tx

import (
	"bytes"
	"fmt"
	"math/big"

	"github.com/huyCuong73/pluto/internal/pqc"
)

// CreateSignedHybridEnvelopeV1 is the client-side assembly helper. The
// Ethereum transaction must already carry its ECDSA signature. Private key
// material is used only for signing and is never placed in the envelope.
func CreateSignedHybridEnvelopeV1(
	ethereumTx []byte,
	pqPublicKey []byte,
	pqPrivateKey []byte,
	chainID *big.Int,
	pqScheme pqc.Scheme,
) ([]byte, error) {
	if pqScheme == nil {
		return nil, ErrNilPQScheme
	}
	envelope := &HybridEnvelopeV1{
		Version:     EnvelopeVersionV1,
		Algorithm:   AlgorithmMLDSA65,
		EthereumTx:  bytes.Clone(ethereumTx),
		PQPublicKey: bytes.Clone(pqPublicKey),
	}
	signBytes, err := BuildHybridSignBytesV1(envelope, chainID)
	if err != nil {
		return nil, fmt.Errorf("build hybrid sign bytes: %w", err)
	}
	signature, err := pqScheme.Sign(
		pqPrivateKey,
		signBytes,
		[]byte(MLDSA65TransactionContext),
	)
	if err != nil {
		return nil, fmt.Errorf("sign hybrid transaction with ML-DSA-65: %w", err)
	}
	envelope.PQSignature = signature
	return EncodeHybridEnvelopeV1(envelope)
}
