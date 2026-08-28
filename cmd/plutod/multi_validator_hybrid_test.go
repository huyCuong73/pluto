package main

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"sort"
	"testing"
	"time"

	"github.com/cometbft/cometbft/config"
	cmtp2p "github.com/cometbft/cometbft/p2p"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	cmttypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	projectconfig "github.com/huyCuong73/pluto/internal/config"
	"github.com/huyCuong73/pluto/internal/pqc"
	plutotx "github.com/huyCuong73/pluto/internal/tx"
)

// TestHybridTransactionReachesIdenticalStateOnTwoValidators upgrades the
// deterministic app test to a real two-validator CometBFT network. Each node
// owns a distinct consensus key, while both use the same genesis, transaction
// policy and application state.
func TestHybridTransactionReachesIdenticalStateOnTwoValidators(t *testing.T) {
	ecdsaPrivateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	sender := crypto.PubkeyToAddress(ecdsaPrivateKey.PublicKey)
	receiver := common.HexToAddress("0xb200000000000000000000000000000000000002")
	initialBalance := big.NewInt(1_000_000)
	transferValue := big.NewInt(4321)
	scheme := pqc.NewMLDSA65()
	pqPublicKey, pqPrivateKey, err := scheme.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate ML-DSA-65 key: %v", err)
	}
	pqHash := plutotx.HashPQPublicKey(pqPublicKey)

	homes := []string{t.TempDir(), t.TempDir()}
	for _, homeDir := range homes {
		if err := runInit([]string{
			"--home", homeDir,
			"--chain-id", "pluto-hybrid-two-validator-1",
			"--tx-policy", projectconfig.TransactionPolicyHybridMLDSA65,
			"--alloc", sender.Hex() + "=" + initialBalance.String(),
			"--pqc-key", sender.Hex() + "=" + pqHash.Hex(),
		}); err != nil {
			t.Fatalf("init validator home %s: %v", homeDir, err)
		}
	}
	installSharedTwoValidatorGenesis(t, homes)

	rpcAddresses := []string{unusedTCPAddress(t), unusedTCPAddress(t)}
	p2pAddresses := []string{unusedTCPAddress(t), unusedTCPAddress(t)}
	nodeKeys := make([]*cmtp2p.NodeKey, len(homes))
	for index, homeDir := range homes {
		cfg := config.DefaultConfig().SetRoot(homeDir)
		nodeKeys[index], err = cmtp2p.LoadNodeKey(cfg.NodeKeyFile())
		if err != nil {
			t.Fatalf("load node key %d: %v", index, err)
		}
	}

	node1 := startTwoValidatorRPCNode(
		t,
		homes[0],
		rpcAddresses[0],
		p2pAddresses[0],
		fmt.Sprintf("%s@%s", nodeKeys[1].ID(), p2pAddresses[1]),
	)
	defer node1.stop(t)
	node2 := startTwoValidatorRPCNode(
		t,
		homes[1],
		rpcAddresses[1],
		p2pAddresses[1],
		fmt.Sprintf("%s@%s", nodeKeys[0].ID(), p2pAddresses[0]),
	)
	defer node2.stop(t)

	waitForMatchingNetworkState(t, node1.client, node2.client, 1)

	ethereumRaw := signedEthereumTransfer(
		t,
		ecdsaPrivateKey,
		receiver,
		0,
		transferValue,
		projectconfig.DefaultEVMChainID,
	)
	hybridRaw, err := plutotx.CreateSignedHybridEnvelopeV1(
		ethereumRaw,
		pqPublicKey,
		pqPrivateKey,
		big.NewInt(projectconfig.DefaultEVMChainID),
		scheme,
	)
	if err != nil {
		t.Fatalf("create hybrid transaction: %v", err)
	}
	result, err := node1.client.BroadcastTxCommit(context.Background(), cmttypes.Tx(hybridRaw))
	if err != nil {
		t.Fatalf("broadcast hybrid transaction: %v", err)
	}
	if result.CheckTx.Code != 0 || result.TxResult.Code != 0 {
		t.Fatalf("hybrid transaction codes = CheckTx:%d Finalize:%d", result.CheckTx.Code, result.TxResult.Code)
	}

	wantSenderBalance := new(big.Int).Sub(new(big.Int).Set(initialBalance), transferValue)
	waitForRPCAccountState(t, node1.client, sender, wantSenderBalance, 1)
	waitForRPCAccountState(t, node2.client, sender, wantSenderBalance, 1)
	waitForRPCAccountState(t, node1.client, receiver, transferValue, 0)
	waitForRPCAccountState(t, node2.client, receiver, transferValue, 0)
	waitForMatchingNetworkState(t, node1.client, node2.client, result.Height)
}

