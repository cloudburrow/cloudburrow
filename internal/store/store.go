// Package store abstracts resource metadata storage behind one interface with
// an in-memory mode and a durable mode, including atomic multi-key commits and
// single-instance ownership of a data directory.
//
// Under the Kubernetes architecture most application state lives in the
// cluster (ADR-0005). This store holds the metadata CloudBurrow itself owns —
// Cloud Tasks queues and tasks, and instance bookkeeping — which no upstream
// component holds for us.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Errors callers are expected to distinguish.
var (
	ErrNotFound = errors.New("key not found")
	// ErrConflict means a precondition failed, so the write was refused.
	ErrConflict = errors.New("write conflict")
	// ErrLocked means another instance owns the data directory.
	ErrLocked = errors.New("data directory is in use by another instance")
	// ErrUnsafeKey means a key could address something outside its namespace.
	ErrUnsafeKey = errors.New("unsafe key")
)

// Store is metadata storage.
type Store interface {
	Get(key string) ([]byte, error)
	// Put writes a value. Commit applies several writes atomically.
	Put(key string, value []byte) error
	Delete(key string) error
	// List returns keys under a prefix, in sorted order.
	List(prefix string) ([]string, error)
	// Commit applies a set of writes atomically: either all land or none do.
	Commit(ops []Op) error
	Close() error
}

// OpKind is the kind of write in a Commit.
type OpKind int

const (
	OpPut OpKind = iota
	OpDelete
)

// Op is one write in an atomic commit.
type Op struct {
	Kind  OpKind
	Key   string
	Value []byte
}

