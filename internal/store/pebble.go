// PebbleDB wrapper tuân thủ interface dbm.DB của CometBFT

package store

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/cockroachdb/pebble"
	dbm "github.com/cometbft/cometbft-db"
)

// PebbleDB implements dbm.DB
type PebbleDB struct {
	db *pebble.DB
}

// NewPebbleDB khởi tạo PebbleDB
func NewPebbleDB(name string, dir string) (*PebbleDB, error) {
	dbPath := filepath.Join(dir, name+".db")

	// Tối ưu ghi blockchain
	cache := pebble.NewCache(512 << 20)
	opts := &pebble.Options{
		Cache:        cache,    // 512MB cache
		MemTableSize: 64 << 20, // 64MB memtable
	}

	db, err := pebble.Open(dbPath, opts)
	// pebble.Open retains its own cache reference. Release the creator's
	// reference so repeated opens do not leak cache objects.
	cache.Unref()
	if err != nil {
		return nil, err
	}
	return &PebbleDB{db: db}, nil
}

// Get đọc value từ key
func (pdb *PebbleDB) Get(key []byte) ([]byte, error) {
	val, closer, err := pdb.db.Get(key)
	if err == pebble.ErrNotFound {
		return nil, nil // Trả về nil nếu ko tìm thấy (CometBFT spec)
	}
	if err != nil {
		return nil, err
	}
	// Copy data trước khi close
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

// Set ghi key-value
func (pdb *PebbleDB) Set(key, value []byte) error {
	return pdb.db.Set(key, value, pebble.Sync)
}

// SetSync đồng bộ
func (pdb *PebbleDB) SetSync(key, value []byte) error {
	return pdb.db.Set(key, value, pebble.Sync)
}

// Delete xoá key
func (pdb *PebbleDB) Delete(key []byte) error {
	return pdb.db.Delete(key, pebble.Sync)
}

// DeleteSync xoá key đồng bộ
func (pdb *PebbleDB) DeleteSync(key []byte) error {
	return pdb.db.Delete(key, pebble.Sync)
}

// Close đóng DB
func (pdb *PebbleDB) Close() error {
	return pdb.db.Close()
}

// NewBatch tạo batch
func (pdb *PebbleDB) NewBatch() dbm.Batch {
	return &PebbleBatch{batch: pdb.db.NewBatch()}
}

// Print in metrics
func (pdb *PebbleDB) Print() error {
	fmt.Println(pdb.db.Metrics().String())
	return nil
}

// Stats trả về metrics
func (pdb *PebbleDB) Stats() map[string]string {
	return map[string]string{
		"metrics": pdb.db.Metrics().String(),
	}
}

// Iterator duyệt key tăng dần
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

// ReverseIterator duyệt key giảm dần
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

// Compact dọn dẹp DB
func (pdb *PebbleDB) Compact(start, end []byte) error {
	return pdb.db.Compact(start, end, true)
}

// Iterator

type pebbleIterator struct {
	iter      *pebble.Iterator
	start     []byte
	end       []byte
	isReverse bool
	isInvalid bool
}

// Domain trả về bounds [start, end)
func (itr *pebbleIterator) Domain() ([]byte, []byte) {
	return itr.start, itr.end
}

// Valid kiểm tra iterator hợp lệ
func (itr *pebbleIterator) Valid() bool {
	if itr.isInvalid {
		return false
	}
	return itr.iter.Valid()
}

// Next chuyển sang key tiếp theo
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

// Key trả về key hiện tại
func (itr *pebbleIterator) Key() []byte {
	if !itr.Valid() {
		return nil
	}
	key := itr.iter.Key()
	// Copy key vì key chỉ valid đến call tiếp theo
	keyCopy := make([]byte, len(key))
	copy(keyCopy, key)
	return keyCopy
}

// Value trả về value hiện tại
func (itr *pebbleIterator) Value() []byte {
	if !itr.Valid() {
		return nil
	}
	val := itr.iter.Value()
	// Copy value vì value chỉ valid đến call tiếp theo
	valCopy := make([]byte, len(val))
	copy(valCopy, val)
	return valCopy
}

// Error trả về lỗi nếu có
func (itr *pebbleIterator) Error() error {
	return itr.iter.Error()
}

// Close đóng iterator
func (itr *pebbleIterator) Close() error {
	itr.isInvalid = true
	return itr.iter.Close()
}

// Batch

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
