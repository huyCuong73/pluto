// Đây là lớp wrapper cho PebbleDB để tuân thủ interface dbm.DB của CometBFT


package store

import (
	"fmt"
	"sync"

	"github.com/cockroachdb/pebble"
	dbm "github.com/cometbft/cometbft-db"
)

// PebbleDB implement dbm.DB interface của CometBFT
type PebbleDB struct {
	db *pebble.DB
}

// NewPebbleDB tạo instance mới
func NewPebbleDB(name string, dir string) (*PebbleDB, error) {
	dbPath := fmt.Sprintf("%s/%s.db", dir, name)
	
	// Cấu hình tối ưu cho Blockchain (Write Heavy)
	opts := &pebble.Options{
		Cache:        pebble.NewCache(512 << 20), // 512MB Cache
		MemTableSize: 64 << 20,                   // 64MB Memtable
	}

	db, err := pebble.Open(dbPath, opts)
	if err != nil {
		return nil, err
	}
	return &PebbleDB{db: db}, nil
}

// Get lấy value từ key
func (pdb *PebbleDB) Get(key []byte) ([]byte, error) {
	val, closer, err := pdb.db.Get(key)
	if err == pebble.ErrNotFound {
		return nil, nil // Return nil nếu không tìm thấy (theo chuẩn CometBFT)
	}
	if err != nil {
		return nil, err
	}
	// Copy data vì closer sẽ đóng khi return
	ret := make([]byte, len(val))
	copy(ret, val)
	closer.Close()
	return ret, nil
}

// Has kiểm tra key tồn tại
func (pdb *PebbleDB) Has(key []byte) (bool, error) {
	_, closer, err := pdb.db.Get(key)
	if err == pebble.ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	closer.Close()
	return true, nil
}

// Set lưu key-value
func (pdb *PebbleDB) Set(key, value []byte) error {
	return pdb.db.Set(key, value, pebble.Sync)
}

// SetSync (tương tự Set trong Pebble với SyncOptions)
func (pdb *PebbleDB) SetSync(key, value []byte) error {
	return pdb.db.Set(key, value, pebble.Sync)
}

// Delete xóa key
func (pdb *PebbleDB) Delete(key []byte) error {
	return pdb.db.Delete(key, pebble.Sync)
}

// DeleteSync xóa key đồng bộ
func (pdb *PebbleDB) DeleteSync(key []byte) error {
	return pdb.db.Delete(key, pebble.Sync)
}

// Close đóng DB
func (pdb *PebbleDB) Close() error {
	return pdb.db.Close()
}

// NewBatch tạo transaction batch
func (pdb *PebbleDB) NewBatch() dbm.Batch {
	return &PebbleBatch{batch: pdb.db.NewBatch()}
}

// Print in thống kê (implement cho đủ interface)
func (pdb *PebbleDB) Print() error {
	fmt.Println(pdb.db.Metrics().String())
	return nil
}

// Stats trả về thống kê
func (pdb *PebbleDB) Stats() map[string]string {
	return map[string]string{
		"metrics": pdb.db.Metrics().String(),
	}
}

// Iterator returns an iterator over a domain of keys in ascending order
func (pdb *PebbleDB) Iterator(start, end []byte) (dbm.Iterator, error) {
	iter, err := pdb.db.NewIter(&pebble.IterOptions{
		LowerBound: start,
		UpperBound: end,
	})
	if err != nil {
		return nil, err
	}
	iter.First()
	return &pebbleIterator{iter: iter, start: start, end: end, isReverse: false, isInvalid: false}, nil
}

// ReverseIterator returns an iterator over a domain of keys in descending order
func (pdb *PebbleDB) ReverseIterator(start, end []byte) (dbm.Iterator, error) {
	iter, err := pdb.db.NewIter(&pebble.IterOptions{
		LowerBound: start,
		UpperBound: end,
	})
	if err != nil {
		return nil, err
	}
	iter.Last()
	return &pebbleIterator{iter: iter, start: start, end: end, isReverse: true, isInvalid: false}, nil
}

// Compact compacts the database in the given key range
func (pdb *PebbleDB) Compact(start, end []byte) error {
	return pdb.db.Compact(start, end, true)
}

// --- Iterator Implementation ---

type pebbleIterator struct {
	iter      *pebble.Iterator
	start     []byte
	end       []byte
	isReverse bool
	isInvalid bool
}

// Domain returns the start (inclusive) and end (exclusive) limits of the iterator
func (itr *pebbleIterator) Domain() ([]byte, []byte) {
	return itr.start, itr.end
}

// Valid returns whether the current iterator is valid
func (itr *pebbleIterator) Valid() bool {
	if itr.isInvalid {
		return false
	}
	return itr.iter.Valid()
}

// Next moves the iterator to the next key in the database
func (itr *pebbleIterator) Next() {
	if itr.isInvalid {
		return
	}
	if itr.isReverse {
		itr.iter.Prev()
	} else {
		itr.iter.Next()
	}
}

// Key returns the key at the current position
func (itr *pebbleIterator) Key() []byte {
	if !itr.Valid() {
		return nil
	}
	key := itr.iter.Key()
	// Make a copy since pebble iterator keys are only valid until the next call
	keyCopy := make([]byte, len(key))
	copy(keyCopy, key)
	return keyCopy
}

// Value returns the value at the current position
func (itr *pebbleIterator) Value() []byte {
	if !itr.Valid() {
		return nil
	}
	val := itr.iter.Value()
	// Make a copy since pebble iterator values are only valid until the next call
	valCopy := make([]byte, len(val))
	copy(valCopy, val)
	return valCopy
}

// Error returns any accumulated error
func (itr *pebbleIterator) Error() error {
	return itr.iter.Error()
}

// Close closes the iterator
func (itr *pebbleIterator) Close() error {
	itr.isInvalid = true
	return itr.iter.Close()
}

// --- Batch Implementation ---

type PebbleBatch struct {
	batch *pebble.Batch
	sync.Mutex
}

func (b *PebbleBatch) Set(key, value []byte) error {
	return b.batch.Set(key, value, nil)
}

func (b *PebbleBatch) Delete(key []byte) error {
	return b.batch.Delete(key, nil)
}

func (b *PebbleBatch) Write() error {
	return b.batch.Commit(pebble.Sync)
}

func (b *PebbleBatch) WriteSync() error {
	return b.batch.Commit(pebble.Sync)
}

func (b *PebbleBatch) Close() error {
	return b.batch.Close()
}