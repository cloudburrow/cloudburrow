// Package bigqueryfront checks BigQuery REST requests before the emulator
// sees them (#861).
//
// CloudBurrow serves BigQuery with goccy/bigquery-emulator, a community
// project run unmodified from its pinned image (dependencies.json). It
// validates almost nothing: measured against that image, it created a
// dataset "d-1" and a table "t!", stored rows missing a REQUIRED value,
// stored "x" in a NUMERIC column as 0, stored a string in a REPEATED
// INTEGER column after which the table could not be read at all, inserted
// the good rows of a batch with a bad one whatever skipInvalidRows said, and
// answered a duplicate dataset, a duplicate column and a wrong-type value
// with 500, which the Go client retries until its deadline.
//
// Rather than fork the emulator, this front stands in its request path and
// refuses what BigQuery refuses, as BigQuery answers it. It runs in the
// emulator's pod (#902), as `cloudburrow-storage bigquery-front` from the
// locally built storage image (Run), and serves the Service's REST port,
// so the host's tunnel, the console and every pod that dials
// bigquery.<namespace> reach the emulator only through it, with or without
// Cloud Run and without pods reaching the host:
//
//   - datasets.insert: an invalid dataset ID is 400 invalid; an existing
//     dataset is 409 duplicate.
//   - tables.insert, tables.update and tables.patch: an invalid table ID or
//     schema (a field name, a duplicate field, an unknown type or mode) is 400.
//   - tabledata.insertAll: each row is checked against the table's schema.
//     Without skipInvalidRows, a batch with an invalid row inserts nothing
//     and every row gets an insertErrors entry: "invalid" for the bad ones,
//     "stopped" for the rest. With it, the valid rows are inserted and only
//     the invalid ones are reported. ignoreUnknownValues drops fields the
//     table does not have instead of refusing the row. A value in a RECORD
//     nested in a RECORD with a REPEATED one among them is 501, for the
//     whole request: the emulator would store it so that the table could
//     not be read again (#874, #881, unstorableNesting).
//   - jobs.insert and jobs.query (#881): the table a load, copy or query job
//     writes to is held to the table ID rule, and a load's schema to the
//     schema rules, 400 invalid; a load into a schema with a RECORD inside a
//     REPEATED RECORD is 501, as the emulator does not load one reliably
//     (unloadable); a CREATE TABLE or CREATE SCHEMA statement
//     is held to the table, column and dataset ID rules, 400 invalidQuery
//     (checkDDL).
//   - (#901) ALTER TABLE's new names, the columns a CREATE TABLE ... AS
//     SELECT's query gives (ctasColumns), and the DDL inside a script's
//     blocks are held to the same rules; an ALTER TABLE or a control-flow
//     block the emulator would report done without doing is 501 (checkDDL).
//     A load with autodetect and no schema is checked in the table it made,
//     and failed as BigQuery fails it (autodetectLoad).
//   - (#916, #917, #918, #919) a view's ID and its query's columns, through
//     CREATE VIEW and tables.insert; a CREATE TABLE ... AS SELECT whose
//     query cannot run alone, after its script; a lone CREATE OR REPLACE of
//     an existing table or view, which the front carries out (replace); a
//     failing script with an EXCEPTION handler, RAISE and materialized
//     views, 501 (serveQuery). A load with no sourceFormat is sent as CSV,
//     a CSV header BigQuery would not detect is 501 (headerDiffers), a
//     resumable upload is served by the front (resumable), and jobs.list,
//     jobs.get and jobs.getQueryResults report the jobs the front failed.
//   - (#931, #932, #934, #937) a CSV load whose columns are given loads
//     every row, as BigQuery does (csvLoad); CREATE TABLE or VIEW ... IF
//     NOT EXISTS of one that exists does nothing (skipIfExists); a job the
//     emulator failed reads back failed (jobFailures.watch); CREATE TABLE
//     LIKE, COPY and CLONE and snapshot tables are 501 (checkDDL).
//   - (#944, #945, #946) a CSV load from Cloud Storage is read by the
//     front and loaded as an upload (gcsload.go); a CSV load's
//     fieldDelimiter, quote, allowJaggedRows and nullMarker are carried
//     out on its data (csvDialect); CREATE SCHEMA of a dataset that exists
//     fails as BigQuery fails it (createSchema).
//   - (#951, #952) CREATE SCHEMA of a new dataset makes it through
//     datasets.insert (createSchema); a CSV load's other options are
//     carried out on its data, or are 501 (csvDialect.withOptions).
//   - (#933, #935, #936, #938, #939) a script's variables are sent under
//     names of their own, so none outlives its script (renameVariables); a
//     script that fails after a statement that changes data is 501, as the
//     emulator rolled all of it back (serveQuery); CREATE OR REPLACE and
//     DROP of a TEMP table the script made are 501 (checkDDL); a CREATE
//     TEMP TABLE ... AS SELECT whose query cannot run alone is checked by
//     running the statements before it (tempColumns); a job the front
//     rewrote shows the client's text (jobTexts); and an extract job is
//     sent on only as the emulator writes it as BigQuery does (extractJob).
//   - (#955, #956, #957, #958) after a failed script, the emulator's
//     catalog and list of tables are put back in step with its tables
//     (resyncCatalog), and a failed query job, which the emulator commits
//     up to the failing statement, fails as BigQuery fails it; only the
//     references to a script variable are renamed, and a statement where
//     BigQuery may read another name of the variable's is 501
//     (renameVariables); the extracts the emulator writes differently from
//     BigQuery (JSON, GZIP, another delimiter, an empty table's header) are
//     written by the front itself (writeExtract); and jobs.list gives each
//     job's configuration (jobConfigs).
//   - (#960, #966) a load's job reports statistics.load: what the front
//     counted of the data it read, or the rows the table gained and the
//     upload's or objects' bytes (countLoad).
//   - (#970, #971, #972, #973, #975, #976) a job with no jobReference is
//     given one (withReference); every job's times are milliseconds
//     (jobRecords.timed); jobs.list is ordered, paged and filtered as
//     BigQuery's (jobRecords.serveJobList); a Parquet load with no schema
//     takes its table's, or is 501 (parquetSchema); a CSV extract of an
//     empty STRING is 501 (emptyStringColumn); a function a failed script
//     made is taken out of the catalog again, and DROP SCHEMA was 501
//     until #990 (scriptFunctions, createSchema).
//   - (#986, #987, #990) CREATE FUNCTION of a function that exists fails
//     as BigQuery fails it, and a lone CREATE OR REPLACE FUNCTION of one
//     replaces it (functionDDL); a copy job is carried out by the front
//     as a job of its own (copyJob); DROP SCHEMA is carried out through
//     datasets.delete (planDropSchema).
//   - (#1000, #1001) a FLOAT column is made FLOAT64 in the emulator's
//     engine, and reads back FLOAT (floattype.go); DROP SCHEMA finds the
//     functions routines.insert made and those of the emulator's jobs from
//     before the front started (knownFunctions).
//
// Everything else passes through untouched.
package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// maxBody bounds a request body the front reads. BigQuery's own limit for
// an insertAll request is 10 MB; this leaves room above it and stops only
// an unbounded one.
const maxBody = 64 << 20

