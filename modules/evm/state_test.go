package evm

import (
	"bytes"
	"errors"
	"math/big"
	"testing"

	dbm "github.com/cometbft/cometbft-db"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
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

func TestPrepareResetsTransactionScopedState(t *testing.T) {
	stateDB := newTestStateDB(t)
	staleAddress := common.HexToAddress("0x4000000000000000000000000000000000000004")
	staleSlot := common.HexToHash("0x04")
	stateDB.AddSlotToAccessList(staleAddress, staleSlot)
	stateDB.SetTransientState(staleAddress, staleSlot, common.HexToHash("0x99"))

	sender := common.HexToAddress("0x4100000000000000000000000000000000000004")
	destination := common.HexToAddress("0x4200000000000000000000000000000000000004")
	precompile := common.HexToAddress("0x0000000000000000000000000000000000000001")
	listedAddress := common.HexToAddress("0x4300000000000000000000000000000000000004")
	listedSlot := common.HexToHash("0x43")
	stateDB.Prepare(
		params.Rules{IsBerlin: true, IsEIP2929: true},
		sender,
		common.Address{},
		&destination,
		[]common.Address{precompile},
		types.AccessList{{Address: listedAddress, StorageKeys: []common.Hash{listedSlot}}},
	)

	if stateDB.AddressInAccessList(staleAddress) {
		t.Fatal("stale address remained warm across transactions")
	}
	if got := stateDB.GetTransientState(staleAddress, staleSlot); got != (common.Hash{}) {
		t.Fatalf("transient value leaked across transactions: %s", got)
	}
	for _, address := range []common.Address{sender, destination, precompile, listedAddress} {
		if !stateDB.AddressInAccessList(address) {
			t.Errorf("expected warm address %s", address)
		}
	}
	if addressOK, slotOK := stateDB.SlotInAccessList(listedAddress, listedSlot); !addressOK || !slotOK {
		t.Fatalf("transaction access-list slot not warm: address=%v slot=%v", addressOK, slotOK)
	}
}

func TestFinaliseResetsRefundAndSnapshotBoundary(t *testing.T) {
	stateDB := newTestStateDB(t)
	address := common.HexToAddress("0x5000000000000000000000000000000000000005")
	stateDB.AddRefund(123)
	stateDB.SetNonce(address, 1, tracing.NonceChangeUnspecified)
	stateDB.Snapshot()

	stateDB.Finalise(true)

	if got := stateDB.GetRefund(); got != 0 {
		t.Fatalf("refund after Finalise = %d, want 0", got)
	}
	if got := stateDB.Snapshot(); got != 0 {
		t.Fatalf("first snapshot after Finalise = %d, want fresh boundary 0", got)
	}
}

func TestCommittedStorageAdvancesAtTransactionBoundary(t *testing.T) {
	stateDB := newTestStateDB(t)
	address := common.HexToAddress("0x6000000000000000000000000000000000000006")
	slot := common.HexToHash("0x01")
	valueA := common.HexToHash("0xaa")
	valueB := common.HexToHash("0xbb")
	valueC := common.HexToHash("0xcc")

	stateDB.SetState(address, slot, valueA)
	if err := stateDB.Commit(); err != nil {
		t.Fatalf("commit initial slot: %v", err)
	}

	stateDB.Prepare(params.Rules{IsBerlin: true, IsEIP2929: true}, address, common.Address{}, &address, nil, nil)
	if got := stateDB.GetCommittedState(address, slot); got != valueA {
		t.Fatalf("tx1 committed state = %s, want A %s", got, valueA)
	}
	stateDB.SetState(address, slot, valueB)
	stateDB.Finalise(true)

	stateDB.Prepare(params.Rules{IsBerlin: true, IsEIP2929: true}, address, common.Address{}, &address, nil, nil)
	if got := stateDB.GetState(address, slot); got != valueB {
		t.Fatalf("tx2 current state = %s, want B %s", got, valueB)
	}
	if got := stateDB.GetCommittedState(address, slot); got != valueB {
		t.Fatalf("tx2 committed state = %s, want B %s", got, valueB)
	}
	stateDB.SetState(address, slot, valueC)
	if got := stateDB.GetState(address, slot); got != valueC {
		t.Fatalf("tx2 updated current state = %s, want C %s", got, valueC)
	}
	if got := stateDB.GetCommittedState(address, slot); got != valueB {
		t.Fatalf("tx2 original changed after SSTORE: got %s, want B %s", got, valueB)
	}
}

func TestConsensusReadsRecordDatabaseErrors(t *testing.T) {
	tests := []struct {
		name string
		read func(*PebbleStateDB)
	}{
		{name: "account", read: func(stateDB *PebbleStateDB) {
			stateDB.GetBalance(common.HexToAddress("0x7100000000000000000000000000000000000001"))
		}},
		{name: "code", read: func(stateDB *PebbleStateDB) {
			stateDB.GetCode(common.HexToAddress("0x7200000000000000000000000000000000000002"))
		}},
		{name: "current storage", read: func(stateDB *PebbleStateDB) {
			stateDB.GetState(common.HexToAddress("0x7300000000000000000000000000000000000003"), common.HexToHash("0x01"))
		}},
		{name: "committed storage", read: func(stateDB *PebbleStateDB) {
			stateDB.GetCommittedState(common.HexToAddress("0x7400000000000000000000000000000000000004"), common.HexToHash("0x01"))
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			injected := errors.New("injected database read failure")
			stateDB := NewPebbleStateDB(failingReadDatabase{err: injected})

			test.read(stateDB)
			if err := stateDB.Error(); err == nil {
				t.Fatal("database read error was silently interpreted as zero state")
			} else if !errors.Is(err, injected) {
				t.Fatalf("sticky error = %v, want injected cause", err)
			}
		})
	}
}

type failingReadDatabase struct{ err error }

func (db failingReadDatabase) Get([]byte) ([]byte, error) { return nil, db.err }
func (failingReadDatabase) NewBatch() dbm.Batch {
	panic("NewBatch must not be called by a read-failure test")
}
