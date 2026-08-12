package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	cfg "github.com/cometbft/cometbft/config"
	"github.com/huyCuong73/pluto/internal/app"
	projectconfig "github.com/huyCuong73/pluto/internal/config"
)

func TestNodeBootsProducesBlocksAndRestarts(t *testing.T) {
	homeDir := filepath.Join(t.TempDir(), "node")
	if err := runInit([]string{"--home", homeDir, "--chain-id", "pluto-smoke-1"}); err != nil {
		t.Fatalf("init node: %v", err)
	}

	runNodeFor(t, homeDir, 4*time.Second)
	firstHeight := persistedHeight(t, homeDir)
	if firstHeight < 1 {
		t.Fatalf("height after first start = %d, want at least 1", firstHeight)
	}

	runNodeFor(t, homeDir, 3*time.Second)
	secondHeight := persistedHeight(t, homeDir)
	if secondHeight <= firstHeight {
		t.Fatalf("height after restart = %d, want greater than %d", secondHeight, firstHeight)
	}
}

func runNodeFor(t *testing.T, homeDir string, duration time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- startNodeWithConfig(ctx, homeDir, func(config *cfg.Config) {
			// The default KV transaction indexer retains a Windows file handle in
			// CometBFT v1.0.0 after an in-process shutdown. It is unrelated to
			// application state, so the smoke test uses the supported null indexer.
			config.TxIndex.Indexer = "null"
		})
	}()

	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case err := <-done:
		t.Fatalf("node stopped before cancellation: %v", err)
	case <-timer.C:
		cancel()
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stop node: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("node did not stop within 10 seconds")
	}
}

func persistedHeight(t *testing.T, homeDir string) int64 {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application, err := app.NewAppWithChainID(filepath.Join(homeDir, "data"), logger, projectconfig.DefaultEVMChainID)
	if err != nil {
		t.Fatalf("open application after node stop: %v", err)
	}
	defer application.Close()

	info, err := application.Info(context.Background(), &abci.InfoRequest{})
	if err != nil {
		t.Fatalf("read application info: %v", err)
	}
	return info.LastBlockHeight
}