// route matches the REST paths the front checks. The prefix is optional
// because the Go client, given an endpoint, sends paths with it and other
// clients may not.
var route = regexp.MustCompile(`^(/bigquery/v2)?/projects/([^/]+)/datasets(?:/([^/]+)(?:/tables(?:/([^/]+)(?:/(insertAll))?)?)?)?$`)

// Wrap returns next with the checks in front of it. next is the path to the
// emulator; the front also sends it the reads a check needs (whether a
// dataset exists, a table's schema), with the client's own Host.
//
// The front keeps two things between requests: the jobs it failed after
// the emulator ran them (a load whose detected schema BigQuery would
// refuse, autodetectLoad; a script checked after it ran, serveQuery), so
// that jobs.get and jobs.list report them failed as BigQuery would; and the
// resumable uploads in progress, which it receives itself (resumable).
//
// Options set what else the front reads: WithStorage, the instance's Cloud
// Storage, which a load from gs:// URIs is read from (#944) and an extract
// job's bucket is looked up in (#939).
//
// The front also keeps the client's text of each job it changed before
// the emulator ran it, so that jobs.get and jobs.list show it (jobTexts).
func Wrap(next http.Handler, opts ...Option) http.Handler {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	storage := newStorageReader(o.storage)
	storageHost := strings.TrimPrefix(o.storage, "http://")
	failed := &jobFailures{}
	uploads := &uploadSessions{}
	texts := &jobTexts{}
	configs := &jobConfigs{}
	own := &frontJobs{}
	records := &jobRecords{}
	functions := &knownFunctions{started: time.Now().UnixMilli()}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if j := jobsRoute.FindStringSubmatch(r.URL.EscapedPath()); j != nil && r.Method == http.MethodGet && j[3] == "jobs" &&
			!strings.HasPrefix(r.URL.EscapedPath(), "/upload/") {
			// jobs.list: the failures the front gave (failed), the
			// configurations the emulator leaves out (configs, #958), the
			// front's own jobs (own, #957), then the client's text of the
			// jobs the front changed (texts).
			// Then the times, paging and filters BigQuery gives it
			// (records, #971, #972).
			base := j[1] + "/projects/" + j[2]
			records.serveJobList(w, r, projectOf("/"+j[2]), func(w http.ResponseWriter) {
				texts.serveJobList(w, func(w http.ResponseWriter) {
					own.serveJobList(w, r, projectOf("/"+j[2]), func(w http.ResponseWriter) {
						configs.serveJobList(w, r, next, base, func(w http.ResponseWriter) { failed.listJobs(next, w, r) })
					})
				})
			})
			return
		}
		if j := jobActionRoute.FindStringSubmatch(r.URL.EscapedPath()); j != nil {
			project := projectOf("/" + j[2])
			if own.serveJobAction(w, r, project, j[3], j[4]) {
				return
			}
			if j[4] == "cancel" && r.Method == http.MethodPost {
				records.serveJob(w, project, true, func(w http.ResponseWriter) { next.ServeHTTP(w, r) })
				return
			}
		}
		if j := jobsRoute.FindStringSubmatch(r.URL.EscapedPath()); j != nil && strings.HasPrefix(r.URL.EscapedPath(), "/upload/") &&
			r.URL.Query().Get("uploadType") == "resumable" && (r.Method == http.MethodPost || r.Method == http.MethodPut) {
			f := front{next: next, base: j[1] + "/projects/" + j[2], failed: failed, uploads: uploads, texts: texts, storage: storage, storageHost: storageHost,
				configs: configs, jobs: own, records: records, functions: functions}
			f.resumable(w, r)
			return
		}
		if j := jobsRoute.FindStringSubmatch(r.URL.EscapedPath()); j != nil && r.Method == http.MethodPost {
			// Reads go to the REST path, never the upload one.
			f := front{next: next, base: j[1] + "/projects/" + j[2], failed: failed, texts: texts, storage: storage, storageHost: storageHost,
				configs: configs, jobs: own, records: records, functions: functions}
			if j[3] == "jobs" {
				f.insertJob(w, r)
			} else {
				f.query(w, r)
			}
			return
		}
		if j := jobRoute.FindStringSubmatch(r.URL.EscapedPath()); j != nil && r.Method == http.MethodGet {
			project := projectOf("/" + j[2])
			if j[3] == "jobs" && own.serveJobAction(w, r, project, j[4], "") {
				return
			}
			serve := func(w http.ResponseWriter) {
				texts.serveJob(w, project, j[4], func(w http.ResponseWriter) { failed.getJob(next, w, r, project, j[4], j[3] == "queries") })
			}
			if j[3] == "jobs" {
				records.serveJob(w, project, false, serve) // #971
				return
			}
			serve(w)
			return
		}
		if m := routinesRoute.FindStringSubmatch(r.URL.EscapedPath()); m != nil && r.Method == http.MethodPost {
			f := front{next: next, base: m[1] + "/projects/" + m[2], functions: functions}
			f.insertRoutine(w, r, projectOf(f.base)) // #1001
			return
		}
		m := route.FindStringSubmatch(r.URL.EscapedPath())
		if m == nil {
			next.ServeHTTP(w, r)
			return
		}
		prefix, project := m[1], m[2]
		dataset, err1 := url.PathUnescape(m[3])
		table, err2 := url.PathUnescape(m[4])
		if err1 != nil || err2 != nil {
			next.ServeHTTP(w, r)
			return
		}
		f := front{next: next, base: prefix + "/projects/" + project, failed: failed, records: records}
		switch {
		case r.Method == http.MethodPost && m[3] == "":
			f.insertDataset(w, r)
		case r.Method == http.MethodPost && m[3] != "" && m[4] == "":
			f.insertTable(w, r, dataset, false)
		case (r.Method == http.MethodPut || r.Method == http.MethodPatch) && m[4] != "" && m[5] == "":
			f.insertTable(w, r, dataset, true)
		case r.Method == http.MethodPost && m[5] == "insertAll":
			f.insertAll(w, r, dataset, table)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

type front struct {
	next http.Handler
	// base is the path up to and including the project, as the client sent
	// it: reads the front makes go to the same prefix.
	base string
	// failed are the jobs the front reports failed (autodetectLoad).
	failed *jobFailures
	// uploads are the resumable uploads in progress (resumable).
	uploads *uploadSessions
	// storage reads the instance's Cloud Storage (gcsload.go), or is nil.
	storage *storageReader
	// storageHost is that Cloud Storage as host:port, or "" (extractJob).
	storageHost string
	// texts are the jobs whose text the front changed (jobTexts).
	texts *jobTexts
	// configs are the configurations of the jobs sent (jobConfigs, #958).
	configs *jobConfigs
	// jobs are the jobs the front carried out itself (frontJobs, #957).
	jobs *frontJobs
	// records are the jobs' times and the front's own queries
	// (jobRecords, #971, #972).
	records *jobRecords
	// functions are the functions CREATE FUNCTION statements may have
	// made (knownFunctions, #990).
	functions *knownFunctions
}

// Option is an option of Wrap.
type Option func(*options)

type options struct {
	storage string
	// configs are the configurations of the jobs the emulator ran
	// (jobConfigs, #958).
	configs *jobConfigs
	// jobs are the jobs the front carried out itself (frontJobs, #957).
	jobs *frontJobs
	// records are the jobs' times and the front's own queries
	// (jobRecords, #971, #972).
	records *jobRecords
}

// WithStorage gives the front the instance's Cloud Storage JSON API, at
// endpoint (http://host:port), to read a load's gs:// URIs from (#944) and
// look an extract job's bucket up in (#939).
func WithStorage(endpoint string) Option {
	return func(o *options) { o.storage = endpoint }
}

// readBody reads r's body and puts it back, so it can still be forwarded.
func readBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	_ = r.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(b) > maxBody {
		return nil, errors.New("request body is too large")
	}
	setBody(r, b)
	return b, nil
}

func setBody(r *http.Request, b []byte) {
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	r.Header.Del("Content-Length")
}

// decode reads r's JSON body into v and returns the body. A body the front
// cannot read as JSON, or a compressed one, is not checked: it is forwarded
// as it is, so the emulator's answer to it is unchanged.
func decode(r *http.Request, v any) ([]byte, bool) {
	if r.Header.Get("Content-Encoding") != "" {
		return nil, false
	}
	b, err := readBody(r)
	if err != nil {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return b, dec.Decode(v) == nil
}

func (f front) insertDataset(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DatasetReference struct {
			DatasetID string `json:"datasetId"`
		} `json:"datasetReference"`
	}
	if _, ok := decode(r, &body); !ok {
		f.next.ServeHTTP(w, r)
		return
	}
	id := body.DatasetReference.DatasetID
	if msg := checkDatasetID(id); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid", msg)
		return
	}
	// The emulator answers an existing dataset with 500 "dataset ... is
	// already created" (measured); BigQuery answers 409.
	if status, _ := f.get(r, "/datasets/"+url.PathEscape(id)); status == http.StatusOK {
		writeError(w, http.StatusConflict, "duplicate", "Already Exists: Dataset "+projectOf(f.base)+":"+id)
		return
	}
	f.next.ServeHTTP(w, r)
}

