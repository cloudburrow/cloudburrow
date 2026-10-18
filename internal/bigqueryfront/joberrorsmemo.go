package bigqueryfront

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// mappedJobErrors are the errors of DONE jobs as mapJobs mapped them, by
// a digest of the project, the job ID and the engine's errors (#1628).
//
// mapJobs maps each failed job's errors the engine gives as BigQuery
// words them, and a "table not found" error asks the engine whether the
// table's dataset exists, a GET per dataset of each jobs.list. Measured
// on one instance after 200 runs of TestBigQueryInformationSchemaJobs
// (9,842 jobs, --services storage,bigquery, arm64, 2026-10-18), a jobs.list
// took 7.1 s through the front and 0.32 s from the engine, its goroutine
// in datasetExists: each run's dataset is another GET, and the
// filtered JOBS queries list every job, so the test, on one 60 s context,
// ran out of it (JOBS_BY_ORGANIZATION gave context deadline exceeded).
// A DONE job's errors do not change, so each is mapped once and the
// mapping kept, the maxMappedJobErrors most recent. A job with no errors
// has nothing to map, and is not kept.
//
// The mappings are kept in the front's state directory too
// (jobErrorsStateFile), a line per mapping, so the first jobs.list after
// a restart of the front's container does not map them all again: that
// list took about 7 s at 10,000 jobs, as the first after the 200 runs had.
type mappedJobErrors struct {
	mu    sync.Mutex
	m     map[jobErrorsKey][]byte // nil: mapped to themselves
	order []jobErrorsKey

	// path is the file the mappings are kept in (keep), or ""; lines
	// are the lines it has.
	path  string
	logf  func(string, ...any)
	lines int
}

// maxMappedJobErrors bounds the mappings kept: about 300 bytes each in
// memory (a key and a failed job's mapped errors), 6 MB at the bound.
const maxMappedJobErrors = 20000

// jobErrorsKey is the digest of a DONE job's reference and errors.
type jobErrorsKey [16]byte

// jobErrorsMapping is a mapping to keep (put): the job's key, and its
// errors as mapped, nil when mapping did not change them.
type jobErrorsMapping struct {
	key    jobErrorsKey
	mapped []byte
}

// key is the memo's key of the DONE job with status, and false for a job
// not DONE or with no errors.
func (m *mappedJobErrors) key(job, status map[string]any) (jobErrorsKey, bool) {
	if m == nil {
		return jobErrorsKey{}, false
	}
	if st, _ := status["state"].(string); st != "DONE" {
		return jobErrorsKey{}, false
	}
	if status["errorResult"] == nil && status["errors"] == nil {
		return jobErrorsKey{}, false
	}
	p, id := jobRef(job)
	if id == "" {
		return jobErrorsKey{}, false
	}
	orig, err := json.Marshal([]any{status["errorResult"], status["errors"]})
	if err != nil {
		return jobErrorsKey{}, false
	}
	h := sha256.New()
	h.Write([]byte(p + "/" + id + "\x00"))
	h.Write(orig)
	var k jobErrorsKey
	copy(k[:], h.Sum(nil))
	return k, true
}

// apply puts the mapping kept of key in status, and reports whether one
// was kept and whether it changed status.
func (m *mappedJobErrors) apply(key jobErrorsKey, status map[string]any) (kept, changed bool) {
	m.mu.Lock()
	b, ok := m.m[key]
	m.mu.Unlock()
	if !ok {
		return false, false
	}
	if b == nil {
		return true, false
	}
	var v []any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if dec.Decode(&v) != nil || len(v) != 2 {
		return false, false
	}
	if v[0] != nil {
		status["errorResult"] = v[0]
	}
	if v[1] != nil {
		status["errors"] = v[1]
	}
	return true, true
}

// mapping is the mapping of key to keep, status as mapped (changed: the
// mapping changed it), or false when it cannot be kept.
func (m *mappedJobErrors) mapping(key jobErrorsKey, status map[string]any, changed bool) (jobErrorsMapping, bool) {
	e := jobErrorsMapping{key: key}
	if changed {
		b, err := json.Marshal([]any{status["errorResult"], status["errors"]})
		if err != nil {
			return e, false
		}
		e.mapped = b
	}
	return e, true
}

