package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Tables made by a job rather than by tables.insert (#881): a load job, a
// query or copy job with a destination table, and a DDL statement in a
// query. The emulator checks none of their names (measured against the
// pinned image): it loaded rows into a table "t!", and ran CREATE TABLE
// ds.`t!`, a column `a b!` and CREATE SCHEMA `bad-name`. The front holds
// them to the rules tables.insert and datasets.insert are held to.

// jobsRoute matches jobs.insert, with or without the media upload prefix a
// load from a local file is sent to, and jobs.query.
var jobsRoute = regexp.MustCompile(`^(?:/upload)?(/bigquery/v2)?/projects/([^/]+)/(jobs|queries)$`)

// tableRef is a TableReference; only the table ID is checked, as a dataset
// or project that does not exist is the emulator's to answer.
type tableRef struct {
	ProjectID string `json:"projectId"`
	DatasetID string `json:"datasetId"`
	TableID   string `json:"tableId"`
}

// jobBody is the part of a Job the checks read.
// https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobConfiguration
type jobBody struct {
	JobReference struct {
		ProjectID string `json:"projectId"`
		JobID     string `json:"jobId"`
	} `json:"jobReference"`
	Configuration struct {
		Load *struct {
			DestinationTable       *tableRef    `json:"destinationTable"`
			Schema                 *tableSchema `json:"schema"`
			Autodetect             bool         `json:"autodetect"`
			SourceFormat           string       `json:"sourceFormat"`
			ColumnNameCharacterMap string       `json:"columnNameCharacterMap"`
			// SkipLeadingRows is a number, or its string (the REST
			// API's int64).
			SkipLeadingRows json.RawMessage `json:"skipLeadingRows"`
			SourceURIs      []string        `json:"sourceUris"`
			// WriteDisposition, for its output rows (#966, countLoad).
			WriteDisposition string `json:"writeDisposition"`
			// SchemaUpdateOptions, for a Parquet load with no schema (#970).
			SchemaUpdateOptions []string `json:"schemaUpdateOptions"`
			// The CSV options the front reads a load's data by (#945).
			FieldDelimiter  string  `json:"fieldDelimiter"`
			Quote           *string `json:"quote"`
			AllowJaggedRows bool    `json:"allowJaggedRows"`
			NullMarker      *string `json:"nullMarker"`
		} `json:"load"`
		Query *struct {
			queryOptions
			DestinationTable *tableRef `json:"destinationTable"`
		} `json:"query"`
		Copy    *copyConfig    `json:"copy"`
		Extract *extractConfig `json:"extract"`
	} `json:"configuration"`
}

// queryOptions are the parts of a query, in jobs.query's QueryRequest or a
// query job's configuration, that a CREATE TABLE ... AS SELECT's query is
// run with to read its columns (ctasColumns): its text, the dataset its
// unqualified names are in, and its parameters.
type queryOptions struct {
	Query           string          `json:"query"`
	UseLegacySQL    *bool           `json:"useLegacySql,omitempty"`
	DefaultDataset  json.RawMessage `json:"defaultDataset,omitempty"`
	ParameterMode   string          `json:"parameterMode,omitempty"`
	QueryParameters json.RawMessage `json:"queryParameters,omitempty"`
}

// maxJobPart bounds how much of a multipart upload the front reads to find
// its first part, the job. A load's data follows it and is not read.
const maxJobPart = 1 << 20

// insertJob checks jobs.insert. f's base is the REST path of the project in
// the request, for the reads a job can need: the destination table's
// schema, and a CREATE TABLE ... AS SELECT's columns.
//
// A job with no jobReference is given one first (withReference, #973), and
// the job is timed (jobRecords.timed, #971).
func (f front) insertJob(w http.ResponseWriter, r *http.Request) {
	created := f.records.clock()
	var job jobBody
	if !decodeJob(r, &job) {
		f.next.ServeHTTP(w, r)
		return
	}
	if !withReference(r, &job, projectOf(f.base)) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not give the job a jobReference")
		return
	}
	project := job.JobReference.ProjectID
	if project == "" {
		project = projectOf(f.base)
	}
	rec := newRecorder()
	f.checkJob(rec, r, job, project)
	f.records.timed(w, rec, created, project, job.JobReference.JobID, true)
}

