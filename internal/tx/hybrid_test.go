package tx

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/huyCuong73/pluto/internal/pqc"
)

const hybridTestChainID int64 = 700001

type hybridFixture struct {
	sender       common.Address
	ethereumRaw  []byte
	pqPublicKey  []byte
	pqPrivateKey []byte
	envelopeRaw  []byte
	scheme       pqc.MLDSA65
	ecdsa        *ECDSAValidator
}

func newHybridFixture(t testing.TB) hybridFixture {
	t.Helper()
	ecdsaPrivateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	sender := crypto.PubkeyToAddress(ecdsaPrivateKey.PublicKey)
	receiver := common.HexToAddress("0x8000000000000000000000000000000000000008")
	unsigned := gethtypes.NewTx(&gethtypes.LegacyTx{
		Nonce:    0,
		GasPrice: big.NewInt(0),
		Gas:      21_000,
		To:       &receiver,
		Value:    big.NewInt(9),
	})
	signed, err := gethtypes.SignTx(
		unsigned,
		gethtypes.LatestSignerForChainID(big.NewInt(hybridTestChainID)),
		ecdsaPrivateKey,
	)
	if err != nil {
		t.Fatalf("sign Ethereum transaction: %v", err)
	}
	ethereumRaw, err := signed.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal Ethereum transaction: %v", err)
	}

	scheme := pqc.NewMLDSA65()
	pqPublicKey, pqPrivateKey, err := scheme.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate ML-DSA-65 key: %v", err)
	}
	envelopeRaw, err := CreateSignedHybridEnvelopeV1(
		ethereumRaw,
		pqPublicKey,
		pqPrivateKey,
		big.NewInt(hybridTestChainID),
		scheme,
	)
	if err != nil {
		t.Fatalf("create hybrid envelope: %v", err)
	}
	ecdsa, err := NewECDSAValidator(hybridTestChainID)
	if err != nil {
		t.Fatalf("create ECDSA validator: %v", err)
	}
	return hybridFixture{
		sender:       sender,
		ethereumRaw:  ethereumRaw,
		pqPublicKey:  pqPublicKey,
		pqPrivateKey: pqPrivateKey,
		envelopeRaw:  envelopeRaw,
		scheme:       scheme,
		ecdsa:        ecdsa,
	}
}

func (f hybridFixture) validator(t testing.TB, hash PQKeyHash, register bool) *HybridValidator {
	t.Helper()
	bindings := map[common.Address]PQKeyHash{}
	if register {
		bindings[f.sender] = hash
	}
	validator, err := NewHybridValidator(
		hybridTestChainID,
		f.ecdsa,
		f.scheme,
		NewStaticPQKeyRegistry(bindings),
	)
	if err != nil {
		t.Fatalf("create hybrid validator: %v", err)
	}
	return validator
}

func TestHybridTransactionSizeBaseline(t *testing.T) {
	fixture := newHybridFixture(t)
	if len(fixture.envelopeRaw) <= len(fixture.ethereumRaw) {
		t.Fatalf("hybrid envelope size %d must exceed Ethereum transaction size %d", len(fixture.envelopeRaw), len(fixture.ethereumRaw))
	}
	if len(fixture.envelopeRaw) > MaxHybridEnvelopeSize {
		t.Fatalf("hybrid envelope size %d exceeds maximum %d", len(fixture.envelopeRaw), MaxHybridEnvelopeSize)
	}
	t.Logf(
		"Ethereum transaction=%d bytes, hybrid envelope=%d bytes, overhead=%d bytes, ratio=%.2fx",
		len(fixture.ethereumRaw),
		len(fixture.envelopeRaw),
		len(fixture.envelopeRaw)-len(fixture.ethereumRaw),
		float64(len(fixture.envelopeRaw))/float64(len(fixture.ethereumRaw)),
	)
}

func BenchmarkECDSAValidator(b *testing.B) {
	fixture := newHybridFixture(b)
	b.ReportAllocs()
	b.ReportMetric(float64(len(fixture.ethereumRaw)), "bytes/tx")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := fixture.ecdsa.Validate(fixture.ethereumRaw); err != nil {
			b.Fatalf("validate ECDSA: %v", err)
		}
	}
}

func BenchmarkHybridValidator(b *testing.B) {
	fixture := newHybridFixture(b)
	validator := fixture.validator(b, HashPQPublicKey(fixture.pqPublicKey), true)
	b.ReportAllocs()
	b.ReportMetric(float64(len(fixture.envelopeRaw)), "bytes/tx")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := validator.Validate(fixture.envelopeRaw); err != nil {
			b.Fatalf("validate hybrid: %v", err)
		}
	}
}

