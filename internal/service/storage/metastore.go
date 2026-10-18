package storage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// The metadata store holds buckets, object generations and upload sessions
// (#489). It is not store.Durable, which rewrites its whole map on every
// write, nor a KubeStore: object metadata changes on every upload, so each
// write must cost what it writes.
//
// The durable form is an append-only log of transactions. Each record is one
// JSON line carrying its operations and a CRC32C of them, fsynced before the
// transaction returns, so a transaction is all or nothing: a record torn by a
// crash fails its checksum and is dropped on replay, with everything after it.
// When the log outgrows the live data it is compacted: the live map is written
// to a snapshot file, fsynced and renamed into place, and the log restarts.
// A dependency such as bbolt is not needed for this: the metadata is small,
// there is one writer process, and the log is a few hundred lines of Go.

// Tx is one transaction's view: reads see its own writes.
type Tx interface {
	Get(key string) ([]byte, bool)
	Put(key string, value []byte)
	// Append adds elem, one JSON value, to the end of the JSON array at key
	// (an absent key is an empty array). The log records elem alone, so an
	// append costs what it adds however long the array is (#1609).
	Append(key string, elem []byte)
	Delete(key string)
	// List returns the keys under prefix, sorted.
	List(prefix string) []string
}

// MetaStore is the metadata store.
type MetaStore interface {
	// View runs fn on a consistent snapshot.
	View(fn func(Tx) error) error
	// Update runs fn and commits its writes atomically if it returns nil.
	Update(fn func(Tx) error) error
	Close() error
}

// memTx buffers a transaction's writes over a base map.
type memTx struct {
	base    map[string][]byte
	writes  map[string][]byte // nil value: deleted
	ordered []op
}

type op struct {
	Key   string `json:"k"`
	Value []byte `json:"v,omitempty"`
	Del   bool   `json:"d,omitempty"`
	// Append: Value is one JSON value appended to the array at Key.
	Append bool `json:"a,omitempty"`
}

func (t *memTx) Get(key string) ([]byte, bool) {
	if v, ok := t.writes[key]; ok {
		return v, v != nil
	}
	v, ok := t.base[key]
	return v, ok
}

func (t *memTx) Put(key string, value []byte) {
	if t.writes == nil {
		t.writes = map[string][]byte{}
	}
	// Never nil, even for an empty value: a nil write is a deletion.
	v := append([]byte{}, value...)
	t.writes[key] = v
	t.ordered = append(t.ordered, op{Key: key, Value: v})
}

func (t *memTx) Append(key string, elem []byte) {
	old, _ := t.Get(key)
	if t.writes == nil {
		t.writes = map[string][]byte{}
	}
	e := append([]byte(nil), elem...)
	t.writes[key] = appendJSON(old, e)
	t.ordered = append(t.ordered, op{Key: key, Value: e, Append: true})
}

// appendJSON returns a new array: arr (a JSON array, or empty for none)
// with elem added at its end. arr is not modified.
func appendJSON(arr, elem []byte) []byte {
	arr = bytes.TrimRight(arr, " \t\r\n")
	inner := bytes.TrimSpace(bytes.TrimSuffix(bytes.TrimPrefix(bytes.TrimSpace(arr), []byte("[")), []byte("]")))
	out := make([]byte, 0, len(arr)+len(elem)+2)
	if len(inner) == 0 {
		out = append(out, '[')
	} else {
		out = append(out, arr[:len(arr)-1]...)
		out = append(out, ',')
	}
	out = append(out, elem...)
	return append(out, ']')
}

func (t *memTx) Delete(key string) {
	if t.writes == nil {
		t.writes = map[string][]byte{}
	}
	t.writes[key] = nil
	t.ordered = append(t.ordered, op{Key: key, Del: true})
}

