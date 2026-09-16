package tx

import (
	"errors"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestPQKeyHashHexAndStaticRegistry(t *testing.T) {
	publicKey := []byte("registered ML-DSA-65 public key fixture")
	want := HashPQPublicKey(publicKey)
	parsed, err := ParsePQKeyHashHex("0x" + strings.ToUpper(want.Hex()))
	if err != nil {
		t.Fatalf("parse key hash: %v", err)
	}
	if parsed != want || parsed.Hex() != want.Hex() {
		t.Fatalf("key hash round trip mismatch")
	}

	address := common.HexToAddress("0x7000000000000000000000000000000000000007")
	bindings := map[common.Address]PQKeyHash{address: want}
	registry := NewStaticPQKeyRegistry(bindings)
	delete(bindings, address)

	got, found, err := registry.ExpectedKeyHash(address)
	if err != nil || !found || got != want {
		t.Fatalf("resolve registered key = (%x, %v, %v)", got, found, err)
	}
	_, found, err = registry.ExpectedKeyHash(common.Address{})
	if err != nil || found {
		t.Fatalf("resolve unregistered key = (found %v, error %v)", found, err)
	}

	var nilRegistry *StaticPQKeyRegistry
	if _, _, err := nilRegistry.ExpectedKeyHash(address); !errors.Is(err, ErrNilPQKeyResolver) {
		t.Fatalf("nil registry error = %v, want %v", err, ErrNilPQKeyResolver)
	}
}

func TestParsePQKeyHashHexRejectsMalformedValues(t *testing.T) {
	for _, input := range []string{"", "01", strings.Repeat("z", PQKeyHashSize*2), strings.Repeat("01", PQKeyHashSize+1)} {
		if _, err := ParsePQKeyHashHex(input); !errors.Is(err, ErrInvalidPQKeyHash) {
			t.Errorf("ParsePQKeyHashHex(%q) error = %v, want ErrInvalidPQKeyHash", input, err)
		}
	}
}

func TestStaticPQKeyRegistryFromHexValidatesGenesisBindings(t *testing.T) {
	address := common.HexToAddress("0xabcdef0000000000000000000000000000000001")
	hash := HashPQPublicKey([]byte("key"))
	registry, err := NewStaticPQKeyRegistryFromHex(map[string]string{address.Hex(): hash.Hex()})
	if err != nil {
		t.Fatalf("create registry from hex: %v", err)
	}
	if got, found, err := registry.ExpectedKeyHash(address); err != nil || !found || got != hash {
		t.Fatalf("resolved binding = (%x, %v, %v)", got, found, err)
	}

	if _, err := NewStaticPQKeyRegistryFromHex(map[string]string{"not-an-address": hash.Hex()}); !errors.Is(err, ErrInvalidPQKeyAddress) {
		t.Fatalf("invalid address error = %v", err)
	}
	if _, err := NewStaticPQKeyRegistryFromHex(map[string]string{address.Hex(): "01"}); !errors.Is(err, ErrInvalidPQKeyHash) {
		t.Fatalf("invalid hash error = %v", err)
	}
	lowercase := strings.ToLower(address.Hex())
	if lowercase == address.Hex() {
		t.Fatal("test address unexpectedly has no checksum case difference")
	}
	if _, err := NewStaticPQKeyRegistryFromHex(map[string]string{
		address.Hex(): hash.Hex(),
		lowercase:     hash.Hex(),
	}); !errors.Is(err, ErrDuplicatePQKeyBinding) {
		t.Fatalf("duplicate binding error = %v", err)
	}
}
