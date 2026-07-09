package evm

import (
	"crypto/sha256"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie/utils"
	"github.com/holiman/uint256"
	"github.com/huyCuong73/pluto/internal/store"
)

// Account EVM, mã hoá RLP khi lưu PebbleDB
type Account struct {
	Nonce       uint64
	Balance     *big.Int
	StorageRoot common.Hash // Placeholder (chưa dùng Merkle trie)
	CodeHash    []byte      // Hash bytecode (nil nếu là EOA)
}

// Snapshot / Journal

// journalEntry lưu thay đổi để revert
type journalEntry struct {
	typ int // 0=balance, 1=nonce, 2=storage, 3=code, 4=selfDestruct, 5=transient

	addr common.Address

	prevBalance *big.Int

	prevNonce uint64

	key     common.Hash
	prevVal common.Hash

	prevCode     []byte
	prevCodeHash []byte

	prevSelfDestructed bool

	prevTransientVal common.Hash
}

// snapshot lưu trạng thái tại một thời điểm
type snapshot struct {
	journalLen int    // Vị trí journal
	refund     uint64 // Gas refund
	logsLen    int    // Số lượng logs
}

// PebbleStateDB implements vm.StateDB for geth v1.16
type PebbleStateDB struct {
	db *store.PebbleDB

	// Cache accounts
	state map[common.Address]*Account

	// Cache code
	code      map[common.Address][]byte
	dirtyCode map[common.Address]bool // Đánh dấu code cần commit

	// Cache storage
	storage      map[common.Address]map[common.Hash]common.Hash
	dirtyStorage map[common.Address]map[common.Hash]bool // Đánh dấu slot cần commit

	// Self-destruct
	selfDestructed map[common.Address]bool

	// Transient storage (EIP-1153) - Xoá sau mỗi tx
	transientStorage map[common.Address]map[common.Hash]common.Hash

	// Gas refund
	refund uint64

	// Event logs
	logs []*types.Log

	// Snapshot / Journal
	journal   []journalEntry
	snapshots []snapshot
	nextRevID int

	// Access list (EIP-2930)
	accessList *accessList

	// Đánh dấu accounts đã đổi (cho AppHash)
	dirtyAccounts map[common.Address]bool
}

// accessList warm addresses/slots cho EIP-2929
type accessList struct {
	addresses map[common.Address]bool
	slots     map[common.Address]map[common.Hash]bool
}

func newAccessList() *accessList {
	return &accessList{
		addresses: make(map[common.Address]bool),
		slots:     make(map[common.Address]map[common.Hash]bool),
	}
}

func NewPebbleStateDB(db *store.PebbleDB) *PebbleStateDB {
	return &PebbleStateDB{
		db:               db,
		state:            make(map[common.Address]*Account),
		code:             make(map[common.Address][]byte),
		dirtyCode:        make(map[common.Address]bool),
		storage:          make(map[common.Address]map[common.Hash]common.Hash),
		dirtyStorage:     make(map[common.Address]map[common.Hash]bool),
		selfDestructed:   make(map[common.Address]bool),
		transientStorage: make(map[common.Address]map[common.Hash]common.Hash),
		logs:             make([]*types.Log, 0),
		journal:          make([]journalEntry, 0, 64),
		snapshots:        make([]snapshot, 0, 4),
		accessList:       newAccessList(),
		dirtyAccounts:    make(map[common.Address]bool),
	}
}

// ============================================================
// Internal helpers
// ============================================================

func (s *PebbleStateDB) getAccount(addr common.Address) *Account {
	if acc, ok := s.state[addr]; ok {
		return acc
	}

	// Key DB: "acc-" + Address
	key := append([]byte("acc-"), addr.Bytes()...)
	data, _ := s.db.Get(key)

	if len(data) == 0 {
		// Account mới nếu chưa có
		acc := &Account{
			Nonce:       0,
			Balance:     big.NewInt(0),
			StorageRoot: common.Hash{},
			CodeHash:    nil,
		}
		s.state[addr] = acc
		return acc
	}

	// Decode RLP
	var acc Account
	if err := rlp.DecodeBytes(data, &acc); err != nil {
		panic(err) // Panic nếu data hỏng
	}
	s.state[addr] = &acc
	return &acc
}

// storageKey: Key DB storage ("storage-" + addr + key)
func storageKey(addr common.Address, key common.Hash) []byte {
	k := make([]byte, 0, 8+20+32)
	k = append(k, []byte("storage-")...)
	k = append(k, addr.Bytes()...)
	k = append(k, key.Bytes()...)
	return k
}