// SafeKey reports whether a key can be used without escaping its namespace.
//
// Keys reach the filesystem in durable mode, so traversal must be impossible
// rather than merely unlikely.
func SafeKey(key string) bool {
	if key == "" || strings.HasPrefix(key, "/") {
		return false
	}
	lower := strings.ToLower(key)
	for _, bad := range []string{"..", "%2e%2e", "%2f", "%5c", "\x00", `\`} {
		if strings.Contains(lower, bad) {
			return false
		}
	}
	return true
}

func checkKey(key string) error {
	if !SafeKey(key) {
		return fmt.Errorf("%w: %q", ErrUnsafeKey, key)
	}
	return nil
}

// Memory is an in-process store. It leaves nothing durable behind.
type Memory struct {
	mu   sync.RWMutex
	data map[string][]byte
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory { return &Memory{data: map[string][]byte{}} }

func (m *Memory) Get(key string) ([]byte, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.data[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return append([]byte(nil), v...), nil
}

func (m *Memory) Put(key string, value []byte) error {
	return m.Commit([]Op{{Kind: OpPut, Key: key, Value: value}})
}

func (m *Memory) Delete(key string) error {
	return m.Commit([]Op{{Kind: OpDelete, Key: key}})
}

func (m *Memory) List(prefix string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	for k := range m.data {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Commit validates every key before applying any write, so a bad key in the
// middle of a batch cannot leave the store half-updated.
func (m *Memory) Commit(ops []Op) error {
	for _, op := range ops {
		if err := checkKey(op.Key); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, op := range ops {
		switch op.Kind {
		case OpPut:
			m.data[op.Key] = append([]byte(nil), op.Value...)
		case OpDelete:
			delete(m.data, op.Key)
		}
	}
	return nil
}

func (m *Memory) Close() error { return nil }

// Durable persists metadata under a data directory owned by one instance.
//
// The format is deliberately simple: one JSON file holding the whole map,
// replaced atomically. The volume of metadata CloudBurrow owns is small, and a
// format that can be inspected with `cat` is worth more here than one that
// scales.
type Durable struct {
	dir  string
	file string
	lock *os.File

	mu   sync.RWMutex
	data map[string][]byte
}

// OpenDurable claims a data directory and loads any existing state.
//
// Ownership is claimed with a lock file. A second instance pointed at a live
// directory refuses to start rather than interleaving writes and corrupting
// both copies.
func OpenDurable(dir string) (*Durable, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	lockPath := filepath.Join(dir, "owner.lock")
	// O_EXCL is the claim: it fails if the file already exists.
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
	if err != nil && os.IsExist(err) && !lockIsLive(lockPath) {
		// The owner is gone. A file on its own cannot tell a live instance
		// from a machine that lost power, so the recorded PID is consulted
		// and a lock belonging to no running process is reclaimed rather than
		// left to be deleted by hand before every subsequent start.
		//
		// Two instances racing here could both reclaim, which is why the
		// claim is re-attempted with O_EXCL rather than assumed: the loser
		// gets the ordinary refusal. That window is one syscall wide, and the
		// alternative — refusing forever after any unclean exit — is the
		// failure people actually hit.
		_ = os.Remove(lockPath)
		lock, err = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
	}
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("%w: %s (remove %s if no instance is running)", ErrLocked, dir, lockPath)
		}
		return nil, err
	}
	if _, err := fmt.Fprintf(lock, "%d\n", os.Getpid()); err != nil {
		_ = lock.Close()
		return nil, err
	}

	d := &Durable{dir: dir, file: filepath.Join(dir, "metadata.json"), lock: lock, data: map[string][]byte{}}
	if err := d.load(); err != nil {
		_ = d.Close()
		return nil, err
	}
	return d, nil
}

// lockIsLive reports whether the process named in a lock file still exists.
//
// Unreadable or malformed content counts as live: the whole point of the lock
// is to refuse rather than to guess, and a file this code cannot understand is
// not evidence that nothing owns the directory.
//
// A reused PID would also count as live and refuse a start that could have
// succeeded. That is the safe direction to be wrong in — the other one lets
// two instances write the same metadata file.
func lockIsLive(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return true
	}
	if pid == os.Getpid() {
		return true
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 performs the permission and existence checks without
	// delivering anything. ESRCH is "no such process"; EPERM means it exists
	// and belongs to somebody else, which is still a live owner.
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}

func (d *Durable) load() error {
	b, err := os.ReadFile(d.file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read metadata: %w", err)
	}
	var raw map[string][]byte
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("parse metadata: %w", err)
	}
	d.data = raw
	return nil
}

// flush writes the whole map through a temporary file and renames it.
//
// Rename is atomic on POSIX, so a crash mid-write leaves either the previous
// state or the new one, never a truncated file.
func (d *Durable) flush() error {
	b, err := json.Marshal(d.data)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(d.dir, "metadata-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	// fsync before rename: a rename is atomic, but without the sync the
	// contents may not have reached disk when the rename does.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, d.file)
}

func (d *Durable) Get(key string) ([]byte, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	v, ok := d.data[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return append([]byte(nil), v...), nil
}

func (d *Durable) Put(key string, value []byte) error {
	return d.Commit([]Op{{Kind: OpPut, Key: key, Value: value}})
}

func (d *Durable) Delete(key string) error {
	return d.Commit([]Op{{Kind: OpDelete, Key: key}})
}

func (d *Durable) List(prefix string) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var out []string
	for k := range d.data {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Commit applies every write and then flushes once.
//
// Atomicity comes from staging into a copy: if the flush fails, the in-memory
// map is left untouched, so memory and disk cannot disagree.
func (d *Durable) Commit(ops []Op) error {
	for _, op := range ops {
		if err := checkKey(op.Key); err != nil {
			return err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	staged := make(map[string][]byte, len(d.data))
	for k, v := range d.data {
		staged[k] = v
	}
	for _, op := range ops {
		switch op.Kind {
		case OpPut:
			staged[op.Key] = append([]byte(nil), op.Value...)
		case OpDelete:
			delete(staged, op.Key)
		}
	}

	prev := d.data
	d.data = staged
	if err := d.flush(); err != nil {
		d.data = prev
		return fmt.Errorf("commit failed, state unchanged: %w", err)
	}
	return nil
}

// Close releases the data directory.
func (d *Durable) Close() error {
	if d.lock == nil {
		return nil
	}
	name := d.lock.Name()
	err := d.lock.Close()
	d.lock = nil
	if rmErr := os.Remove(name); rmErr != nil && err == nil {
		err = rmErr
	}
	return err
}
