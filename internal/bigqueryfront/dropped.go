package bigqueryfront

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Deleted datasets and tables, kept for their time travel window (#1460).
//
// BigQuery documents (https://cloud.google.com/bigquery/docs/time-travel,
// read 2026-10-16) that "you can undelete a dataset that is within your
// time travel window" (datasets.undelete, UNDROP SCHEMA,
// https://cloud.google.com/bigquery/docs/restore-deleted-datasets), and
// that a deleted table is restored by copying it at a time it existed,
// `bq cp dataset.table@TIME dataset.restored`, "within the time travel
// window", the dataset's maxTimeTravelHours (two to seven days, seven by
// default) after the deletion
// (https://cloud.google.com/bigquery/docs/restore-deleted-tables). After
// the window only Cloud Customer Care can restore it (the fail-safe
// period), which the emulator has nothing of.
//
// Measured first: datasets.undelete and UNDROP SCHEMA were 501, and a copy
// of table@TIME of a dropped table failed notFound. The engine drops a
// table's rows and its change table (#1418, googlesqlite patch 0036) with
// it, and the emulator drops every table of a dataset it deletes the same
// way (its DeleteTables, DROP TABLE through the engine).
//
// So before the front lets a table be deleted (tables.delete, DROP TABLE
// in a query, datasets.delete with deleteContents, DROP SCHEMA CASCADE) it
// copies the table's change table, every change of its last seven days
// with the change's row, order and time, into a table of its own dataset,
// _cloudburrow_dropped (which the engine records no history of and
// datasets.list leaves out), and keeps the table's resource (tables.get)
// and the deletion time in dropped.json on its state directory. A table
// whose change table cannot be read (one made before googlesqlite patch
// 0041) is copied as its rows are, which restores it as it was when it
// was deleted, not at an earlier time. A view keeps its resource only.
//
// Restoring writes a new table from the kept resource (its schema,
// description, labels, partitioning, clustering) through tables.insert,
// and its rows as they were at the time asked for, as timetravel.go reads
// a table's past from its change table: of each row, its last change at
// or before the time, when not a DELETE. Its change history starts again
// at the restore (BigQuery's restored table keeps none either, UNVERIFIED).
//
// A kept deletion expires once the window of the dataset (as it was when
// deleted) has passed, by the front's clock, which the admin's
// clock:advance moves (streamingbuffer.go); the front looks for expired
// ones whenever it keeps, lists or restores one, and deletes their
// copies. Not kept: a materialized view, a table snapshot (its own rows),
// an external table, a dataset's routines and models, the settings the
// front keeps of a table (enable_change_history, privacy_policy,
// tableoptions.go), its IAM policy and row access policies.

// droppedDatasetID is the front's dataset of the copies.
const droppedDatasetID = "_cloudburrow_dropped"

// droppedTable is a deleted table the front keeps.
type droppedTable struct {
	Project string `json:"project"`
	Dataset string `json:"dataset"`
	Table   string `json:"table"`
	// Copy is the table of droppedDatasetID holding its changes, "" for
	// a view.
	Copy string `json:"copy,omitempty"`
	// Final: Copy holds the rows as they were when it was deleted, not
	// its changes.
	Final    bool            `json:"final,omitempty"`
	Resource json.RawMessage `json:"resource"`
	// Created and Deleted are milliseconds since the epoch.
	Created int64 `json:"created"`
	Deleted int64 `json:"deleted"`
	Hours   int64 `json:"hours"` // the dataset's time travel window
	// WithDataset: deleted with its dataset, which an undelete restores.
	WithDataset bool `json:"withDataset,omitempty"`
}

// droppedDataset is a deleted dataset the front keeps.
type droppedDataset struct {
	Project  string          `json:"project"`
	Dataset  string          `json:"dataset"`
	Resource json.RawMessage `json:"resource"`
	Deleted  int64           `json:"deleted"`
	Hours    int64           `json:"hours"`
}

func (d droppedTable) expires() int64   { return d.Deleted + d.Hours*int64(time.Hour/time.Millisecond) }
func (d droppedDataset) expires() int64 { return d.Deleted + d.Hours*int64(time.Hour/time.Millisecond) }

type savedDropped struct {
	Tables   []droppedTable   `json:"tables"`
	Datasets []droppedDataset `json:"datasets"`
}

// droppedStore is what the front keeps of deleted tables and datasets.
type droppedStore struct {
	mu   sync.Mutex
	s    savedDropped
	path string
	logf func(string, ...any)
}

func droppedStateFile(dir string) string { return filepath.Join(dir, "dropped.json") }

// keep restores the store from path and from then on keeps it there.
func (s *droppedStore) keep(path string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	b, err := os.ReadFile(path)
	var saved savedDropped
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		logf("bigquery front: read %s: %v; starting without the deleted tables it kept", path, err)
	case json.Unmarshal(b, &saved) != nil:
		logf("bigquery front: %s is unreadable; starting without the deleted tables it kept", path)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.s, s.path, s.logf = saved, path, logf
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logf("bigquery front: %v", err)
	}
	s.saveLocked()
}

