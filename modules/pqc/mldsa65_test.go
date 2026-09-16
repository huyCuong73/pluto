package pqc

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

var testContext = []byte("PLUTO-TX-V1")

func TestMLDSA65ParameterSizes(t *testing.T) {
	const (
		fips204PublicKeySize  = 1952
		fips204PrivateKeySize = 4032
		fips204SignatureSize  = 3309
	)
	if MLDSA65PublicKeySize != fips204PublicKeySize {
		t.Fatalf("ML-DSA-65 public key size = %d, want %d", MLDSA65PublicKeySize, fips204PublicKeySize)
	}
	if MLDSA65PrivateKeySize != fips204PrivateKeySize {
		t.Fatalf("ML-DSA-65 private key size = %d, want %d", MLDSA65PrivateKeySize, fips204PrivateKeySize)
	}
	if MLDSA65SignatureSize != fips204SignatureSize {
		t.Fatalf("ML-DSA-65 signature size = %d, want %d", MLDSA65SignatureSize, fips204SignatureSize)
	}
}

func TestMLDSA65GenerateSignVerify(t *testing.T) {
	scheme := NewMLDSA65()
	publicKey, privateKey := generateTestKeyPair(t, scheme, 1)
	message := []byte("canonical Pluto transaction bytes")

	signature, err := scheme.Sign(privateKey, message, testContext)
	if err != nil {
		t.Fatalf("sign message: %v", err)
	}
	if len(publicKey) != MLDSA65PublicKeySize {
		t.Fatalf("public key size = %d, want %d", len(publicKey), MLDSA65PublicKeySize)
	}
	if len(privateKey) != MLDSA65PrivateKeySize {
		t.Fatalf("private key size = %d, want %d", len(privateKey), MLDSA65PrivateKeySize)
	}
	if len(signature) != MLDSA65SignatureSize {
		t.Fatalf("signature size = %d, want %d", len(signature), MLDSA65SignatureSize)
	}
	if err := scheme.Verify(publicKey, message, testContext, signature); err != nil {
		t.Fatalf("verify valid signature: %v", err)
	}

	// The adapter deliberately selects deterministic FIPS 204 signing.
	secondSignature, err := scheme.Sign(privateKey, message, testContext)
	if err != nil {
		t.Fatalf("sign message again: %v", err)
	}
	if !bytes.Equal(signature, secondSignature) {
		t.Fatal("deterministic signatures differ for identical inputs")
	}
}

func TestMLDSA65RejectsModifiedInputs(t *testing.T) {
	scheme := NewMLDSA65()
	publicKey, privateKey := generateTestKeyPair(t, scheme, 2)
	otherPublicKey, _ := generateTestKeyPair(t, scheme, 3)
	message := []byte("transaction payload")
	signature, err := scheme.Sign(privateKey, message, testContext)
	if err != nil {
		t.Fatalf("sign message: %v", err)
	}

	modifiedMessage := append([]byte(nil), message...)
	modifiedMessage[0] ^= 0x01
	modifiedSignature := append([]byte(nil), signature...)
	modifiedSignature[len(modifiedSignature)/2] ^= 0x01
	zeroPublicKey := make([]byte, MLDSA65PublicKeySize)
	zeroSignature := make([]byte, MLDSA65SignatureSize)

	tests := []struct {
		name      string
		key       []byte
		message   []byte
		context   []byte
		signature []byte
	}{
		{name: "message changed", key: publicKey, message: modifiedMessage, context: testContext, signature: signature},
		{name: "signature changed", key: publicKey, message: message, context: testContext, signature: modifiedSignature},
		{name: "different public key", key: otherPublicKey, message: message, context: testContext, signature: signature},
		{name: "different context", key: publicKey, message: message, context: []byte("PLUTO-TX-V2"), signature: signature},
		{name: "all-zero public key", key: zeroPublicKey, message: message, context: testContext, signature: signature},
		{name: "all-zero signature", key: publicKey, message: message, context: testContext, signature: zeroSignature},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := scheme.Verify(test.key, test.message, test.context, test.signature)
			if !errors.Is(err, ErrVerificationFailed) {
				t.Fatalf("verify error = %v, want ErrVerificationFailed", err)
			}
		})
	}
}

