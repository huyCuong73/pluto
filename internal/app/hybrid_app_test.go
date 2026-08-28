package app

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	projectconfig "github.com/huyCuong73/pluto/internal/config"
	"github.com/huyCuong73/pluto/internal/pqc"
	plutotx "github.com/huyCuong73/pluto/internal/tx"
)

type hybridAppFixture struct {
	ecdsaPrivateKey *ecdsa.PrivateKey
	sender          common.Address
	receiver        common.Address
	pqPublicKey     []byte
	pqPrivateKey    []byte
	scheme          pqc.MLDSA65
	raw             []byte
}

func newHybridAppFixture(t *testing.T) hybridAppFixture {
	t.Helper()
	ecdsaPrivateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	sender := crypto.PubkeyToAddress(ecdsaPrivateKey.PublicKey)
	receiver := common.HexToAddress("0xa100000000000000000000000000000000000001")
	scheme := pqc.NewMLDSA65()
	pqPublicKey, pqPrivateKey, err := scheme.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate ML-DSA-65 key: %v", err)
	}
	ethereumRaw := signedAppEthereumTransfer(t, ecdsaPrivateKey, receiver, 0, big.NewInt(1234))
	raw, err := plutotx.CreateSignedHybridEnvelopeV1(
		ethereumRaw,
		pqPublicKey,
		pqPrivateKey,
		big.NewInt(projectconfig.DefaultEVMChainID),
		scheme,
	)
	if err != nil {
		t.Fatalf("create hybrid transaction: %v", err)
	}
	return hybridAppFixture{
		ecdsaPrivateKey: ecdsaPrivateKey,
		sender:          sender,
		receiver:        receiver,
		pqPublicKey:     pqPublicKey,
		pqPrivateKey:    pqPrivateKey,
		scheme:          scheme,
		raw:             raw,
	}
}

func (f hybridAppFixture) validator(t *testing.T) *plutotx.HybridValidator {
	t.Helper()
	ecdsaValidator, err := plutotx.NewECDSAValidator(projectconfig.DefaultEVMChainID)
	if err != nil {
		t.Fatalf("create ECDSA validator: %v", err)
	}
	registry := plutotx.NewStaticPQKeyRegistry(map[common.Address]plutotx.PQKeyHash{
		f.sender: plutotx.HashPQPublicKey(f.pqPublicKey),
	})
	validator, err := plutotx.NewHybridValidator(
		projectconfig.DefaultEVMChainID,
		ecdsaValidator,
		f.scheme,
		registry,
	)
	if err != nil {
		t.Fatalf("create hybrid validator: %v", err)
	}
	return validator
}

func (f hybridAppFixture) invalidSignature(t *testing.T) []byte {
	t.Helper()
	envelope, err := plutotx.DecodeHybridEnvelopeV1(f.raw)
	if err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	envelope.PQSignature[0] ^= 0x01
	raw, err := plutotx.EncodeHybridEnvelopeV1(envelope)
	if err != nil {
		t.Fatalf("encode invalid envelope: %v", err)
	}
	return raw
}

func TestHybridPQCFailureIsRejectedInEveryABCIBoundary(t *testing.T) {
	fixture := newHybridAppFixture(t)
	application, err := NewAppWithComponents(
		t.TempDir(),
		nil,
		projectconfig.DefaultEVMChainID,
		fixture.validator(t),
	)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	defer application.Close()
	invalid := fixture.invalidSignature(t)

	check, err := application.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: invalid})
	if err != nil || check.Code != CodeInvalidPQC {
		t.Fatalf("CheckTx = code %d, error %v, log %q", check.Code, err, check.Log)
	}
	proposal, err := application.ProcessProposal(context.Background(), &abci.ProcessProposalRequest{Txs: [][]byte{invalid}})
	if err != nil || proposal.Status != abci.PROCESS_PROPOSAL_STATUS_REJECT {
		t.Fatalf("ProcessProposal = status %v, error %v", proposal.Status, err)
	}
	finalized, err := application.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{
		Txs:    [][]byte{invalid},
		Height: 1,
		Time:   time.Unix(1, 0),
	})
	if err != nil {
		t.Fatalf("FinalizeBlock: %v", err)
	}
	if len(finalized.TxResults) != 1 || finalized.TxResults[0].Code != CodeInvalidPQC {
		t.Fatalf("FinalizeBlock result = %#v", finalized.TxResults)
	}
}