func (s *droppedStore) saveLocked() {
	if s.path == "" {
		return
	}
	b, err := json.Marshal(s.s)
	if err == nil {
		err = writeFileAtomic(s.path, b)
	}
	if err != nil {
		s.logf("bigquery front: keeping the deleted tables in %s: %v", s.path, err)
	}
}

func (s *droppedStore) add(tables []droppedTable, ds *droppedDataset) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.s.Tables = append(s.s.Tables, tables...)
	if ds != nil {
		s.s.Datasets = append(s.s.Datasets, *ds)
	}
	s.saveLocked()
}

// expire takes out what expired by now (milliseconds) and returns the
// copies to delete.
func (s *droppedStore) expire(now int64) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var copies []string
	tables := s.s.Tables[:0]
	for _, t := range s.s.Tables {
		if t.expires() < now {
			if t.Copy != "" {
				copies = append(copies, t.Copy)
			}
			continue
		}
		tables = append(tables, t)
	}
	s.s.Tables = tables
	datasets := s.s.Datasets[:0]
	for _, d := range s.s.Datasets {
		if d.expires() >= now {
			datasets = append(datasets, d)
		}
	}
	s.s.Datasets = datasets
	if len(copies) > 0 {
		s.saveLocked()
	}
	return copies
}

// table is the latest kept deletion of project.dataset.table that existed
// at at (milliseconds), and whether there is one; deleted reports whether
// there is any deletion of it, at that time or not.
func (s *droppedStore) table(project, dataset, table string, at int64) (droppedTable, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var got droppedTable
	found, any := false, false
	for _, t := range s.s.Tables {
		if t.Project != project || t.Dataset != dataset || t.Table != table {
			continue
		}
		any = true
		if t.Created <= at && at < t.Deleted && (!found || t.Deleted > got.Deleted) {
			got, found = t, true
		}
	}
	return got, found, any
}

// dataset is the latest kept deletion of project.dataset, with the tables
// deleted with it, and takes them out of the store.
func (s *droppedStore) takeDataset(project, dataset string, deleted int64) (droppedDataset, []droppedTable, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := -1
	for i, d := range s.s.Datasets {
		if d.Project == project && d.Dataset == dataset && (deleted == 0 || d.Deleted == deleted) &&
			(at < 0 || d.Deleted > s.s.Datasets[at].Deleted) {
			at = i
		}
	}
	if at < 0 {
		return droppedDataset{}, nil, false
	}
	d := s.s.Datasets[at]
	s.s.Datasets = append(s.s.Datasets[:at], s.s.Datasets[at+1:]...)
	var with []droppedTable
	tables := s.s.Tables[:0]
	for _, t := range s.s.Tables {
		if t.WithDataset && t.Project == project && t.Dataset == dataset && t.Deleted == d.Deleted {
			with = append(with, t)
			continue
		}
		tables = append(tables, t)
	}
	s.s.Tables = tables
	s.saveLocked()
	return d, with, true
}

