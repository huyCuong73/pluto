package tx

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math/big"
	"testing"

	"github.com/huyCuong73/pluto/internal/pqc"
)

func TestBuildHybridSignBytesV1Golden(t *testing.T) {
	envelope := testHybridEnvelope()
	signBytes, err := BuildHybridSignBytesV1(envelope, big.NewInt(700001))
	if err != nil {
		t.Fatalf("build sign bytes: %v", err)
	}

	const wantHex = "f83f92504c55544f5f4859425249445f54585f56310101830aae618402c0017fa0d692326a64f31f5c1e41c46d1a742696e034d58e17ee076a4a7f3d37c9691240"
	if got := hex.EncodeToString(signBytes); got != wantHex {
		t.Fatalf("golden sign bytes changed\ngot:  %s\nwant: %s", got, wantHex)
	}

	second, err := BuildHybridSignBytesV1(envelope, big.NewInt(700001))
	if err != nil {
		t.Fatalf("build sign bytes again: %v", err)
	}
	if !bytes.Equal(signBytes, second) {
		t.Fatal("identical inputs produced different sign bytes")
	}
}

func TestHybridSignBytesBindTransactionChainAndPublicKey(t *testing.T) {
	scheme := pqc.NewMLDSA65()
	publicKey, privateKey, err := scheme.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{0x5a}, 64)))
	if err != nil {
		t.Fatalf("generate ML-DSA key: %v", err)
	}
	envelope := testHybridEnvelope()
	envelope.PQPublicKey = publicKey
	envelope.PQSignature = nil
	chainID := big.NewInt(700001)

	originalSignBytes, err := BuildHybridSignBytesV1(envelope, chainID)
	if err != nil {
		t.Fatalf("build original sign bytes: %v", err)
	}
	signature, err := scheme.Sign(privateKey, originalSignBytes, []byte(MLDSA65TransactionContext))
	if err != nil {
		t.Fatalf("sign canonical bytes: %v", err)
	}
	if err := scheme.Verify(publicKey, originalSignBytes, []byte(MLDSA65TransactionContext), signature); err != nil {
		t.Fatalf("verify original sign bytes: %v", err)
	}

	changedTransaction := cloneEnvelope(envelope)
	changedTransaction.EthereumTx[0] ^= 0x01
	changedPublicKey := cloneEnvelope(envelope)
	changedPublicKey.PQPublicKey[0] ^= 0x01

	tests := []struct {
		name     string
		envelope *HybridEnvelopeV1
		chainID  *big.Int
	}{
		{name: "Ethereum transaction changed", envelope: changedTransaction, chainID: chainID},
		{name: "chain ID changed", envelope: envelope, chainID: big.NewInt(700002)},
		{name: "public key changed", envelope: changedPublicKey, chainID: chainID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changedSignBytes, err := BuildHybridSignBytesV1(test.envelope, test.chainID)
			if err != nil {
				t.Fatalf("build changed sign bytes: %v", err)
			}
			if bytes.Equal(originalSignBytes, changedSignBytes) {
				t.Fatal("changed input did not change canonical sign bytes")
			}
			if err := scheme.Verify(publicKey, changedSignBytes, []byte(MLDSA65TransactionContext), signature); !errors.Is(err, pqc.ErrVerificationFailed) {
				t.Fatalf("verify changed sign bytes error = %v, want verification failure", err)
			}
		})
	}
}

func TestHybridSignBytesExcludeSignature(t *testing.T) {
	envelope := testHybridEnvelope()
	first, err := BuildHybridSignBytesV1(envelope, big.NewInt(700001))
	if err != nil {
		t.Fatalf("build first sign bytes: %v", err)
	}
	envelope.PQSignature[0] ^= 0xff
	second, err := BuildHybridSignBytesV1(envelope, big.NewInt(700001))
	if err != nil {
		t.Fatalf("build second sign bytes: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("PQ signature must not be included in its own sign bytes")
	}
}

func TestBuildHybridSignBytesV1RejectsInvalidInput(t *testing.T) {
	partialSignatureEnvelope := testHybridEnvelope()
	partialSignatureEnvelope.PQSignature = []byte{1}
	tests := []struct {
		name     string
		envelope *HybridEnvelopeV1
		chainID  *big.Int
		want     error
	}{
		{name: "nil envelope", envelope: nil, chainID: big.NewInt(1), want: ErrNilEnvelope},
		{name: "nil chain ID", envelope: testHybridEnvelope(), chainID: nil, want: ErrInvalidChainID},
		{name: "zero chain ID", envelope: testHybridEnvelope(), chainID: new(big.Int), want: ErrInvalidChainID},
		{name: "negative chain ID", envelope: testHybridEnvelope(), chainID: big.NewInt(-1), want: ErrInvalidChainID},
		{name: "partial signature", envelope: partialSignatureEnvelope, chainID: big.NewInt(1), want: ErrInvalidPQSignatureSize},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := BuildHybridSignBytesV1(test.envelope, test.chainID)
			if !errors.Is(err, test.want) {
				t.Fatalf("build error = %v, want %v", err, test.want)
			}
		})
	}
}

func cloneEnvelope(source *HybridEnvelopeV1) *HybridEnvelopeV1 {
	return &HybridEnvelopeV1{
		Version:     source.Version,
		Algorithm:   source.Algorithm,
		EthereumTx:  bytes.Clone(source.EthereumTx),
		PQPublicKey: bytes.Clone(source.PQPublicKey),
		PQSignature: bytes.Clone(source.PQSignature),
	}
}
