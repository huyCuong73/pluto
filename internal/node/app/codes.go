package app

const (
	CodeOK uint32 = iota
	CodeInvalidEncoding
	CodeInvalidSignature
	CodeNonceMismatch
	CodeExecutionFailed
	CodeInvalidPQC
	CodeInvalidQuery
	// CodeTransactionRejected is a consensus-invalid transaction that never
	// entered EVM execution (for example intrinsic gas or block gas failure).
	// CodeExecutionFailed instead means an accepted transition whose EVM call
	// reverted or otherwise failed after nonce/gas processing.
	CodeTransactionRejected
)

const Codespace = "pluto"
