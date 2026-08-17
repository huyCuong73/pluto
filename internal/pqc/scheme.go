// Package pqc exposes post-quantum signature primitives without depending on
// blockchain, consensus, transaction, EVM, or storage packages.
package pqc

import "io"

// Scheme is the replaceable post-quantum signature boundary consumed by the
// transaction module. Implementations operate only on byte slices and context.
type Scheme interface {
	GenerateKey(random io.Reader) (publicKey, privateKey []byte, err error)
	Sign(privateKey, message, context []byte) ([]byte, error)
	Verify(publicKey, message, context, signature []byte) error
}
