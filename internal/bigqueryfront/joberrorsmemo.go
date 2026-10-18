package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"sync"
)

// mappedJobErrors are the errors of DONE jobs as mapJobs mapped them, by
// project, job ID and the engine's errors (#1628).
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
// mapping kept, the maxMappedJobErrors most recent.
type mappedJobErrors struct {
	mu    sync.Mutex
	m     map[string][]byte // nil: mapped to themselves
	order []string
}

const maxMappedJobErrors = 20000

// key is the memo's key of the DONE job with status, or "".
func (m *mappedJobErrors) key(job, status map[string]any) string {
	if m == nil {
		return ""
	}
	if st, _ := status["state"].(string); st != "DONE" {
		return ""
	}
	p, id := jobRef(job)
	if id == "" {
		return ""
	}
	orig, err := json.Marshal([]any{status["errorResult"], status["errors"]})
	if err != nil {
		return ""
	}
	return p + "/" + id + "\x00" + string(orig)
}

// apply puts the mapping kept of key in status, and reports whether one
// was kept and whether it changed status.
func (m *mappedJobErrors) apply(key string, status map[string]any) (kept, changed bool) {
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

// put keeps the mapping of key, status as mapped, nil when unchanged.
func (m *mappedJobErrors) put(key string, status map[string]any, changed bool) {
	var b []byte
	if changed {
		var err error
		if b, err = json.Marshal([]any{status["errorResult"], status["errors"]}); err != nil {
			return
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.m == nil {
		m.m = map[string][]byte{}
	}
	if _, ok := m.m[key]; !ok {
		m.order = append(m.order, key)
	}
	m.m[key] = b
	for len(m.order) > maxMappedJobErrors {
		delete(m.m, m.order[0])
		m.order = m.order[1:]
	}
}

// reset forgets the mappings: the emulator restarted (restart.go).
func (m *mappedJobErrors) reset() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m, m.order = nil, nil
}