// put keeps the mappings es, in memory and, when they are kept, in the
// file, one write for all of them (a jobs.list maps many).
func (m *mappedJobErrors) put(es []jobErrorsMapping) {
	if m == nil || len(es) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range es {
		m.addLocked(e)
	}
	m.trimLocked()
	if m.path == "" {
		return
	}
	if m.lines+len(es) >= 2*max(len(m.order), 1000) {
		m.saveLocked()
		return
	}
	var out bytes.Buffer
	for _, e := range es {
		out.Write(jobErrorsLine(e))
	}
	f, err := os.OpenFile(m.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, err = f.Write(out.Bytes())
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil && m.logf != nil {
		m.logf("bigquery front: keeping the mapped job errors in %s: %v", m.path, err)
	}
	m.lines += len(es)
}

func (m *mappedJobErrors) addLocked(e jobErrorsMapping) {
	if m.m == nil {
		m.m = map[jobErrorsKey][]byte{}
	}
	if _, ok := m.m[e.key]; !ok {
		m.order = append(m.order, e.key)
	}
	m.m[e.key] = e.mapped
}

func (m *mappedJobErrors) trimLocked() {
	if n := len(m.order) - maxMappedJobErrors; n > 0 {
		for _, k := range m.order[:n] {
			delete(m.m, k)
		}
		// A copy, so the array dropped from does not grow without end.
		m.order = append([]jobErrorsKey(nil), m.order[n:]...)
	}
}

// len is the number of mappings kept.
func (m *mappedJobErrors) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.m)
}

// reset forgets the mappings: the emulator restarted (restart.go).
func (m *mappedJobErrors) reset() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m, m.order = nil, nil
	m.saveLocked()
}

// jobErrorsStateFile is the file the mapped job errors are kept in, in dir.
func jobErrorsStateFile(dir string) string { return filepath.Join(dir, "joberrors.json") }

// savedJobErrors is a line of the file: a key (base64) and its errors as
// mapped, absent when mapping left them as they were.
type savedJobErrors struct {
	Key    string          `json:"k"`
	Mapped json.RawMessage `json:"v,omitempty"`
}

func jobErrorsLine(e jobErrorsMapping) []byte {
	b, _ := json.Marshal(savedJobErrors{Key: base64.RawURLEncoding.EncodeToString(e.key[:]), Mapped: e.mapped})
	return append(b, '\n')
}

// keep restores the mappings from path, when it holds them, and from then
// on keeps them there, a line per mapping put, the file written again
// once it has twice the lines of the mappings kept. It must be called
// before the front serves. A file that cannot be read is logged and
// replaced.
func (m *mappedJobErrors) keep(path string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		logf("bigquery front: read %s: %v; starting without the mapped job errors it kept", path, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 64<<20)
	n := 0
	for sc.Scan() {
		var s savedJobErrors
		if json.Unmarshal(sc.Bytes(), &s) != nil {
			continue // a line cut short by a crash
		}
		k, err := base64.RawURLEncoding.DecodeString(s.Key)
		if err != nil || len(k) != len(jobErrorsKey{}) {
			continue
		}
		e := jobErrorsMapping{}
		copy(e.key[:], k)
		if len(s.Mapped) > 0 {
			e.mapped = []byte(s.Mapped)
		}
		m.addLocked(e)
		n++
	}
	m.trimLocked()
	if n > 0 {
		logf("bigquery front: restored %d mapped job errors from %s", len(m.order), path)
	}
	m.path, m.logf = path, logf
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logf("bigquery front: %v", err)
	}
	m.saveLocked()
}

// saveLocked writes the file again, a line per mapping kept, when they
// are kept; m.mu is held.
func (m *mappedJobErrors) saveLocked() {
	if m.path == "" {
		return
	}
	var out bytes.Buffer
	for _, k := range m.order {
		out.Write(jobErrorsLine(jobErrorsMapping{key: k, mapped: m.m[k]}))
	}
	if err := writeFileAtomic(m.path, out.Bytes()); err != nil && m.logf != nil {
		m.logf("bigquery front: keeping the mapped job errors in %s: %v", m.path, err)
	}
	m.lines = len(m.order)
}