func TestHybridValidatorRequiresECDSAKeyBindingAndMLDSA65(t *testing.T) {
	fixture := newHybridFixture(t)
	registeredHash := HashPQPublicKey(fixture.pqPublicKey)
	validator := fixture.validator(t, registeredHash, true)

	validated, err := validator.Validate(fixture.envelopeRaw)
	if err != nil {
		t.Fatalf("validate hybrid transaction: %v", err)
	}
	if validated.Sender != fixture.sender || validated.Message == nil || validated.Message.From != fixture.sender {
		t.Fatalf("validated sender/message mismatch")
	}

	t.Run("malformed envelope", func(t *testing.T) {
		_, err := validator.Validate([]byte("not RLP"))
		if FailureOf(err) != FailureEncoding {
			t.Fatalf("failure = %v (%v), want encoding", FailureOf(err), err)
		}
	})

	t.Run("unregistered sender", func(t *testing.T) {
		_, err := fixture.validator(t, PQKeyHash{}, false).Validate(fixture.envelopeRaw)
		if FailureOf(err) != FailurePQC || !errors.Is(err, ErrPQKeyNotRegistered) {
			t.Fatalf("error = %v, want unregistered PQC failure", err)
		}
	})

	t.Run("wrong binding", func(t *testing.T) {
		wrongHash := registeredHash
		wrongHash[0] ^= 0xff
		_, err := fixture.validator(t, wrongHash, true).Validate(fixture.envelopeRaw)
		if FailureOf(err) != FailurePQC || !errors.Is(err, ErrPQKeyBindingMismatch) {
			t.Fatalf("error = %v, want binding PQC failure", err)
		}
	})

	t.Run("modified ML-DSA signature", func(t *testing.T) {
		envelope, err := DecodeHybridEnvelopeV1(fixture.envelopeRaw)
		if err != nil {
			t.Fatalf("decode fixture: %v", err)
		}
		envelope.PQSignature[0] ^= 0x01
		modified, err := EncodeHybridEnvelopeV1(envelope)
		if err != nil {
			t.Fatalf("encode modified signature: %v", err)
		}
		_, err = validator.Validate(modified)
		if FailureOf(err) != FailurePQC || !errors.Is(err, pqc.ErrVerificationFailed) {
			t.Fatalf("error = %v, want ML-DSA verification failure", err)
		}
	})

	t.Run("wrong EVM chain ID", func(t *testing.T) {
		privateKey, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		unsigned := gethtypes.NewTx(&gethtypes.LegacyTx{Nonce: 0, Gas: 21_000, GasPrice: big.NewInt(0)})
		signed, err := gethtypes.SignTx(unsigned, gethtypes.LatestSignerForChainID(big.NewInt(1)), privateKey)
		if err != nil {
			t.Fatalf("sign wrong-chain transaction: %v", err)
		}
		raw, err := signed.MarshalBinary()
		if err != nil {
			t.Fatalf("marshal wrong-chain transaction: %v", err)
		}
		envelopeRaw, err := CreateSignedHybridEnvelopeV1(raw, fixture.pqPublicKey, fixture.pqPrivateKey, big.NewInt(hybridTestChainID), fixture.scheme)
		if err != nil {
			t.Fatalf("create wrong-chain envelope: %v", err)
		}
		_, err = validator.Validate(envelopeRaw)
		if FailureOf(err) != FailureAuthentication {
			t.Fatalf("failure = %v (%v), want authentication", FailureOf(err), err)
		}
	})
}

func TestNewHybridValidatorRejectsMissingDependencies(t *testing.T) {
	fixture := newHybridFixture(t)
	registry := NewStaticPQKeyRegistry(nil)
	tests := []struct {
		name     string
		chainID  int64
		ethereum TransactionValidator
		scheme   pqc.Scheme
		resolver PQKeyResolver
		want     error
	}{
		{name: "chain ID", chainID: 0, ethereum: fixture.ecdsa, scheme: fixture.scheme, resolver: registry, want: ErrInvalidChainID},
		{name: "Ethereum validator", chainID: hybridTestChainID, scheme: fixture.scheme, resolver: registry, want: ErrNilEthereumValidator},
		{name: "PQC scheme", chainID: hybridTestChainID, ethereum: fixture.ecdsa, resolver: registry, want: ErrNilPQScheme},
		{name: "key resolver", chainID: hybridTestChainID, ethereum: fixture.ecdsa, scheme: fixture.scheme, want: ErrNilPQKeyResolver},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewHybridValidator(test.chainID, test.ethereum, test.scheme, test.resolver)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestCreateSignedHybridEnvelopeV1DoesNotAliasInputs(t *testing.T) {
	fixture := newHybridFixture(t)
	encoded, err := CreateSignedHybridEnvelopeV1(
		fixture.ethereumRaw,
		fixture.pqPublicKey,
		fixture.pqPrivateKey,
		big.NewInt(hybridTestChainID),
		fixture.scheme,
	)
	if err != nil {
		t.Fatalf("create signed envelope: %v", err)
	}
	fixture.ethereumRaw[0] ^= 0xff
	fixture.pqPublicKey[0] ^= 0xff
	decoded, err := DecodeHybridEnvelopeV1(encoded)
	if err != nil {
		t.Fatalf("decode envelope after source mutation: %v", err)
	}
	if decoded.EthereumTx[0] == fixture.ethereumRaw[0] || decoded.PQPublicKey[0] == fixture.pqPublicKey[0] {
		t.Fatal("encoded envelope changed after source mutation")
	}
}