func TestHybridExecutionIsDeterministicAcrossIndependentApps(t *testing.T) {
	fixture := newHybridAppFixture(t)
	initialBalance := big.NewInt(1_000_000)
	genesisBytes, err := json.Marshal(GenesisState{
		EVMChainID:        projectconfig.DefaultEVMChainID,
		TransactionPolicy: projectconfig.TransactionPolicyHybridMLDSA65,
		Alloc: map[string]GenesisAccount{
			fixture.sender.Hex(): {Balance: initialBalance.String()},
		},
		PQCKeys: map[string]string{
			fixture.sender.Hex(): plutotx.HashPQPublicKey(fixture.pqPublicKey).Hex(),
		},
	})
	if err != nil {
		t.Fatalf("encode genesis: %v", err)
	}

	type appResult struct {
		appHash         []byte
		txCode          uint32
		senderBalance   string
		receiverBalance string
		nonce           string
	}
	results := make([]appResult, 2)
	for index := range results {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		application, err := NewAppWithComponents(
			t.TempDir(),
			logger,
			projectconfig.DefaultEVMChainID,
			fixture.validator(t),
		)
		if err != nil {
			t.Fatalf("create app %d: %v", index, err)
		}

		initResponse, err := application.InitChain(context.Background(), &abci.InitChainRequest{AppStateBytes: genesisBytes})
		if err != nil || len(initResponse.AppHash) == 0 {
			application.Close()
			t.Fatalf("init app %d: hash %x, error %v", index, initResponse.AppHash, err)
		}
		check, err := application.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: fixture.raw})
		if err != nil || check.Code != CodeOK {
			application.Close()
			t.Fatalf("check app %d: code %d, error %v", index, check.Code, err)
		}
		proposal, err := application.ProcessProposal(context.Background(), &abci.ProcessProposalRequest{Txs: [][]byte{fixture.raw}})
		if err != nil || proposal.Status != abci.PROCESS_PROPOSAL_STATUS_ACCEPT {
			application.Close()
			t.Fatalf("proposal app %d: status %v, error %v", index, proposal.Status, err)
		}
		finalized, err := application.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{
			Txs:    [][]byte{fixture.raw},
			Height: 1,
			Time:   time.Unix(123, 0),
		})
		if err != nil {
			application.Close()
			t.Fatalf("finalize app %d: %v", index, err)
		}
		if _, err := application.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
			application.Close()
			t.Fatalf("commit app %d: %v", index, err)
		}
		results[index] = appResult{
			appHash:         bytes.Clone(finalized.AppHash),
			txCode:          finalized.TxResults[0].Code,
			senderBalance:   queryAppValue(t, application, QueryBalance, fixture.sender),
			receiverBalance: queryAppValue(t, application, QueryBalance, fixture.receiver),
			nonce:           queryAppValue(t, application, QueryNonce, fixture.sender),
		}
		if err := application.Close(); err != nil {
			t.Fatalf("close app %d: %v", index, err)
		}
	}

	if !bytes.Equal(results[0].appHash, results[1].appHash) || results[0].txCode != results[1].txCode ||
		results[0].senderBalance != results[1].senderBalance || results[0].receiverBalance != results[1].receiverBalance ||
		results[0].nonce != results[1].nonce {
		t.Fatalf("independent app results diverged: %#v != %#v", results[0], results[1])
	}
	if results[0].txCode != CodeOK || results[0].nonce != "1" {
		t.Fatalf("unexpected deterministic result: %#v", results[0])
	}
}

func signedAppEthereumTransfer(t *testing.T, privateKey *ecdsa.PrivateKey, receiver common.Address, nonce uint64, value *big.Int) []byte {
	t.Helper()
	unsigned := gethtypes.NewTx(&gethtypes.LegacyTx{
		Nonce:    nonce,
		GasPrice: big.NewInt(0),
		Gas:      21_000,
		To:       &receiver,
		Value:    new(big.Int).Set(value),
	})
	signed, err := gethtypes.SignTx(
		unsigned,
		gethtypes.LatestSignerForChainID(big.NewInt(projectconfig.DefaultEVMChainID)),
		privateKey,
	)
	if err != nil {
		t.Fatalf("sign Ethereum transaction: %v", err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal Ethereum transaction: %v", err)
	}
	return raw
}

func queryAppValue(t *testing.T, application *App, path string, address common.Address) string {
	t.Helper()
	response, err := application.Query(context.Background(), &abci.QueryRequest{Path: path, Data: []byte(address.Hex())})
	if err != nil || response.Code != CodeOK {
		t.Fatalf("query %s: code %d, error %v", path, response.Code, err)
	}
	return string(response.Value)
}
