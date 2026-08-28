package main

import (
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	projectconfig "github.com/huyCuong73/pluto/internal/config"
	"github.com/huyCuong73/pluto/internal/pqc"
	plutotx "github.com/huyCuong73/pluto/internal/tx"
)

func TestPQCKeygenAndWrapCommands(t *testing.T) {
	directory := t.TempDir()
	publicKeyPath := filepath.Join(directory, "wallet.pq.pub")
	privateKeyPath := filepath.Join(directory, "wallet.pq.key")
	if err := runPQCKeygen([]string{"--public-key", publicKeyPath, "--private-key", privateKeyPath}); err != nil {
		t.Fatalf("pqc-keygen: %v", err)
	}
	publicKey, err := os.ReadFile(publicKeyPath)
	if err != nil {
		t.Fatalf("read generated public key: %v", err)
	}
	privateKey, err := os.ReadFile(privateKeyPath)
	if err != nil {
		t.Fatalf("read generated private key: %v", err)
	}
	if len(publicKey) != pqc.MLDSA65PublicKeySize || len(privateKey) != pqc.MLDSA65PrivateKeySize {
		t.Fatalf("generated key sizes = (%d, %d)", len(publicKey), len(privateKey))
	}
	// Không được ghi đè key hiện hữu.
	if err := runPQCKeygen([]string{"--public-key", publicKeyPath, "--private-key", privateKeyPath}); err == nil {
		t.Fatal("second pqc-keygen unexpectedly overwrote existing keys")
	}

	ecdsaKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	sender := crypto.PubkeyToAddress(ecdsaKey.PublicKey)
	receiver := common.HexToAddress("0xe100000000000000000000000000000000000001")
	ethereumRaw := signedEthereumTransfer(t, ecdsaKey, receiver, 0, big.NewInt(1), projectconfig.DefaultEVMChainID)
	outputPath := filepath.Join(directory, "transaction.hybrid")
	if err := runPQCWrap([]string{
		"--ethereum-tx", "0x" + common.Bytes2Hex(ethereumRaw),
		"--public-key", publicKeyPath,
		"--private-key", privateKeyPath,
		"--out", outputPath,
	}); err != nil {
		t.Fatalf("pqc-wrap: %v", err)
	}
	hybridRaw, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read hybrid output: %v", err)
	}

	ecdsaValidator, err := plutotx.NewECDSAValidator(projectconfig.DefaultEVMChainID)
	if err != nil {
		t.Fatalf("create ECDSA validator: %v", err)
	}
	registry := plutotx.NewStaticPQKeyRegistry(map[common.Address]plutotx.PQKeyHash{
		sender: plutotx.HashPQPublicKey(publicKey),
	})
	hybridValidator, err := plutotx.NewHybridValidator(
		projectconfig.DefaultEVMChainID,
		ecdsaValidator,
		pqc.NewMLDSA65(),
		registry,
	)
	if err != nil {
		t.Fatalf("create hybrid validator: %v", err)
	}
	if _, err := hybridValidator.Validate(hybridRaw); err != nil {
		t.Fatalf("CLI-created hybrid envelope failed validation: %v", err)
	}
}

func TestReadProtectedPrivateKeyRequiresOwnerOnlyPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wallet.pq.key")
	if err := os.WriteFile(path, []byte("secret-test-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readProtectedPrivateKey(path); err == nil {
		t.Fatal("private key có quyền 0644 unexpectedly accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := readProtectedPrivateKey(path)
	if err != nil {
		t.Fatalf("private key quyền 0600 bị từ chối: %v", err)
	}
	clear(data)
}
