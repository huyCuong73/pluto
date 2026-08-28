package main

import (
	"context"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/cometbft/cometbft/config"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	gethrpc "github.com/ethereum/go-ethereum/rpc"
	projectconfig "github.com/huyCuong73/pluto/internal/config"
	"github.com/huyCuong73/pluto/internal/pqc"
	"github.com/huyCuong73/pluto/internal/pqcproxy"
	plutotx "github.com/huyCuong73/pluto/internal/tx"
)

// TestMetaMaskStyleTransferThroughEthereumJSONRPC chứng minh đường đi mà
// MetaMask sử dụng: eth_chainId -> query account -> eth_sendRawTransaction ->
// receipt -> state mới. Test không gọi helper ABCI trực tiếp.
func TestMetaMaskStyleTransferThroughEthereumJSONRPC(t *testing.T) {
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	sender := crypto.PubkeyToAddress(privateKey.PublicKey)
	receiver := common.HexToAddress("0xc100000000000000000000000000000000000001")
	initialBalance := big.NewInt(1_000_000)
	transferValue := big.NewInt(12_345)

	homeDir := t.TempDir()
	if err := runInit([]string{
		"--home", homeDir,
		"--chain-id", "pluto-metamask-e2e-1",
		"--alloc", sender.Hex() + "=" + initialBalance.String(),
	}); err != nil {
		t.Fatalf("init MetaMask-compatible node: %v", err)
	}

	node := startEthereumRPCNode(t, homeDir)
	defer node.stop(t)

	var chainID hexutil.Uint64
	if err := node.client.CallContext(context.Background(), &chainID, "eth_chainId"); err != nil {
		t.Fatalf("eth_chainId: %v", err)
	}
	if int64(chainID) != projectconfig.DefaultEVMChainID {
		t.Fatalf("eth_chainId = %d, want %d", chainID, projectconfig.DefaultEVMChainID)
	}

	assertEthereumRPCAccount(t, node.client, sender, initialBalance, 0)
	raw := signedEthereumTransfer(t, privateKey, receiver, 0, transferValue, projectconfig.DefaultEVMChainID)
	var transactionHash common.Hash
	if err := node.client.CallContext(context.Background(), &transactionHash, "eth_sendRawTransaction", hexutil.Bytes(raw)); err != nil {
		t.Fatalf("eth_sendRawTransaction: %v", err)
	}

	type rpcReceipt struct {
		TransactionHash common.Hash    `json:"transactionHash"`
		BlockNumber     hexutil.Uint64 `json:"blockNumber"`
		Status          hexutil.Uint64 `json:"status"`
	}
	var receipt *rpcReceipt
	if err := node.client.CallContext(context.Background(), &receipt, "eth_getTransactionReceipt", transactionHash); err != nil {
		t.Fatalf("eth_getTransactionReceipt: %v", err)
	}
	if receipt == nil || receipt.TransactionHash != transactionHash || receipt.Status != 1 || receipt.BlockNumber < 1 {
		t.Fatalf("unexpected receipt: %#v", receipt)
	}

	wantSender := new(big.Int).Sub(new(big.Int).Set(initialBalance), transferValue)
	assertEthereumRPCAccount(t, node.client, sender, wantSender, 1)
	assertEthereumRPCAccount(t, node.client, receiver, transferValue, 0)
}

