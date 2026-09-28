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
	Configuration struct {
		Load *struct {
			DestinationTable *tableRef    `json:"destinationTable"`
			Schema           *tableSchema `json:"schema"`
		} `json:"load"`
		Query *struct {
			Query            string    `json:"query"`
			DestinationTable *tableRef `json:"destinationTable"`
		} `json:"query"`
		Copy *struct {
			DestinationTable *tableRef `json:"destinationTable"`
		} `json:"copy"`
	} `json:"configuration"`
}

// maxJobPart bounds how much of a multipart upload the front reads to find
// its first part, the job. A load's data follows it and is not read.
const maxJobPart = 1 << 20

// insertJob checks jobs.insert. f's base is the REST path of the project in
// the request, for the one read a load can need: the destination table's
// schema.
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
	case c.Copy != nil:
		reason, msg = "invalid", check(c.Copy.DestinationTable)
	case c.Query != nil:
		reason, msg = "invalid", check(c.Query.DestinationTable)
		if msg == "" {
			reason, msg = "invalidQuery", checkDDL(c.Query.Query)
		}
	}
	if msg != "" {
		writeError(w, http.StatusBadRequest, reason, msg)
		return
	}
	next.ServeHTTP(w, r)
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
func query(next http.Handler, w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query string `json:"query"`
	}
	if _, ok := decode(r, &body); ok {
		if msg := checkDDL(body.Query); msg != "" {
			writeError(w, http.StatusBadRequest, "invalidQuery", msg)
			return
		}
	}
	next.ServeHTTP(w, r)
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
