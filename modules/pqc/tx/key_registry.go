package tx

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

const PQKeyHashSize = sha256.Size

// PQKeyHash is the stable identity stored for a registered post-quantum key.
// Keeping the full 1,952-byte ML-DSA-65 key outside account identity makes the
// registry independent of one concrete key representation.
type PQKeyHash [PQKeyHashSize]byte

func HashPQPublicKey(publicKey []byte) PQKeyHash {
	return sha256.Sum256(publicKey)
}

func ParsePQKeyHashHex(encoded string) (PQKeyHash, error) {
	var hash PQKeyHash
	normalized := strings.TrimSpace(encoded)
	normalized = strings.TrimPrefix(normalized, "0x")
	normalized = strings.TrimPrefix(normalized, "0X")
	decoded, err := hex.DecodeString(normalized)
	if err != nil || len(decoded) != PQKeyHashSize {
		return hash, fmt.Errorf("%w: expected %d-byte hexadecimal value", ErrInvalidPQKeyHash, PQKeyHashSize)
	}
	copy(hash[:], decoded)
	return hash, nil
}

func (h PQKeyHash) Hex() string {
	return hex.EncodeToString(h[:])
}

// PQKeyResolver is the replaceable identity-binding boundary used by the
// hybrid validator. A later implementation may read an authenticated state
// registry without changing transaction validation.
type PQKeyResolver interface {
	ExpectedKeyHash(sender common.Address) (hash PQKeyHash, found bool, err error)
}

// StaticPQKeyRegistry is the phase-one, genesis-backed resolver. The map is
// copied at construction so validation cannot change because a caller mutates
// its configuration after the node starts.
type StaticPQKeyRegistry struct {
	bindings map[common.Address]PQKeyHash
}

func NewStaticPQKeyRegistry(bindings map[common.Address]PQKeyHash) *StaticPQKeyRegistry {
	copyOfBindings := make(map[common.Address]PQKeyHash, len(bindings))
	for address, hash := range bindings {
		copyOfBindings[address] = hash
	}
	return &StaticPQKeyRegistry{bindings: copyOfBindings}
}

// NewStaticPQKeyRegistryFromHex validates the JSON/CLI representation used by
// genesis and canonicalizes addresses before constructing the immutable map.
func NewStaticPQKeyRegistryFromHex(bindings map[string]string) (*StaticPQKeyRegistry, error) {
	parsed := make(map[common.Address]PQKeyHash, len(bindings))
	for addressText, hashText := range bindings {
		if !common.IsHexAddress(addressText) {
			return nil, fmt.Errorf("%w: %q", ErrInvalidPQKeyAddress, addressText)
		}
		address := common.HexToAddress(addressText)
		if _, exists := parsed[address]; exists {
			return nil, fmt.Errorf("%w: %s", ErrDuplicatePQKeyBinding, address.Hex())
		}
		hash, err := ParsePQKeyHashHex(hashText)
		if err != nil {
			return nil, fmt.Errorf("post-quantum key for %s: %w", address.Hex(), err)
		}
		parsed[address] = hash
	}
	return NewStaticPQKeyRegistry(parsed), nil
}

func (r *StaticPQKeyRegistry) ExpectedKeyHash(sender common.Address) (PQKeyHash, bool, error) {
	if r == nil {
		return PQKeyHash{}, false, ErrNilPQKeyResolver
	}
	hash, found := r.bindings[sender]
	return hash, found, nil
}