// TestHybridTransactionThroughPlutoJSONRPC chứng minh module PQC dùng endpoint
// riêng nhưng đi qua cùng CometBFT, EVM và database như transaction MetaMask.
func TestHybridTransactionThroughPlutoJSONRPC(t *testing.T) {
	ecdsaPrivateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	sender := crypto.PubkeyToAddress(ecdsaPrivateKey.PublicKey)
	receiver := common.HexToAddress("0xc200000000000000000000000000000000000002")
	initialBalance := big.NewInt(1_000_000)
	transferValue := big.NewInt(7_777)
	scheme := pqc.NewMLDSA65()
	pqPublicKey, pqPrivateKey, err := scheme.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate ML-DSA-65 key: %v", err)
	}

	homeDir := t.TempDir()
	if err := runInit([]string{
		"--home", homeDir,
		"--chain-id", "pluto-pqc-jsonrpc-e2e-1",
		"--tx-policy", projectconfig.TransactionPolicyHybridMLDSA65,
		"--alloc", sender.Hex() + "=" + initialBalance.String(),
		"--pqc-key", sender.Hex() + "=" + plutotx.HashPQPublicKey(pqPublicKey).Hex(),
	}); err != nil {
		t.Fatalf("init PQC node: %v", err)
	}

	node := startEthereumRPCNode(t, homeDir)
	defer node.stop(t)

	ethereumRaw := signedEthereumTransfer(t, ecdsaPrivateKey, receiver, 0, transferValue, projectconfig.DefaultEVMChainID)
	hybridRaw, err := plutotx.CreateSignedHybridEnvelopeV1(
		ethereumRaw,
		pqPublicKey,
		pqPrivateKey,
		big.NewInt(projectconfig.DefaultEVMChainID),
		scheme,
	)
	if err != nil {
		t.Fatalf("create hybrid envelope: %v", err)
	}
	var transactionHash common.Hash
	if err := node.client.CallContext(context.Background(), &transactionHash, "pluto_sendHybridTransaction", hexutil.Bytes(hybridRaw)); err != nil {
		t.Fatalf("pluto_sendHybridTransaction: %v", err)
	}
	if transactionHash == (common.Hash{}) {
		t.Fatal("hybrid JSON-RPC returned empty Ethereum transaction hash")
	}

	wantSender := new(big.Int).Sub(new(big.Int).Set(initialBalance), transferValue)
	assertEthereumRPCAccount(t, node.client, sender, wantSender, 1)
	assertEthereumRPCAccount(t, node.client, receiver, transferValue, 0)
}

// TestPQCOptInPolicyRunsMetaMaskAndHybridTogether là bằng chứng tích hợp quan
// trọng nhất: cùng một chain nhận MetaMask tx của account thường, bắt account
// đã opt-in dùng PQC, rồi vẫn thực thi hybrid transaction thành công.
func TestPQCOptInPolicyRunsMetaMaskAndHybridTogether(t *testing.T) {
	metaMaskKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate MetaMask account: %v", err)
	}
	pqcECDSAKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate PQC ECDSA account: %v", err)
	}
	metaMaskSender := crypto.PubkeyToAddress(metaMaskKey.PublicKey)
	pqcSender := crypto.PubkeyToAddress(pqcECDSAKey.PublicKey)
	receiver := common.HexToAddress("0xc300000000000000000000000000000000000003")
	initialBalance := big.NewInt(1_000_000)
	scheme := pqc.NewMLDSA65()
	pqPublicKey, pqPrivateKey, err := scheme.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate ML-DSA-65 key: %v", err)
	}

	homeDir := t.TempDir()
	if err := runInit([]string{
		"--home", homeDir,
		"--chain-id", "pluto-pqc-opt-in-e2e-1",
		"--tx-policy", projectconfig.TransactionPolicyPQCOptInMLDSA65,
		"--alloc", metaMaskSender.Hex() + "=" + initialBalance.String(),
		"--alloc", pqcSender.Hex() + "=" + initialBalance.String(),
		"--pqc-key", pqcSender.Hex() + "=" + plutotx.HashPQPublicKey(pqPublicKey).Hex(),
	}); err != nil {
		t.Fatalf("init PQC opt-in node: %v", err)
	}

	node := startEthereumRPCNode(t, homeDir)
	defer node.stop(t)

	// Account thường gửi raw transaction đúng chuẩn MetaMask.
	metaMaskRaw := signedEthereumTransfer(t, metaMaskKey, receiver, 0, big.NewInt(100), projectconfig.DefaultEVMChainID)
	var metaMaskHash common.Hash
	if err := node.client.CallContext(context.Background(), &metaMaskHash, "eth_sendRawTransaction", hexutil.Bytes(metaMaskRaw)); err != nil {
		t.Fatalf("MetaMask transaction on opt-in chain: %v", err)
	}

	// Account đã opt-in không được phép bỏ chữ ký PQC.
	pqcEthereumRaw := signedEthereumTransfer(t, pqcECDSAKey, receiver, 0, big.NewInt(200), projectconfig.DefaultEVMChainID)
	var rejectedHash common.Hash
	if err := node.client.CallContext(context.Background(), &rejectedHash, "eth_sendRawTransaction", hexutil.Bytes(pqcEthereumRaw)); err == nil {
		t.Fatal("registered PQC account unexpectedly downgraded to ECDSA-only")
	}

	hybridRaw, err := plutotx.CreateSignedHybridEnvelopeV1(
		pqcEthereumRaw,
		pqPublicKey,
		pqPrivateKey,
		big.NewInt(projectconfig.DefaultEVMChainID),
		scheme,
	)
	if err != nil {
		t.Fatalf("create opt-in hybrid envelope: %v", err)
	}
	var hybridHash common.Hash
	if err := node.client.CallContext(context.Background(), &hybridHash, "pluto_sendHybridTransaction", hexutil.Bytes(hybridRaw)); err != nil {
		t.Fatalf("registered PQC hybrid transaction: %v", err)
	}

	assertEthereumRPCAccount(t, node.client, metaMaskSender, new(big.Int).Sub(new(big.Int).Set(initialBalance), big.NewInt(100)), 1)
	assertEthereumRPCAccount(t, node.client, pqcSender, new(big.Int).Sub(new(big.Int).Set(initialBalance), big.NewInt(200)), 1)
	assertEthereumRPCAccount(t, node.client, receiver, big.NewInt(300), 0)
}