// codeKey: Key DB code ("code-" + addr)
func codeKey(addr common.Address) []byte {
	k := make([]byte, 0, 5+20)
	k = append(k, []byte("code-")...)
	k = append(k, addr.Bytes()...)
	return k
}

// markDirty đánh dấu account thay đổi (cho AppHash)
func (s *PebbleStateDB) markDirty(addr common.Address) {
	s.dirtyAccounts[addr] = true
}

// ============================================================
// Implement vm.StateDB interface (go-ethereum v1.16)
// ============================================================

func (s *PebbleStateDB) CreateAccount(addr common.Address) {
	acc := s.getAccount(addr)
	// Reset state cho address mới, giữ balance (EIP-161)
	acc.Nonce = 0
	acc.CodeHash = nil
	acc.StorageRoot = common.Hash{}
	s.markDirty(addr)
}

func (s *PebbleStateDB) CreateContract(addr common.Address) {
	acc := s.getAccount(addr)
	acc.Nonce = 1 // Nonce contract bắt đầu từ 1 (EIP-161)
	s.markDirty(addr)
}

// --- Balance ---

func (s *PebbleStateDB) SubBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	acc := s.getAccount(addr)
	prev := new(uint256.Int)
	prev.SetFromBig(acc.Balance)

	// Journal revert
	s.journal = append(s.journal, journalEntry{
		typ:         0,
		addr:        addr,
		prevBalance: new(big.Int).Set(acc.Balance),
	})

	acc.Balance.Sub(acc.Balance, amount.ToBig())
	s.markDirty(addr)
	return *prev
}

func (s *PebbleStateDB) AddBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	acc := s.getAccount(addr)
	prev := new(uint256.Int)
	prev.SetFromBig(acc.Balance)

	// Journal revert
	s.journal = append(s.journal, journalEntry{
		typ:         0,
		addr:        addr,
		prevBalance: new(big.Int).Set(acc.Balance),
	})

	acc.Balance.Add(acc.Balance, amount.ToBig())
	s.markDirty(addr)
	return *prev
}

func (s *PebbleStateDB) GetBalance(addr common.Address) *uint256.Int {
	acc := s.getAccount(addr)
	bal := new(uint256.Int)
	bal.SetFromBig(acc.Balance)
	return bal
}

// --- Nonce ---

func (s *PebbleStateDB) GetNonce(addr common.Address) uint64 {
	acc := s.getAccount(addr)
	return acc.Nonce
}

func (s *PebbleStateDB) SetNonce(addr common.Address, nonce uint64, reason tracing.NonceChangeReason) {
	acc := s.getAccount(addr)

	// Journal revert
	s.journal = append(s.journal, journalEntry{
		typ:       1,
		addr:      addr,
		prevNonce: acc.Nonce,
	})

	acc.Nonce = nonce
	s.markDirty(addr)
}

// --- Code (Smart Contract Bytecode) ---

func (s *PebbleStateDB) GetCodeHash(addr common.Address) common.Hash {
	acc := s.getAccount(addr)
	if len(acc.CodeHash) == 0 {
		// EOA hoặc trống -> trả về empty hash (EVM spec)
		return common.BytesToHash(crypto.Keccak256(nil))
	}
	return common.BytesToHash(acc.CodeHash)
}

func (s *PebbleStateDB) GetCode(addr common.Address) []byte {
	// Check cache
	if code, ok := s.code[addr]; ok {
		return code
	}

	// Check DB
	data, _ := s.db.Get(codeKey(addr))
	if len(data) > 0 {
		s.code[addr] = data // Cache lại
		return data
	}

	return nil
}

func (s *PebbleStateDB) SetCode(addr common.Address, code []byte, reason tracing.CodeChangeReason) []byte {
	acc := s.getAccount(addr)

	// Journal revert
	s.journal = append(s.journal, journalEntry{
		typ:          3,
		addr:         addr,
		prevCode:     s.GetCode(addr),
		prevCodeHash: acc.CodeHash,
	})

	// Lưu cache
	s.code[addr] = code
	s.dirtyCode[addr] = true

	// Cập nhật CodeHash
	if len(code) > 0 {
		hash := crypto.Keccak256(code)
		acc.CodeHash = hash
	} else {
		acc.CodeHash = nil
	}

	s.markDirty(addr)

	// go-ethereum v1.16 trả về code hash cũ
	return acc.CodeHash
}

