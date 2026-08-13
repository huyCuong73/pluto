package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	dbm "github.com/cometbft/cometbft-db"
	cfg "github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/crypto"
	"github.com/cometbft/cometbft/crypto/ed25519"
	cmtlog "github.com/cometbft/cometbft/libs/log"
	cmtnode "github.com/cometbft/cometbft/node"
	cmtp2p "github.com/cometbft/cometbft/p2p"
	cmtprivval "github.com/cometbft/cometbft/privval"
	cmtproxy "github.com/cometbft/cometbft/proxy"

	"github.com/huyCuong73/pluto/internal/app"
	projectconfig "github.com/huyCuong73/pluto/internal/config"
	"github.com/huyCuong73/pluto/internal/store"
	plutotx "github.com/huyCuong73/pluto/internal/tx"
)

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
	case "help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q (expected init or start)", command)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  plutod init  [--home PATH] [--chain-id ID] [--alloc ADDRESS=WEI]")
	fmt.Fprintln(os.Stderr, "  plutod start [--home PATH]")
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
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected start arguments: %v", flags.Args())
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	return startNode(ctx, *homeDir)
}

func startNode(ctx context.Context, homeDir string) error {
	return startNodeWithConfig(ctx, homeDir, nil)
}

func startNodeWithConfig(ctx context.Context, homeDir string, configure func(*cfg.Config)) error {
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

	transactionValidator, err := plutotx.NewECDSAValidator(projectconfig.DefaultEVMChainID)
	if err != nil {
		return fmt.Errorf("create ECDSA transaction validator: %w", err)
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

	logger.Info("Node Started", "home", homeDir, "evm_chain_id", projectconfig.DefaultEVMChainID)
	<-ctx.Done()
	logger.Info("Stopping Node...")
	if err := node.Stop(); err != nil {
		return fmt.Errorf("stop node: %w", err)
	}
	node.Wait()
	return nil
}
