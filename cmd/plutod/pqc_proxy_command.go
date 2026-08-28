package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	projectconfig "github.com/huyCuong73/pluto/internal/config"
	"github.com/huyCuong73/pluto/internal/pqcproxy"
)

// runPQCProxy chạy companion signer ở phía client. MetaMask trỏ vào listen
// address này; validator node chỉ nhận public key và hybrid envelope.
func runPQCProxy(args []string) error {
	flags := flag.NewFlagSet("pqc-proxy", flag.ContinueOnError)
	listenAddress := flags.String("listen", "127.0.0.1:8546", "địa chỉ local cho MetaMask kết nối")
	upstreamURL := flags.String("upstream", "http://127.0.0.1:8545", "Ethereum JSON-RPC của Pluto node")
	accountText := flags.String("account", "", "EVM account đã bind PQ key trong genesis")
	publicKeyPath := flags.String("public-key", "pqc-public.key", "file ML-DSA-65 public key")
	privateKeyPath := flags.String("private-key", "pqc-private.key", "file ML-DSA-65 private key")
	chainID := flags.Int64("evm-chain-id", projectconfig.DefaultEVMChainID, "EVM chain ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected pqc-proxy arguments: %v", flags.Args())
	}
	if !common.IsHexAddress(strings.TrimSpace(*accountText)) {
		return fmt.Errorf("--account phải là địa chỉ EVM 20-byte hợp lệ")
	}

	publicKey, err := os.ReadFile(*publicKeyPath)
	if err != nil {
		return fmt.Errorf("read public key: %w", err)
	}
	privateKey, err := readProtectedPrivateKey(*privateKeyPath)
	if err != nil {
		return err
	}
	// Xóa buffer của command khi kết thúc; server quản lý một bản sao riêng.
	defer clear(privateKey)

	server, err := pqcproxy.NewServer(pqcproxy.Config{
		UpstreamURL: *upstreamURL,
		Account:     common.HexToAddress(*accountText),
		ChainID:     *chainID,
		PublicKey:   publicKey,
		PrivateKey:  privateKey,
	})
	if err != nil {
		return fmt.Errorf("create PQC companion proxy: %w", err)
	}
	if err := server.Start(*listenAddress); err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = server.Close(shutdownCtx)
		cancel()
		return fmt.Errorf("start PQC companion proxy: %w", err)
	}

	fmt.Printf("PQC companion proxy đã chạy tại http://%s\n", server.Addr().String())
	fmt.Printf("Upstream Pluto node: %s\n", *upstreamURL)
	fmt.Printf("Account được ký ML-DSA-65: %s\n", common.HexToAddress(*accountText).Hex())
	fmt.Println("Hãy đặt RPC URL của MetaMask thành địa chỉ companion proxy ở trên.")

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-server.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	closeErr := server.Close(shutdownCtx)
	cancel()
	if serveErr != nil {
		return fmt.Errorf("PQC companion proxy stopped: %w", serveErr)
	}
	if closeErr != nil && !errors.Is(closeErr, context.Canceled) {
		return fmt.Errorf("close PQC companion proxy: %w", closeErr)
	}
	return nil
}

func readProtectedPrivateKey(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect private key: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("private key phải là regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("private key %s có quyền %04o; yêu cầu 0600 hoặc chặt hơn", path, info.Mode().Perm())
	}
	privateKey, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	return privateKey, nil
}