func (s *PebbleStateDB) GetCodeSize(addr common.Address) int {
	code := s.GetCode(addr)
	return len(code)
}

// --- Gas Refund ---

func (s *PebbleStateDB) AddRefund(gas uint64) {
	s.refund += gas
}

func (s *PebbleStateDB) SubRefund(gas uint64) {
	if gas > s.refund {
		panic("SubRefund: refund counter below zero")
	}
	s.refund -= gas
}

func (s *PebbleStateDB) GetRefund() uint64 {
	return s.refund
}

// --- Contract Storage (SLOAD/SSTORE) ---

// getStorage: đọc storage slot (cache -> DB)
func (s *PebbleStateDB) getStorage(addr common.Address, key common.Hash) common.Hash {
	// Check cache
	if slots, ok := s.storage[addr]; ok {
		if val, ok := slots[key]; ok {
			return val
		}
	}

	// Đọc DB
	data, _ := s.db.Get(storageKey(addr, key))
	if len(data) == 0 {
		return common.Hash{}
	}

	val := common.BytesToHash(data)

	// Lưu cache (không dirty)
	if s.storage[addr] == nil {
		s.storage[addr] = make(map[common.Hash]common.Hash)
	}
	s.storage[addr][key] = val
	return val
}

func (s *PebbleStateDB) GetCommittedState(addr common.Address, key common.Hash) common.Hash {
	// Đọc thẳng từ DB (EIP-2200)
	data, _ := s.db.Get(storageKey(addr, key))
	if len(data) == 0 {
		return common.Hash{}
	}
	return common.BytesToHash(data)
}

func (s *PebbleStateDB) GetStateAndCommittedState(addr common.Address, key common.Hash) (common.Hash, common.Hash) {
	return s.GetState(addr, key), s.GetCommittedState(addr, key)
}

func (s *PebbleStateDB) GetState(addr common.Address, key common.Hash) common.Hash {
	return s.getStorage(addr, key)
}

func (s *PebbleStateDB) SetState(addr common.Address, key, value common.Hash) common.Hash {
	prev := s.GetState(addr, key)

	// Journal revert
	s.journal = append(s.journal, journalEntry{
		typ:     2,
		addr:    addr,
		key:     key,
		prevVal: prev,
	})

	// Ghi cache
	if s.storage[addr] == nil {
		s.storage[addr] = make(map[common.Hash]common.Hash)
	}
	s.storage[addr][key] = value

	// Đánh dấu dirty
	if s.dirtyStorage[addr] == nil {
		s.dirtyStorage[addr] = make(map[common.Hash]bool)
	}
	s.dirtyStorage[addr][key] = true

	s.markDirty(addr)
	return prev
}

func (s *PebbleStateDB) GetStorageRoot(addr common.Address) common.Hash {
	// Placeholder (chưa có Merkle trie)
	return common.Hash{}
}

// --- Transient Storage (EIP-1153) ---

func (s *PebbleStateDB) GetTransientState(addr common.Address, key common.Hash) common.Hash {
	if slots, ok := s.transientStorage[addr]; ok {
		return slots[key]
	}
	return common.Hash{}
}

func (s *PebbleStateDB) SetTransientState(addr common.Address, key, value common.Hash) {
	// Journal revert
	s.journal = append(s.journal, journalEntry{
		typ:              5,
		addr:             addr,
		key:              key,
		prevTransientVal: s.GetTransientState(addr, key),
	})

	if s.transientStorage[addr] == nil {
		s.transientStorage[addr] = make(map[common.Hash]common.Hash)
	}
	s.transientStorage[addr][key] = value
}

// --- Self-Destruct ---

func (s *PebbleStateDB) SelfDestruct(addr common.Address) uint256.Int {
	bal := s.GetBalance(addr)

	// Journal
	s.journal = append(s.journal, journalEntry{
		typ:                4,
		addr:               addr,
		prevSelfDestructed: s.selfDestructed[addr],
		prevBalance:        new(big.Int).Set(s.getAccount(addr).Balance),
	})

	s.selfDestructed[addr] = true

	// Zero balance
	s.getAccount(addr).Balance = big.NewInt(0)
	s.markDirty(addr)
	return *bal
}

func (s *PebbleStateDB) HasSelfDestructed(addr common.Address) bool {
	return s.selfDestructed[addr]
}

