package tx

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/rlp"
	"github.com/huyCuong73/pluto/internal/pqc"
)

func TestHybridEnvelopeV1RoundTripCanonical(t *testing.T) {
	envelope := testHybridEnvelope()
	encoded, err := EncodeHybridEnvelopeV1(envelope)
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	decoded, err := DecodeHybridEnvelopeV1(encoded)
	if err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !reflect.DeepEqual(decoded, envelope) {
		t.Fatalf("decoded envelope differs from input")
	}

	reencoded, err := EncodeHybridEnvelopeV1(decoded)
	if err != nil {
		t.Fatalf("re-encode envelope: %v", err)
	}
	if !bytes.Equal(encoded, reencoded) {
		t.Fatal("canonical envelope bytes changed after round trip")
	}

	// Decode owns its byte fields instead of retaining aliases into RPC input.
	wantLastSignatureByte := decoded.PQSignature[len(decoded.PQSignature)-1]
	encoded[len(encoded)-1] ^= 0xff
	if decoded.PQSignature[len(decoded.PQSignature)-1] != wantLastSignatureByte {
		t.Fatal("decoded envelope aliases encoded input")
	}
}

func TestEncodeHybridEnvelopeV1RejectsInvalidFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*HybridEnvelopeV1) *HybridEnvelopeV1
		want   error
	}{
		{name: "nil envelope", mutate: func(*HybridEnvelopeV1) *HybridEnvelopeV1 { return nil }, want: ErrNilEnvelope},
		{name: "wrong version", mutate: func(e *HybridEnvelopeV1) *HybridEnvelopeV1 { e.Version++; return e }, want: ErrUnsupportedEnvelopeVersion},
		{name: "wrong algorithm", mutate: func(e *HybridEnvelopeV1) *HybridEnvelopeV1 { e.Algorithm++; return e }, want: ErrUnsupportedAlgorithm},
		{name: "empty Ethereum transaction", mutate: func(e *HybridEnvelopeV1) *HybridEnvelopeV1 { e.EthereumTx = nil; return e }, want: ErrInvalidEthereumTxSize},
		{name: "oversized Ethereum transaction", mutate: func(e *HybridEnvelopeV1) *HybridEnvelopeV1 {
			e.EthereumTx = make([]byte, MaxEthereumTxSize+1)
			return e
		}, want: ErrInvalidEthereumTxSize},
		{name: "short public key", mutate: func(e *HybridEnvelopeV1) *HybridEnvelopeV1 {
			e.PQPublicKey = e.PQPublicKey[:len(e.PQPublicKey)-1]
			return e
		}, want: ErrInvalidPQPublicKeySize},
		{name: "long public key", mutate: func(e *HybridEnvelopeV1) *HybridEnvelopeV1 { e.PQPublicKey = append(e.PQPublicKey, 0); return e }, want: ErrInvalidPQPublicKeySize},
		{name: "empty signature", mutate: func(e *HybridEnvelopeV1) *HybridEnvelopeV1 { e.PQSignature = nil; return e }, want: ErrInvalidPQSignatureSize},
		{name: "short signature", mutate: func(e *HybridEnvelopeV1) *HybridEnvelopeV1 {
			e.PQSignature = e.PQSignature[:len(e.PQSignature)-1]
			return e
		}, want: ErrInvalidPQSignatureSize},
		{name: "long signature", mutate: func(e *HybridEnvelopeV1) *HybridEnvelopeV1 { e.PQSignature = append(e.PQSignature, 0); return e }, want: ErrInvalidPQSignatureSize},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			envelope := test.mutate(testHybridEnvelope())
			_, err := EncodeHybridEnvelopeV1(envelope)
			if !errors.Is(err, test.want) {
				t.Fatalf("encode error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestDecodeHybridEnvelopeV1RejectsMalformedInput(t *testing.T) {
	validEnvelope := testHybridEnvelope()
	valid := mustEncodeHybridEnvelope(t, validEnvelope)

	tests := []struct {
		name string
		raw  func() []byte
		want error
	}{
		{name: "empty", raw: func() []byte { return nil }, want: ErrEmptyEnvelope},
		{name: "total size limit", raw: func() []byte { return make([]byte, MaxHybridEnvelopeSize+1) }, want: ErrEnvelopeTooLarge},
		{name: "not a list", raw: func() []byte { return []byte{0x80} }, want: ErrMalformedEnvelope},
		{name: "truncated", raw: func() []byte { return append([]byte(nil), valid[:len(valid)-1]...) }, want: ErrMalformedEnvelope},
		{name: "trailing bytes", raw: func() []byte { return append(append([]byte(nil), valid...), 0x80) }, want: ErrMalformedEnvelope},
		{name: "non-canonical version", raw: func() []byte {
			return rawEnvelopeWithFields(t, rlp.RawValue{0x81, 0x01}, rawRLP(t, uint8(1)), rawRLP(t, validEnvelope.EthereumTx), rawRLP(t, validEnvelope.PQPublicKey), rawRLP(t, validEnvelope.PQSignature))
		}, want: ErrMalformedEnvelope},
		{name: "unsupported version", raw: func() []byte {
			return encodeUncheckedEnvelope(t, 2, AlgorithmMLDSA65, validEnvelope.EthereumTx, validEnvelope.PQPublicKey, validEnvelope.PQSignature)
		}, want: ErrUnsupportedEnvelopeVersion},
		{name: "unsupported algorithm", raw: func() []byte {
			return encodeUncheckedEnvelope(t, EnvelopeVersionV1, 2, validEnvelope.EthereumTx, validEnvelope.PQPublicKey, validEnvelope.PQSignature)
		}, want: ErrUnsupportedAlgorithm},
		{name: "missing fields", raw: func() []byte {
			return rawEnvelopeWithFields(t, rawRLP(t, EnvelopeVersionV1), rawRLP(t, AlgorithmMLDSA65))
		}, want: ErrMalformedEnvelope},
		{name: "extra field", raw: func() []byte {
			return rawEnvelopeWithFields(t, rawRLP(t, EnvelopeVersionV1), rawRLP(t, AlgorithmMLDSA65), rawRLP(t, validEnvelope.EthereumTx), rawRLP(t, validEnvelope.PQPublicKey), rawRLP(t, validEnvelope.PQSignature), rawRLP(t, uint8(0)))
		}, want: ErrMalformedEnvelope},
		{name: "oversized Ethereum transaction", raw: func() []byte {
			return encodeUncheckedEnvelope(t, EnvelopeVersionV1, AlgorithmMLDSA65, make([]byte, MaxEthereumTxSize+1), validEnvelope.PQPublicKey, validEnvelope.PQSignature)
		}, want: ErrInvalidEthereumTxSize},
		{name: "wrong public key size", raw: func() []byte {
			return encodeUncheckedEnvelope(t, EnvelopeVersionV1, AlgorithmMLDSA65, validEnvelope.EthereumTx, validEnvelope.PQPublicKey[:len(validEnvelope.PQPublicKey)-1], validEnvelope.PQSignature)
		}, want: ErrInvalidPQPublicKeySize},
		{name: "wrong signature size", raw: func() []byte {
			return encodeUncheckedEnvelope(t, EnvelopeVersionV1, AlgorithmMLDSA65, validEnvelope.EthereumTx, validEnvelope.PQPublicKey, validEnvelope.PQSignature[:len(validEnvelope.PQSignature)-1])
		}, want: ErrInvalidPQSignatureSize},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeHybridEnvelopeV1(test.raw())
			if !errors.Is(err, test.want) {
				t.Fatalf("decode error = %v, want %v", err, test.want)
			}
		})
	}
}

