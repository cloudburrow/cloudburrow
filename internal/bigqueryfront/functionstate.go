package bigqueryfront

// The functions the front knows of (knownFunctions), kept in a file on the
// front's state directory, beside the Storage Write streams (#1115).
//
// The emulator has no routines.list, so the front learns a dataset's
// functions from the statements and routines.insert calls it sends on,
// and, the first time a project's are asked for after it starts, from
// every job the emulator ran before then (scanJobs: a jobs.get each).
// Measured on an instance with 587 jobs, that first scan made the DROP
// SCHEMA that asked for it take 97 seconds, so every restart of the
// front's container, the emulator still running, made its next DROP
// SCHEMA or bare function name slow past a client's deadline. Kept here,
// what the front knew, and whether it had scanned, outlive a restart of
// its container, and go with the pod, as the emulator's jobs and
// functions do; the emulator's restart (reset) empties the file too.
// Nothing reaches the emulator while the front is down: it listens on
// the pod's loopback alone (#1114).

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// functionsStateFile is the file the functions are kept in, in dir.
func functionsStateFile(dir string) string { return filepath.Join(dir, "functions.json") }

// writeStateDir is the directory the Storage Write streams are kept in,
// in dir; "" for none.
func writeStateDir(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "storage-write")
}

// savedFunctions is the functions file's content.
type savedFunctions struct {
	Funcs   map[string]map[string][]string `json:"funcs,omitempty"`
	Scanned map[string]bool                `json:"scanned,omitempty"`
	// Started is when the front that scanned started, Unix milliseconds.
	Started int64 `json:"started"`
}

// keep restores the functions from path, when it holds them, and from then
// on keeps them there. It must be called before the front serves. A file
// that cannot be read is logged and replaced: the front starts knowing
// none, and scans.
func (k *knownFunctions) keep(path string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	b, err := os.ReadFile(path)
	var s savedFunctions
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		logf("bigquery front: read %s: %v; starting without the functions it knew", path, err)
	case json.Unmarshal(b, &s) != nil:
		logf("bigquery front: %s is unreadable; starting without the functions it knew", path)
		s = savedFunctions{}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if s.Funcs != nil || s.Scanned != nil {
		k.funcs, k.scanned, k.started = s.Funcs, s.Scanned, s.Started
		logf("bigquery front: restored the functions it knew from %s", path)
	}
	k.path, k.logf = path, logf
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logf("bigquery front: %v", err)
	}
	k.saveLocked()
}

// saveLocked writes the functions, when they are kept; k.mu is held.
func (k *knownFunctions) saveLocked() {
	if k == nil || k.path == "" {
		return
	}
	b, err := json.Marshal(savedFunctions{Funcs: k.funcs, Scanned: k.scanned, Started: k.started})
	if err == nil {
		err = writeFileAtomic(k.path, b)
	}
	if err != nil && k.logf != nil {
		k.logf("bigquery front: keeping the functions it knows in %s: %v", k.path, err)
	}
}
