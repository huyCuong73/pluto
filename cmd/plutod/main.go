package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	dbm "github.com/cometbft/cometbft-db"
	cfg "github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/crypto"
	"github.com/cometbft/cometbft/crypto/ed25519"
	cmtlog "github.com/cometbft/cometbft/libs/log"
	cmtnode "github.com/cometbft/cometbft/node"
	cmtp2p "github.com/cometbft/cometbft/p2p"
	cmtprivval "github.com/cometbft/cometbft/privval"
	cmtproxy "github.com/cometbft/cometbft/proxy"
	rpclocal "github.com/cometbft/cometbft/rpc/client/local"
	cmttypes "github.com/cometbft/cometbft/types"

	"github.com/huyCuong73/pluto/internal/app"
	projectconfig "github.com/huyCuong73/pluto/internal/config"
	"github.com/huyCuong73/pluto/internal/ethrpc"
	"github.com/huyCuong73/pluto/internal/pqc"
	"github.com/huyCuong73/pluto/internal/store"
	plutotx "github.com/huyCuong73/pluto/internal/tx"
)

const DefaultEthereumRPCListenAddress = "127.0.0.1:8545"

func PebbleDBProvider(ctx *cfg.DBContext) (dbm.DB, error) {
	return store.NewPebbleDB(ctx.ID, ctx.Config.DBDir())
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "plutod:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "start"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}

	switch command {
	case "init":
		return runInit(args)
	case "start":
		return runStart(args)
	case "pqc-keygen":
		return runPQCKeygen(args)
	case "pqc-wrap":
		return runPQCWrap(args)
	case "pqc-proxy":
		return runPQCProxy(args)
	case "help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q (expected init, start, pqc-keygen, pqc-wrap, pqc-proxy or help)", command)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  plutod init  [--home PATH] [--chain-id ID] [--tx-policy POLICY] [--alloc ADDRESS=WEI] [--pqc-key ADDRESS=SHA256_HEX]")
	fmt.Fprintln(os.Stderr, "  plutod start [--home PATH] [--eth-rpc ADDRESS]")
	fmt.Fprintln(os.Stderr, "  plutod pqc-keygen [--public-key FILE] [--private-key FILE]")
	fmt.Fprintln(os.Stderr, "  plutod pqc-wrap --ethereum-tx 0xHEX [--public-key FILE] [--private-key FILE] [--out FILE]")
	fmt.Fprintln(os.Stderr, "  plutod pqc-proxy --account ADDRESS [--listen ADDRESS] [--upstream URL] [--public-key FILE] [--private-key FILE]")
}

func defaultHomeDir() string {
	userHome, err := os.UserHomeDir()
	if err != nil || userHome == "" {
		return ".plutod"
	}
	return filepath.Join(userHome, ".plutod")
}

func runStart(args []string) error {
	flags := flag.NewFlagSet("start", flag.ContinueOnError)
	homeDir := flags.String("home", defaultHomeDir(), "path to the Pluto home directory")
	ethRPCAddress := flags.String("eth-rpc", DefaultEthereumRPCListenAddress, "Ethereum JSON-RPC listen address; empty disables it")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected start arguments: %v", flags.Args())
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	return startNodeWithRuntimeConfig(ctx, *homeDir, nil, *ethRPCAddress)
}

func startNode(ctx context.Context, homeDir string) error {
	return startNodeWithRuntimeConfig(ctx, homeDir, nil, DefaultEthereumRPCListenAddress)
}

// startNodeWithConfig được giữ cho test CometBFT cũ. Các test đó tự quản lý
// port nên không tự mở thêm 8545 để tránh xung đột khi chạy song song.
func startNodeWithConfig(ctx context.Context, homeDir string, configure func(*cfg.Config)) error {
	return startNodeWithRuntimeConfig(ctx, homeDir, configure, "")
}

