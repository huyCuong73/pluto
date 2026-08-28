package tx

import (
	"bytes"
	"fmt"
	"math"

	"github.com/ethereum/go-ethereum/rlp"
	"github.com/huyCuong73/pluto/internal/pqc"
)

const (
	EnvelopeVersionV1 uint8 = 1
	AlgorithmMLDSA65  uint8 = 1

	// MaxEthereumTxSize is the prototype protocol limit. It is large enough for
	// ordinary transfers and prototype contract deployment while bounding RLP
	// allocation before deeper parsing. It can become chain configuration later.
	MaxEthereumTxSize = 128 * 1024

	// The extra allowance covers RLP list/string headers and future-compatible
	// framing metadata without weakening the field-specific limits below.
	MaxHybridEnvelopeSize = MaxEthereumTxSize +
		pqc.MLDSA65PublicKeySize + pqc.MLDSA65SignatureSize + 256
)

// HybridEnvelopeV1 carries a classically signed Ethereum transaction and the
// post-quantum material that authenticates its canonical sign bytes.
type HybridEnvelopeV1 struct {
	Version     uint8
	Algorithm   uint8
	EthereumTx  []byte
	PQPublicKey []byte
	PQSignature []byte
}

// EncodeHybridEnvelopeV1 encodes exactly five fields as canonical RLP.
func EncodeHybridEnvelopeV1(envelope *HybridEnvelopeV1) ([]byte, error) {
	if err := validateEnvelopeV1(envelope, true); err != nil {
		return nil, err
	}
	encoded, err := encodeEnvelopeRLP(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode hybrid envelope v1: %w", err)
	}
	if len(encoded) > MaxHybridEnvelopeSize {
		return nil, fmt.Errorf("%w: got %d, max %d", ErrEnvelopeTooLarge, len(encoded), MaxHybridEnvelopeSize)
	}
	return encoded, nil
}

// DecodeHybridEnvelopeV1 bounds the complete input first, then reads and
// validates version and algorithm before exposing the larger byte fields.
func DecodeHybridEnvelopeV1(encoded []byte) (*HybridEnvelopeV1, error) {
	if len(encoded) == 0 {
		return nil, ErrEmptyEnvelope
	}
	if len(encoded) > MaxHybridEnvelopeSize {
		return nil, fmt.Errorf("%w: got %d, max %d", ErrEnvelopeTooLarge, len(encoded), MaxHybridEnvelopeSize)
	}

	payload, trailing, err := rlp.SplitList(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: outer list: %v", ErrMalformedEnvelope, err)
	}
	if len(trailing) != 0 {
		return nil, fmt.Errorf("%w: trailing bytes", ErrMalformedEnvelope)
	}

	version, payload, err := splitUint8("version", payload)
	if err != nil {
		return nil, err
	}
	if version != EnvelopeVersionV1 {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrUnsupportedEnvelopeVersion, version, EnvelopeVersionV1)
	}

	algorithm, payload, err := splitUint8("algorithm", payload)
	if err != nil {
		return nil, err
	}
	if algorithm != AlgorithmMLDSA65 {
		return nil, fmt.Errorf("%w: got %d", ErrUnsupportedAlgorithm, algorithm)
	}

	ethereumTx, payload, err := splitBytes("Ethereum transaction", payload)
	if err != nil {
		return nil, err
	}
	publicKey, payload, err := splitBytes("post-quantum public key", payload)
	if err != nil {
		return nil, err
	}
	signature, payload, err := splitBytes("post-quantum signature", payload)
	if err != nil {
		return nil, err
	}
	if len(payload) != 0 {
		return nil, fmt.Errorf("%w: expected exactly five fields", ErrMalformedEnvelope)
	}

	envelope := &HybridEnvelopeV1{
		Version:     version,
		Algorithm:   algorithm,
		EthereumTx:  bytes.Clone(ethereumTx),
		PQPublicKey: bytes.Clone(publicKey),
		PQSignature: bytes.Clone(signature),
	}
	if err := validateEnvelopeV1(envelope, true); err != nil {
		return nil, err
	}

	canonical, err := encodeEnvelopeRLP(envelope)
	if err != nil {
		return nil, fmt.Errorf("re-encode hybrid envelope v1: %w", err)
	}
	if !bytes.Equal(canonical, encoded) {
		return nil, ErrNonCanonicalEnvelope
	}
	return envelope, nil
}

func validateEnvelopeV1(envelope *HybridEnvelopeV1, requireSignature bool) error {
	if envelope == nil {
		return ErrNilEnvelope
	}
	if envelope.Version != EnvelopeVersionV1 {
		return fmt.Errorf("%w: got %d, want %d", ErrUnsupportedEnvelopeVersion, envelope.Version, EnvelopeVersionV1)
	}
	if envelope.Algorithm != AlgorithmMLDSA65 {
		return fmt.Errorf("%w: got %d", ErrUnsupportedAlgorithm, envelope.Algorithm)
	}
	if len(envelope.EthereumTx) == 0 || len(envelope.EthereumTx) > MaxEthereumTxSize {
		return fmt.Errorf("%w: got %d, allowed 1..%d", ErrInvalidEthereumTxSize, len(envelope.EthereumTx), MaxEthereumTxSize)
	}
	if len(envelope.PQPublicKey) != pqc.MLDSA65PublicKeySize {
		return fmt.Errorf("%w: got %d, want %d", ErrInvalidPQPublicKeySize, len(envelope.PQPublicKey), pqc.MLDSA65PublicKeySize)
	}
	if (requireSignature || len(envelope.PQSignature) != 0) && len(envelope.PQSignature) != pqc.MLDSA65SignatureSize {
		return fmt.Errorf("%w: got %d, want %d", ErrInvalidPQSignatureSize, len(envelope.PQSignature), pqc.MLDSA65SignatureSize)
	}
	return nil
}

func encodeEnvelopeRLP(envelope *HybridEnvelopeV1) ([]byte, error) {
	return rlp.EncodeToBytes(struct {
		Version     uint8
		Algorithm   uint8
		EthereumTx  []byte
		PQPublicKey []byte
		PQSignature []byte
	}{
		Version:     envelope.Version,
		Algorithm:   envelope.Algorithm,
		EthereumTx:  envelope.EthereumTx,
		PQPublicKey: envelope.PQPublicKey,
		PQSignature: envelope.PQSignature,
	})
}

func splitUint8(field string, payload []byte) (uint8, []byte, error) {
	value, rest, err := rlp.SplitUint64(payload)
	if err != nil {
		return 0, payload, fmt.Errorf("%w: %s: %v", ErrMalformedEnvelope, field, err)
	}
	if value > math.MaxUint8 {
		return 0, payload, fmt.Errorf("%w: %s overflows uint8", ErrMalformedEnvelope, field)
	}
	return uint8(value), rest, nil
}

func splitBytes(field string, payload []byte) ([]byte, []byte, error) {
	value, rest, err := rlp.SplitString(payload)
	if err != nil {
		return nil, payload, fmt.Errorf("%w: %s: %v", ErrMalformedEnvelope, field, err)
	}
	return value, rest, nil
}