// TestPQCCompanionProxyLetsRegisteredAccountUseMetaMask chứng minh UX cuối:
// MetaMask vẫn gọi eth_sendRawTransaction, companion proxy tự bổ sung ML-DSA-65
// và account đã opt-in được node chấp nhận mà không hạ policy bảo mật.
func TestPQCCompanionProxyLetsRegisteredAccountUseMetaMask(t *testing.T) {
	ecdsaPrivateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate MetaMask ECDSA key: %v", err)
	}
	sender := crypto.PubkeyToAddress(ecdsaPrivateKey.PublicKey)
	receiver := common.HexToAddress("0xc400000000000000000000000000000000000004")
	initialBalance := big.NewInt(1_000_000)
	transferValue := big.NewInt(4_321)
	scheme := pqc.NewMLDSA65()
	publicKey, privateKey, err := scheme.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate ML-DSA-65 key: %v", err)
	}

	homeDir := t.TempDir()
	if err := runInit([]string{
		"--home", homeDir,
		"--chain-id", "pluto-pqc-proxy-e2e-1",
		"--tx-policy", projectconfig.TransactionPolicyPQCOptInMLDSA65,
		"--alloc", sender.Hex() + "=" + initialBalance.String(),
		"--pqc-key", sender.Hex() + "=" + plutotx.HashPQPublicKey(publicKey).Hex(),
	}); err != nil {
		t.Fatalf("init PQC companion E2E node: %v", err)
	}
	node := startEthereumRPCNode(t, homeDir)
	defer node.stop(t)

	proxyServer, err := pqcproxy.NewServer(pqcproxy.Config{
		UpstreamURL: node.endpoint,
		Account:     sender,
		ChainID:     projectconfig.DefaultEVMChainID,
		PublicKey:   publicKey,
		PrivateKey:  privateKey,
	})
	if err != nil {
		t.Fatalf("create PQC companion proxy: %v", err)
	}
	if err := proxyServer.Start(unusedTCPAddress(t)); err != nil {
		t.Fatalf("start PQC companion proxy: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := proxyServer.Close(shutdownCtx); err != nil {
			t.Errorf("close PQC companion proxy: %v", err)
		}
	}()
	proxyClient, err := gethrpc.DialHTTP("http://" + proxyServer.Addr().String())
	if err != nil {
		t.Fatalf("connect MetaMask-style client to PQC proxy: %v", err)
	}
	defer proxyClient.Close()

	// MetaMask thường chọn EIP-1559/type-2 khi block RPC có baseFeePerGas.
	raw := signedEthereumDynamicFeeTransfer(t, ecdsaPrivateKey, receiver, 0, transferValue, projectconfig.DefaultEVMChainID)
	var transactionHash common.Hash
	if err := proxyClient.CallContext(context.Background(), &transactionHash, "eth_sendRawTransaction", hexutil.Bytes(raw)); err != nil {
		t.Fatalf("MetaMask eth_sendRawTransaction through PQC proxy: %v", err)
	}
	ethereumTx := new(gethtypes.Transaction)
	if err := ethereumTx.UnmarshalBinary(raw); err != nil {
		t.Fatal(err)
	}
	if transactionHash != ethereumTx.Hash() {
		t.Fatalf("returned hash = %s, want Ethereum hash %s", transactionHash, ethereumTx.Hash())
	}

	assertEthereumRPCAccount(t, proxyClient, sender, new(big.Int).Sub(new(big.Int).Set(initialBalance), transferValue), 1)
	assertEthereumRPCAccount(t, proxyClient, receiver, transferValue, 0)
}

