package app

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	projectconfig "github.com/huyCuong73/pluto/internal/node/config"
	"github.com/huyCuong73/pluto/modules/evm"
)

func newTestApp(t *testing.T) *App {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application, err := NewAppWithChainID(t.TempDir(), logger, projectconfig.DefaultEVMChainID)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	t.Cleanup(func() {
		if err := application.Close(); err != nil {
			t.Errorf("close app: %v", err)
		}
	})
	return application
}

func TestFinalizeBlockHandlesTwoConsecutiveEVMFailures(t *testing.T) {
	application := newTestApp(t)
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	sender := crypto.PubkeyToAddress(privateKey.PublicKey)
	contract := common.HexToAddress("0x3000000000000000000000000000000000000003")

	stateDB := evm.NewPebbleStateDB(application.db)
	stateDB.AddBalanceBig(sender, new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil))
	stateDB.SetCode(contract, []byte{0xfe}, tracing.CodeChangeUnspecified) // INVALID opcode
	if err := stateDB.Commit(); err != nil {
		t.Fatalf("seed EVM state: %v", err)
	}

	signer := types.LatestSignerForChainID(big.NewInt(projectconfig.DefaultEVMChainID))
	rawTransactions := make([][]byte, 2)
	for nonce := uint64(0); nonce < 2; nonce++ {
		unsigned := types.NewTx(&types.LegacyTx{
			Nonce:    nonce,
			GasPrice: big.NewInt(0),
			Gas:      100_000,
			To:       &contract,
			Value:    big.NewInt(0),
		})
		signed, signErr := types.SignTx(unsigned, signer, privateKey)
		if signErr != nil {
			t.Fatalf("sign transaction %d: %v", nonce, signErr)
		}
		rawTransactions[nonce], err = signed.MarshalBinary()
		if err != nil {
			t.Fatalf("marshal transaction %d: %v", nonce, err)
		}
	}

	response, err := application.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{
		Txs:    rawTransactions,
		Height: 1,
		Time:   time.Unix(1, 0),
	})
	if err != nil {
		t.Fatalf("finalize block: %v", err)
	}
	if len(response.TxResults) != 2 {
		t.Fatalf("got %d tx results, want 2", len(response.TxResults))
	}
	for i, result := range response.TxResults {
		if result.Code != CodeExecutionFailed {
			t.Errorf("transaction %d code = %d, want %d", i, result.Code, CodeExecutionFailed)
		}
	}

	committedState := evm.NewPebbleStateDB(application.db)
	if got := committedState.GetNonce(sender); got != 2 {
		t.Fatalf("sender nonce after two failed transactions = %d, want 2", got)
	}
}

func TestIntrinsicGasIsEnforced(t *testing.T) {
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	sender := crypto.PubkeyToAddress(privateKey.PublicKey)
	receiver := common.HexToAddress("0x1000000000000000000000000000000000000001")

	tests := []struct {
		name     string
		gas      uint64
		wantCode uint32
		wantGas  int64
	}{
		{name: "below intrinsic", gas: 20_999, wantCode: CodeExecutionFailed},
		{name: "exact intrinsic", gas: 21_000, wantCode: CodeOK, wantGas: 21_000},
		{name: "above intrinsic", gas: 30_000, wantCode: CodeOK, wantGas: 21_000},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			application := newTestApp(t)
			seedBalance(t, application, sender, new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil))
			raw := signedLegacyTransaction(t, privateKey, 0, test.gas, &receiver, big.NewInt(1), nil)
			response := finalizeTransactions(t, application, 1, raw)
			result := response.TxResults[0]
			if result.Code != test.wantCode || result.GasUsed != test.wantGas {
				t.Fatalf("result = code:%d gas:%d log:%q, want code:%d gas:%d", result.Code, result.GasUsed, result.Log, test.wantCode, test.wantGas)
			}
		})
	}
}

func TestCheckTxReportsGasWanted(t *testing.T) {
	application := newTestApp(t)
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	receiver := common.HexToAddress("0x1100000000000000000000000000000000000011")
	raw := signedLegacyTransaction(t, privateKey, 0, 45_678, &receiver, big.NewInt(0), nil)
	response, err := application.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: raw})
	if err != nil {
		t.Fatalf("check transaction: %v", err)
	}
	if response.Code != CodeOK || response.GasWanted != 45_678 {
		t.Fatalf("CheckTx = code:%d gasWanted:%d log:%q", response.Code, response.GasWanted, response.Log)
	}
}

