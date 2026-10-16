package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Log Analytics linked datasets (#1423).
//
// Cloud Logging's CreateLink makes a dataset here, through the front, with
// the bucket's entries and an _AllLogs view of them
// (internal/service/logging/analytics.go). Google documents the dataset as
// "a read-only pointer to your log bucket"
// (docs.cloud.google.com/logging/docs/analyze/query-linked-dataset), so the
// front refuses, before anything runs, every write into a linked dataset
// (linkedDatasets: one the Logging server made, with the linkedLabel label
// for whoever reads it) that does not carry linkedWriterHeader, which only
// the Logging server's own requests do:
//
//   - any request that changes the dataset or a table of it: datasets.patch,
//     datasets.update, datasets.delete, tables.insert, tables.patch,
//     tables.update, tables.delete and tabledata.insertAll;
//   - a load, copy or query job whose destination table is in it;
//   - a statement that writes a table of it: DML, TRUNCATE, ALTER TABLE,
//     CREATE (any object) or DROP (any object) in it.
//
// Google does not document the error; the front answers 403 accessDenied
// (a failed job for a query job), UNVERIFIED. Reads are the emulator's.
// LinkedEntriesTable, which the views read, is left out of tables.list, as
// a linked dataset lists only its views (one per log view of the bucket,
// #1435).

const (
	// linkedLabel is logging.LinkedDatasetLabel.
	linkedLabel = "cloudburrow_logging_link"
	// linkedWriterHeader is logging.LinkedWriterHeader.
	linkedWriterHeader = "X-Cloudburrow-Logging-Link"
	// linkedEntriesTable is logging.LinkedEntriesTable.
	linkedEntriesTable = "_cloudburrow_log_entries"
)

// linkedDatasets are the linked datasets the Logging server made through
// this front, by project and dataset. The front learns of one from the
// Logging server's own datasets.insert, which carries linkedWriterHeader,
// and forgets it at the server's datasets.delete or an emulator restart
// (when the Logging server makes it again); a write to another dataset
// costs no read. With a state directory they are kept in it
// (linkedDatasetsStateFile, #1435), so a front that restarts while the
// emulator keeps running still refuses writes to them.
type linkedDatasets struct {
	mu   sync.Mutex
	m    map[string]bool
	path string
	logf func(string, ...any)
}

func linkedDatasetsStateFile(dir string) string { return filepath.Join(dir, "linkeddatasets.json") }

// keep restores the linked datasets from path and from then on keeps them
// there.
func (l *linkedDatasets) keep(path string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var saved []string
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		logf("bigquery front: read %s: %v; starting without the linked datasets it kept", path, err)
	case json.Unmarshal(b, &saved) != nil:
		logf("bigquery front: %s is unreadable; starting without the linked datasets it kept", path)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m = map[string]bool{}
	for _, k := range saved {
		l.m[k] = true
	}
	l.path, l.logf = path, logf
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logf("bigquery front: %v", err)
	}
	l.saveLocked()
}

func (l *linkedDatasets) saveLocked() {
	if l.path == "" {
		return
	}
	keys := make([]string, 0, len(l.m))
	for k := range l.m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b, err := json.Marshal(keys)
	if err == nil {
		err = writeFileAtomic(l.path, b)
	}
	if err != nil {
		l.logf("bigquery front: keeping the linked datasets in %s: %v", l.path, err)
	}
}

func (l *linkedDatasets) set(project, dataset string, on bool) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]bool{}
	}
	if on {
		l.m[project+"."+dataset] = true
	} else {
		delete(l.m, project+"."+dataset)
	}
	l.saveLocked()
}

func (l *linkedDatasets) has(project, dataset string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m[project+"."+dataset]
}

func (l *linkedDatasets) reset() {
	if l != nil {
		l.mu.Lock()
		l.m = nil
		l.saveLocked()
		l.mu.Unlock()
	}
}

// serveLinkedWriter serves the Logging server's datasets.insert and
// datasets.delete (r carries linkedWriterHeader), noting the linked
// dataset made or deleted, and reports whether r was one.
func (f front) serveLinkedWriter(w http.ResponseWriter, r *http.Request, dataset string, serve func(http.ResponseWriter, *http.Request)) bool {
	if r.Header.Get(linkedWriterHeader) == "" || f.linked == nil {
		return false
	}
	project := projectOf(f.base)
	switch {
	case r.Method == http.MethodPost && dataset == "":
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return false
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var body struct {
			DatasetReference struct {
				DatasetID string `json:"datasetId"`
			} `json:"datasetReference"`
		}
		_ = json.Unmarshal(raw, &body)
		sw := &statusWriter{ResponseWriter: w} // resumable.go
		serve(sw, r)
		if sw.status == http.StatusOK && body.DatasetReference.DatasetID != "" {
			f.linked.set(project, body.DatasetReference.DatasetID, true)
		}
		return true
	case r.Method == http.MethodDelete && dataset != "" && !strings.Contains(r.URL.Path, "/tables/"):
		serve(w, r)
		f.linked.set(project, dataset, false)
		return true
	}
	return false
}

// isLinked reports whether dataset is a Log Analytics linked dataset.
func (f front) isLinked(_ *http.Request, dataset string) bool {
	return dataset != "" && f.linked.has(projectOf(f.base), dataset)
}

// linkedWriteMessage is the message a write to a linked dataset is refused
// with (Google's wording UNVERIFIED).
func linkedWriteMessage(project, dataset, what string) string {
	return fmt.Sprintf("Access Denied: Dataset %s:%s is a Log Analytics linked dataset, which is read-only: %s cannot write to it. "+
		"Its views read the Cloud Logging bucket it is linked to; write log entries with Cloud Logging instead.", project, dataset, what)
}