// checkJob is insertJob's checks of job, whose project is project.
func (f front) checkJob(w http.ResponseWriter, r *http.Request, job jobBody, project string) {
	f.next = f.failed.watch(f.next, project, job.JobReference.JobID)
	if f.configs != nil {
		f.next = f.configs.recording(f.next, project)
	}
	next := f.next
	c := job.Configuration
	if c.Load != nil && c.Load.SourceFormat == "" && setLoadSourceFormat(r, "CSV") {
		c.Load.SourceFormat = "CSV"
	}
	check := func(ref *tableRef) string {
		if ref == nil {
			return ""
		}
		return checkTableID(ref.TableID)
	}
	var msg, reason string
	var afterLoad func() // #1000, loadFloat
	switch {
	case c.Load != nil:
		reason = "invalid"
		msg = check(c.Load.DestinationTable)
		if msg == "" && !f.parquetSchema(w, r, &job) { // #970
			return
		}
		if msg == "" && c.Load.Schema != nil {
			msg = checkSchema(c.Load.Schema.Fields, "")
		}
		if msg == "" {
			if loc := f.unloadable(r, c.Load.Schema, c.Load.DestinationTable); loc != "" {
				writeError(w, http.StatusNotImplemented, "notImplemented", fmt.Sprintf(
					"Not implemented here: the load's schema has %s, a RECORD inside a REPEATED RECORD. BigQuery loads it, "+
						"but the emulator behind CloudBurrow does not load such a value reliably: measured, some loads stored it "+
						"so that the table could no longer be read (\"failed to scan rows\"). Nothing was loaded. The same rows "+
						"written by a DML INSERT are read back.", loc))
				return
			}
		}
		if msg == "" {
			afterLoad = f.loadFloat(r, c.Load.Schema, c.Load.DestinationTable)
		}
		var read *dataFailure
		if msg == "" && strings.EqualFold(c.Load.SourceFormat, "CSV") {
			var ok bool
			if r, next, read, ok = f.csvLoad(w, r, job, next); !ok {
				return
			}
		}
		if msg == "" {
			r, next = f.countLoad(r, next, job, read) // #960, #966, loadstats.go
			f.next = next
		}
		if msg == "" && c.Load.Autodetect && c.Load.Schema == nil && c.Load.DestinationTable != nil {
			f.autodetectLoad(w, r, job)
			return
		}
	case c.Copy != nil:
		if reason, msg = "invalid", check(c.Copy.DestinationTable); msg == "" {
			f.copyJob(w, r, c.Copy) // #987
			return
		}
	case c.Extract != nil:
		f.extractJob(w, r, c.Extract)
		return
	case c.Query != nil:
		if msg = check(c.Query.DestinationTable); msg != "" {
			reason = "invalid"
			break
		}
		f.runQuery(w, r, c.Query.queryOptions, true) // #1008, #1014
		return
	}
	if msg != "" {
		writeError(w, http.StatusBadRequest, reason, msg)
		return
	}
	if afterLoad != nil {
		rec := newRecorder()
		next.ServeHTTP(rec, r)
		afterLoad()
		rec.copyTo(w)
		return
	}
	next.ServeHTTP(w, r)
}

// lone runs a query alone, with no rows, through jobs.query with the
// options of the statement it is part of (its default dataset and
// parameters), as `SELECT * FROM (query) LIMIT 0`, which returns no rows
// and writes nothing. It returns the result's columns, or, when the query
// cannot run alone, the emulator's status and body.
func (f front) lone(r *http.Request, q queryOptions, sel string) (fields []field, status int, body []byte) {
	legacy := false
	req, err := json.Marshal(queryOptions{Query: "SELECT * FROM (\n" + sel + "\n) LIMIT 0", UseLegacySQL: &legacy,
		DefaultDataset: q.DefaultDataset, ParameterMode: q.ParameterMode, QueryParameters: q.QueryParameters})
	if err != nil {
		return nil, http.StatusInternalServerError, nil
	}
	status, got := f.send(r, http.MethodPost, "/queries", req)
	var res struct {
		Schema tableSchema `json:"schema"`
	}
	if status != http.StatusOK || json.Unmarshal(got, &res) != nil {
		if status == http.StatusOK {
			status = http.StatusInternalServerError
		}
		return nil, status, got
	}
	return res.Schema.Fields, http.StatusOK, nil
}

