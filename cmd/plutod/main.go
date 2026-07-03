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

// PebbleDBProvider: Inject Pebble vào CometBFT Core
// cfg.DBProvider signature: func(*cfg.DBContext) (dbm.DB, error)
func PebbleDBProvider(ctx *cfg.DBContext) (dbm.DB, error) {
	return store.NewPebbleDB(ctx.ID, ctx.Config.DBDir())
}

func main() {
	flag.Parse()
	if homeDir == "" {
		homeDir = os.ExpandEnv("$HOME/.plutod")
	}

	// 1. Setup Config
	config := cfg.DefaultConfig()
	config.SetRoot(homeDir)

	// Ensure required directories exist before loading any files
	for _, dir := range []string{config.DBDir(), filepath.Dir(config.NodeKeyFile()), filepath.Dir(config.PrivValidatorStateFile())} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			panic(fmt.Errorf("failed to create directory %s: %w", dir, err))
		}
	}

	// 2. Setup Logger (CometBFT style)
	logger := cmtlog.NewTMLogger(cmtlog.NewSyncWriter(os.Stdout))
	logger = logger.With("module", "main")

	// Setup Logger cho App (Go Slog standard)
	appLogger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// 3. Load Node Key & Validator Key
	nodeKey, err := cmtp2p.LoadOrGenNodeKey(config.NodeKeyFile())
	if err != nil {
		panic(fmt.Errorf("failed to load node key: %w", err))
	}

	// In CometBFT v1.0.0, LoadOrGenFilePV returns (pv, error) and requires a key generator function
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

	// 4. Khởi tạo Application
	// DB của App nằm riêng trong thư mục data/application.db
	appDBDir := filepath.Join(homeDir, "data")

	myApp, err := app.NewApp(appDBDir, appLogger)
	if err != nil {
		panic(fmt.Errorf("failed to create app: %w", err))
	}
	defer myApp.Close()

	// 5. Create context for node
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 6. Khởi tạo Node
	node, err := cmtnode.NewNode(
		ctx,
		config,
		pv,
		nodeKey,
		cmtproxy.NewLocalClientCreator(myApp),
		cmtnode.DefaultGenesisDocProviderFunc(config),
		PebbleDBProvider, // <--- SỬ DỤNG PEBBLE Ở ĐÂY
		cmtnode.DefaultMetricsProvider(config.Instrumentation),
		logger,
	)

	if err != nil {
		panic(fmt.Errorf("failed to create node: %v", err))
	}

	// 7. Start Node
	if err := node.Start(); err != nil {
		panic(fmt.Errorf("failed to start node: %v", err))
	}

	logger.Info("Node Started", "home", homeDir)

	// 8. Wait for Shutdown Signal
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	logger.Info("Stopping Node...")
	cancel() // Cancel context to signal node to stop
	node.Stop()
	node.Wait()
}