// insertTable checks tables.insert's body, or with update, tables.update's
// and tables.patch's, whose table ID is in the path. A table made with a
// FLOAT field is made through createTable (#1000, floattype.go).
func (f front) insertTable(w http.ResponseWriter, r *http.Request, dataset string, update bool) {
	var body struct {
		TableReference *struct {
			TableID string `json:"tableId"`
		} `json:"tableReference"`
		Schema *tableSchema `json:"schema"`
		View   *struct {
			Query        string `json:"query"`
			UseLegacySQL *bool  `json:"useLegacySql"`
		} `json:"view"`
		MaterializedView *struct {
			Query string `json:"query"`
		} `json:"materializedView"`
	}
	raw, ok := decode(r, &body)
	if !ok {
		f.next.ServeHTTP(w, r)
		return
	}
	if !update {
		id := ""
		if body.TableReference != nil {
			id = body.TableReference.TableID
		}
		if msg := checkTableID(id); msg != "" {
			writeError(w, http.StatusBadRequest, "invalid", msg)
			return
		}
	}
	if body.Schema != nil {
		if msg := checkSchema(body.Schema.Fields, ""); msg != "" {
			writeError(w, http.StatusBadRequest, "invalid", msg)
			return
		}
	}
	// A view's columns, and a materialized view's, are its query's
	// (#916): the emulator made a view whose query gave a column `w!`
	// (measured). The query is run alone to read them, as a CREATE VIEW's
	// is; one that cannot run is left to the emulator, which refuses it
	// (measured: 400 "Table not found"). A legacy SQL view is not read:
	// the emulator parses every view as GoogleSQL (measured: a legacy
	// view's [ds.t] was a syntax error).
	var viewQuery string
	switch {
	case body.View != nil && (body.View.UseLegacySQL == nil || !*body.View.UseLegacySQL):
		viewQuery = body.View.Query
	case body.MaterializedView != nil:
		viewQuery = body.MaterializedView.Query
	}
	if strings.TrimSpace(viewQuery) != "" {
		if msg, _ := f.ctasColumns(r, queryOptions{}, viewQuery); msg != "" {
			writeError(w, http.StatusBadRequest, "invalid", msg)
			return
		}
	}
	if !update && body.View == nil && body.MaterializedView == nil && body.Schema != nil &&
		f.createTableFloat64(w, r, dataset, raw, body.Schema.Fields) {
		return
	}
	f.next.ServeHTTP(w, r)
}