// ctasColumns returns why a column of a CREATE TABLE ... AS SELECT's or a
// CREATE VIEW's query has a name BigQuery refuses for a table's column, or
// "", and whether the query could be run alone to tell (#901, #916).
//
// The query is run alone (lone). The engine names the columns, so what is
// checked is exactly what the table or view would be given, nested STRUCT
// fields and names from SELECT * and WITH included, not a guess from the
// select list's text. The emulator stored CREATE TABLE ds.c AS SELECT 1 AS
// `x!`, STRUCT(2 AS `y?`) AS s with both names, and CREATE VIEW ds.v AS
// SELECT a AS `x!` (measured). A query that cannot run alone, such as one
// naming a script variable or a table an earlier statement of the same
// script makes, fails there, and ran is false: its columns are checked
// after the script, in the table it made (serveQuery). The emulator names
// a result column with no alias $col1, $col2 ... in the query run alone;
// those are not refused here, as the statement itself is refused by the
// emulator's analyser, 400 "CREATE TABLE columns must be named" or
// "CREATE VIEW columns must be named, but column 1 has no name" (measured).
func (f front) ctasColumns(r *http.Request, q queryOptions, sel string) (msg string, ran bool) {
	fields, status, _ := f.lone(r, q, sel)
	if status != http.StatusOK {
		return "", false
	}
	return checkNames(fields, "", anonymousColumn, checkColumnName), true
}

// anonymousColumn matches the emulator's name for a result column with no
// alias.
var anonymousColumn = regexp.MustCompile(`^\$col[0-9]+$`)

// checkNames returns why a name in fields, or a nested field's, fails
// check, or why two at one level clash (they differ only in case), or "".
// A name skip matches is not checked.
func checkNames(fields []field, prefix string, skip *regexp.Regexp, check func(string) string) string {
	seen := map[string]bool{}
	for _, fl := range fields {
		if skip == nil || !skip.MatchString(fl.Name) {
			if msg := check(fl.Name); msg != "" {
				return strings.Replace(msg, fmt.Sprintf("%q", fl.Name), fmt.Sprintf("%q", prefix+fl.Name), 1)
			}
		}
		key := strings.ToLower(fl.Name)
		if seen[key] {
			return fmt.Sprintf("Field %s%s already exists in schema; field names are case-insensitive", prefix, fl.Name)
		}
		seen[key] = true
		if msg := checkNames(fl.Fields, prefix+fl.Name+".", skip, check); msg != "" {
			return msg
		}
	}
	return ""
}

// unloadable returns the first RECORD inside a REPEATED RECORD in a load's
// schema, or, when the job gives none, in its destination table's, or "".
//
// Measured (#881): a JSON load of a value into such a RECORD left the
// table unreadable, 500 "failed to scan rows: failed to convert struct
// from string", in one compat run against the pinned image, and in 5 of 60
// loads of one row against v0.8.1's source, the same data each time; the
// rest read back. The fields of the REPEATED RECORD's element were stored
// out of order. A REPEATED
// RECORD inside a RECORD loaded and read back in 40 of 40, and a DML
// INSERT of either in 40 of 40. The upstream fix for the streaming fault
// (goccy/googlesqlite#76, unreleased) fixed this too, measured the same
// way. The data itself is not read (it may be in Cloud Storage, or
// megabytes of an upload), so a load into such a schema is refused
// whatever its rows hold.
func (f front) unloadable(r *http.Request, schema *tableSchema, dest *tableRef) string {
	if schema == nil && dest != nil && dest.DatasetID != "" && dest.TableID != "" {
		status, got := f.get(r, "/datasets/"+url.PathEscape(dest.DatasetID)+"/tables/"+url.PathEscape(dest.TableID))
		var meta struct {
			Schema *tableSchema `json:"schema"`
		}
		if status == http.StatusOK && json.Unmarshal(got, &meta) == nil {
			schema = meta.Schema
		}
	}
	if schema == nil {
		return ""
	}
	return recordUnderRepeated(schema.Fields, "", false)
}

// recordUnderRepeated returns the first RECORD with a REPEATED RECORD above
// it, or "".
func recordUnderRepeated(fields []field, prefix string, repeatedAbove bool) string {
	for _, f := range fields {
		typ := strings.ToUpper(f.Type)
		if typ != "RECORD" && typ != "STRUCT" {
			continue
		}
		if repeatedAbove {
			return prefix + f.Name
		}
		if loc := recordUnderRepeated(f.Fields, prefix+f.Name+".", strings.ToUpper(f.Mode) == "REPEATED"); loc != "" {
			return loc
		}
	}
	return ""
}

