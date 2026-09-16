package app

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	dbm "github.com/cometbft/cometbft-db"
	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
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
	if _, err := application.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatalf("commit block: %v", err)
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
		data     []byte
		create   bool
		wantCode uint32
		wantGas  int64
	}{
		{name: "below intrinsic", gas: 20_999, wantCode: CodeTransactionRejected},
		{name: "exact intrinsic", gas: 21_000, wantCode: CodeOK, wantGas: 21_000},
		{name: "above intrinsic", gas: 30_000, wantCode: CodeOK, wantGas: 21_000},
		{name: "calldata below intrinsic", gas: 21_015, data: []byte{1}, wantCode: CodeTransactionRejected},
		{name: "calldata exact intrinsic", gas: 21_016, data: []byte{1}, wantCode: CodeOK, wantGas: 21_016},
		{name: "creation below intrinsic", gas: 52_999, create: true, wantCode: CodeTransactionRejected},
		{name: "creation exact intrinsic", gas: 53_000, create: true, wantCode: CodeOK, wantGas: 53_000},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			application := newTestApp(t)
			seedBalance(t, application, sender, new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil))
			to := &receiver
			if test.create {
				to = nil
			}
			raw := signedLegacyTransaction(t, privateKey, 0, test.gas, to, big.NewInt(0), test.data)
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