// nowMillis is the front's clock (streamingbuffer.go), in milliseconds.
func (f front) nowMillis() int64 { return f.buffers.clock().UnixMilli() }

// expireDropped deletes the copies of the kept deletions whose window has
// passed.
func (f front) expireDropped(r *http.Request) {
	if f.dropped == nil {
		return
	}
	for _, c := range f.dropped.expire(f.nowMillis()) {
		f.send(r, http.MethodDelete, tablePath(droppedDatasetID, c), nil)
	}
}

// timeTravelHours is the dataset's maxTimeTravelHours, 168 by default.
func (f front) timeTravelHours(r *http.Request, dataset string) (int64, json.RawMessage, bool) {
	st, got := f.get(r, "/datasets/"+url.PathEscape(dataset))
	if st != http.StatusOK {
		return 0, nil, false
	}
	var ds struct {
		Hours string `json:"maxTimeTravelHours"`
	}
	hours := int64(168)
	if json.Unmarshal(got, &ds) == nil {
		if h, err := strconv.ParseInt(ds.Hours, 10, 64); err == nil && h > 0 {
			hours = h
		}
	}
	return hours, got, true
}

// keepTable copies table of dataset before it is deleted (above), and
// returns what to keep, or false when the table is not one the front keeps
// (or is not there). Nothing is kept in the store until commit.
func (f front) keepTable(r *http.Request, dataset, table string, hours int64) (droppedTable, bool) {
	project := projectOf(f.base)
	st, got := f.get(r, tablePath(dataset, table))
	if st != http.StatusOK {
		return droppedTable{}, false
	}
	var meta struct {
		Type         string `json:"type"`
		CreationTime string `json:"creationTime"`
		Schema       struct {
			Fields []field `json:"fields"`
		} `json:"schema"`
		Snapshot json.RawMessage `json:"snapshotDefinition"`
	}
	if json.Unmarshal(got, &meta) != nil {
		return droppedTable{}, false
	}
	created, _ := strconv.ParseInt(meta.CreationTime, 10, 64)
	d := droppedTable{Project: project, Dataset: dataset, Table: table, Resource: got, Created: created, Hours: hours}
	switch strings.ToUpper(meta.Type) {
	case "VIEW":
		// The client's text of the view's query, not the engine's
		// (views.go), which names tables as the engine does.
		var res map[string]any
		if json.Unmarshal(got, &res) == nil {
			if v, ok := res["view"].(map[string]any); ok {
				if t, ok := f.views.get(viewKey(project, dataset, table)); ok && t.engine == v["query"] {
					v["query"] = t.client
					d.Resource, _ = json.Marshal(res)
				}
			}
		}
		return d, true
	case "", "TABLE":
	default:
		return droppedTable{}, false
	}
	if len(meta.Snapshot) > 0 && string(meta.Snapshot) != "null" || len(meta.Schema.Fields) == 0 {
		return droppedTable{}, false
	}
	body, _ := json.Marshal(map[string]any{"datasetReference": map[string]string{"projectId": project, "datasetId": droppedDatasetID}})
	f.send(r, http.MethodPost, "/datasets", body) // made once; 409 after
	var cols []string
	for _, fl := range meta.Schema.Fields {
		if pseudoColumn(fl.Name) {
			continue
		}
		cols = append(cols, quotePath([]string{fl.Name}))
	}
	list := strings.Join(cols, ", ")
	d.Copy = strings.Replace(scratchTable(), "_cloudburrow_replace_", "t_", 1)
	dest := quotePath([]string{project, droppedDatasetID, d.Copy})
	run := func(sql string) bool {
		legacy := false
		b, err := json.Marshal(queryOptions{UseLegacySQL: &legacy, Query: sql})
		if err != nil {
			return false
		}
		status, _ := f.send(r, http.MethodPost, "/queries", b)
		return status == http.StatusOK
	}
	if run(fmt.Sprintf("CREATE TABLE %s AS SELECT %s, _CHANGE_TYPE AS _cloudburrow_change, googlesqlite_change_micros AS "+
		"_cloudburrow_micros, googlesqlite_change_rowid AS _cloudburrow_rowid, rowid AS _cloudburrow_order FROM %s",
		dest, list, quotePath([]string{project, dataset, table + changeTableSuffix}))) {
		return d, true
	}
	d.Final = true
	if run(fmt.Sprintf("CREATE TABLE %s AS SELECT %s, 'INSERT' AS _cloudburrow_change, 0 AS _cloudburrow_micros, "+
		"ROW_NUMBER() OVER () AS _cloudburrow_rowid, 0 AS _cloudburrow_order FROM %s",
		dest, list, quotePath([]string{project, dataset, table}))) {
		return d, true
	}
	return droppedTable{}, false
}