// query checks jobs.query's statement.
func (f front) query(w http.ResponseWriter, r *http.Request) {
	created := f.records.clock()
	var body queryOptions
	if _, ok := decode(r, &body); !ok {
		f.next.ServeHTTP(w, r)
		return
	}
	rec := newRecorder()
	f.runQuery(rec, r, body, false) // #1008, #1014
	f.records.timed(w, rec, created, projectOf(f.base), "", false)
}

// decodeJob reads the Job in r: the JSON body of jobs.insert and of a
// resumable upload's first request, or the first part of a multipart
// upload. Only that part is read; the rest of the body, the load's data, is
// passed on untouched. It reports false for a body it cannot read, which is
// then forwarded unchecked.
func decodeJob(r *http.Request, v any) bool {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mt, "multipart/") {
		_, ok := decode(r, v)
		return ok
	}
	if r.Header.Get("Content-Encoding") != "" || params["boundary"] == "" {
		return false
	}
	head, err := io.ReadAll(io.LimitReader(r.Body, maxJobPart))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}
	if err != nil {
		return false
	}
	part, err := multipart.NewReader(bytes.NewReader(head), params["boundary"]).NextPart()
	if err != nil {
		return false
	}
	b, err := io.ReadAll(part)
	if err != nil {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return dec.Decode(v) == nil
}

// setLoadSourceFormat sets a load job's sourceFormat in r's body, the JSON
// of jobs.insert or the first part of a multipart upload, and reports
// whether it did (#919). BigQuery's default is CSV, and the Go client
// sends none for a CSV load unless one is set; the emulator has no
// default: it answered such a load 400 "not support sourceFormat: " with
// a schema, and 500 "nil pointer dereference" with autodetect, which the
// Go client retries until its deadline (measured).
func setLoadSourceFormat(r *http.Request, format string) bool {
	return editJob(r, func(job map[string]any) bool {
		conf, _ := job["configuration"].(map[string]any)
		load, _ := conf["load"].(map[string]any)
		if load == nil {
			return false
		}
		load["sourceFormat"] = format
		return true
	})
}

// editJob changes the Job in r's body, the JSON of jobs.insert or the
// first part of a multipart upload, with edit, and reports whether it did:
// edit reports whether it changed the job.
func editJob(r *http.Request, edit func(job map[string]any) bool) bool {
	set := func(b []byte) ([]byte, bool) {
		var job map[string]any
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		if dec.Decode(&job) != nil || !edit(job) {
			return nil, false
		}
		out, err := json.Marshal(job)
		return out, err == nil
	}
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mt, "multipart/") {
		b, err := readBody(r)
		if err != nil {
			return false
		}
		out, ok := set(b)
		if ok {
			setBody(r, out)
		}
		return ok
	}
	// The first part is replaced in the head of the body; the rest, the
	// load's data, is passed on untouched.
	head, err := io.ReadAll(io.LimitReader(r.Body, maxJobPart))
	rest := r.Body
	restore := func(prefix []byte) {
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(prefix), rest), rest}
	}
	if err != nil {
		restore(head)
		return false
	}
	delim := []byte("--" + params["boundary"])
	start := bytes.Index(head, delim)
	var hdrEnd, partEnd int
	if start >= 0 {
		hdrEnd = bytes.Index(head[start:], []byte("\r\n\r\n"))
	}
	if start < 0 || hdrEnd < 0 {
		restore(head)
		return false
	}
	hdrEnd += start + 4
	partEnd = bytes.Index(head[hdrEnd:], append([]byte("\r\n"), delim...))
	if partEnd < 0 {
		restore(head)
		return false
	}
	partEnd += hdrEnd
	out, ok := set(head[hdrEnd:partEnd])
	if !ok {
		restore(head)
		return false
	}
	prefix := append(append(append([]byte{}, head[:hdrEnd]...), out...), head[partEnd:]...)
	restore(prefix)
	if r.ContentLength > 0 {
		r.ContentLength += int64(len(prefix) - len(head))
		r.Header.Del("Content-Length")
	}
	return true
}
