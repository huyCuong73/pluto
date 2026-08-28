package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	cfg "github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/crypto"
	"github.com/cometbft/cometbft/crypto/ed25519"
	cmtp2p "github.com/cometbft/cometbft/p2p"
	cmtprivval "github.com/cometbft/cometbft/privval"
	cmttypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/common"

	"github.com/huyCuong73/pluto/internal/app"
	projectconfig "github.com/huyCuong73/pluto/internal/config"
	plutotx "github.com/huyCuong73/pluto/internal/tx"
)

type allocations map[string]app.GenesisAccount

type pqcKeyBindings map[string]string

func (a *allocations) String() string {
	return fmt.Sprint(map[string]app.GenesisAccount(*a))
}

func (a *allocations) Set(value string) error {
	addressText, balanceText, ok := strings.Cut(value, "=")
	if !ok || !common.IsHexAddress(addressText) {
		return fmt.Errorf("allocation must be ADDRESS=WEI with a valid EVM address")
	}
	balance, ok := new(big.Int).SetString(balanceText, 10)
	if !ok || balance.Sign() < 0 {
		return fmt.Errorf("allocation balance must be a non-negative base-10 integer")
	}
	address := common.HexToAddress(addressText).Hex()
	if _, exists := (*a)[address]; exists {
		return fmt.Errorf("duplicate allocation for %s", address)
	}
	(*a)[address] = app.GenesisAccount{Balance: balance.String()}
	return nil
}

func (b *pqcKeyBindings) String() string {
	return fmt.Sprint(map[string]string(*b))
}

func (b *pqcKeyBindings) Set(value string) error {
	addressText, hashText, ok := strings.Cut(value, "=")
	if !ok || !common.IsHexAddress(addressText) {
		return fmt.Errorf("PQC key binding must be ADDRESS=SHA256_HEX with a valid EVM address")
	}
	hash, err := plutotx.ParsePQKeyHashHex(hashText)
	if err != nil {
		return err
	}
	address := common.HexToAddress(addressText).Hex()
	if _, exists := (*b)[address]; exists {
		return fmt.Errorf("duplicate PQC key binding for %s", address)
	}
	(*b)[address] = hash.Hex()
	return nil
}

func runInit(args []string) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	homeDir := flags.String("home", defaultHomeDir(), "path to the Pluto home directory")
	chainID := flags.String("chain-id", projectconfig.DefaultCometChainID, "CometBFT chain ID")
	transactionPolicy := flags.String("tx-policy", projectconfig.TransactionPolicyECDSA, "transaction policy: ecdsa, hybrid-mldsa65 or pqc-opt-in-mldsa65")
	alloc := allocations{}
	pqcKeys := pqcKeyBindings{}
	flags.Var(&alloc, "alloc", "genesis allocation ADDRESS=WEI; may be repeated")
	flags.Var(&pqcKeys, "pqc-key", "PQC key binding ADDRESS=SHA256_HEX; may be repeated")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected init arguments: %v", flags.Args())
	}
	policy, err := projectconfig.NormalizeTransactionPolicy(*transactionPolicy)
	if err != nil {
		return err
	}
	if (policy == projectconfig.TransactionPolicyHybridMLDSA65 || policy == projectconfig.TransactionPolicyPQCOptInMLDSA65) && len(pqcKeys) == 0 {
		return fmt.Errorf("transaction policy %s requires at least one --pqc-key binding", policy)
	}

	config := cfg.DefaultConfig().SetRoot(*homeDir)
	if _, err := os.Stat(config.GenesisFile()); err == nil {
		return fmt.Errorf("genesis already exists at %s", config.GenesisFile())
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect genesis file: %w", err)
	}

	for _, dir := range []string{
		config.RootDir,
		filepath.Dir(config.GenesisFile()),
		config.DBDir(),
		filepath.Dir(config.NodeKeyFile()),
		filepath.Dir(config.PrivValidatorStateFile()),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
	}

	if _, err := cmtp2p.LoadOrGenNodeKey(config.NodeKeyFile()); err != nil {
		return fmt.Errorf("create node key: %w", err)
	}
	pv, err := cmtprivval.LoadOrGenFilePV(
		config.PrivValidatorKeyFile(),
		config.PrivValidatorStateFile(),
		func() (crypto.PrivKey, error) { return ed25519.GenPrivKey(), nil },
	)
	if err != nil {
		return fmt.Errorf("create private validator: %w", err)
	}
	pubKey, err := pv.GetPubKey()
	if err != nil {
		return fmt.Errorf("read validator public key: %w", err)
	}

	appState, err := json.Marshal(app.GenesisState{
		EVMChainID:        projectconfig.DefaultEVMChainID,
		TransactionPolicy: policy,
		Alloc:             alloc,
		PQCKeys:           pqcKeys,
	})
	if err != nil {
		return fmt.Errorf("encode application genesis: %w", err)
	}

	genesis := &cmttypes.GenesisDoc{
		GenesisTime:   time.Now().UTC(),
		ChainID:       *chainID,
		InitialHeight: 1,
		Validators: []cmttypes.GenesisValidator{{
			Address: pv.GetAddress(),
			PubKey:  pubKey,
			Power:   10,
			Name:    "pluto-validator-0",
		}},
		AppState: appState,
	}
	if err := genesis.ValidateAndComplete(); err != nil {
		return fmt.Errorf("validate genesis: %w", err)
	}
	if err := genesis.SaveAs(config.GenesisFile()); err != nil {
		return fmt.Errorf("write genesis: %w", err)
	}

	fmt.Printf("Initialized Pluto home at %s\n", *homeDir)
	fmt.Printf("CometBFT chain ID: %s\n", genesis.ChainID)
	fmt.Printf("EVM chain ID: %d\n", projectconfig.DefaultEVMChainID)
	fmt.Printf("Transaction policy: %s\n", policy)
	fmt.Printf("Genesis accounts: %d\n", len(alloc))
	fmt.Printf("PQC key bindings: %d\n", len(pqcKeys))
	return nil
}
