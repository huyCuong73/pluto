package tx

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/rlp"
)

const (
	HybridSignDomainV1        = "PLUTO_HYBRID_TX_V1"
	MLDSA65TransactionContext = "PLUTO-TX-V1"
)

// BuildHybridSignBytesV1 commits to the protocol domain, envelope header, EVM
// chain, complete signed Ethereum transaction, and hash of the PQ public key.
// PQSignature is intentionally excluded because these bytes are its input.
func BuildHybridSignBytesV1(envelope *HybridEnvelopeV1, chainID *big.Int) ([]byte, error) {
	if err := validateEnvelopeV1(envelope, false); err != nil {
		return nil, err
	}
	if chainID == nil || chainID.Sign() <= 0 {
		return nil, ErrInvalidChainID
	}

	publicKeyHash := HashPQPublicKey(envelope.PQPublicKey)
	payload := struct {
		Domain          []byte
		Version         uint8
		Algorithm       uint8
		ChainID         *big.Int
		EthereumTx      []byte
		PQPublicKeyHash []byte
	}{
		Domain:          []byte(HybridSignDomainV1),
		Version:         envelope.Version,
		Algorithm:       envelope.Algorithm,
		ChainID:         new(big.Int).Set(chainID),
		EthereumTx:      envelope.EthereumTx,
		PQPublicKeyHash: publicKeyHash[:],
	}
	encoded, err := rlp.EncodeToBytes(payload)
	if err != nil {
		return nil, fmt.Errorf("encode hybrid sign bytes v1: %w", err)
	}
	return encoded, nil
}
