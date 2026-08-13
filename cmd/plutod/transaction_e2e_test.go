package main

import (
	"context"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cometbft/cometbft/config"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	cmttypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/huyCuong73/pluto/internal/app"
	projectconfig "github.com/huyCuong73/pluto/internal/config"
)

const e2eNodeTimeout = 15 * time.Second

// TestSignedECDSATransactionThroughRPCPersistsAfterRestart is the Day 2.5
// baseline. It proves the complete path before a hybrid ML-DSA envelope is
// introduced: RPC -> mempool -> proposal -> consensus -> EVM -> Pebble -> RPC.
func TestSignedECDSATransactionThroughRPCPersistsAfterRestart(t *testing.T) {
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate sender key: %v", err)
	}
	sender := crypto.PubkeyToAddress(privateKey.PublicKey)
	receiver := common.HexToAddress("0x2000000000000000000000000000000000000002")
	initialBalance := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	transferValue := big.NewInt(123_456_789)

	homeDir := t.TempDir()
	if err := runInit([]string{
		"--home", homeDir,
		"--chain-id", "pluto-transaction-e2e-1",
		"--alloc", sender.Hex() + "=" + initialBalance.String(),
	}); err != nil {
		t.Fatalf("init funded node: %v", err)
	}

	unsigned := gethtypes.NewTx(&gethtypes.LegacyTx{
		Nonce:    0,
		GasPrice: big.NewInt(0),
		Gas:      21_000,
		To:       &receiver,
		Value:    transferValue,
	})
	signed, err := gethtypes.SignTx(
		unsigned,
		gethtypes.LatestSignerForChainID(big.NewInt(projectconfig.DefaultEVMChainID)),
		privateKey,
	)
	if err != nil {
		t.Fatalf("sign transaction: %v", err)
	}
	rawTransaction, err := signed.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal transaction: %v", err)
	}

	firstNode := startRPCNode(t, homeDir)
	rejected, err := firstNode.client.CheckTx(
		context.Background(),
		cmttypes.Tx("malformed transaction"),
	)
	if err != nil {
		firstNode.stop(t)
		t.Fatalf("check malformed transaction through RPC: %v", err)
	}
	if rejected.Code != app.CodeInvalidEncoding {
		firstNode.stop(t)
		t.Fatalf("malformed CheckTx code = %d, want %d", rejected.Code, app.CodeInvalidEncoding)
	}

	result, err := firstNode.client.BroadcastTxCommit(
		context.Background(),
		cmttypes.Tx(rawTransaction),
	)
	if err != nil {
		firstNode.stop(t)
		t.Fatalf("broadcast signed transaction: %v", err)
	}
	if result.CheckTx.Code != app.CodeOK {
		firstNode.stop(t)
		t.Fatalf("CheckTx code = %d, log = %q", result.CheckTx.Code, result.CheckTx.Log)
	}
	if result.TxResult.Code != app.CodeOK {
		firstNode.stop(t)
		t.Fatalf("FinalizeBlock code = %d, log = %q", result.TxResult.Code, result.TxResult.Log)
	}
	if result.Height < 1 {
		firstNode.stop(t)
		t.Fatalf("committed height = %d, want at least 1", result.Height)
	}

	wantSenderBalance := new(big.Int).Sub(new(big.Int).Set(initialBalance), transferValue)
	assertRPCAccountState(t, firstNode.client, sender, wantSenderBalance, 1)
	assertRPCAccountState(t, firstNode.client, receiver, transferValue, 0)
	firstNode.stop(t)

	// Start a fresh CometBFT/App instance with the same home. Reading the state
	// again through RPC proves that the result came from persisted data, not an
	// in-memory cache owned by the first process.
	restartedNode := startRPCNode(t, homeDir)
	defer restartedNode.stop(t)
	assertRPCAccountState(t, restartedNode.client, sender, wantSenderBalance, 1)
	assertRPCAccountState(t, restartedNode.client, receiver, transferValue, 0)
}

type rpcTestNode struct {
	client *rpchttp.HTTP
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
}

func startRPCNode(t *testing.T, homeDir string) *rpcTestNode {
	t.Helper()
	rpcAddress := unusedTCPAddress(t)
	p2pAddress := unusedTCPAddress(t)
	remote := "http://" + rpcAddress

	ctx, cancel := context.WithCancel(context.Background())
	node := &rpcTestNode{
		cancel: cancel,
		done:   make(chan error, 1),
	}
	go func() {
		node.done <- startNodeWithConfig(ctx, homeDir, func(cfg *config.Config) {
			cfg.RPC.ListenAddress = "tcp://" + rpcAddress
			cfg.P2P.ListenAddress = "tcp://" + p2pAddress
			// Avoid the CometBFT v1.0.0 Windows KV-indexer handle leak during
			// an in-process restart. broadcast_tx_commit uses the event bus and
			// remains available with the supported null indexer.
			cfg.TxIndex.Indexer = "null"
		})
	}()

	client, err := rpchttp.NewWithTimeout(remote, uint(e2eNodeTimeout/time.Second))
	if err != nil {
		node.stop(t)
		t.Fatalf("create RPC client: %v", err)
	}
	node.client = client
	waitForRPC(t, node)
	return node
}

func waitForRPC(t *testing.T, node *rpcTestNode) {
	t.Helper()
	deadline := time.Now().Add(e2eNodeTimeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-node.done:
			t.Fatalf("node stopped before RPC became ready: %v", err)
		default:
		}

		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_, err := node.client.Status(ctx)
		cancel()
		if err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	node.stop(t)
	t.Fatal("RPC did not become ready before timeout")
}

func (node *rpcTestNode) stop(t *testing.T) {
	t.Helper()
	node.once.Do(func() {
		node.cancel()
		select {
		case err := <-node.done:
			if err != nil {
				t.Errorf("stop node: %v", err)
			}
		case <-time.After(e2eNodeTimeout):
			t.Error("node did not stop before timeout")
		}
	})
}

func assertRPCAccountState(t *testing.T, client *rpchttp.HTTP, address common.Address, wantBalance *big.Int, wantNonce uint64) {
	t.Helper()
	if got := queryRPCValue(t, client, app.QueryBalance, address); got != wantBalance.String() {
		t.Fatalf("balance of %s = %s, want %s", address.Hex(), got, wantBalance)
	}
	if got := queryRPCValue(t, client, app.QueryNonce, address); got != strconv.FormatUint(wantNonce, 10) {
		t.Fatalf("nonce of %s = %s, want %d", address.Hex(), got, wantNonce)
	}
}

func queryRPCValue(t *testing.T, client *rpchttp.HTTP, path string, address common.Address) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), e2eNodeTimeout)
	defer cancel()
	result, err := client.ABCIQuery(ctx, path, []byte(address.Hex()))
	if err != nil {
		t.Fatalf("query %s for %s: %v", path, address.Hex(), err)
	}
	if result.Response.Code != app.CodeOK {
		t.Fatalf("query %s code = %d, log = %q", path, result.Response.Code, result.Response.Log)
	}
	return string(result.Response.Value)
}

func unusedTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve TCP address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release TCP address %s: %v", address, err)
	}
	if address == "" {
		t.Fatal(fmt.Errorf("reserved an empty TCP address"))
	}
	return address
}
