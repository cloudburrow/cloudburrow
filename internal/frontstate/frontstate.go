// Package frontstate keeps a front's records and data copies in a
// directory (#1514), as the Pub/Sub (#898) and BigQuery (#1115) fronts
// keep theirs: `cloudburrow up` mounts it on a size-limited emptyDir of the
// front's pod, so a restart of the front's container alone keeps them, and
// a restart of the pod loses them with the emulator's data they describe.
//
// The records are one JSON file, replaced whole by a rename, so a reader
// never sees half of one. A data copy (a backup's rows, a snapshot's cells,
// a deleted table kept for undelete) is a file of its own, written once
// when the copy is first saved and removed when the copy is gone, so the
// records file stays small whatever the copies hold.
package frontstate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const (
	stateFile = "state.json"
	copiesDir = "copies"
)

// Store is a front's state directory.
type Store struct {
	dir string
	mu  sync.Mutex
	// last is the records last written; an unchanged save writes nothing.
	last []byte
	// written is the copy last written to each copy file, by file name.
	written map[string]any
}

// Open makes dir and its copies directory, and returns its store.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, copiesDir), 0o700); err != nil {
		return nil, fmt.Errorf("the front's state directory %s: %w", dir, err)
	}
	return &Store{dir: dir, written: map[string]any{}}, nil
}

// Load reads the records into v, reporting whether there were any.
func (s *Store) Load(v any) (bool, error) {
	b, err := os.ReadFile(filepath.Join(s.dir, stateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("the records in %s: %w", s.dir, err)
	}
	return true, nil
}

// Copy is one data copy to keep: Data is the copy itself, whose identity
// says whether it was already written, and Encode its content.
type Copy struct {
	Data   any
	Encode func() ([]byte, error)
}

// Save writes the records v, the copies a front still holds, by key, and
// removes the copy files of the copies it no longer holds. A key names one
// copy for its whole life: a copy taken again under the same name is a
// different Data, and is written again.
func (s *Store) Save(v any, copies map[string]Copy) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode the front's records: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := make(map[string]bool, len(copies))
	for key, c := range copies {
		name := fileName(key)
		keep[name] = true
		if s.written[name] == c.Data {
			continue
		}
		data, err := c.Encode()
		if err != nil {
			return fmt.Errorf("encode the copy %s: %w", key, err)
		}
		if err := writeAtomic(filepath.Join(s.dir, copiesDir, name), data); err != nil {
			return err
		}
		s.written[name] = c.Data
	}
	// The records go after the copies they name, so a records file never
	// names a copy that is not there.
	if string(b) != string(s.last) {
		if err := writeAtomic(filepath.Join(s.dir, stateFile), b); err != nil {
			return err
		}
		s.last = b
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, copiesDir))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !keep[e.Name()] {
			_ = os.Remove(filepath.Join(s.dir, copiesDir, e.Name()))
			delete(s.written, e.Name())
		}
	}
	return nil
}

// ReadCopy returns the content of the copy saved under key.
func (s *Store) ReadCopy(key string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.dir, copiesDir, fileName(key)))
}

// MarkRead records that data is what the copy file of key holds, so the
// next Save does not write it again.
func (s *Store) MarkRead(key string, data any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written[fileName(key)] = data
}

// Clear removes the records and every copy (`cloudburrow reset`).
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = nil
	s.written = map[string]any{}
	if err := os.Remove(filepath.Join(s.dir, stateFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, copiesDir))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(s.dir, copiesDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func fileName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func writeAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("write the front's state: %w", err)
	}
	_, werr := tmp.Write(b)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write the front's state %s: %w", path, werr)
	}
	return nil
}

// Pack joins parts into one copy file's content; Unpack splits it.
func Pack(parts ...[]byte) []byte {
	var b []byte
	for _, p := range parts {
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendBytes(b, p)
	}
	return b
}

// Unpack splits what Pack joined.
func Unpack(b []byte) ([][]byte, error) {
	var parts [][]byte
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 || num != 1 || typ != protowire.BytesType {
			return nil, errors.New("not a packed copy")
		}
		b = b[n:]
		p, n := protowire.ConsumeBytes(b)
		if n < 0 {
			return nil, errors.New("a packed copy cut short")
		}
		parts = append(parts, p)
		b = b[n:]
	}
	return parts, nil
}

// EncodeMap is m's messages in their wire form, for the records.
func EncodeMap[M proto.Message](m map[string]M) (map[string][]byte, error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string][]byte, len(m))
	for k, v := range m {
		b, err := proto.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("encode %s: %w", k, err)
		}
		out[k] = b
	}
	return out, nil
}

// DecodeMap is the messages EncodeMap encoded, each made by newM.
func DecodeMap[M proto.Message](in map[string][]byte, newM func() M) (map[string]M, error) {
	out := make(map[string]M, len(in))
	for k, b := range in {
		m := newM()
		if err := proto.Unmarshal(b, m); err != nil {
			return nil, fmt.Errorf("decode %s: %w", k, err)
		}
		out[k] = m
	}
	return out, nil
}

// Encode is one message's wire form; Decode the reverse.
func Encode(m proto.Message) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	return proto.Marshal(m)
}

// Decode decodes b into m when b is not empty, reporting whether it did.
func Decode(b []byte, m proto.Message) (bool, error) {
	if len(b) == 0 {
		return false, nil
	}
	return true, proto.Unmarshal(b, m)
}

// ResetPath is the path on a front's port that forgets the front's records
// and copies (`cloudburrow reset`, #1514): a POST, answered 204. With
// ?project= it forgets one project's only, and deletes that project's data
// in the emulator too, as a project's purge does on Google (#1540).
const ResetPath = "/cloudburrow/front-state/reset"

// ServeReset answers a call to ResetPath with reset, passing it the
// project= parameter: "" for every project.
func ServeReset(w http.ResponseWriter, r *http.Request, reset func(project string) error) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := reset(r.URL.Query().Get("project")); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DropProject deletes from m every key naming a resource of project, those
// beginning "projects/<project>/" (#1540).
func DropProject[T any](m map[string]T, project string) {
	prefix := "projects/" + project + "/"
	for k := range m {
		if strings.HasPrefix(k, prefix) || k == "projects/"+project {
			delete(m, k)
		}
	}
}

// KeepOthers returns the names in names not of project's resources.
func KeepOthers(names []string, project string) []string {
	prefix := "projects/" + project + "/"
	out := names[:0:0]
	for _, n := range names {
		if !strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	return out
}