type ethereumRPCNode struct {
	client   *gethrpc.Client
	endpoint string
	cancel   context.CancelFunc
	done     chan error
	once     sync.Once
}

func startEthereumRPCNode(t *testing.T, homeDir string) *ethereumRPCNode {
	t.Helper()
	ethAddress := unusedTCPAddress(t)
	rpcAddress := unusedTCPAddress(t)
	p2pAddress := unusedTCPAddress(t)
	ctx, cancel := context.WithCancel(context.Background())
	node := &ethereumRPCNode{cancel: cancel, done: make(chan error, 1)}
	go func() {
		node.done <- startNodeWithRuntimeConfig(ctx, homeDir, func(cfg *config.Config) {
			cfg.RPC.ListenAddress = "tcp://" + rpcAddress
			cfg.P2P.ListenAddress = "tcp://" + p2pAddress
			cfg.TxIndex.Indexer = "null"
		}, ethAddress)
	}()

	deadline := time.Now().Add(e2eNodeTimeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-node.done:
			t.Fatalf("node stopped before Ethereum JSON-RPC became ready: %v", err)
		default:
		}
		client, err := gethrpc.DialHTTP("http://" + ethAddress)
		if err == nil {
			var chainID hexutil.Uint64
			callCtx, callCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			err = client.CallContext(callCtx, &chainID, "eth_chainId")
			callCancel()
			if err == nil {
				node.client = client
				node.endpoint = "http://" + ethAddress
				return node
			}
			client.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	node.stop(t)
	t.Fatal("Ethereum JSON-RPC did not become ready before timeout")
	return nil
}

func (node *ethereumRPCNode) stop(t *testing.T) {
	t.Helper()
	node.once.Do(func() {
		if node.client != nil {
			node.client.Close()
		}
		node.cancel()
		select {
		case err := <-node.done:
			if err != nil {
				t.Errorf("stop Ethereum RPC node: %v", err)
			}
		case <-time.After(e2eNodeTimeout):
			t.Error("Ethereum RPC node did not stop before timeout")
		}
	})
}

func assertEthereumRPCAccount(t *testing.T, client *gethrpc.Client, address common.Address, balance *big.Int, nonce uint64) {
	t.Helper()
	var gotBalance hexutil.Big
	if err := client.CallContext(context.Background(), &gotBalance, "eth_getBalance", address, "latest"); err != nil {
		t.Fatalf("eth_getBalance(%s): %v", address.Hex(), err)
	}
	if (*big.Int)(&gotBalance).Cmp(balance) != 0 {
		t.Fatalf("balance %s = %s, want %s", address.Hex(), (*big.Int)(&gotBalance), balance)
	}
	var gotNonce hexutil.Uint64
	if err := client.CallContext(context.Background(), &gotNonce, "eth_getTransactionCount", address, "latest"); err != nil {
		t.Fatalf("eth_getTransactionCount(%s): %v", address.Hex(), err)
	}
	if uint64(gotNonce) != nonce {
		t.Fatalf("nonce %s = %d, want %d", address.Hex(), gotNonce, nonce)
	}
}
