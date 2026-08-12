package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	cfg "github.com/cometbft/cometbft/config"
	cmtprivval "github.com/cometbft/cometbft/privval"
	cmttypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/huyCuong73/pluto/internal/app"
	projectconfig "github.com/huyCuong73/pluto/internal/config"
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
	account, ok := state.Alloc[address.Hex()]
	if !ok || account.Balance != "123456" {
		t.Fatalf("genesis allocation = %#v, present=%v", account, ok)
	}

	if err := runInit([]string{"--home", homeDir}); err == nil {
		t.Fatal("second init unexpectedly overwrote existing genesis")
	}
}