// commitDropped keeps those of kept whose table is gone, and deletes the
// copies of the others.
func (f front) commitDropped(r *http.Request, kept []droppedTable, ds *droppedDataset) {
	var gone []droppedTable
	now := f.nowMillis()
	for _, t := range kept {
		if st, _ := f.get(r, tablePath(t.Dataset, t.Table)); st == http.StatusNotFound {
			t.Deleted = now
			gone = append(gone, t)
			continue
		}
		if t.Copy != "" {
			f.send(r, http.MethodDelete, tablePath(droppedDatasetID, t.Copy), nil)
		}
	}
	if ds != nil {
		ds.Deleted = now
		for i := range gone {
			gone[i].WithDataset = true
		}
	}
	if f.dropped != nil {
		f.dropped.add(gone, ds)
	}
}

// deleteTableKeeping serves tables.delete, keeping the table (above).
func (f front) deleteTableKeeping(w http.ResponseWriter, r *http.Request, dataset, table string) {
	f.expireDropped(r)
	hours, _, ok := f.timeTravelHours(r, dataset)
	t, kept := f.keepTable(r, dataset, table, hours)
	if !ok || !kept {
		f.next.ServeHTTP(w, r)
		return
	}
	sw := &statusWriter{ResponseWriter: w}
	f.next.ServeHTTP(sw, r)
	f.commitDropped(r, []droppedTable{t}, nil)
}

// keepDataset copies the tables of dataset before it is deleted, and
// returns them and what to keep of the dataset.
func (f front) keepDataset(r *http.Request, dataset string) ([]droppedTable, *droppedDataset) {
	hours, res, ok := f.timeTravelHours(r, dataset)
	if !ok {
		return nil, nil
	}
	d := &droppedDataset{Project: projectOf(f.base), Dataset: dataset, Resource: res, Hours: hours}
	var kept []droppedTable
	token := ""
	for {
		p := "/datasets/" + url.PathEscape(dataset) + "/tables?maxResults=1000"
		if token != "" {
			p += "&pageToken=" + url.QueryEscape(token)
		}
		st, got := f.get(r, p)
		var list struct {
			Tables []struct {
				TableReference tableRef `json:"tableReference"`
			} `json:"tables"`
			Next string `json:"nextPageToken"`
		}
		if st != http.StatusOK || json.Unmarshal(got, &list) != nil {
			break
		}
		for _, t := range list.Tables {
			if k, ok := f.keepTable(r, dataset, t.TableReference.TableID, hours); ok {
				kept = append(kept, k)
			}
		}
		if token = list.Next; token == "" {
			break
		}
	}
	return kept, d
}