func (t *memTx) List(prefix string) []string {
	seen := map[string]bool{}
	for k := range t.base {
		if strings.HasPrefix(k, prefix) {
			seen[k] = true
		}
	}
	for k, v := range t.writes {
		if strings.HasPrefix(k, prefix) {
			seen[k] = v != nil
		}
	}
	var out []string
	for k, live := range seen {
		if live {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// apply replays a logged transaction's operations, in order.
func apply(m map[string][]byte, ops []op) {
	for _, o := range ops {
		switch {
		case o.Del:
			delete(m, o.Key)
		case o.Append:
			m[o.Key] = appendJSON(m[o.Key], o.Value)
		default:
			m[o.Key] = o.Value
		}
	}
}

// commit stores a transaction's final writes in m, and returns how much the
// live size changed. It is what apply of the transaction's operations
// would leave, without merging an append twice.
func commit(m map[string][]byte, writes map[string][]byte) (delta int64) {
	for k, v := range writes {
		if old, ok := m[k]; ok {
			delta -= int64(len(k) + len(old))
		}
		if v == nil {
			delete(m, k)
			continue
		}
		m[k] = v
		delta += int64(len(k) + len(v))
	}
	return delta
}

// MemMetaStore is the in-memory store, for ephemeral mode and tests.
type MemMetaStore struct {
	mu   sync.RWMutex
	data map[string][]byte
}

// NewMemMetaStore returns an empty store.
func NewMemMetaStore() *MemMetaStore { return &MemMetaStore{data: map[string][]byte{}} }

func (s *MemMetaStore) View(fn func(Tx) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fn(&memTx{base: s.data})
}

func (s *MemMetaStore) Update(fn func(Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := &memTx{base: s.data}
	if err := fn(tx); err != nil {
		return err
	}
	commit(s.data, tx.writes)
	return nil
}

func (s *MemMetaStore) Close() error { return nil }

// LogMetaStore is the durable store: snapshot.json plus log.jsonl in dir.
type LogMetaStore struct {
	mu       sync.RWMutex
	dir      string
	data     map[string][]byte
	log      *os.File
	logBytes int64
	liveSize int64
}

type logRecord struct {
	Ops []op   `json:"ops"`
	CRC uint32 `json:"crc"`
}

func opsCRC(ops []op) uint32 {
	b, _ := json.Marshal(ops)
	return crc32.Checksum(b, castagnoli)
}

// OpenLogMetaStore opens or creates the store in dir, replaying the log over
// the snapshot and dropping a torn tail.
func OpenLogMetaStore(dir string) (*LogMetaStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &LogMetaStore{dir: dir, data: map[string][]byte{}}
	if b, err := os.ReadFile(filepath.Join(dir, "snapshot.json")); err == nil {
		if err := json.Unmarshal(b, &s.data); err != nil {
			return nil, fmt.Errorf("Cloud Storage metadata snapshot is unreadable: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	logPath := filepath.Join(dir, "log.jsonl")
	good, err := s.replay(logPath)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(logPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	// Cut a torn tail off, so the next record starts on a clean line.
	if err := f.Truncate(good); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(good, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	s.log, s.logBytes = f, good
	s.recountLive()
	return s, nil
}

// replay applies each whole, checksummed record, and returns how many bytes
// of the log were good.
func (s *LogMetaStore) replay(path string) (int64, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var good int64
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return good, nil // io.EOF, or a last line with no newline: torn
		}
		var rec logRecord
		if json.Unmarshal(line, &rec) != nil || rec.CRC != opsCRC(rec.Ops) {
			return good, nil
		}
		apply(s.data, rec.Ops)
		good += int64(len(line))
	}
}

func (s *LogMetaStore) recountLive() {
	s.liveSize = 0
	for k, v := range s.data {
		s.liveSize += int64(len(k) + len(v))
	}
}

func (s *LogMetaStore) View(fn func(Tx) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fn(&memTx{base: s.data})
}

func (s *LogMetaStore) Update(fn func(Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := &memTx{base: s.data}
	if err := fn(tx); err != nil {
		return err
	}
	if len(tx.ordered) == 0 {
		return nil
	}
	line, err := json.Marshal(logRecord{Ops: tx.ordered, CRC: opsCRC(tx.ordered)})
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if _, err := s.log.Write(line); err != nil {
		return fmt.Errorf("write Cloud Storage metadata: %w", err)
	}
	if err := s.log.Sync(); err != nil {
		return fmt.Errorf("sync Cloud Storage metadata: %w", err)
	}
	s.logBytes += int64(len(line))
	// The live size is kept up to date by what changed: counting every key
	// again at each write cost a pass over the whole store.
	s.liveSize += commit(s.data, tx.writes)
	// Compact once the log is several times the live data, and big enough
	// for it to matter.
	if s.logBytes > 1<<20 && s.logBytes > 4*s.liveSize {
		return s.compact()
	}
	return nil
}

// compact writes the live map as the snapshot and restarts the log. A crash
// before the rename leaves the old snapshot and the full log, which replay to
// the same state; after it, the new snapshot plus a log that replays onto it
// harmlessly (every operation is idempotent).
func (s *LogMetaStore) compact() error {
	b, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, "snapshot.json.tmp")
	if err := writeFileSync(tmp, b); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, "snapshot.json")); err != nil {
		return err
	}
	if err := s.log.Truncate(0); err != nil {
		return err
	}
	if _, err := s.log.Seek(0, io.SeekStart); err != nil {
		return err
	}
	s.logBytes = 0
	return s.log.Sync()
}

func writeFileSync(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (s *LogMetaStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.log.Close()
}