// insertRequest is the part of a TableDataInsertAllRequest the checks read.
// What is forwarded is what the client sent, less any row or unknown field
// the front drops.
// https://cloud.google.com/bigquery/docs/reference/rest/v2/tabledata/insertAll
type insertRequest struct {
	SkipInvalidRows     bool `json:"skipInvalidRows"`
	IgnoreUnknownValues bool `json:"ignoreUnknownValues"`
	Rows                []struct {
		InsertID string         `json:"insertId,omitempty"`
		JSON     map[string]any `json:"json"`
	} `json:"rows"`
}

// insertErrorEntry is one element of a TableDataInsertAllResponse's
// insertErrors.
type insertErrorEntry struct {
	Index  int        `json:"index"`
	Errors []rowError `json:"errors"`
}

type insertResponse struct {
	Kind         string             `json:"kind"`
	InsertErrors []insertErrorEntry `json:"insertErrors,omitempty"`
}

func (f front) insertAll(w http.ResponseWriter, r *http.Request, dataset, table string) {
	// The whole request is decoded to find its rows, and re-encoded with
	// the rest of its fields (templateSuffix, traceId) as they came.
	var raw map[string]json.RawMessage
	body, ok := decode(r, &raw)
	var req insertRequest
	if ok {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		ok = dec.Decode(&req) == nil
	}
	if !ok {
		f.next.ServeHTTP(w, r)
		return
	}

	status, got := f.get(r, "/datasets/"+url.PathEscape(dataset)+"/tables/"+url.PathEscape(table))
	if status != http.StatusOK {
		// Not found, or the emulator failing: forward, so the client gets
		// the emulator's own answer to the insert.
		f.next.ServeHTTP(w, r)
		return
	}
	var meta struct {
		Schema tableSchema `json:"schema"`
	}
	if err := json.Unmarshal(got, &meta); err != nil {
		f.next.ServeHTTP(w, r)
		return
	}

	// A value the emulator would store unreadably refuses the whole request
	// as not implemented, before any row reaches it (unstorableNesting).
	for i, row := range req.Rows {
		if loc := unstorableNesting(meta.Schema.Fields, row.JSON, "", false, false); loc != "" {
			writeError(w, http.StatusNotImplemented, "notImplemented", fmt.Sprintf(
				"Not implemented here: the row at index %d holds a value in %s, a RECORD nested in a RECORD with a REPEATED one among them. "+
					"BigQuery accepts it, but the emulator behind CloudBurrow cannot read a table back once such a value "+
					"is streamed into it (\"failed to scan rows\"). Nothing was inserted. The same row written by a DML "+
					"INSERT or a load job is read back.", i, loc))
			return
		}
	}

	var invalid []insertErrorEntry
	keep := make([]int, 0, len(req.Rows)) // original index of each forwarded row
	type outRow struct {
		InsertID string         `json:"insertId,omitempty"`
		JSON     map[string]any `json:"json"`
	}
	var rows []outRow
	for i, row := range req.Rows {
		cleaned, errs := checkRow(meta.Schema.Fields, row.JSON, req.IgnoreUnknownValues)
		if len(errs) > 0 {
			invalid = append(invalid, insertErrorEntry{Index: i, Errors: errs})
			continue
		}
		keep = append(keep, i)
		rows = append(rows, outRow{InsertID: row.InsertID, JSON: cleaned})
	}

	if len(invalid) > 0 && !req.SkipInvalidRows {
		// "skipInvalidRows: Insert all valid rows of a request, even if
		// invalid rows exist. The default value is false, which causes the
		// entire request to fail if any invalid rows exist." The valid
		// rows are reported "stopped", as BigQuery reports them.
		all := invalid
		for _, i := range keep {
			all = append(all, insertErrorEntry{Index: i, Errors: []rowError{{Reason: "stopped"}}})
		}
		sort.Slice(all, func(a, b int) bool { return all[a].Index < all[b].Index })
		writeJSON(w, http.StatusOK, insertResponse{Kind: "bigquery#tableDataInsertAllResponse", InsertErrors: all})
		return
	}
	if len(rows) == 0 {
		writeJSON(w, http.StatusOK, insertResponse{Kind: "bigquery#tableDataInsertAllResponse", InsertErrors: invalid})
		return
	}

	encoded, err := json.Marshal(rows)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internalError", err.Error())
		return
	}
	raw["rows"] = encoded
	out, err := json.Marshal(raw)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internalError", err.Error())
		return
	}
	setBody(r, out)

	rec := newRecorder()
	f.next.ServeHTTP(rec, r)
	if len(invalid) == 0 || rec.status != http.StatusOK {
		rec.copyTo(w)
		return
	}
	// Some rows were dropped before forwarding: the emulator's indexes are
	// into the rows it saw, so they are mapped back to the request's.
	var resp insertResponse
	if err := json.Unmarshal(rec.body.Bytes(), &resp); err != nil {
		rec.copyTo(w)
		return
	}
	for _, e := range resp.InsertErrors {
		if e.Index >= 0 && e.Index < len(keep) {
			e.Index = keep[e.Index]
		}
		invalid = append(invalid, e)
	}
	sort.Slice(invalid, func(a, b int) bool { return invalid[a].Index < invalid[b].Index })
	writeJSON(w, http.StatusOK, insertResponse{Kind: "bigquery#tableDataInsertAllResponse", InsertErrors: invalid})
}

