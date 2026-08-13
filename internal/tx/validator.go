// Package tx defines the transaction boundary consumed by the ABCI app.
//
// Consensus and EVM execution depend on Validator, not on one wire format or
// signature scheme. An ECDSA transaction, a future hybrid ECDSA+ML-DSA
// envelope, or another policy can therefore be selected at the composition
// root without changing the ABCI lifecycle.
package tx

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
)

// FailureKind is stable across validator implementations so the app can map
// module errors to deterministic ABCI response codes.
type FailureKind uint8

const (
	FailureUnknown FailureKind = iota
	FailureEncoding
	FailureAuthentication
	FailurePQC
)

// ValidationError keeps the module-specific cause while exposing a stable
// category to the application layer.
type ValidationError struct {
	Kind FailureKind
	Err  error
}

func (e *ValidationError) Error() string {
	if e == nil || e.Err == nil {
		return "transaction validation failed"
	}
	return e.Err.Error()
}

func (e *ValidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func newValidationError(kind FailureKind, format string, args ...any) error {
	return &ValidationError{Kind: kind, Err: fmt.Errorf(format, args...)}
}

// FailureOf extracts a stable failure category without coupling the app to a
// concrete validator implementation.
func FailureOf(err error) FailureKind {
	var validationError *ValidationError
	if errors.As(err, &validationError) {
		return validationError.Kind
	}
	return FailureUnknown
}

// ValidatedTransaction is the normalized output understood by the EVM layer.
// Wire-format and signature-specific fields remain inside the validator, so
// the application does not need to decode or authenticate the transaction a
// second time.
type ValidatedTransaction struct {
	Message *core.Message
	Sender  common.Address
}

// TransactionValidator is the replaceable transaction admission boundary.
// Implementations must be deterministic because every validator node calls it.
type TransactionValidator interface {
	Validate(raw []byte) (*ValidatedTransaction, error)
}