// deleteDatasetKeeping serves datasets.delete, keeping the dataset and its
// tables (above).
func (f front) deleteDatasetKeeping(w http.ResponseWriter, r *http.Request, dataset string) {
	f.expireDropped(r)
	kept, d := f.keepDataset(r, dataset)
	if d == nil {
		f.next.ServeHTTP(w, r)
		return
	}
	sw := &statusWriter{ResponseWriter: w}
	f.next.ServeHTTP(sw, r)
	if sw.status/100 != 2 && sw.status != 0 {
		f.commitDropped(r, kept, nil) // the tables are still there: their copies go
		return
	}
	f.commitDropped(r, kept, d)
}

// droppedRead is the subquery of the rows of t as they were at at
// (microseconds since the epoch).
func droppedRead(t droppedTable, fields []field, at int64) string {
	var cols []string
	for _, fl := range fields {
		if pseudoColumn(fl.Name) {
			continue
		}
		cols = append(cols, quotePath([]string{fl.Name}))
	}
	list := strings.Join(cols, ", ")
	return fmt.Sprintf("(SELECT %s FROM (SELECT %s, _cloudburrow_change, ROW_NUMBER() OVER (PARTITION BY _cloudburrow_rowid "+
		"ORDER BY _cloudburrow_order DESC) AS _cloudburrow_last FROM %s WHERE _cloudburrow_change != 'START' AND "+
		"_cloudburrow_micros <= %d) WHERE _cloudburrow_last = 1 AND _cloudburrow_change != 'DELETE')",
		list, list, quotePath([]string{t.Project, droppedDatasetID, t.Copy}), at)
}

// droppedStartsAfter reports whether the history kept of t starts after at
// (microseconds): an ALTER TABLE that rebuilt it started it over (#1466).
func (f front) droppedStartsAfter(r *http.Request, t droppedTable, at int64) bool {
	if t.Final {
		return true
	}
	legacy := false
	body, _ := json.Marshal(queryOptions{UseLegacySQL: &legacy, Query: fmt.Sprintf(
		"SELECT COUNTIF(_cloudburrow_change = 'START' AND _cloudburrow_micros > %d) FROM %s", at,
		quotePath([]string{t.Project, droppedDatasetID, t.Copy}))})
	status, got := f.send(r, http.MethodPost, "/queries", body)
	var res struct {
		Rows []struct {
			F []struct {
				V *string `json:"v"`
			} `json:"f"`
		} `json:"rows"`
	}
	if status != http.StatusOK || json.Unmarshal(got, &res) != nil || len(res.Rows) != 1 || len(res.Rows[0].F) != 1 ||
		res.Rows[0].F[0].V == nil {
		return true
	}
	return *res.Rows[0].F[0].V != "0"
}

// droppedFields is the schema of the kept resource.
func droppedFields(t droppedTable) (json.RawMessage, []field) {
	var meta struct {
		Schema json.RawMessage `json:"schema"`
	}
	_ = json.Unmarshal(t.Resource, &meta)
	var s tableSchema
	_ = json.Unmarshal(meta.Schema, &s)
	return meta.Schema, s.Fields
}

// restoreTable writes t back as dataset.table, with its rows of when it
// was deleted, and returns why it could not, or "".
func (f front) restoreTable(r *http.Request, t droppedTable) string {
	var res map[string]any
	if json.Unmarshal(t.Resource, &res) != nil {
		return "could not read the table's resource"
	}
	for _, k := range []string{"etag", "id", "selfLink", "creationTime", "lastModifiedTime", "numBytes", "numLongTermBytes",
		"numRows", "numActiveLogicalBytes", "numLongTermLogicalBytes", "numTotalLogicalBytes", "numPhysicalBytes",
		"streamingBuffer", "location", "kind"} {
		delete(res, k)
	}
	res["tableReference"] = map[string]string{"projectId": t.Project, "datasetId": t.Dataset, "tableId": t.Table}
	if status, got := f.createTable(r, t.Dataset, res); status != http.StatusOK {
		return errorMessage(got, status)
	}
	if t.Copy == "" {
		return ""
	}
	_, fields := droppedFields(t)
	src := copyTable{fields: fields, from: droppedRead(t, fields, 1<<62)}
	if msg := f.insertSelect(r, tableRef{DatasetID: t.Dataset, TableID: t.Table}, []copyTable{src}); msg != "" {
		f.send(r, http.MethodDelete, tablePath(t.Dataset, t.Table), nil)
		return msg
	}
	f.send(r, http.MethodDelete, tablePath(droppedDatasetID, t.Copy), nil)
	return ""
}

