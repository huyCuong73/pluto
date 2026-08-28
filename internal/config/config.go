package config

import (
	"fmt"
	"strings"
)

const (
	// DefaultCometChainID identifies the local Pluto consensus network.
	DefaultCometChainID = "pluto-local-1"

	// DefaultEVMChainID is intentionally different from Ethereum mainnet (1)
	// to prevent accidental cross-chain transaction replay.
	DefaultEVMChainID int64 = 700001

	// TransactionPolicyECDSA keeps the original signed-Ethereum-transaction
	// wire format. TransactionPolicyHybridMLDSA65 requires the versioned Pluto
	// envelope and both ECDSA and ML-DSA-65 authentication.
	TransactionPolicyECDSA         = "ecdsa"
	TransactionPolicyHybridMLDSA65 = "hybrid-mldsa65"
	// TransactionPolicyPQCOptInMLDSA65 giữ MetaMask cho account thường, nhưng
	// account có binding trong pqc_keys bắt buộc dùng hybrid transaction.
	TransactionPolicyPQCOptInMLDSA65 = "pqc-opt-in-mldsa65"
)

// NormalizeTransactionPolicy applies the backward-compatible ECDSA default
// and rejects values that would make validators assemble different pipelines.
func NormalizeTransactionPolicy(policy string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(policy))
	if normalized == "" {
		return TransactionPolicyECDSA, nil
	}
	switch normalized {
	case TransactionPolicyECDSA, TransactionPolicyHybridMLDSA65, TransactionPolicyPQCOptInMLDSA65:
		return normalized, nil
	default:
		return "", fmt.Errorf("unsupported transaction policy %q", policy)
	}
}
