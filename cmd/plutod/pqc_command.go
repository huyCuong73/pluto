package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"

	projectconfig "github.com/huyCuong73/pluto/internal/config"
	"github.com/huyCuong73/pluto/internal/pqc"
	plutotx "github.com/huyCuong73/pluto/internal/tx"
)

// runPQCKeygen tạo key ở phía client. Node validator không cần và không được
// giữ ML-DSA private key của người dùng.
func runPQCKeygen(args []string) error {
	flags := flag.NewFlagSet("pqc-keygen", flag.ContinueOnError)
	publicKeyPath := flags.String("public-key", "pqc-public.key", "file lưu ML-DSA-65 public key")
	privateKeyPath := flags.String("private-key", "pqc-private.key", "file lưu ML-DSA-65 private key")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected pqc-keygen arguments: %v", flags.Args())
	}
	if strings.TrimSpace(*publicKeyPath) == "" || strings.TrimSpace(*privateKeyPath) == "" {
		return fmt.Errorf("public/private key path không được rỗng")
	}
	if *publicKeyPath == *privateKeyPath {
		return fmt.Errorf("public key và private key phải dùng hai file khác nhau")
	}

	scheme := pqc.NewMLDSA65()
	publicKey, privateKey, err := scheme.GenerateKey(nil)
	if err != nil {
		return err
	}
	// O_EXCL bảo vệ file/key cũ khỏi bị ghi đè ngoài ý muốn.
	if err := writeNewFile(*privateKeyPath, privateKey, 0o600); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}
	if err := writeNewFile(*publicKeyPath, publicKey, 0o644); err != nil {
		// File private vừa được tạo trong chính lệnh này; xóa nó để không để lại
		// một key pair nửa chừng. Không bao giờ xóa file có sẵn vì O_EXCL.
		_ = os.Remove(*privateKeyPath)
		return fmt.Errorf("write public key: %w", err)
	}

	hash := plutotx.HashPQPublicKey(publicKey)
	fmt.Printf("Đã tạo ML-DSA-65 key pair\n")
	fmt.Printf("Public key: %s\n", *publicKeyPath)
	fmt.Printf("Private key: %s (hãy giữ bí mật)\n", *privateKeyPath)
	fmt.Printf("Genesis --pqc-key value (SHA-256): %s\n", hash.Hex())
	return nil
}

// runPQCWrap nhận raw Ethereum tx đã có ECDSA signature, ký thêm ML-DSA-65 và
// tạo HybridEnvelope để gửi bằng pluto_sendHybridTransaction.
func runPQCWrap(args []string) error {
	flags := flag.NewFlagSet("pqc-wrap", flag.ContinueOnError)
	ethereumTxHex := flags.String("ethereum-tx", "", "raw signed Ethereum transaction dạng 0x-hex")
	publicKeyPath := flags.String("public-key", "pqc-public.key", "file ML-DSA-65 public key")
	privateKeyPath := flags.String("private-key", "pqc-private.key", "file ML-DSA-65 private key")
	chainID := flags.Int64("evm-chain-id", projectconfig.DefaultEVMChainID, "EVM chain ID")
	outputPath := flags.String("out", "", "file output binary; để rỗng thì in 0x-hex")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected pqc-wrap arguments: %v", flags.Args())
	}
	rawHex := strings.TrimPrefix(strings.TrimSpace(*ethereumTxHex), "0x")
	if rawHex == "" {
		return fmt.Errorf("--ethereum-tx là bắt buộc")
	}
	ethereumTx, err := hex.DecodeString(rawHex)
	if err != nil {
		return fmt.Errorf("decode --ethereum-tx: %w", err)
	}
	publicKey, err := os.ReadFile(*publicKeyPath)
	if err != nil {
		return fmt.Errorf("read public key: %w", err)
	}
	privateKey, err := os.ReadFile(*privateKeyPath)
	if err != nil {
		return fmt.Errorf("read private key: %w", err)
	}

	envelope, err := plutotx.CreateSignedHybridEnvelopeV1(
		ethereumTx,
		publicKey,
		privateKey,
		big.NewInt(*chainID),
		pqc.NewMLDSA65(),
	)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*outputPath) == "" {
		fmt.Printf("0x%s\n", hex.EncodeToString(envelope))
		return nil
	}
	if err := writeNewFile(*outputPath, envelope, 0o644); err != nil {
		return fmt.Errorf("write hybrid envelope: %w", err)
	}
	fmt.Printf("Đã ghi HybridEnvelope V1 vào %s (%d bytes)\n", *outputPath, len(envelope))
	return nil
}

func writeNewFile(path string, data []byte, permission os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, permission)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