func (s *PebbleStateDB) SelfDestruct6780(addr common.Address) (uint256.Int, bool) {
	bal := s.GetBalance(addr)

	// EIP-6780: chỉ zero balance, ko xoá
	s.journal = append(s.journal, journalEntry{
		typ:                4,
		addr:               addr,
		prevSelfDestructed: s.selfDestructed[addr],
		prevBalance:        new(big.Int).Set(s.getAccount(addr).Balance),
	})

	s.selfDestructed[addr] = true
	s.getAccount(addr).Balance = big.NewInt(0)
	s.markDirty(addr)
	return *bal, false
}

// --- Account Existence ---

func (s *PebbleStateDB) Exist(addr common.Address) bool {
	acc := s.getAccount(addr)
	// Tồn tại nếu có state
	return acc.Nonce > 0 || acc.Balance.Sign() > 0 || len(acc.CodeHash) > 0
}

func (s *PebbleStateDB) Empty(addr common.Address) bool {
	acc := s.getAccount(addr)
	return acc.Nonce == 0 && acc.Balance.Sign() == 0 && len(acc.CodeHash) == 0
}

// --- Access List (EIP-2929) ---

func (s *PebbleStateDB) Prepare(rules params.Rules, sender, coinbase common.Address, dest *common.Address, precompiles []common.Address, txAccesses types.AccessList) {
	// Reset access list mỗi tx
	s.accessList = newAccessList()

	// Sender luôn warm
	s.accessList.addresses[sender] = true

	// Coinbase warm (EIP-3651)
	if rules.IsShanghai {
		s.accessList.addresses[coinbase] = true
	}

	// Dest warm
	if dest != nil {
		s.accessList.addresses[*dest] = true
	}

	// Precompiles warm
	for _, addr := range precompiles {
		s.accessList.addresses[addr] = true
	}

	// EIP-2930 access list
	for _, entry := range txAccesses {
		s.accessList.addresses[entry.Address] = true
		if s.accessList.slots[entry.Address] == nil {
			s.accessList.slots[entry.Address] = make(map[common.Hash]bool)
		}
		for _, key := range entry.StorageKeys {
			s.accessList.slots[entry.Address][key] = true
		}
	}

	// Reset transient storage (EIP-1153)
	s.transientStorage = make(map[common.Address]map[common.Hash]common.Hash)
}

func (s *PebbleStateDB) AddressInAccessList(addr common.Address) bool {
	return s.accessList.addresses[addr]
}

func (s *PebbleStateDB) SlotInAccessList(addr common.Address, slot common.Hash) (addressOk bool, slotOk bool) {
	addressOk = s.accessList.addresses[addr]
	if !addressOk {
		return false, false
	}
	if slots, ok := s.accessList.slots[addr]; ok {
		slotOk = slots[slot]
	}
	return addressOk, slotOk
}

func (s *PebbleStateDB) AddAddressToAccessList(addr common.Address) {
	s.accessList.addresses[addr] = true
}

func (s *PebbleStateDB) AddSlotToAccessList(addr common.Address, slot common.Hash) {
	s.accessList.addresses[addr] = true
	if s.accessList.slots[addr] == nil {
		s.accessList.slots[addr] = make(map[common.Hash]bool)
	}
	s.accessList.slots[addr][slot] = true
}

// --- Snapshot / Revert ---

func (s *PebbleStateDB) Snapshot() int {
	id := s.nextRevID
	s.nextRevID++
	s.snapshots = append(s.snapshots, snapshot{
		journalLen: len(s.journal),
		refund:     s.refund,
		logsLen:    len(s.logs),
	})
	return id
}

func (s *PebbleStateDB) RevertToSnapshot(revid int) {
	if revid < 0 || revid >= len(s.snapshots) {
		panic("RevertToSnapshot: invalid revision id")
	}

	snap := s.snapshots[revid]

	// Revert ngược journal
	for i := len(s.journal) - 1; i >= snap.journalLen; i-- {
		entry := s.journal[i]
		switch entry.typ {
		case 0: // balance
			s.getAccount(entry.addr).Balance = entry.prevBalance
		case 1: // nonce
			s.getAccount(entry.addr).Nonce = entry.prevNonce
		case 2: // storage
			if s.storage[entry.addr] == nil {
				s.storage[entry.addr] = make(map[common.Hash]common.Hash)
			}
			s.storage[entry.addr][entry.key] = entry.prevVal
		case 3: // code
			acc := s.getAccount(entry.addr)
			if entry.prevCode != nil {
				s.code[entry.addr] = entry.prevCode
			} else {
				delete(s.code, entry.addr)
			}
			acc.CodeHash = entry.prevCodeHash
		case 4: // selfDestruct
			s.selfDestructed[entry.addr] = entry.prevSelfDestructed
			s.getAccount(entry.addr).Balance = entry.prevBalance
		case 5: // transient storage
			if s.transientStorage[entry.addr] == nil {
				s.transientStorage[entry.addr] = make(map[common.Hash]common.Hash)
			}
			s.transientStorage[entry.addr][entry.key] = entry.prevTransientVal
		}
	}

	// Truncate journal & snapshots
	s.journal = s.journal[:snap.journalLen]
	s.snapshots = s.snapshots[:revid]
	s.refund = snap.refund
	s.logs = s.logs[:snap.logsLen]
}