func TestAccessListIntrinsicGasIsEnforced(t *testing.T) {
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	sender := crypto.PubkeyToAddress(privateKey.PublicKey)
	receiver := common.HexToAddress("0x1110000000000000000000000000000000000011")
	accessList := types.AccessList{{
		Address:     common.HexToAddress("0x1120000000000000000000000000000000000011"),
		StorageKeys: []common.Hash{common.HexToHash("0x01")},
	}}
	for _, test := range []struct {
		name     string
		gas      uint64
		wantCode uint32
		wantUsed int64
	}{
		{name: "below", gas: 25_299, wantCode: CodeTransactionRejected},
		{name: "exact", gas: 25_300, wantCode: CodeOK, wantUsed: 25_300},
	} {
		t.Run(test.name, func(t *testing.T) {
			application := newTestApp(t)
			seedBalance(t, application, sender, big.NewInt(1))
			unsigned := types.NewTx(&types.AccessListTx{
				ChainID: big.NewInt(projectconfig.DefaultEVMChainID), Nonce: 0,
				GasPrice: big.NewInt(0), Gas: test.gas, To: &receiver,
				Value: big.NewInt(0), AccessList: accessList,
			})
			signed, signErr := types.SignTx(unsigned, types.LatestSignerForChainID(big.NewInt(projectconfig.DefaultEVMChainID)), privateKey)
			if signErr != nil {
				t.Fatalf("sign access-list transaction: %v", signErr)
			}
			raw, marshalErr := signed.MarshalBinary()
			if marshalErr != nil {
				t.Fatalf("marshal access-list transaction: %v", marshalErr)
			}
			result := finalizeTransactions(t, application, 1, raw).TxResults[0]
			if result.Code != test.wantCode || result.GasUsed != test.wantUsed {
				t.Fatalf("result = code:%d gas:%d log:%q, want code:%d gas:%d", result.Code, result.GasUsed, result.Log, test.wantCode, test.wantUsed)
			}
		})
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

func TestFailedContractCreationDoesNotCommitRevertedAccountToAppHash(t *testing.T) {
	application := newTestApp(t)
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	sender := crypto.PubkeyToAddress(privateKey.PublicKey)
	seedBalance(t, application, sender, big.NewInt(1))

	raw := signedLegacyTransaction(t, privateKey, 0, 100_000, nil, big.NewInt(0), []byte{0xfe})
	response := finalizeTransactions(t, application, 1, raw)
	if response.TxResults[0].Code != CodeExecutionFailed {
		t.Fatalf("failed creation code = %d, want EVM execution failure", response.TxResults[0].Code)
	}
	if got := application.pendingState.GetNonce(sender); got != 1 {
		t.Fatalf("sender nonce after failed creation = %d, want 1", got)
	}
	created := crypto.CreateAddress(sender, 0)
	if got := application.pendingState.GetCode(created); len(got) != 0 {
		t.Fatalf("failed creation left code at %s: %x", created, got)
	}

	encoded, err := rlp.EncodeToBytes(&evm.Account{Nonce: 1, Balance: big.NewInt(1)})
	if err != nil {
		t.Fatalf("encode expected sender account: %v", err)
	}
	hasher := sha256.New()
	hasher.Write(sender.Bytes())
	hasher.Write(encoded)
	wantHash := hasher.Sum(nil)
	if !bytes.Equal(response.AppHash, wantHash) {
		t.Fatalf("failed creation AppHash = %x, want sender-only state transition %x", response.AppHash, wantHash)
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

func TestStorageOriginalValueAdvancesBetweenTransactions(t *testing.T) {
	application := newTestApp(t)
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	sender := crypto.PubkeyToAddress(privateKey.PublicKey)
	contract := common.HexToAddress("0x1210000000000000000000000000000000000012")
	slot := common.Hash{}
	valueA := common.HexToHash("0xaa")
	valueB := common.HexToHash("0xbb")
	valueC := common.HexToHash("0xcc")

	// Runtime: SSTORE(slot 0, CALLDATALOAD(0)); STOP.
	stateDB := evm.NewPebbleStateDB(application.db)
	stateDB.AddBalanceBig(sender, big.NewInt(1))
	stateDB.SetCode(contract, common.FromHex("0x60003560005500"), tracing.CodeChangeUnspecified)
	stateDB.SetState(contract, slot, valueA)
	if err := stateDB.Commit(); err != nil {
		t.Fatalf("seed storage contract: %v", err)
	}

	response := finalizeTransactions(t, application, 1,
		signedLegacyTransaction(t, privateKey, 0, 100_000, &contract, big.NewInt(0), valueB.Bytes()),
		signedLegacyTransaction(t, privateKey, 1, 100_000, &contract, big.NewInt(0), valueC.Bytes()),
	)
	for index, result := range response.TxResults {
		if result.Code != CodeOK {
			t.Fatalf("transaction %d failed: %q", index, result.Log)
		}
		if result.GasUsed != 26_149 {
			t.Fatalf("transaction %d gas used = %d, want 26149 (reset from transaction-original nonzero slot)", index, result.GasUsed)
		}
	}
	if _, err := application.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatalf("commit block: %v", err)
	}
	stateDB = evm.NewPebbleStateDB(application.db)
	if got := stateDB.GetState(contract, slot); got != valueC {
		t.Fatalf("committed storage = %s, want %s", got, valueC)
	}
}

func TestSuccessfulTransactionContinuesAfterRevertedExecution(t *testing.T) {
	application := newTestApp(t)
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	sender := crypto.PubkeyToAddress(privateKey.PublicKey)
	receiver := common.HexToAddress("0x1220000000000000000000000000000000000012")
	contract := common.HexToAddress("0x1230000000000000000000000000000000000012")
	stateDB := evm.NewPebbleStateDB(application.db)
	stateDB.AddBalanceBig(sender, big.NewInt(10))
	// Runtime writes slot 0, emits LOG0, then REVERTs. Both effects must roll back.
	stateDB.SetCode(contract, common.FromHex("0x600160005560006000a060006000fd"), tracing.CodeChangeUnspecified)
	if err := stateDB.Commit(); err != nil {
		t.Fatalf("seed reverting contract: %v", err)
	}

	response := finalizeTransactions(t, application, 1,
		signedLegacyTransaction(t, privateKey, 0, 21_000, &receiver, big.NewInt(2), nil),
		signedLegacyTransaction(t, privateKey, 1, 100_000, &contract, big.NewInt(0), nil),
		signedLegacyTransaction(t, privateKey, 2, 21_000, &receiver, big.NewInt(3), nil),
	)
	wantCodes := []uint32{CodeOK, CodeExecutionFailed, CodeOK}
	for index, want := range wantCodes {
		if response.TxResults[index].Code != want {
			t.Fatalf("transaction %d code = %d, want %d; log=%q", index, response.TxResults[index].Code, want, response.TxResults[index].Log)
		}
	}
	if got := len(application.pendingState.GetLogs()); got != 0 {
		t.Fatalf("reverted transaction leaked %d logs", got)
	}
	if _, err := application.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatalf("commit block: %v", err)
	}
	stateDB = evm.NewPebbleStateDB(application.db)
	if got := stateDB.GetNonce(sender); got != 3 {
		t.Fatalf("sender nonce = %d, want 3", got)
	}
	if got := stateDB.GetBalance(receiver).ToBig(); got.Cmp(big.NewInt(5)) != 0 {
		t.Fatalf("receiver balance = %s, want 5", got)
	}
	if got := stateDB.GetState(contract, common.Hash{}); got != (common.Hash{}) {
		t.Fatalf("reverted storage write persisted: %s", got)
	}
}

func TestLegacySelfDestructDoesNotResurrectCodeOrStorage(t *testing.T) {
	dbPath := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application, err := NewAppWithChainID(dbPath, logger, projectconfig.DefaultEVMChainID)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			if err := application.Close(); err != nil {
				t.Errorf("close app: %v", err)
			}
		}
	})

	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	sender := crypto.PubkeyToAddress(privateKey.PublicKey)
	contract := common.HexToAddress("0x1231000000000000000000000000000000000012")
	beneficiary := common.HexToAddress("0x1232000000000000000000000000000000000012")
	slot := common.HexToHash("0x01")
	oldValue := common.HexToHash("0xdeadbeef")
	selfDestructCode := append([]byte{0x73}, beneficiary.Bytes()...)
	selfDestructCode = append(selfDestructCode, 0xff) // PUSH20 beneficiary; SELFDESTRUCT

	stateDB := evm.NewPebbleStateDB(application.db)
	stateDB.AddBalanceBig(sender, big.NewInt(1))
	stateDB.SetCode(contract, selfDestructCode, tracing.CodeChangeUnspecified)
	stateDB.SetState(contract, slot, oldValue)
	if err := stateDB.Commit(); err != nil {
		t.Fatalf("seed self-destruct contract: %v", err)
	}

	response := finalizeTransactions(t, application, 1,
		signedLegacyTransaction(t, privateKey, 0, 100_000, &contract, big.NewInt(0), nil),
		signedLegacyTransaction(t, privateKey, 1, 100_000, &contract, big.NewInt(0), nil),
	)
	for index, result := range response.TxResults {
		if result.Code != CodeOK {
			t.Fatalf("transaction %d failed: code=%d log=%q", index, result.Code, result.Log)
		}
	}
	if got := response.TxResults[1].GasUsed; got != 21_000 {
		t.Fatalf("second call gas used = %d, want 21000 for a logically absent account", got)
	}
	assertLogicallyDeletedContract(t, application.pendingState, contract, slot)

	if _, err := application.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatalf("commit self-destruct block: %v", err)
	}
	if err := application.Close(); err != nil {
		t.Fatalf("close app before restart: %v", err)
	}
	closed = true

	reopened, err := NewAppWithChainID(dbPath, logger, projectconfig.DefaultEVMChainID)
	if err != nil {
		t.Fatalf("reopen app: %v", err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("close reopened app: %v", err)
		}
	}()
	restartedState := evm.NewPebbleStateDB(reopened.db)
	assertLogicallyDeletedContract(t, restartedState, contract, slot)
}

