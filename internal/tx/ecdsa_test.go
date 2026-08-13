package tx

import (
	"math/big"
	"testing"

	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestECDSAValidator(t *testing.T) {
	const chainID int64 = 700001
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	unsigned := gethtypes.NewTx(&gethtypes.LegacyTx{
		Nonce:    0,
		GasPrice: big.NewInt(0),
		Gas:      21_000,
		Value:    big.NewInt(1),
	})
	signed, err := gethtypes.SignTx(unsigned, gethtypes.LatestSignerForChainID(big.NewInt(chainID)), privateKey)
	if err != nil {
		t.Fatalf("sign transaction: %v", err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal transaction: %v", err)
	}

	validator, err := NewECDSAValidator(chainID)
	if err != nil {
		t.Fatalf("create validator: %v", err)
	}
	validated, err := validator.Validate(raw)
	if err != nil {
		t.Fatalf("validate signed transaction: %v", err)
	}
	wantSender := crypto.PubkeyToAddress(privateKey.PublicKey)
	if validated.Sender != wantSender {
		t.Fatalf("sender = %s, want %s", validated.Sender.Hex(), wantSender.Hex())
	}
	if validated.Message == nil || validated.Message.From != wantSender {
		t.Fatalf("normalized EVM message has unexpected sender")
	}
}

func TestECDSAValidatorClassifiesFailures(t *testing.T) {
	validator, err := NewECDSAValidator(700001)
	if err != nil {
		t.Fatalf("create validator: %v", err)
	}

	if _, err := validator.Validate([]byte("not a transaction")); FailureOf(err) != FailureEncoding {
		t.Fatalf("malformed transaction failure = %v, want encoding", FailureOf(err))
	}

	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	wrongChainTx, err := gethtypes.SignTx(
		gethtypes.NewTx(&gethtypes.LegacyTx{Gas: 21_000, GasPrice: big.NewInt(0)}),
		gethtypes.LatestSignerForChainID(big.NewInt(700002)),
		privateKey,
	)
	if err != nil {
		t.Fatalf("sign wrong-chain transaction: %v", err)
	}
	raw, err := wrongChainTx.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal wrong-chain transaction: %v", err)
	}
	if _, err := validator.Validate(raw); FailureOf(err) != FailureAuthentication {
		t.Fatalf("wrong-chain transaction failure = %v, want authentication", FailureOf(err))
	}

	unprotectedTx, err := gethtypes.SignTx(
		gethtypes.NewTx(&gethtypes.LegacyTx{Gas: 21_000, GasPrice: big.NewInt(0)}),
		gethtypes.HomesteadSigner{},
		privateKey,
	)
	if err != nil {
		t.Fatalf("sign unprotected transaction: %v", err)
	}
	raw, err = unprotectedTx.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal unprotected transaction: %v", err)
	}
	if _, err := validator.Validate(raw); FailureOf(err) != FailureAuthentication {
		t.Fatalf("unprotected transaction failure = %v, want authentication", FailureOf(err))
	}
}
