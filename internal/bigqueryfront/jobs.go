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
		} `json:"load"`
		Query *struct {
			queryOptions
			DestinationTable *tableRef `json:"destinationTable"`
		} `json:"query"`
		Copy *struct {
			DestinationTable *tableRef `json:"destinationTable"`
		} `json:"copy"`
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
func (f front) insertJob(w http.ResponseWriter, r *http.Request) {
	next := f.next
	var job jobBody
	if !decodeJob(r, &job) {
		next.ServeHTTP(w, r)
		return
	}
	c := job.Configuration
	check := func(ref *tableRef) string {
		if ref == nil {
			return ""
		}
		return checkTableID(ref.TableID)
	}
	var msg, reason string
	switch {
	case c.Load != nil:
		reason = "invalid"
		msg = check(c.Load.DestinationTable)
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
		if msg == "" && c.Load.Autodetect && c.Load.Schema == nil && c.Load.DestinationTable != nil {
			f.autodetectLoad(w, r, job)
			return
		}
	case c.Copy != nil:
		reason, msg = "invalid", check(c.Copy.DestinationTable)
	case c.Query != nil:
		if msg = check(c.Query.DestinationTable); msg != "" {
			reason = "invalid"
			break
		}
		if f.refuseQuery(w, r, c.Query.queryOptions) {
			return
		}
	}
	if msg != "" {
		writeError(w, http.StatusBadRequest, reason, msg)
		return
	}
	next.ServeHTTP(w, r)
}

// refuseQuery answers a query the front refuses, and reports whether it
// did: a name BigQuery refuses in its DDL (checkDDL), 400; a statement the
// emulator would not run, 501; or a CREATE TABLE ... AS SELECT whose query
// gives a column a name BigQuery refuses, 400 (ctasColumns).
func (f front) refuseQuery(w http.ResponseWriter, r *http.Request, q queryOptions) bool {
	if q.UseLegacySQL != nil && *q.UseLegacySQL {
		// Legacy SQL has no DDL or scripting.
		return false
	}
	v := checkDDL(q.Query)
	if v.code != 0 {
		writeError(w, v.code, v.reason, v.msg)
		return true
	}
	for _, sel := range v.selects {
		if msg := f.ctasColumns(r, q, sel); msg != "" {
			writeError(w, http.StatusBadRequest, "invalidQuery", msg)
			return true
		}
	}
	return false
}

// ctasColumns runs a CREATE TABLE ... AS SELECT's query alone, with no
// rows, and returns why a column of its result has a name BigQuery refuses
// for a table's column, or "" (#901).
//
// The engine names the columns, so what is checked is exactly what the
// table would be given, nested STRUCT fields and names from SELECT * and
// WITH included, not a guess from the select list's text. The emulator
// stored CREATE TABLE ds.c AS SELECT 1 AS `x!`, STRUCT(2 AS `y?`) AS s
// with both names (measured). The query is run through jobs.query with the
// statement's default dataset and parameters, as `SELECT * FROM (query)
// LIMIT 0`, which returns no rows and writes nothing. A query that cannot
// run alone, such as one naming a script variable or a table an earlier
// statement of the same script makes, fails there, and the statement is
// then sent on unchecked: the emulator's own answer to it stands. The
// emulator names a result column with no alias $col1, $col2 ... in the
// query run alone; those are not refused here, as the statement itself is
// refused by the emulator's analyser, 400 "CREATE TABLE columns must be
// named" (measured).
func (f front) ctasColumns(r *http.Request, q queryOptions, sel string) string {
	legacy := false
	body, err := json.Marshal(queryOptions{Query: "SELECT * FROM (\n" + sel + "\n) LIMIT 0", UseLegacySQL: &legacy,
		DefaultDataset: q.DefaultDataset, ParameterMode: q.ParameterMode, QueryParameters: q.QueryParameters})
	if err != nil {
		return ""
	}
	status, got := f.send(r, http.MethodPost, "/queries", body)
	var res struct {
		Schema tableSchema `json:"schema"`
	}
	if status != http.StatusOK || json.Unmarshal(got, &res) != nil {
		return ""
	}
	return checkNames(res.Schema.Fields, "", anonymousColumn, checkColumnName)
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
	var body queryOptions
	if _, ok := decode(r, &body); ok && f.refuseQuery(w, r, body) {
		return
	}
	f.next.ServeHTTP(w, r)
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