func TestContractCreationUsesPreTransactionNonce(t *testing.T) {
	application := newTestApp(t)
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	sender := crypto.PubkeyToAddress(privateKey.PublicKey)
	seedBalance(t, application, sender, new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil))

	// This init code returns one STOP byte as deployed runtime code.
	initCode := common.FromHex("0x6001600c60003960016000f300")
	raw := signedLegacyTransaction(t, privateKey, 0, 100_000, nil, big.NewInt(0), initCode)
	response := finalizeTransactions(t, application, 1, raw)
	if response.TxResults[0].Code != CodeOK {
		t.Fatalf("contract creation failed: %q", response.TxResults[0].Log)
	}
	if _, err := application.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatalf("commit block: %v", err)
	}

	expected := crypto.CreateAddress(sender, 0)
	stateDB := evm.NewPebbleStateDB(application.db)
	if got := stateDB.GetNonce(sender); got != 1 {
		t.Fatalf("sender nonce = %d, want 1", got)
	}
	if got := stateDB.GetCode(expected); !bytes.Equal(got, []byte{0}) {
		t.Fatalf("code at expected address %s = %x, want 00", expected, got)
	}
}

func TestMultipleTransactionsFromSameSenderInOneBlock(t *testing.T) {
	application := newTestApp(t)
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	sender := crypto.PubkeyToAddress(privateKey.PublicKey)
	receiver := common.HexToAddress("0x1200000000000000000000000000000000000012")
	seedBalance(t, application, sender, big.NewInt(100))

	response := finalizeTransactions(t, application, 1,
		signedLegacyTransaction(t, privateKey, 0, 21_000, &receiver, big.NewInt(10), nil),
		signedLegacyTransaction(t, privateKey, 1, 21_000, &receiver, big.NewInt(15), nil),
	)
	for index, result := range response.TxResults {
		if result.Code != CodeOK {
			t.Fatalf("transaction %d failed: %q", index, result.Log)
		}
	}
	if _, err := application.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatalf("commit block: %v", err)
	}
	stateDB := evm.NewPebbleStateDB(application.db)
	if got := stateDB.GetNonce(sender); got != 2 {
		t.Fatalf("sender nonce = %d, want 2", got)
	}
	if got := stateDB.GetBalance(receiver).ToBig(); got.Cmp(big.NewInt(25)) != 0 {
		t.Fatalf("receiver balance = %s, want 25", got)
	}
}

func TestBlockGasLimitIsSharedAcrossTransactions(t *testing.T) {
	application := newTestApp(t)
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	sender := crypto.PubkeyToAddress(privateKey.PublicKey)
	contract := common.HexToAddress("0x1300000000000000000000000000000000000013")
	receiver := common.HexToAddress("0x1400000000000000000000000000000000000014")
	stateDB := evm.NewPebbleStateDB(application.db)
	stateDB.AddBalanceBig(sender, big.NewInt(100))
	stateDB.SetCode(contract, []byte{0xfe}, tracing.CodeChangeUnspecified)
	if err := stateDB.Commit(); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	response := finalizeTransactions(t, application, 1,
		signedLegacyTransaction(t, privateKey, 0, 9_990_000, &contract, big.NewInt(0), nil),
		signedLegacyTransaction(t, privateKey, 1, 21_000, &receiver, big.NewInt(1), nil),
	)
	if response.TxResults[0].Code != CodeExecutionFailed {
		t.Fatalf("first transaction code = %d, want execution failure", response.TxResults[0].Code)
	}
	if response.TxResults[1].Code != CodeExecutionFailed {
		t.Fatalf("second transaction code = %d, want block gas rejection", response.TxResults[1].Code)
	}
	if _, err := application.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatalf("commit block: %v", err)
	}
	stateDB = evm.NewPebbleStateDB(application.db)
	if got := stateDB.GetNonce(sender); got != 1 {
		t.Fatalf("sender nonce = %d, want 1", got)
	}
	if got := stateDB.GetBalance(receiver).ToBig(); got.Sign() != 0 {
		t.Fatalf("receiver balance = %s, want 0", got)
	}
}

