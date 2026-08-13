package tx

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/core"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
)

// ECDSAValidator accepts the baseline Pluto wire format: one signed Ethereum
// transaction encoded with Ethereum's canonical binary encoding.
type ECDSAValidator struct {
	chainID *big.Int
	signer  gethtypes.Signer
}

func NewECDSAValidator(chainID int64) (*ECDSAValidator, error) {
	if chainID <= 0 {
		return nil, fmt.Errorf("EVM chain ID must be positive")
	}
	configuredChainID := big.NewInt(chainID)
	return &ECDSAValidator{
		chainID: configuredChainID,
		signer:  gethtypes.LatestSignerForChainID(configuredChainID),
	}, nil
}

func (v *ECDSAValidator) Validate(raw []byte) (*ValidatedTransaction, error) {
	if v == nil {
		return nil, newValidationError(FailureAuthentication, "ECDSA validator is nil")
	}

	ethereumTx := new(gethtypes.Transaction)
	if err := ethereumTx.UnmarshalBinary(raw); err != nil {
		return nil, newValidationError(FailureEncoding, "decode Ethereum transaction: %w", err)
	}
	if !ethereumTx.Protected() || ethereumTx.ChainId().Cmp(v.chainID) != 0 {
		return nil, newValidationError(
			FailureAuthentication,
			"EVM chain ID mismatch: got %s, want %s",
			ethereumTx.ChainId(),
			v.chainID,
		)
	}

	message, err := core.TransactionToMessage(ethereumTx, v.signer, big.NewInt(0))
	if err != nil {
		return nil, newValidationError(FailureAuthentication, "convert Ethereum transaction: %w", err)
	}

	return &ValidatedTransaction{
		Message: message,
		Sender:  message.From,
	}, nil
}