// refuseLinkedWrite answers 403 accessDenied when dataset is a linked
// dataset and r is not the Logging server's, and reports whether it did.
func (f front) refuseLinkedWrite(w http.ResponseWriter, r *http.Request, dataset, what string) bool {
	if r.Header.Get(linkedWriterHeader) != "" || !f.isLinked(r, dataset) {
		return false
	}
	writeError(w, http.StatusForbidden, "accessDenied", linkedWriteMessage(projectOf(f.base), dataset, what))
	return true
}

// linkedRequestWhat names a REST write for the refusal, or "" for a read.
func linkedRequestWhat(method, table, action string) string {
	switch {
	case method == http.MethodGet || method == http.MethodHead:
		return ""
	case action == "insertAll":
		return "tabledata.insertAll"
	case table == "" && method == http.MethodPost:
		return "tables.insert"
	case table == "" && method == http.MethodDelete:
		return "datasets.delete"
	case table == "" && method == http.MethodPatch:
		return "datasets.patch"
	case table == "":
		return "datasets.update"
	case method == http.MethodDelete:
		return "tables.delete"
	case method == http.MethodPatch:
		return "tables.patch"
	case method == http.MethodPut:
		return "tables.update"
	}
	return "this request"
}

// ddlTarget returns the path a CREATE or DROP statement names, as the
// statement gives it: CREATE [OR REPLACE] [TEMP|TEMPORARY] [MATERIALIZED]
// TABLE|VIEW|FUNCTION|PROCEDURE|... [IF NOT EXISTS] path, DROP kind [IF
// EXISTS] path.
func ddlTarget(t []token) []string {
	if len(t) < 3 || t[0].kind != tokWord {
		return nil
	}
	first := strings.ToUpper(t[0].text)
	if first != "CREATE" && first != "DROP" {
		return nil
	}
	i := 1
	for i < len(t) && t[i].kind == tokWord {
		u := strings.ToUpper(t[i].text)
		if u == "OR" || u == "REPLACE" || u == "TEMP" || u == "TEMPORARY" || u == "MATERIALIZED" || u == "EXTERNAL" ||
			u == "SNAPSHOT" || u == "AGGREGATE" || u == "TABLE" || u == "VIEW" || u == "FUNCTION" || u == "PROCEDURE" ||
			u == "IF" || u == "NOT" || u == "EXISTS" || u == "SEARCH" || u == "VECTOR" || u == "INDEX" || u == "ROW" ||
			u == "ACCESS" || u == "POLICY" || u == "MODEL" || u == "CLONE" {
			i++
			continue
		}
		break
	}
	if i >= len(t) {
		return nil
	}
	parts, _ := path(t, i)
	return parts
}

// queryWritesLinked returns the linked dataset a statement of q writes
// into, with the statement's text, or "" when none does.
func (f front) queryWritesLinked(r *http.Request, q queryOptions) (dataset, stmt string) {
	toks, ok := lex(q.Query)
	if !ok {
		return "", ""
	}
	project := projectOf(f.base)
	seen := map[string]bool{}
	for _, s := range splitStatements(toks) {
		body, _, _ := stripControlFlow(s)
		parts := writtenTable(body)
		if parts == nil {
			parts = ddlTarget(body)
		}
		var ds string
		switch len(parts) {
		case 1:
			ds = defaultDatasetOf(q)
		case 2:
			ds = parts[0]
		case 3:
			if parts[0] != project {
				continue
			}
			ds = parts[1]
		default:
			continue
		}
		if ds == "" || seen[ds] {
			continue
		}
		seen[ds] = true
		if f.isLinked(r, ds) {
			return ds, statementText(q.Query, body)
		}
	}
	return "", ""
}

// refuseLinkedQuery refuses a query, before anything of it runs, that
// writes into a linked dataset, and reports whether it did.
func (f front) refuseLinkedQuery(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	if r.Header.Get(linkedWriterHeader) != "" {
		return false
	}
	upper, writes := strings.ToUpper(q.Query), false
	for _, w := range []string{"INSERT", "UPDATE", "DELETE", "MERGE", "TRUNCATE", "ALTER", "CREATE", "DROP"} {
		writes = writes || strings.Contains(upper, w)
	}
	if !writes {
		return false
	}
	dataset, stmt := f.queryWritesLinked(r, q)
	if dataset == "" {
		return false
	}
	f.failBeforeRun(w, r, q, insert, dataset, http.StatusForbidden, rowError{Reason: "accessDenied",
		Message: linkedWriteMessage(projectOf(f.base), dataset, "the statement "+stmt)})
	return true
}

// withoutLinkedEntries drops LinkedEntriesTable from a tables.list answer
// of a linked dataset, and reports whether it did.
func withoutLinkedEntries(list map[string]any) bool {
	tables, _ := list["tables"].([]any)
	out := tables[:0:0]
	for _, t := range tables {
		m, _ := t.(map[string]any)
		ref, _ := m["tableReference"].(map[string]any)
		if id, _ := ref["tableId"].(string); id == linkedEntriesTable {
			continue
		}
		out = append(out, t)
	}
	if len(out) == len(tables) {
		return false
	}
	list["tables"] = out
	if n, ok := list["totalItems"].(float64); ok {
		list["totalItems"] = n - float64(len(tables)-len(out))
	}
	return true
}
