package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	cfg "github.com/cometbft/cometbft/config"
	cmtprivval "github.com/cometbft/cometbft/privval"
	cmttypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/huyCuong73/pluto/internal/app"
	projectconfig "github.com/huyCuong73/pluto/internal/config"
	plutotx "github.com/huyCuong73/pluto/internal/tx"
)

func TestRunInitCreatesMatchingValidatorAndApplicationGenesis(t *testing.T) {
	homeDir := filepath.Join(t.TempDir(), "node")
	address := common.HexToAddress("0x5000000000000000000000000000000000000005")
	if err := runInit([]string{
		"--home", homeDir,
		"--chain-id", "pluto-test-1",
		"--alloc", address.Hex() + "=123456",
	}); err != nil {
		t.Fatalf("init home: %v", err)
	}

	config := cfg.DefaultConfig().SetRoot(homeDir)
	genesis, err := cmttypes.GenesisDocFromFile(config.GenesisFile())
	if err != nil {
		t.Fatalf("read genesis: %v", err)
	}
	if genesis.ChainID != "pluto-test-1" {
		t.Fatalf("chain ID = %q, want pluto-test-1", genesis.ChainID)
	}
	if len(genesis.Validators) != 1 {
		t.Fatalf("validator count = %d, want 1", len(genesis.Validators))
	}

	pv := cmtprivval.LoadFilePV(config.PrivValidatorKeyFile(), config.PrivValidatorStateFile())
	pubKey, err := pv.GetPubKey()
	if err != nil {
		t.Fatalf("read private validator public key: %v", err)
	}
	if !bytes.Equal(genesis.Validators[0].PubKey.Bytes(), pubKey.Bytes()) {
		t.Fatal("genesis validator does not match private validator key")
	}

	var state app.GenesisState
	if err := json.Unmarshal(genesis.AppState, &state); err != nil {
		t.Fatalf("decode app state: %v", err)
	}
	if state.EVMChainID != projectconfig.DefaultEVMChainID {
		t.Fatalf("EVM chain ID = %d, want %d", state.EVMChainID, projectconfig.DefaultEVMChainID)
	}
	if state.TransactionPolicy != projectconfig.TransactionPolicyECDSA {
		t.Fatalf("transaction policy = %q, want %q", state.TransactionPolicy, projectconfig.TransactionPolicyECDSA)
	}
	account, ok := state.Alloc[address.Hex()]
	if !ok || account.Balance != "123456" {
		t.Fatalf("genesis allocation = %#v, present=%v", account, ok)
	}

	if err := runInit([]string{"--home", homeDir}); err == nil {
		t.Fatal("second init unexpectedly overwrote existing genesis")
	}
}

func TestRunInitCreatesHybridGenesisAndValidatorPipeline(t *testing.T) {
	homeDir := filepath.Join(t.TempDir(), "hybrid-node")
	address := common.HexToAddress("0x6000000000000000000000000000000000000006")
	keyHash := strings.Repeat("ab", plutotx.PQKeyHashSize)
	if err := runInit([]string{
		"--home", homeDir,
		"--chain-id", "pluto-hybrid-test-1",
		"--tx-policy", projectconfig.TransactionPolicyHybridMLDSA65,
		"--alloc", address.Hex() + "=123456",
		"--pqc-key", address.Hex() + "=" + keyHash,
	}); err != nil {
		t.Fatalf("init hybrid home: %v", err)
	}

	config := cfg.DefaultConfig().SetRoot(homeDir)
	genesis, err := cmttypes.GenesisDocFromFile(config.GenesisFile())
	if err != nil {
		t.Fatalf("read hybrid genesis: %v", err)
	}
	var state app.GenesisState
	if err := json.Unmarshal(genesis.AppState, &state); err != nil {
		t.Fatalf("decode hybrid app state: %v", err)
	}
	if state.TransactionPolicy != projectconfig.TransactionPolicyHybridMLDSA65 {
		t.Fatalf("transaction policy = %q", state.TransactionPolicy)
	}
	if got := state.PQCKeys[address.Hex()]; got != keyHash {
		t.Fatalf("PQC key hash = %q, want %q", got, keyHash)
	}

	validator, policy, err := transactionValidatorFromGenesis(config.GenesisFile())
	if err != nil {
		t.Fatalf("compose transaction validator: %v", err)
	}
	if policy != projectconfig.TransactionPolicyHybridMLDSA65 {
		t.Fatalf("composed policy = %q", policy)
	}
	if _, ok := validator.(*plutotx.HybridValidator); !ok {
		t.Fatalf("validator type = %T, want *tx.HybridValidator", validator)
	}
}

func TestRunInitRejectsHybridPolicyWithoutKeyBinding(t *testing.T) {
	err := runInit([]string{
		"--home", filepath.Join(t.TempDir(), "invalid-hybrid"),
		"--tx-policy", projectconfig.TransactionPolicyHybridMLDSA65,
	})
	if err == nil || !strings.Contains(err.Error(), "requires at least one --pqc-key") {
		t.Fatalf("error = %v, want missing PQ key binding", err)
	}
}
