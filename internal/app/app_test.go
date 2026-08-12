package app

import (
	"context"
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
	projectconfig "github.com/huyCuong73/pluto/internal/config"
	"github.com/huyCuong73/pluto/internal/evm"
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