// undeleteDataset restores the latest kept deletion of dataset (deleted,
// milliseconds, when not 0) and its tables, and returns the HTTP status,
// reason and message of why it could not.
func (f front) undeleteDataset(r *http.Request, dataset string, deleted int64) (int, string, string) {
	project := projectOf(f.base)
	f.expireDropped(r)
	if st, _ := f.get(r, "/datasets/"+url.PathEscape(dataset)); st == http.StatusOK {
		return http.StatusConflict, "duplicate", "Already Exists: Dataset " + project + ":" + dataset // UNVERIFIED wording
	}
	if f.dropped == nil {
		return http.StatusNotFound, "notFound", "Not found: Dataset " + project + ":" + dataset
	}
	d, tables, ok := f.dropped.takeDataset(project, dataset, deleted)
	if !ok {
		// UNVERIFIED wording.
		return http.StatusNotFound, "notFound", "Not found: Dataset " + project + ":" + dataset +
			" was not deleted within its time travel window"
	}
	var res map[string]any
	_ = json.Unmarshal(d.Resource, &res)
	for _, k := range []string{"etag", "id", "selfLink", "creationTime", "lastModifiedTime", "kind"} {
		delete(res, k)
	}
	body, _ := json.Marshal(res)
	if st, got := f.send(r, http.MethodPost, "/datasets", body); st != http.StatusOK {
		f.dropped.add(tables, &d)
		return st, "backendError", errorMessage(got, st)
	}
	for _, t := range tables {
		if msg := f.restoreTable(r, t); msg != "" {
			return http.StatusInternalServerError, "backendError", "restoring " + tableName(project,
				tableRef{DatasetID: t.Dataset, TableID: t.Table}) + ": " + msg
		}
	}
	return 0, "", ""
}

// undeleteRoute is datasets.undelete.
var undeleteRoute = regexp.MustCompile(`^((?:/bigquery/v2)?/projects/[^/]+)/datasets/([^/:]+):undelete$`)

// serveUndelete serves datasets.undelete, and reports whether r is one.
func (f front) serveUndelete(w http.ResponseWriter, r *http.Request) bool {
	m := undeleteRoute.FindStringSubmatch(r.URL.EscapedPath())
	if m == nil || r.Method != http.MethodPost {
		return false
	}
	f.base = m[1]
	dataset, err := url.PathUnescape(m[2])
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "Invalid dataset ID")
		return true
	}
	var req struct {
		DeletionTime string `json:"deletionTime"`
	}
	if b, err := readBody(r); err == nil && len(b) > 0 {
		_ = json.Unmarshal(b, &req)
	}
	var deleted int64
	if req.DeletionTime != "" {
		t, err := time.Parse(time.RFC3339Nano, req.DeletionTime)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid", "Invalid value for deletionTime: "+req.DeletionTime)
			return true
		}
		deleted = t.UnixMilli()
	}
	if code, reason, msg := f.undeleteDataset(r, dataset, deleted); code != 0 {
		writeError(w, code, reason, msg)
		return true
	}
	st, got := f.get(r, "/datasets/"+url.PathEscape(dataset))
	writeRaw(w, st, got)
	return true
}

// pseudoColumn reports whether name is an ingestion-time partitioned
// table's pseudo-column, which the change table does not have.
func pseudoColumn(name string) bool {
	n := strings.ToUpper(name)
	return n == "_PARTITIONTIME" || n == "_PARTITIONDATE"
}