func assertLogicallyDeletedContract(t *testing.T, stateDB *evm.PebbleStateDB, contract common.Address, slot common.Hash) {
	t.Helper()
	if stateDB.Exist(contract) {
		t.Fatalf("destroyed contract %s still exists", contract)
	}
	if code := stateDB.GetCode(contract); len(code) != 0 {
		t.Fatalf("destroyed contract code resurrected: %x", code)
	}
	if value := stateDB.GetState(contract, slot); value != (common.Hash{}) {
		t.Fatalf("destroyed contract storage resurrected: %s", value)
	}
}

func TestRejectedPrecheckDoesNotChangeAppHash(t *testing.T) {
	application := newTestApp(t)
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	sender := crypto.PubkeyToAddress(privateKey.PublicKey)
	other := common.HexToAddress("0x1240000000000000000000000000000000000012")
	receiver := common.HexToAddress("0x1250000000000000000000000000000000000012")
	stateDB := evm.NewPebbleStateDB(application.db)
	stateDB.AddBalanceBig(sender, big.NewInt(1))
	stateDB.AddBalanceBig(other, big.NewInt(1))
	baselineHash, err := stateDB.ComputeAppHash()
	if err != nil {
		t.Fatalf("compute baseline AppHash: %v", err)
	}
	if err := stateDB.Commit(); err != nil {
		t.Fatalf("seed accounts: %v", err)
	}
	application.appHash = bytes.Clone(baselineHash)

	response := finalizeTransactions(t, application, 1,
		signedLegacyTransaction(t, privateKey, 0, 21_000, &receiver, big.NewInt(2), nil),
	)
	if response.TxResults[0].Code != CodeTransactionRejected {
		t.Fatalf("insufficient-funds transaction code = %d, want precheck rejection", response.TxResults[0].Code)
	}
	if !bytes.Equal(response.AppHash, baselineHash) {
		t.Fatalf("rejected precheck changed AppHash: got %x, want %x", response.AppHash, baselineHash)
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
	if response.TxResults[1].Code != CodeTransactionRejected {
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

func TestProposalGasLimitMatchesCometConsensusBudget(t *testing.T) {
	application := newTestApp(t)
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	receiver := common.HexToAddress("0x1410000000000000000000000000000000000014")
	txs := [][]byte{
		signedLegacyTransaction(t, privateKey, 0, 6_000_000, &receiver, big.NewInt(0), nil),
		signedLegacyTransaction(t, privateKey, 1, 5_000_000, &receiver, big.NewInt(0), nil),
	}
	prepared, err := application.PrepareProposal(context.Background(), &abci.PrepareProposalRequest{
		Txs: txs, Height: 1, Time: time.Unix(1, 0),
	})
	if err != nil {
		t.Fatalf("prepare proposal: %v", err)
	}
	if len(prepared.Txs) != 1 || !bytes.Equal(prepared.Txs[0], txs[0]) {
		t.Fatalf("prepared %d transactions, want only the first transaction within gas budget", len(prepared.Txs))
	}
	processed, err := application.ProcessProposal(context.Background(), &abci.ProcessProposalRequest{
		Txs: txs, Height: 1, Time: time.Unix(1, 0),
	})
	if err != nil {
		t.Fatalf("process proposal: %v", err)
	}
	if processed.Status != abci.PROCESS_PROPOSAL_STATUS_REJECT {
		t.Fatalf("proposal status = %v, want reject for cumulative gas limit", processed.Status)
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

func TestDatabaseReadFailureReachesApplicationBoundary(t *testing.T) {
	application := newTestApp(t)
	injected := errors.New("injected consensus read failure")
	application.db = &readFailureApplicationDB{applicationDB: application.db, err: injected}

	response, err := application.Query(context.Background(), &abci.QueryRequest{
		Path: QueryBalance, Data: []byte("0x1600000000000000000000000000000000000016"),
	})
	if err == nil || !errors.Is(err, injected) {
		t.Fatalf("Query response=%#v error=%v, want injected read failure", response, err)
	}

	privateKey, keyErr := crypto.GenerateKey()
	if keyErr != nil {
		t.Fatalf("generate ECDSA key: %v", keyErr)
	}
	receiver := common.HexToAddress("0x1610000000000000000000000000000000000016")
	raw := signedLegacyTransaction(t, privateKey, 0, 21_000, &receiver, big.NewInt(0), nil)
	finalized, err := application.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{
		Txs: [][]byte{raw}, Height: 1, Time: time.Unix(1, 0),
	})
	if err == nil || !errors.Is(err, injected) {
		t.Fatalf("FinalizeBlock response=%#v error=%v, want injected read failure", finalized, err)
	}
}

func TestAtomicCommitFailureLeavesStateAndMetadataUnchanged(t *testing.T) {
	application := newTestApp(t)
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	sender := crypto.PubkeyToAddress(privateKey.PublicKey)
	receiver := common.HexToAddress("0x1700000000000000000000000000000000000017")
	seedBalance(t, application, sender, big.NewInt(10))
	underlying := application.db
	failing := &commitFailureApplicationDB{applicationDB: underlying, err: errors.New("injected batch failure")}
	application.db = failing

	finalized := finalizeTransactions(t, application, 1,
		signedLegacyTransaction(t, privateKey, 0, 21_000, &receiver, big.NewInt(2), nil),
	)
	if _, err := application.Commit(context.Background(), &abci.CommitRequest{}); !errors.Is(err, failing.err) {
		t.Fatalf("Commit error = %v, want injected batch failure", err)
	}
	stateDB := evm.NewPebbleStateDB(underlying)
	if got := stateDB.GetNonce(sender); got != 0 {
		t.Fatalf("nonce after failed atomic commit = %d, want 0", got)
	}
	info, err := application.Info(context.Background(), &abci.InfoRequest{})
	if err != nil {
		t.Fatalf("info after failed commit: %v", err)
	}
	if info.LastBlockHeight != 0 || len(info.LastBlockAppHash) != 0 {
		t.Fatalf("metadata changed after failed commit: height=%d hash=%x", info.LastBlockHeight, info.LastBlockAppHash)
	}

	failing.disabled = true
	if _, err := application.Commit(context.Background(), &abci.CommitRequest{}); err != nil {
		t.Fatalf("retry Commit: %v", err)
	}
	stateDB = evm.NewPebbleStateDB(underlying)
	if got := stateDB.GetNonce(sender); got != 1 {
		t.Fatalf("nonce after successful retry = %d, want 1", got)
	}
	info, err = application.Info(context.Background(), &abci.InfoRequest{})
	if err != nil {
		t.Fatalf("info after retry: %v", err)
	}
	if info.LastBlockHeight != 1 || !bytes.Equal(info.LastBlockAppHash, finalized.AppHash) {
		t.Fatalf("metadata after retry: height=%d hash=%x, want height=1 hash=%x", info.LastBlockHeight, info.LastBlockAppHash, finalized.AppHash)
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

type readFailureApplicationDB struct {
	applicationDB
	err error
}

func (db *readFailureApplicationDB) Get([]byte) ([]byte, error) { return nil, db.err }

type commitFailureApplicationDB struct {
	applicationDB
	err      error
	disabled bool
}

func (db *commitFailureApplicationDB) NewBatch() dbm.Batch {
	return &commitFailureBatch{Batch: db.applicationDB.NewBatch(), owner: db}
}

type commitFailureBatch struct {
	dbm.Batch
	owner *commitFailureApplicationDB
}

func (batch *commitFailureBatch) WriteSync() error {
	if !batch.owner.disabled {
		return batch.owner.err
	}
	return batch.Batch.WriteSync()
}
