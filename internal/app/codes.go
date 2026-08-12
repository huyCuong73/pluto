package app

const (
	CodeOK uint32 = iota
	CodeInvalidEncoding
	CodeInvalidSignature
	CodeNonceMismatch
	CodeExecutionFailed
	CodeInvalidPQC
	CodeInvalidQuery
)

const Codespace = "pluto"