// get reads base+path from the emulator as r's client would, and returns
// the status and body.
func (f front) get(r *http.Request, path string) (int, []byte) {
	return f.send(r, http.MethodGet, path, nil)
}

// send makes a request of its own to base+path on the emulator, with r's
// Host and context, and returns the status and body: a read a check needs,
// a query that reads a result's columns, or the removal of a table a
// refused load made.
func (f front) send(r *http.Request, method, path string, body []byte) (int, []byte) {
	u := *r.URL
	u.Path, u.RawPath = "", ""
	p := f.base + path
	if unescaped, err := url.PathUnescape(p); err == nil {
		u.Path, u.RawPath = unescaped, p
	}
	u.RawQuery = ""
	if i := strings.Index(p, "?"); i >= 0 {
		// A path with a query string: tabledata.list's pageToken.
		u.RawQuery = p[i+1:]
		p = p[:i]
		u.Path, u.RawPath = "", ""
		if unescaped, err := url.PathUnescape(p); err == nil {
			u.Path, u.RawPath = unescaped, p
		}
	}
	// A request a server receives always has a body, if an empty one.
	var rd io.Reader = http.NoBody
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(r.Context(), method, u.String(), rd)
	if err != nil {
		return 0, nil
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Host = r.Host
	req.RemoteAddr = r.RemoteAddr
	req.Proto, req.ProtoMajor, req.ProtoMinor = "HTTP/1.1", 1, 1
	rec := newRecorder()
	f.next.ServeHTTP(rec, req)
	if method == http.MethodPost && path == "/queries" && (rec.status == 0 || rec.status == http.StatusOK) {
		// The front's own query: not a job of the client's (#972).
		var resp map[string]any
		if json.Unmarshal(rec.body.Bytes(), &resp) == nil {
			project, id := jobRef(resp)
			if project == "" {
				project = projectOf(f.base)
			}
			f.records.markInternal(project, id)
		}
	}
	return rec.status, rec.body.Bytes()
}

// projectOf returns the project in a base path.
func projectOf(base string) string {
	p := base[strings.LastIndex(base, "/")+1:]
	if u, err := url.PathUnescape(p); err == nil {
		return u
	}
	return p
}

// writeError answers with BigQuery's error body: an ErrorProto list inside
// the standard Google API error.
func writeError(w http.ResponseWriter, code int, reason, message string) {
	status := map[int]string{
		http.StatusBadRequest:          "INVALID_ARGUMENT",
		http.StatusConflict:            "ALREADY_EXISTS",
		http.StatusNotImplemented:      "UNIMPLEMENTED",
		http.StatusInternalServerError: "INTERNAL",
	}[code]
	writeJSON(w, code, map[string]any{"error": map[string]any{
		"code":    code,
		"message": message,
		"errors":  []map[string]string{{"message": message, "domain": "global", "reason": reason}},
		"status":  status,
	}})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

// recorder captures a response from next, for a read the front makes or an
// answer it rewrites.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newRecorder() *recorder { return &recorder{header: http.Header{}} }

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}

// copyTo writes the captured response to w as it was.
func (r *recorder) copyTo(w http.ResponseWriter) {
	for k, v := range r.header {
		w.Header()[k] = v
	}
	w.Header().Set("Content-Length", fmt.Sprint(r.body.Len()))
	if r.status == 0 {
		r.status = http.StatusOK
	}
	w.WriteHeader(r.status)
	_, _ = w.Write(r.body.Bytes())
}