func installSharedTwoValidatorGenesis(t *testing.T, homes []string) {
	t.Helper()
	if len(homes) != 2 {
		t.Fatalf("shared genesis requires two homes, got %d", len(homes))
	}
	configs := []*config.Config{
		config.DefaultConfig().SetRoot(homes[0]),
		config.DefaultConfig().SetRoot(homes[1]),
	}
	genesis1, err := cmttypes.GenesisDocFromFile(configs[0].GenesisFile())
	if err != nil {
		t.Fatalf("read validator 1 genesis: %v", err)
	}
	genesis2, err := cmttypes.GenesisDocFromFile(configs[1].GenesisFile())
	if err != nil {
		t.Fatalf("read validator 2 genesis: %v", err)
	}
	if !bytes.Equal(genesis1.AppState, genesis2.AppState) || genesis1.ChainID != genesis2.ChainID {
		t.Fatal("validator homes do not have matching application genesis")
	}
	genesis1.Validators = append(genesis1.Validators, genesis2.Validators[0])
	sort.Slice(genesis1.Validators, func(i, j int) bool {
		return bytes.Compare(genesis1.Validators[i].Address, genesis1.Validators[j].Address) < 0
	})
	if err := genesis1.ValidateAndComplete(); err != nil {
		t.Fatalf("validate shared genesis: %v", err)
	}
	for _, cfg := range configs {
		if err := genesis1.SaveAs(cfg.GenesisFile()); err != nil {
			t.Fatalf("write shared genesis %s: %v", cfg.GenesisFile(), err)
		}
	}
}

func startTwoValidatorRPCNode(t *testing.T, homeDir, rpcAddress, p2pAddress, persistentPeer string) *rpcTestNode {
	t.Helper()
	remote := "http://" + rpcAddress
	ctx, cancel := context.WithCancel(context.Background())
	node := &rpcTestNode{cancel: cancel, done: make(chan error, 1)}
	go func() {
		node.done <- startNodeWithConfig(ctx, homeDir, func(cfg *config.Config) {
			cfg.RPC.ListenAddress = "tcp://" + rpcAddress
			cfg.P2P.ListenAddress = "tcp://" + p2pAddress
			cfg.P2P.ExternalAddress = p2pAddress
			cfg.P2P.PersistentPeers = persistentPeer
			cfg.P2P.AddrBookStrict = false
			cfg.P2P.AllowDuplicateIP = true
			cfg.P2P.PexReactor = false
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

func waitForRPCAccountState(t *testing.T, client *rpchttp.HTTP, address common.Address, balance *big.Int, nonce uint64) {
	t.Helper()
	deadline := time.Now().Add(e2eNodeTimeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		balanceResult, balanceErr := client.ABCIQuery(ctx, "/balance", []byte(address.Hex()))
		nonceResult, nonceErr := client.ABCIQuery(ctx, "/nonce", []byte(address.Hex()))
		cancel()
		if balanceErr == nil && nonceErr == nil &&
			string(balanceResult.Response.Value) == balance.String() &&
			string(nonceResult.Response.Value) == fmt.Sprintf("%d", nonce) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("account %s did not reach balance=%s nonce=%d", address.Hex(), balance, nonce)
}

func waitForMatchingNetworkState(t *testing.T, first, second *rpchttp.HTTP, minimumHeight int64) {
	t.Helper()
	deadline := time.Now().Add(e2eNodeTimeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		firstStatus, firstErr := first.Status(ctx)
		secondStatus, secondErr := second.Status(ctx)
		cancel()
		if firstErr == nil && secondErr == nil &&
			firstStatus.SyncInfo.LatestBlockHeight >= minimumHeight &&
			firstStatus.SyncInfo.LatestBlockHeight == secondStatus.SyncInfo.LatestBlockHeight &&
			bytes.Equal(firstStatus.SyncInfo.LatestAppHash, secondStatus.SyncInfo.LatestAppHash) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("validator nodes did not reach matching height/AppHash at height >= %d", minimumHeight)
}