// --- Logs ---

func (s *PebbleStateDB) AddLog(log *types.Log) {
	s.logs = append(s.logs, log)
}

func (s *PebbleStateDB) GetLogs() []*types.Log {
	return s.logs
}

// --- Misc interface methods ---

func (s *PebbleStateDB) AddPreimage(hash common.Hash, preimage []byte) {
	// No-op
}

func (s *PebbleStateDB) ForEachStorage(addr common.Address, cb func(common.Hash, common.Hash) bool) error {
	// Duyệt cached storage
	if slots, ok := s.storage[addr]; ok {
		for key, val := range slots {
			if !cb(key, val) {
				return nil
			}
		}
	}
	return nil
}

func (s *PebbleStateDB) Witness() *stateless.Witness {
	return nil
}

func (s *PebbleStateDB) AccessEvents() *state.AccessEvents {
	return nil
}

func (s *PebbleStateDB) PointCache() *utils.PointCache {
	return nil
}

func (s *PebbleStateDB) Finalise(deleteEmptyObjects bool) {
	// Xoá accounts tự huỷ
	for addr := range s.selfDestructed {
		if s.selfDestructed[addr] {
			delete(s.storage, addr)
			delete(s.code, addr)
			s.markDirty(addr)
		}
	}
}

// Commit toàn bộ dirty state xuống PebbleDB
func (s *PebbleStateDB) Commit() error {
	batch := s.db.NewBatch()
	defer batch.Close()

	// Ghi accounts
	for addr, acc := range s.state {
		data, err := rlp.EncodeToBytes(acc)
		if err != nil {
			return err
		}
		key := append([]byte("acc-"), addr.Bytes()...)
		if err := batch.Set(key, data); err != nil {
			return err
		}
	}

	// Ghi code dirty
	for addr := range s.dirtyCode {
		code := s.code[addr]
		if len(code) > 0 {
			if err := batch.Set(codeKey(addr), code); err != nil {
				return err
			}
		}
	}

	// Ghi storage dirty
	for addr, dirtyKeys := range s.dirtyStorage {
		slots := s.storage[addr]
		for key := range dirtyKeys {
			val := slots[key]
			if val == (common.Hash{}) {
				// Clear storage (EIP-2200), dùng Set(key, nil) thay cho Delete
				if err := batch.Set(storageKey(addr, key), nil); err != nil {
					return err
				}
			} else {
				if err := batch.Set(storageKey(addr, key), val.Bytes()); err != nil {
					return err
				}
			}
		}
	}

	return batch.WriteSync()
}

// AppHash — Tính SHA256 từ dirty accounts (Phase 1 simple approach)
func (s *PebbleStateDB) ComputeAppHash() []byte {
	if len(s.dirtyAccounts) == 0 {
		return nil
	}

	// Sort address deterministic
	addrs := make([]common.Address, 0, len(s.dirtyAccounts))
	for addr := range s.dirtyAccounts {
		addrs = append(addrs, addr)
	}
	sort.Slice(addrs, func(i, j int) bool {
		return addrs[i].Hex() < addrs[j].Hex()
	})

	h := sha256.New()
	for _, addr := range addrs {
		acc := s.state[addr]
		if acc == nil {
			continue
		}
		data, err := rlp.EncodeToBytes(acc)
		if err != nil {
			continue
		}
		h.Write(addr.Bytes())
		h.Write(data)
	}

	return h.Sum(nil)
}

// Add balance cho genesis
func (s *PebbleStateDB) AddBalanceBig(addr common.Address, amount *big.Int) {
	acc := s.getAccount(addr)
	acc.Balance.Add(acc.Balance, amount)
	s.markDirty(addr)
}