func TestMLDSA65RejectsInvalidSizesWithoutPanic(t *testing.T) {
	scheme := NewMLDSA65()
	publicKey, privateKey := generateTestKeyPair(t, scheme, 4)
	message := []byte("message")
	signature, err := scheme.Sign(privateKey, message, testContext)
	if err != nil {
		t.Fatalf("sign message: %v", err)
	}

	signCases := []struct {
		name       string
		privateKey []byte
	}{
		{name: "empty private key", privateKey: nil},
		{name: "short private key", privateKey: privateKey[:len(privateKey)-1]},
		{name: "long private key", privateKey: append(append([]byte(nil), privateKey...), 0)},
	}
	for _, test := range signCases {
		t.Run("sign "+test.name, func(t *testing.T) {
			_, err := scheme.Sign(test.privateKey, message, testContext)
			if !errors.Is(err, ErrInvalidPrivateKeySize) {
				t.Fatalf("sign error = %v, want ErrInvalidPrivateKeySize", err)
			}
		})
	}

	verifyCases := []struct {
		name      string
		publicKey []byte
		signature []byte
		want      error
	}{
		{name: "empty public key", publicKey: nil, signature: signature, want: ErrInvalidPublicKeySize},
		{name: "short public key", publicKey: publicKey[:len(publicKey)-1], signature: signature, want: ErrInvalidPublicKeySize},
		{name: "long public key", publicKey: append(append([]byte(nil), publicKey...), 0), signature: signature, want: ErrInvalidPublicKeySize},
		{name: "empty signature", publicKey: publicKey, signature: nil, want: ErrInvalidSignatureSize},
		{name: "short signature", publicKey: publicKey, signature: signature[:len(signature)-1], want: ErrInvalidSignatureSize},
		{name: "long signature", publicKey: publicKey, signature: append(append([]byte(nil), signature...), 0), want: ErrInvalidSignatureSize},
	}
	for _, test := range verifyCases {
		t.Run("verify "+test.name, func(t *testing.T) {
			err := scheme.Verify(test.publicKey, message, testContext, test.signature)
			if !errors.Is(err, test.want) {
				t.Fatalf("verify error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestMLDSA65ContextBoundary(t *testing.T) {
	scheme := NewMLDSA65()
	publicKey, privateKey := generateTestKeyPair(t, scheme, 5)
	message := []byte("message")
	maximumContext := bytes.Repeat([]byte{0x42}, MLDSA65MaxContextSize)

	signature, err := scheme.Sign(privateKey, message, maximumContext)
	if err != nil {
		t.Fatalf("sign with maximum context: %v", err)
	}
	if err := scheme.Verify(publicKey, message, maximumContext, signature); err != nil {
		t.Fatalf("verify with maximum context: %v", err)
	}

	overlongContext := append(maximumContext, 0x42)
	if _, err := scheme.Sign(privateKey, message, overlongContext); !errors.Is(err, ErrContextTooLong) {
		t.Fatalf("sign overlong context error = %v, want ErrContextTooLong", err)
	}
	if err := scheme.Verify(publicKey, message, overlongContext, signature); !errors.Is(err, ErrContextTooLong) {
		t.Fatalf("verify overlong context error = %v, want ErrContextTooLong", err)
	}
}

func TestMLDSA65PropagatesEntropyFailure(t *testing.T) {
	scheme := NewMLDSA65()
	_, _, err := scheme.GenerateKey(failingReader{})
	if err == nil {
		t.Fatal("GenerateKey returned nil error for failed entropy source")
	}
}

func generateTestKeyPair(t *testing.T, scheme MLDSA65, fill byte) ([]byte, []byte) {
	t.Helper()
	entropy := bytes.NewReader(bytes.Repeat([]byte{fill}, 64))
	publicKey, privateKey, err := scheme.GenerateKey(entropy)
	if err != nil {
		t.Fatalf("generate key pair: %v", err)
	}
	return publicKey, privateKey
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, fmt.Errorf("entropy unavailable")
}

func BenchmarkMLDSA65Sign(b *testing.B) {
	scheme := NewMLDSA65()
	publicKey, privateKey, err := scheme.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{0x5a}, 64)))
	if err != nil {
		b.Fatalf("generate key pair: %v", err)
	}
	_ = publicKey
	message := bytes.Repeat([]byte{0x42}, 256)
	b.ReportAllocs()
	b.SetBytes(int64(len(message)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := scheme.Sign(privateKey, message, testContext); err != nil {
			b.Fatalf("sign: %v", err)
		}
	}
}

func BenchmarkMLDSA65Verify(b *testing.B) {
	scheme := NewMLDSA65()
	publicKey, privateKey, err := scheme.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{0x6b}, 64)))
	if err != nil {
		b.Fatalf("generate key pair: %v", err)
	}
	message := bytes.Repeat([]byte{0x42}, 256)
	signature, err := scheme.Sign(privateKey, message, testContext)
	if err != nil {
		b.Fatalf("sign fixture: %v", err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(message)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := scheme.Verify(publicKey, message, testContext, signature); err != nil {
			b.Fatalf("verify: %v", err)
		}
	}
}
