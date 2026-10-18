package bigqueryfront

// The jobs whose text or statistics the front changed (jobTexts), kept in
// a file on the front's state directory, beside the functions (#1028).
//
// What the front reports of a job and the emulator does not (a DML
// statement's counts, #1008; the client's text, parameters, destination
// and dispositions) was in its memory alone, so after a restart of the
// front's container, the emulator still running with those jobs, jobs.get
// of an earlier DML job reported no count again. Kept here, it outlives a
// restart of the container and goes with the pod, as the emulator's jobs
// do; the emulator's restart (reset) empties the file too.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// jobTextsStateFile is the file the job texts are kept in, in dir.
func jobTextsStateFile(dir string) string { return filepath.Join(dir, "jobtexts.json") }

// savedJobText is a jobText as the file keeps it.
type savedJobText struct {
	Key              string            `json:"key"`
	Query            string            `json:"query,omitempty"`
	URIs             []string          `json:"uris,omitempty"`
	Names            map[string]string `json:"names,omitempty"`
	DML              *savedDML         `json:"dml,omitempty"`
	WriteDisposition string            `json:"writeDisposition,omitempty"`
	Params           json.RawMessage   `json:"params,omitempty"`
	ParamMode        string            `json:"paramMode,omitempty"`
	ParamsSet        bool              `json:"paramsSet,omitempty"`
	Dest             *tableRef         `json:"dest,omitempty"`
	QueryWrite       string            `json:"queryWrite,omitempty"`
	Load             json.RawMessage   `json:"load,omitempty"`
	DDL              *savedDDL         `json:"ddl,omitempty"`
}

type savedDDL struct {
	StatementType string   `json:"statementType"`
	Operation     string   `json:"operation"`
	Target        tableRef `json:"target"`
}

type savedDML struct {
	StatementType string `json:"statementType"`
	Inserted      int64  `json:"inserted,omitempty"`
	Updated       int64  `json:"updated,omitempty"`
	Deleted       int64  `json:"deleted,omitempty"`
}

func saveJobText(key string, t jobText) savedJobText {
	s := savedJobText{Key: key, Query: t.query, URIs: t.uris, Names: t.names, WriteDisposition: t.writeDisposition,
		Params: t.params, ParamMode: t.paramMode, ParamsSet: t.paramsSet, Dest: t.dest, QueryWrite: t.queryWrite, Load: t.load}
	if t.dml != nil {
		s.DML = &savedDML{t.dml.statementType, t.dml.inserted, t.dml.updated, t.dml.deleted}
	}
	if t.ddl != nil {
		s.DDL = &savedDDL{t.ddl.statementType, t.ddl.operation, t.ddl.target}
	}
	return s
}

func (s savedJobText) jobText() jobText {
	t := jobText{query: s.Query, uris: s.URIs, names: s.Names, writeDisposition: s.WriteDisposition,
		params: s.Params, paramMode: s.ParamMode, paramsSet: s.ParamsSet, dest: s.Dest, queryWrite: s.QueryWrite, load: s.Load}
	if s.DML != nil {
		t.dml = &dmlCounts{statementType: s.DML.StatementType, inserted: s.DML.Inserted, updated: s.DML.Updated, deleted: s.DML.Deleted}
	}
	if s.DDL != nil {
		t.ddl = &ownDDL{statementType: s.DDL.StatementType, operation: s.DDL.Operation, target: s.DDL.Target}
	}
	return t
}

// keep restores the job texts from path, when it holds them, and from then
// on keeps them there. It must be called before the front serves. A file
// that cannot be read is logged and replaced.
//
// The file is a line of JSON per job text added (appendLocked), a later
// line of a job replacing an earlier one; it is written again, a line per
// job kept, here and once it has twice the lines (compactLocked). A file
// of one JSON array, as the front wrote before #1629, is read too.
func (j *jobTexts) keep(path string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	b, err := os.ReadFile(path)
	var saved []savedJobText
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		logf("bigquery front: read %s: %v; starting without the job texts it kept", path, err)
	case len(bytes.TrimSpace(b)) > 0 && bytes.TrimSpace(b)[0] == '[':
		if json.Unmarshal(b, &saved) != nil {
			logf("bigquery front: %s is unreadable; starting without the job texts it kept", path)
			saved = nil
		}
	default:
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(nil, 64<<20)
		for sc.Scan() {
			var s savedJobText
			if json.Unmarshal(sc.Bytes(), &s) != nil {
				continue // a line cut short by a crash
			}
			saved = append(saved, s)
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, s := range saved {
		if s.Key == "" {
			continue
		}
		if j.texts == nil {
			j.texts = map[string]jobText{}
		}
		if _, ok := j.texts[s.Key]; !ok {
			j.order = append(j.order, s.Key)
		}
		j.texts[s.Key] = s.jobText()
	}
	for len(j.order) > maxJobTexts {
		delete(j.texts, j.order[0])
		j.order = j.order[1:]
	}
	if len(saved) > 0 {
		logf("bigquery front: restored %d job texts from %s", len(j.order), path)
	}
	j.path, j.logf = path, logf
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logf("bigquery front: %v", err)
	}
	j.saveLocked()
}

// jobTextLine is the file's line of the job text t of key.
func jobTextLine(key string, t jobText) []byte {
	b, _ := json.Marshal(saveJobText(key, t))
	return append(b, '\n')
}

// saveLocked writes the job texts, when they are kept, a line each; j.mu
// is held.
func (j *jobTexts) saveLocked() {
	if j == nil || j.path == "" {
		return
	}
	var out bytes.Buffer
	for _, k := range j.order {
		out.Write(jobTextLine(k, j.texts[k]))
	}
	if err := writeFileAtomic(j.path, out.Bytes()); err != nil && j.logf != nil {
		j.logf("bigquery front: keeping the job texts in %s: %v", j.path, err)
	}
	j.lines = len(j.order)
}

// appendLocked appends the job text t of key to the file, when they are
// kept, and writes the file again (saveLocked) once it has twice the lines
// of the job texts kept, so that it never grows past them; j.mu is held.
func (j *jobTexts) appendLocked(key string, t jobText) {
	if j == nil || j.path == "" {
		return
	}
	if j.lines >= 2*max(len(j.order), 1000) {
		j.saveLocked()
		return
	}
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, err = f.Write(jobTextLine(key, t))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil && j.logf != nil {
		j.logf("bigquery front: keeping the job text of %s in %s: %v", key, j.path, err)
	}
	j.lines++
}
