package main

import (
	"context"
	"crypto/ecdsa"
	"math/big"
	"testing"

	cmttypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/huyCuong73/pluto/internal/app"
	projectconfig "github.com/huyCuong73/pluto/internal/config"
	"github.com/huyCuong73/pluto/internal/pqc"
	plutotx "github.com/huyCuong73/pluto/internal/tx"
)

// TestHybridMLDSA65TransactionThroughRPCPersistsAfterRestart is the complete
// phase-one proof: genesis key binding -> hybrid validation in every ABCI
// phase -> EVM state transition -> persistent query after restart.
func TestHybridMLDSA65TransactionThroughRPCPersistsAfterRestart(t *testing.T) {
	ecdsaPrivateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	sender := crypto.PubkeyToAddress(ecdsaPrivateKey.PublicKey)
	receiver := common.HexToAddress("0x9100000000000000000000000000000000000009")
	initialBalance := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	transferValue := big.NewInt(987_654_321)

	scheme := pqc.NewMLDSA65()
	pqPublicKey, pqPrivateKey, err := scheme.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate ML-DSA-65 key: %v", err)
	}
	pqKeyHash := plutotx.HashPQPublicKey(pqPublicKey)

	homeDir := t.TempDir()
	if err := runInit([]string{
		"--home", homeDir,
		"--chain-id", "pluto-hybrid-e2e-1",
		"--tx-policy", projectconfig.TransactionPolicyHybridMLDSA65,
		"--alloc", sender.Hex() + "=" + initialBalance.String(),
		"--pqc-key", sender.Hex() + "=" + pqKeyHash.Hex(),
	}); err != nil {
		t.Fatalf("init hybrid node: %v", err)
	}

	ethereumRaw := signedEthereumTransfer(t, ecdsaPrivateKey, receiver, 0, transferValue, projectconfig.DefaultEVMChainID)
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

	firstNode := startRPCNode(t, homeDir)

	// ECDSA-only bytes are not silently accepted when genesis requires hybrid.
	assertCheckTxCode(t, firstNode, ethereumRaw, app.CodeInvalidEncoding)

	// A canonical envelope with one changed signature byte reaches the hybrid
	// validator and is classified as a PQC failure before execution.
	tamperedSignature := mutateHybridEnvelope(t, hybridRaw, func(envelope *plutotx.HybridEnvelopeV1) {
		envelope.PQSignature[0] ^= 0x01
	})
	assertCheckTxCode(t, firstNode, tamperedSignature, app.CodeInvalidPQC)

	// A valid signature made by an unregistered key still fails key binding.
	otherPublicKey, otherPrivateKey, err := scheme.GenerateKey(nil)
	if err != nil {
		firstNode.stop(t)
		t.Fatalf("generate unregistered ML-DSA key: %v", err)
	}
	unregisteredEnvelope, err := plutotx.CreateSignedHybridEnvelopeV1(
		ethereumRaw,
		otherPublicKey,
		otherPrivateKey,
		big.NewInt(projectconfig.DefaultEVMChainID),
		scheme,
	)
	if err != nil {
		firstNode.stop(t)
		t.Fatalf("create unregistered-key envelope: %v", err)
	}
	assertCheckTxCode(t, firstNode, unregisteredEnvelope, app.CodeInvalidPQC)

	// The ML-DSA signature binds the complete signed Ethereum transaction.
	modifiedEthereumTx := signedEthereumTransfer(t, ecdsaPrivateKey, receiver, 0, new(big.Int).Add(transferValue, big.NewInt(1)), projectconfig.DefaultEVMChainID)
	tamperedTransaction := mutateHybridEnvelope(t, hybridRaw, func(envelope *plutotx.HybridEnvelopeV1) {
		envelope.EthereumTx = modifiedEthereumTx
	})
	assertCheckTxCode(t, firstNode, tamperedTransaction, app.CodeInvalidPQC)

	// Truncated RLP is an encoding failure and must not panic the node.
	assertCheckTxCode(t, firstNode, hybridRaw[:len(hybridRaw)-1], app.CodeInvalidEncoding)
	assertCheckTxCode(t, firstNode, make([]byte, plutotx.MaxHybridEnvelopeSize+1), app.CodeInvalidEncoding)

	// Even a valid ML-DSA signature cannot rescue an Ethereum transaction that
	// was ECDSA-signed for another chain.
	wrongChainEthereumTx := signedEthereumTransfer(t, ecdsaPrivateKey, receiver, 0, transferValue, 1)
	wrongChainEnvelope, err := plutotx.CreateSignedHybridEnvelopeV1(
		wrongChainEthereumTx,
		pqPublicKey,
		pqPrivateKey,
		big.NewInt(projectconfig.DefaultEVMChainID),
		scheme,
	)
	if err != nil {
		firstNode.stop(t)
		t.Fatalf("create wrong-chain envelope: %v", err)
	}
	assertCheckTxCode(t, firstNode, wrongChainEnvelope, app.CodeInvalidSignature)

	result, err := firstNode.client.BroadcastTxCommit(context.Background(), cmttypes.Tx(hybridRaw))
	if err != nil {
		firstNode.stop(t)
		t.Fatalf("broadcast hybrid transaction: %v", err)
	}
	if result.CheckTx.Code != app.CodeOK || result.TxResult.Code != app.CodeOK {
		firstNode.stop(t)
		t.Fatalf(
			"hybrid result CheckTx=%d FinalizeBlock=%d logs=(%q, %q)",
			result.CheckTx.Code,
			result.TxResult.Code,
			result.CheckTx.Log,
			result.TxResult.Log,
		)
	}

	wantSenderBalance := new(big.Int).Sub(new(big.Int).Set(initialBalance), transferValue)
	assertRPCAccountState(t, firstNode.client, sender, wantSenderBalance, 1)
	assertRPCAccountState(t, firstNode.client, receiver, transferValue, 0)

	// A different transaction with the already-consumed nonce passes stateless
	// authentication, then is rejected by FinalizeBlock's stateful nonce check.
	replayEthereumTx := signedEthereumTransfer(t, ecdsaPrivateKey, receiver, 0, big.NewInt(1), projectconfig.DefaultEVMChainID)
	replayEnvelope, err := plutotx.CreateSignedHybridEnvelopeV1(
		replayEthereumTx,
		pqPublicKey,
		pqPrivateKey,
		big.NewInt(projectconfig.DefaultEVMChainID),
		scheme,
	)
	if err != nil {
		firstNode.stop(t)
		t.Fatalf("create replay envelope: %v", err)
	}
	replay, err := firstNode.client.BroadcastTxCommit(context.Background(), cmttypes.Tx(replayEnvelope))
	if err != nil {
		firstNode.stop(t)
		t.Fatalf("broadcast replay transaction: %v", err)
	}
	if replay.CheckTx.Code != app.CodeOK || replay.TxResult.Code != app.CodeNonceMismatch {
		firstNode.stop(t)
		t.Fatalf("replay codes = CheckTx:%d FinalizeBlock:%d", replay.CheckTx.Code, replay.TxResult.Code)
	}
	assertRPCAccountState(t, firstNode.client, sender, wantSenderBalance, 1)
	assertRPCAccountState(t, firstNode.client, receiver, transferValue, 0)
	firstNode.stop(t)

	restartedNode := startRPCNode(t, homeDir)
	defer restartedNode.stop(t)
	assertRPCAccountState(t, restartedNode.client, sender, wantSenderBalance, 1)
	assertRPCAccountState(t, restartedNode.client, receiver, transferValue, 0)
}

