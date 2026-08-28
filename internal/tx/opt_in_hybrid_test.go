package tx

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestOptInHybridValidatorKeepsMetaMaskAndEnforcesRegisteredPQC(t *testing.T) {
	fixture := newHybridFixture(t)
	registry := NewStaticPQKeyRegistry(map[common.Address]PQKeyHash{
		fixture.sender: HashPQPublicKey(fixture.pqPublicKey),
	})
	hybrid, err := NewHybridValidator(hybridTestChainID, fixture.ecdsa, fixture.scheme, registry)
	if err != nil {
		t.Fatalf("create hybrid validator: %v", err)
	}
	validator, err := NewOptInHybridValidator(fixture.ecdsa, hybrid, registry)
	if err != nil {
		t.Fatalf("create opt-in validator: %v", err)
	}

	// Account đã opt-in vẫn chạy được khi gửi envelope đầy đủ.
	if _, err := validator.Validate(fixture.envelopeRaw); err != nil {
		t.Fatalf("validate registered hybrid transaction: %v", err)
	}

	// Không cho account đã opt-in hạ cấp xuống ECDSA-only.
	if _, err := validator.Validate(fixture.ethereumRaw); !errors.Is(err, ErrPQRequiredForSender) || FailureOf(err) != FailurePQC {
		t.Fatalf("registered raw Ethereum error = %v, want ErrPQRequiredForSender", err)
	}

	// Account chưa đăng ký PQ key tiếp tục dùng raw tx do MetaMask ký.
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate unregistered ECDSA key: %v", err)
	}
	receiver := common.HexToAddress("0xd100000000000000000000000000000000000001")
	unsigned := gethtypes.NewTx(&gethtypes.LegacyTx{
		Nonce: 0, GasPrice: big.NewInt(0), Gas: 21_000, To: &receiver, Value: big.NewInt(1),
	})
	signed, err := gethtypes.SignTx(unsigned, gethtypes.LatestSignerForChainID(big.NewInt(hybridTestChainID)), privateKey)
	if err != nil {
		t.Fatalf("sign unregistered Ethereum transaction: %v", err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal unregistered Ethereum transaction: %v", err)
	}
	if _, err := validator.Validate(raw); err != nil {
		t.Fatalf("validate unregistered MetaMask-style transaction: %v", err)
	}
}

func TestNewOptInHybridValidatorRejectsMissingDependencies(t *testing.T) {
	fixture := newHybridFixture(t)
	registry := NewStaticPQKeyRegistry(nil)
	hybrid, err := NewHybridValidator(hybridTestChainID, fixture.ecdsa, fixture.scheme, registry)
	if err != nil {
		t.Fatalf("create hybrid validator: %v", err)
	}

	if _, err := NewOptInHybridValidator(nil, hybrid, registry); !errors.Is(err, ErrNilEthereumValidator) {
		t.Fatalf("nil Ethereum validator error = %v", err)
	}
	if _, err := NewOptInHybridValidator(fixture.ecdsa, nil, registry); !errors.Is(err, ErrNilPQScheme) {
		t.Fatalf("nil hybrid validator error = %v", err)
	}
	if _, err := NewOptInHybridValidator(fixture.ecdsa, hybrid, nil); !errors.Is(err, ErrNilPQKeyResolver) {
		t.Fatalf("nil registry error = %v", err)
	}
}
