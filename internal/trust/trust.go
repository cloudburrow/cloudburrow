// Package trust records which directories' configuration and hooks the
// developer has agreed to run (#598).
//
// `up` reads ./cloudburrow.json and runs .cloudburrow/hooks from the
// directory it is started in without either being named, which is the
// checked-in-script problem of `make` in a cloned repository. The first `up`
// in such a directory refuses and lists the files; `up --trust` or
// `cloudburrow trust` records a hash of their paths and contents, and any
// change to them asks again.
//
// The record lives in the developer's state directory, never in a directory
// the repository controls, so a repository cannot vouch for itself.
package trust

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// FileName is the trust store's name in the state directory.
const FileName = "trust.json"

// Entry is one directory's trust record.
type Entry struct {
	SHA256  string    `json:"sha256"`
	Files   []string  `json:"files"`
	Trusted time.Time `json:"trusted"`
}

type store struct {
	Dirs map[string]Entry `json:"dirs"`
}

// Hash digests files' paths, executable bits and contents, in path order.
// A file added, removed, renamed, made executable or edited changes it.
func Hash(files []string) (string, error) {
	sorted := append([]string(nil), files...)
	sort.Strings(sorted)
	h := sha256.New()
	for _, p := range sorted {
		info, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		f, err := os.Open(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%o\x00%d\x00", p, info.Mode().Perm()&0o111, info.Size())
		_, err = io.Copy(h, f)
		_ = f.Close()
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func load(stateDir string) (store, error) {
	s := store{Dirs: map[string]Entry{}}
	b, err := os.ReadFile(filepath.Join(stateDir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("read %s: %w", filepath.Join(stateDir, FileName), err)
	}
	if s.Dirs == nil {
		s.Dirs = map[string]Entry{}
	}
	return s, nil
}

// Lookup returns dir's record, if there is one.
func Lookup(stateDir, dir string) (Entry, bool, error) {
	s, err := load(stateDir)
	if err != nil {
		return Entry{}, false, err
	}
	e, ok := s.Dirs[dir]
	return e, ok, nil
}

// Record trusts files, as hashed to sum, for dir. The store is replaced
// atomically, so a concurrent reader sees the old record or the new one.
func Record(stateDir, dir, sum string, files []string) error {
	s, err := load(stateDir)
	if err != nil {
		return err
	}
	sorted := append([]string(nil), files...)
	sort.Strings(sorted)
	s.Dirs[dir] = Entry{SHA256: sum, Files: sorted, Trusted: time.Now().UTC()}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(stateDir, FileName+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(stateDir, FileName))
}