// undropSchema carries out a lone UNDROP SCHEMA [IF NOT EXISTS] name
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#undrop_schema_statement):
// "Undeletes a dataset within your time travel window"; "IF NOT EXISTS: If
// a dataset with the same name already exists, the statement has no
// effect". It reports whether q was one.
func (f front) undropSchema(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	if !strings.Contains(strings.ToUpper(q.Query), "UNDROP") {
		return false
	}
	toks, ok := lex(q.Query)
	if !ok {
		return false
	}
	var stmt []token
	for _, s := range splitStatements(toks) {
		if len(s) > 0 {
			stmt = s
		}
	}
	if len(stmt) < 3 || !stmt[0].is("UNDROP") || !stmt[1].is("SCHEMA") {
		return false
	}
	project := projectOf(f.base)
	i, ifNotExists := 2, false
	if i+2 < len(stmt) && stmt[i].is("IF") && stmt[i+1].is("NOT") && stmt[i+2].is("EXISTS") {
		i, ifNotExists = i+3, true
	}
	p, next := path(stmt, i)
	if len(p) == 0 || len(p) > 2 || next != len(stmt) || len(p) == 2 && p[0] != project {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: UNDROP SCHEMA of "+
			strings.Join(p, ".")+": CloudBurrow undeletes a dataset of the instance's project, named alone or with the "+
			"project (docs/compatibility.md). Nothing was run.")
		return true
	}
	ds := p[len(p)-1]
	if st, _ := f.get(r, "/datasets/"+url.PathEscape(ds)); st == http.StatusOK && ifNotExists {
		// nothing to do
	} else if code, reason, msg := f.undeleteDataset(r, ds, 0); code != 0 {
		if code == http.StatusInternalServerError {
			reason = "backendError"
		} else if reason == "duplicate" {
			code = http.StatusConflict
		}
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", code, rowError{Reason: reason, Message: msg})
		return true
	}
	if !setQueryText(r, insert, "DROP TABLE IF EXISTS "+quotePath([]string{"_cloudburrow", scratchTable()})) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return true
	}
	f.forward(w, r, jobText{query: q.Query})
	return true
}

// keptDropsKey marks a query whose dropped tables keepQueryDrops kept.
type keptDropsKey struct{}