func signedEthereumTransfer(
	t *testing.T,
	privateKey *ecdsa.PrivateKey,
	receiver common.Address,
	nonce uint64,
	value *big.Int,
	chainID int64,
) []byte {
	t.Helper()
	unsigned := gethtypes.NewTx(&gethtypes.LegacyTx{
		Nonce:    nonce,
		GasPrice: big.NewInt(0),
		Gas:      21_000,
		To:       &receiver,
		Value:    new(big.Int).Set(value),
	})
	signed, err := gethtypes.SignTx(unsigned, gethtypes.LatestSignerForChainID(big.NewInt(chainID)), privateKey)
	if err != nil {
		t.Fatalf("sign Ethereum transaction: %v", err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal Ethereum transaction: %v", err)
	}
	return raw
}

// signedEthereumDynamicFeeTransfer mô phỏng transaction type 2 mà MetaMask
// thường tạo khi RPC block có baseFeePerGas.
func signedEthereumDynamicFeeTransfer(
	t *testing.T,
	privateKey *ecdsa.PrivateKey,
	receiver common.Address,
	nonce uint64,
	value *big.Int,
	chainID int64,
) []byte {
	t.Helper()
	configuredChainID := big.NewInt(chainID)
	unsigned := gethtypes.NewTx(&gethtypes.DynamicFeeTx{
		ChainID:   configuredChainID,
		Nonce:     nonce,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1),
		Gas:       21_000,
		To:        &receiver,
		Value:     new(big.Int).Set(value),
	})
	signed, err := gethtypes.SignTx(unsigned, gethtypes.LatestSignerForChainID(configuredChainID), privateKey)
	if err != nil {
		t.Fatalf("sign dynamic-fee Ethereum transaction: %v", err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal dynamic-fee Ethereum transaction: %v", err)
	}
	return raw
}

func mutateHybridEnvelope(t *testing.T, raw []byte, mutate func(*plutotx.HybridEnvelopeV1)) []byte {
	t.Helper()
	envelope, err := plutotx.DecodeHybridEnvelopeV1(raw)
	if err != nil {
		t.Fatalf("decode hybrid fixture: %v", err)
	}
	mutate(envelope)
	encoded, err := plutotx.EncodeHybridEnvelopeV1(envelope)
	if err != nil {
		t.Fatalf("encode mutated hybrid fixture: %v", err)
	}
	return encoded
}

func assertCheckTxCode(t *testing.T, node *rpcTestNode, raw []byte, want uint32) {
	t.Helper()
	result, err := node.client.CheckTx(context.Background(), cmttypes.Tx(raw))
	if err != nil {
		node.stop(t)
		t.Fatalf("CheckTx through RPC: %v", err)
	}
	if result.Code != want {
		node.stop(t)
		t.Fatalf("CheckTx code = %d, want %d, log = %q", result.Code, want, result.Log)
	}
}