func startNodeWithRuntimeConfig(ctx context.Context, homeDir string, configure func(*cfg.Config), ethRPCAddress string) error {
	config := cfg.DefaultConfig().SetRoot(homeDir)
	if configure != nil {
		configure(config)
	}
	if _, err := os.Stat(config.GenesisFile()); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("genesis not found at %s; run 'plutod init --home %s' first", config.GenesisFile(), homeDir)
		}
		return fmt.Errorf("inspect genesis file: %w", err)
	}

	for _, dir := range []string{config.DBDir(), filepath.Dir(config.NodeKeyFile()), filepath.Dir(config.PrivValidatorStateFile())} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
	}

	logger := cmtlog.NewTMLogger(cmtlog.NewSyncWriter(os.Stdout)).With("module", "main")
	appLogger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	nodeKey, err := cmtp2p.LoadOrGenNodeKey(config.NodeKeyFile())
	if err != nil {
		return fmt.Errorf("load node key: %w", err)
	}

	pv, err := cmtprivval.LoadOrGenFilePV(
		config.PrivValidatorKeyFile(),
		config.PrivValidatorStateFile(),
		func() (crypto.PrivKey, error) { return ed25519.GenPrivKey(), nil },
	)
	if err != nil {
		return fmt.Errorf("load private validator: %w", err)
	}

	transactionValidator, transactionPolicy, err := transactionValidatorFromGenesis(config.GenesisFile())
	if err != nil {
		return fmt.Errorf("create transaction validator from genesis: %w", err)
	}

	myApp, err := app.NewAppWithComponents(
		filepath.Join(homeDir, "data"),
		appLogger,
		projectconfig.DefaultEVMChainID,
		transactionValidator,
	)
	if err != nil {
		return fmt.Errorf("create application: %w", err)
	}
	defer myApp.Close()

	node, err := cmtnode.NewNode(
		ctx,
		config,
		pv,
		nodeKey,
		cmtproxy.NewLocalClientCreator(myApp),
		cmtnode.DefaultGenesisDocProviderFunc(config),
		PebbleDBProvider,
		cmtnode.DefaultMetricsProvider(config.Instrumentation),
		logger,
	)
	if err != nil {
		return fmt.Errorf("create node: %w", err)
	}
	if err := node.Start(); err != nil {
		return fmt.Errorf("start node: %w", err)
	}

	var ethereumRPC *ethrpc.Server
	if strings.TrimSpace(ethRPCAddress) != "" {
		ethereumRPC, err = ethrpc.NewServer(rpclocal.New(node), projectconfig.DefaultEVMChainID)
		if err != nil {

			node.Stop()
			node.Wait()
			return fmt.Errorf("create Ethereum JSON-RPC server: %w", err)
		}
		if err := ethereumRPC.Start(ethRPCAddress); err != nil {
			node.Stop()
			node.Wait()
			return fmt.Errorf("start Ethereum JSON-RPC server at %s: %w", ethRPCAddress, err)
		}
		logger.Info("Ethereum JSON-RPC Started", "listen", ethereumRPC.Addr().String())
	}

	logger.Info("Node Started", "home", homeDir, "evm_chain_id", projectconfig.DefaultEVMChainID, "transaction_policy", transactionPolicy)
	if ethereumRPC == nil {
		<-ctx.Done()
	} else {
		select {
		case <-ctx.Done():
		case rpcErr := <-ethereumRPC.Done():
			if rpcErr != nil {
				logger.Error("Ethereum JSON-RPC stopped unexpectedly", "error", rpcErr)
			}
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := ethereumRPC.Close(shutdownCtx); err != nil {
			logger.Error("Ethereum JSON-RPC shutdown failed", "error", err)
		}
		cancel()
	}
	logger.Info("Stopping Node...")
	if err := node.Stop(); err != nil {
		return fmt.Errorf("stop node: %w", err)
	}
	node.Wait()
	return nil
}

// transactionValidatorFromGenesis is the cryptographic composition boundary.
// The policy is part of genesis so every validator and every restart assembles
// the same transaction pipeline.
func transactionValidatorFromGenesis(genesisFile string) (plutotx.TransactionValidator, string, error) {
	genesis, err := cmttypes.GenesisDocFromFile(genesisFile)
	if err != nil {
		return nil, "", fmt.Errorf("read genesis: %w", err)
	}
	var genesisState app.GenesisState
	if len(genesis.AppState) > 0 {
		if err := json.Unmarshal(genesis.AppState, &genesisState); err != nil {
			return nil, "", fmt.Errorf("decode application genesis: %w", err)
		}
	}
	if genesisState.EVMChainID != 0 && genesisState.EVMChainID != projectconfig.DefaultEVMChainID {
		return nil, "", fmt.Errorf(
			"genesis EVM chain ID %d does not match configured chain ID %d",
			genesisState.EVMChainID,
			projectconfig.DefaultEVMChainID,
		)
	}
	policy, err := projectconfig.NormalizeTransactionPolicy(genesisState.TransactionPolicy)
	if err != nil {
		return nil, "", err
	}
	ethereumValidator, err := plutotx.NewECDSAValidator(projectconfig.DefaultEVMChainID)
	if err != nil {
		return nil, "", err
	}
	if policy == projectconfig.TransactionPolicyECDSA {
		return ethereumValidator, policy, nil
	}
	if len(genesisState.PQCKeys) == 0 {
		return nil, "", fmt.Errorf("transaction policy %s requires at least one PQ key binding", policy)
	}
	registry, err := plutotx.NewStaticPQKeyRegistryFromHex(genesisState.PQCKeys)
	if err != nil {
		return nil, "", fmt.Errorf("create genesis PQ key registry: %w", err)
	}
	hybridValidator, err := plutotx.NewHybridValidator(
		projectconfig.DefaultEVMChainID,
		ethereumValidator,
		pqc.NewMLDSA65(),
		registry,
	)
	if err != nil {
		return nil, "", err
	}
	if policy == projectconfig.TransactionPolicyPQCOptInMLDSA65 {
		optInValidator, err := plutotx.NewOptInHybridValidator(ethereumValidator, hybridValidator, registry)
		if err != nil {
			return nil, "", err
		}
		return optInValidator, policy, nil
	}
	return hybridValidator, policy, nil
}