// keepQueryDrops keeps each table a DROP TABLE statement of q drops
// (above): it copies them, runs the query, and keeps those that are gone
// after it. It reports whether it ran the query.
func (f front) keepQueryDrops(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	if f.dropped == nil || r.Context().Value(keptDropsKey{}) != nil || !strings.Contains(strings.ToUpper(q.Query), "DROP") {
		return false
	}
	toks, ok := lex(q.Query)
	if !ok {
		return false
	}
	project := projectOf(f.base)
	seen := map[string]bool{}
	var kept []droppedTable
	expired := false
	for _, stmt := range splitStatements(toks) {
		body, _, _ := stripControlFlow(stmt)
		if len(body) < 3 || !body[0].is("DROP") || !body[1].is("TABLE") || body[2].is("FUNCTION") {
			continue
		}
		i := 2
		if i+1 < len(body) && body[i].is("IF") && body[i+1].is("EXISTS") {
			i += 2
		}
		p, _ := path(body, i)
		ds, table, ok := tableOf(q, p)
		if !ok || len(p) >= 3 && p[len(p)-3] != project || frontDataset(ds) || seen[ds+"."+table] {
			continue
		}
		seen[ds+"."+table] = true
		if !expired {
			f.expireDropped(r)
			expired = true
		}
		hours, _, ok := f.timeTravelHours(r, ds)
		if !ok {
			continue
		}
		if t, ok := f.keepTable(r, ds, table, hours); ok {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		return false
	}
	f.runQuery(w, r.WithContext(context.WithValue(r.Context(), keptDropsKey{}, true)), q, insert)
	f.commitDropped(r, kept, nil)
	return true
}

// copySourceAt is a copy job's source table@time
// (https://cloud.google.com/bigquery/docs/restore-deleted-tables: "bq cp
// mydataset.mytable@-3600000 mydataset.newtable", milliseconds since the
// epoch or, negative, before now): the table as it was then, read from its
// change history when it exists (timetravel.go) and from the kept copy of
// a deleted one when not. It returns the status, reason and message of a
// refusal.
func (f front) copySourceAt(r *http.Request, s tableRef) (copyTable, int, string, string) {
	project := projectOf(f.base)
	id, dec, _ := strings.Cut(s.TableID, "@")
	ref := tableRef{ProjectID: s.ProjectID, DatasetID: s.DatasetID, TableID: id}
	name := tableName(project, ref)
	ms, err := strconv.ParseInt(dec, 10, 64)
	if err != nil || ms == 0 {
		return copyTable{}, http.StatusNotImplemented, "notImplemented", "Not implemented here: a copy job of " + name + "@" +
			dec + ". CloudBurrow reads a snapshot decorator of milliseconds since the epoch, or of a negative offset from " +
			"now (@-3600000); @0 and a range decorator are not built (docs/compatibility.md). Nothing was copied."
	}
	now := f.nowMillis()
	at := ms
	if ms < 0 {
		at = now + ms
	}
	if live := f.copyTableMeta(r, ref); live.exists {
		var m partitionMeta
		if st, got := f.get(r, tablePath(ref.DatasetID, id)); st == http.StatusOK {
			_ = json.Unmarshal(got, &m)
		}
		past, code, msg := f.systemTime(r, queryOptions{}, ref.DatasetID, name, m, fmt.Sprintf("TIMESTAMP_MILLIS(%d)", at))
		switch {
		case code == http.StatusNotImplemented:
			return copyTable{}, code, "notImplemented", msg
		case code != 0:
			return copyTable{}, code, "invalid", msg
		case past == 0:
			return live, 0, "", ""
		}
		sub, code, msg := f.historySubquery(r, ref.DatasetID, id, m, name, past, "TRUE")
		if code != 0 {
			return copyTable{}, code, "notImplemented", msg
		}
		live.from = sub
		return live, 0, "", ""
	}
	if f.dropped == nil {
		return copyTable{ref: ref}, 0, "", ""
	}
	f.expireDropped(r)
	t, found, deleted := f.dropped.table(project, ref.DatasetID, id, at)
	switch {
	case !found && deleted:
		// UNVERIFIED wording.
		return copyTable{}, http.StatusBadRequest, "invalid", fmt.Sprintf("Invalid snapshot time %d for table %s@%d: "+
			"the table did not exist at that time", at, name, at)
	case !found:
		return copyTable{ref: ref}, 0, "", ""
	}
	if earliest := now - t.Hours*int64(time.Hour/time.Millisecond); at < earliest {
		// As https://cloud.google.com/bigquery/docs/time-travel documents it.
		return copyTable{}, http.StatusBadRequest, "invalid", fmt.Sprintf("Invalid snapshot time %d for table %s@%d. "+
			"Cannot read before %d", at, name, at, earliest)
	}
	if t.Copy == "" {
		return copyTable{ref: ref, exists: true, kind: "VIEW"}, 0, "", ""
	}
	if f.droppedStartsAfter(r, t, at*1000+999) {
		return copyTable{}, http.StatusNotImplemented, "notImplemented", "Not implemented here: a copy job of " + name + "@" +
			dec + ". The history CloudBurrow kept of the deleted table starts after that time: an ALTER TABLE that rebuilds " +
			"a table, and CREATE OR REPLACE TABLE, start it over, and a table made before CloudBurrow kept row identities " +
			"is kept as it was when deleted (docs/compatibility.md). Nothing was copied."
	}
	schema, fields := droppedFields(t)
	return copyTable{ref: ref, exists: true, kind: "TABLE", schema: schema, fields: fields,
		from: droppedRead(t, fields, at*1000+999)}, 0, "", ""
}