func FuzzDecodeHybridEnvelopeV1(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0xc0})
	f.Add(mustEncodeHybridEnvelope(f, testHybridEnvelope()))
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = DecodeHybridEnvelopeV1(raw)
	})
}

type fatalHelper interface {
	Helper()
	Fatalf(format string, args ...any)
}

func testHybridEnvelope() *HybridEnvelopeV1 {
	return &HybridEnvelopeV1{
		Version:     EnvelopeVersionV1,
		Algorithm:   AlgorithmMLDSA65,
		EthereumTx:  []byte{0x02, 0xc0, 0x01, 0x7f},
		PQPublicKey: patternBytes(pqc.MLDSA65PublicKeySize, 0x11),
		PQSignature: patternBytes(pqc.MLDSA65SignatureSize, 0x22),
	}
}

func patternBytes(size int, seed byte) []byte {
	result := make([]byte, size)
	for i := range result {
		result[i] = seed + byte(i%251)
	}
	return result
}

func mustEncodeHybridEnvelope(t fatalHelper, envelope *HybridEnvelopeV1) []byte {
	t.Helper()
	encoded, err := EncodeHybridEnvelopeV1(envelope)
	if err != nil {
		t.Fatalf("encode test envelope: %v", err)
	}
	return encoded
}

func encodeUncheckedEnvelope(t *testing.T, version, algorithm uint8, ethereumTx, publicKey, signature []byte) []byte {
	t.Helper()
	return rawEnvelopeWithFields(
		t,
		rawRLP(t, version),
		rawRLP(t, algorithm),
		rawRLP(t, ethereumTx),
		rawRLP(t, publicKey),
		rawRLP(t, signature),
	)
}

func rawEnvelopeWithFields(t *testing.T, fields ...rlp.RawValue) []byte {
	t.Helper()
	encoded, err := rlp.EncodeToBytes(fields)
	if err != nil {
		t.Fatalf("encode raw envelope fields: %v", err)
	}
	return encoded
}

func rawRLP(t *testing.T, value any) rlp.RawValue {
	t.Helper()
	encoded, err := rlp.EncodeToBytes(value)
	if err != nil {
		t.Fatalf("encode raw RLP value: %v", err)
	}
	return encoded
}