func TestFinalizeBlockLeavesStatePendingUntilCommit(t *testing.T) {
	application := newTestApp(t)
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	sender := crypto.PubkeyToAddress(privateKey.PublicKey)
	receiver := common.HexToAddress("0x1500000000000000000000000000000000000015")
	seedBalance(t, application, sender, big.NewInt(10))
	raw := signedLegacyTransaction(t, privateKey, 0, 21_000, &receiver, big.NewInt(3), nil)

	response := finalizeTransactions(t, application, 1, raw)
	if response.TxResults[0].Code != CodeOK || len(response.AppHash) == 0 {
		t.Fatalf("finalize result = %#v", response)
	}
	beforeCommit := evm.NewPebbleStateDB(application.db)
	if got := beforeCommit.GetNonce(sender); got != 0 {
		t.Fatalf("durable nonce before Commit = %d, want 0", got)
	}
	info, err := application.Info(context.Background(), &abci.InfoRequest{})
	if err != nil {
		t.Fatalf("info before commit: %v", err)
	}
	if info.LastBlockHeight != 0 || len(info.LastBlockAppHash) != 0 {
		t.Fatalf("committed metadata before Commit = height:%d hash:%x", info.LastBlockHeight, info.LastBlockAppHash)
	}
	if _, err := application.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatalf("commit block: %v", err)
	}
	afterCommit := evm.NewPebbleStateDB(application.db)
	if got := afterCommit.GetNonce(sender); got != 1 {
		t.Fatalf("durable nonce after Commit = %d, want 1", got)
	}
	info, err = application.Info(context.Background(), &abci.InfoRequest{})
	if err != nil {
		t.Fatalf("info after commit: %v", err)
	}
	if info.LastBlockHeight != 1 || !bytes.Equal(info.LastBlockAppHash, response.AppHash) {
		t.Fatalf("committed metadata after Commit = height:%d hash:%x, want height:1 hash:%x", info.LastBlockHeight, info.LastBlockAppHash, response.AppHash)
	}
}

func TestQueryBalanceAndNonce(t *testing.T) {
	application := newTestApp(t)
	address := common.HexToAddress("0x4000000000000000000000000000000000000004")
	stateDB := evm.NewPebbleStateDB(application.db)
	stateDB.AddBalanceBig(address, big.NewInt(12345))
	stateDB.SetNonce(address, 7, tracing.NonceChangeUnspecified)
	if err := stateDB.Commit(); err != nil {
		t.Fatalf("seed query state: %v", err)
	}
	application.currentHeight = 9

	tests := []struct {
		path string
		want string
	}{
		{path: QueryBalance, want: "12345"},
		{path: QueryNonce, want: "7"},
	}
	for _, test := range tests {
		response, err := application.Query(context.Background(), &abci.QueryRequest{
			Path: test.path,
			Data: []byte(address.Hex()),
		})
		if err != nil {
			t.Fatalf("query %s: %v", test.path, err)
		}
		if response.Code != CodeOK || string(response.Value) != test.want || response.Height != 9 {
			t.Fatalf("query %s = code:%d value:%q height:%d", test.path, response.Code, response.Value, response.Height)
		}
	}
}

func signedLegacyTransaction(t *testing.T, privateKey *ecdsa.PrivateKey, nonce, gas uint64, to *common.Address, value *big.Int, data []byte) []byte {
	t.Helper()
	unsigned := types.NewTx(&types.LegacyTx{
		Nonce: nonce, GasPrice: big.NewInt(0), Gas: gas, To: to,
		Value: new(big.Int).Set(value), Data: bytes.Clone(data),
	})
	signed, err := types.SignTx(unsigned, types.LatestSignerForChainID(big.NewInt(projectconfig.DefaultEVMChainID)), privateKey)
	if err != nil {
		t.Fatalf("sign transaction: %v", err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal transaction: %v", err)
	}
	return raw
}

func seedBalance(t *testing.T, application *App, address common.Address, balance *big.Int) {
	t.Helper()
	stateDB := evm.NewPebbleStateDB(application.db)
	stateDB.AddBalanceBig(address, balance)
	if err := stateDB.Commit(); err != nil {
		t.Fatalf("seed balance: %v", err)
	}
}

func finalizeTransactions(t *testing.T, application *App, height int64, transactions ...[]byte) *abci.FinalizeBlockResponse {
	t.Helper()
	response, err := application.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{
		Txs: transactions, Height: height, Time: time.Unix(height, 0),
	})
	if err != nil {
		t.Fatalf("finalize block: %v", err)
	}
	return response
}
