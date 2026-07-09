package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	cfg "github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/crypto"
	"github.com/cometbft/cometbft/crypto/ed25519"
	cmtlog "github.com/cometbft/cometbft/libs/log"
	cmtnode "github.com/cometbft/cometbft/node"
	cmtp2p "github.com/cometbft/cometbft/p2p"
	cmtprivval "github.com/cometbft/cometbft/privval"
	cmtproxy "github.com/cometbft/cometbft/proxy"

	dbm "github.com/cometbft/cometbft-db"

	"github.com/huyCuong73/pluto/internal/app"
	"github.com/huyCuong73/pluto/internal/store"
)

var homeDir string

func init() {
	flag.StringVar(&homeDir, "home", "", "Path to the home directory")
}

// Pebble DB provider
func PebbleDBProvider(ctx *cfg.DBContext) (dbm.DB, error) {
	return store.NewPebbleDB(ctx.ID, ctx.Config.DBDir())
}

func main() {
	flag.Parse()
	if homeDir == "" {
		homeDir = os.ExpandEnv("$HOME/.plutod")
	}

	// Setup config
	config := cfg.DefaultConfig()
	config.SetRoot(homeDir)

	// Đảm bảo thư mục tồn tại
	for _, dir := range []string{config.DBDir(), filepath.Dir(config.NodeKeyFile()), filepath.Dir(config.PrivValidatorStateFile())} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			panic(fmt.Errorf("failed to create directory %s: %w", dir, err))
		}
	}

	// Setup Logger (CometBFT)
	logger := cmtlog.NewTMLogger(cmtlog.NewSyncWriter(os.Stdout))
	logger = logger.With("module", "main")

	// Setup Logger cho App (Slog)
	appLogger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// Load Node & Validator Key
	nodeKey, err := cmtp2p.LoadOrGenNodeKey(config.NodeKeyFile())
	if err != nil {
		panic(fmt.Errorf("failed to load node key: %w", err))
	}

	// Load/Gen validator key (CometBFT v1.0.0)
	pv, err := cmtprivval.LoadOrGenFilePV(
		config.PrivValidatorKeyFile(),
		config.PrivValidatorStateFile(),
		func() (crypto.PrivKey, error) {
			return ed25519.GenPrivKey(), nil
		},
	)
	if err != nil {
		panic(fmt.Errorf("failed to load or generate priv validator: %w", err))
	}

	// Khởi tạo App (DB nằm trong data/)
	appDBDir := filepath.Join(homeDir, "data")

	myApp, err := app.NewApp(appDBDir, appLogger)
	if err != nil {
		panic(fmt.Errorf("failed to create app: %w", err))
	}
	defer myApp.Close()

	// Context cho node
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Khởi tạo Node
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
		panic(fmt.Errorf("failed to create node: %v", err))
	}

	// Start Node
	if err := node.Start(); err != nil {
		panic(fmt.Errorf("failed to start node: %v", err))
	}

	logger.Info("Node Started", "home", homeDir)

	// Chờ tín hiệu shutdown
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	logger.Info("Stopping Node...")
	cancel() // Dừng node
	node.Stop()
	node.Wait()
}