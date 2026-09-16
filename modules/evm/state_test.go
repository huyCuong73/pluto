package evm

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	store "github.com/huyCuong73/pluto/internal/platform/storage"
)

func newTestStateDB(t *testing.T) *PebbleStateDB {
	t.Helper()
	db, err := store.NewPebbleDB("state-test", t.TempDir())
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})
	return NewPebbleStateDB(db)
}

func TestRepeatedSnapshotRevertsReuseValidSliceIndex(t *testing.T) {
	stateDB := newTestStateDB(t)
	address := common.HexToAddress("0x1000000000000000000000000000000000000001")
	stateDB.AddBalanceBig(address, big.NewInt(100))

	for i := 0; i < 3; i++ {
		snapshotID := stateDB.Snapshot()
		if snapshotID != 0 {
			t.Fatalf("snapshot %d: got ID %d, want 0 after previous revert", i, snapshotID)
		}
		stateDB.AddBalance(address, uint256.NewInt(1), tracing.BalanceChangeUnspecified)
		stateDB.SetNonce(address, uint64(i+1), tracing.NonceChangeUnspecified)
		stateDB.RevertToSnapshot(snapshotID)

		if got := stateDB.GetBalance(address).ToBig(); got.Cmp(big.NewInt(100)) != 0 {
			t.Fatalf("snapshot %d: balance after revert = %s, want 100", i, got)
		}
		if got := stateDB.GetNonce(address); got != 0 {
			t.Fatalf("snapshot %d: nonce after revert = %d, want 0", i, got)
		}
	}
}

func TestNestedSnapshotsRestoreCorrectBoundary(t *testing.T) {
	stateDB := newTestStateDB(t)
	address := common.HexToAddress("0x2000000000000000000000000000000000000002")
	stateDB.AddBalanceBig(address, big.NewInt(100))

	outer := stateDB.Snapshot()
	stateDB.AddBalance(address, uint256.NewInt(10), tracing.BalanceChangeUnspecified)
	inner := stateDB.Snapshot()
	stateDB.AddBalance(address, uint256.NewInt(20), tracing.BalanceChangeUnspecified)

	stateDB.RevertToSnapshot(inner)
	if got := stateDB.GetBalance(address).ToBig(); got.Cmp(big.NewInt(110)) != 0 {
		t.Fatalf("balance after inner revert = %s, want 110", got)
	}

	stateDB.RevertToSnapshot(outer)
	if got := stateDB.GetBalance(address).ToBig(); got.Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("balance after outer revert = %s, want 100", got)
	}
}

func TestSetCodeReturnsPreviousBytecodeAndReverts(t *testing.T) {
	stateDB := newTestStateDB(t)
	address := common.HexToAddress("0x3000000000000000000000000000000000000003")
	oldCode := []byte{0x60, 0x01}
	newCode := []byte{0x60, 0x02, 0x00}

	if previous := stateDB.SetCode(address, oldCode, tracing.CodeChangeUnspecified); len(previous) != 0 {
		t.Fatalf("first SetCode returned %x, want no previous code", previous)
	}
	if err := stateDB.Commit(); err != nil {
		t.Fatalf("commit old code: %v", err)
	}

	snapshotID := stateDB.Snapshot()
	if previous := stateDB.SetCode(address, newCode, tracing.CodeChangeUnspecified); !bytes.Equal(previous, oldCode) {
		t.Fatalf("SetCode returned %x, want previous bytecode %x", previous, oldCode)
	}
	if got := stateDB.GetCode(address); !bytes.Equal(got, newCode) {
		t.Fatalf("GetCode = %x, want %x", got, newCode)
	}
	wantHash := crypto.Keccak256Hash(newCode)
	if got := stateDB.GetCodeHash(address); got != wantHash {
		t.Fatalf("GetCodeHash = %s, want %s", got, wantHash)
	}

	stateDB.RevertToSnapshot(snapshotID)
	if got := stateDB.GetCode(address); !bytes.Equal(got, oldCode) {
		t.Fatalf("GetCode after revert = %x, want %x", got, oldCode)
	}
	if got := stateDB.GetCodeHash(address); got != crypto.Keccak256Hash(oldCode) {
		t.Fatalf("GetCodeHash after revert = %s, want old-code hash", got)
	}
}
