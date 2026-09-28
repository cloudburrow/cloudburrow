package bigqueryfront

import (
	"net/http"
	"strings"
)

// WRITE_TRUNCATE_DATA, and a query job's writeDisposition (#1067).
//
// BigQuery: "WRITE_TRUNCATE_DATA: If the table already exists, BigQuery
// overwrites the data, but keeps the constraints and schema of the
// existing table"
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobConfigurationLoad).
// The emulator knows only WRITE_TRUNCATE and WRITE_EMPTY in a load (its
// source, server/handler.go, uploadContentHandler.Handle), so it appended
// such a load's rows: measured through the front with the official Go
// client, a NEWLINE_DELIMITED_JSON upload, a CSV upload and a JSON load of
// two objects from Cloud Storage, each with WRITE_TRUNCATE_DATA into a
// table with rows, kept the table's rows and added the load's. Its
// WRITE_TRUNCATE of a table that exists deletes the table's rows in the
// same transaction as it inserts the load's, and keeps the table's schema
// (the load writes into the table's columns; the same source): that is
// WRITE_TRUNCATE_DATA. So the front sends a load with WRITE_TRUNCATE_DATA
// to the emulator as WRITE_TRUNCATE; the job reads back (jobs.insert,
// jobs.get, jobs.list) with the client's WRITE_TRUNCATE_DATA (jobTexts). A
// load the emulator reads from Cloud Storage itself writes each object in
// a transaction of its own, the first replacing the table's rows (its
// source, importFromGCS); the front reads a JSON or CSV load from Cloud
// Storage itself when it can (gcsload.go, jsonload.go), and sends the
// objects as one upload, into one transaction. A Parquet load the front
// carries out itself (parquetload.go).
//
// A query job with a destination table: the emulator writes the result
// into the table whatever writeDisposition says; the front carries out
// WRITE_TRUNCATE, WRITE_TRUNCATE_DATA and WRITE_EMPTY into a table that
// exists (querywrite.go, #1080).

// truncateData sends a load job with WRITE_TRUNCATE_DATA, in r's body,
// with WRITE_TRUNCATE (above), and reports whether it did, with the
// client's text for the job's answers.
func truncateData(r *http.Request, write string) (jobText, bool) {
	if !strings.EqualFold(write, "WRITE_TRUNCATE_DATA") {
		return jobText{}, false
	}
	if !editJob(r, func(job map[string]any) bool {
		conf, _ := job["configuration"].(map[string]any)
		load, _ := conf["load"].(map[string]any)
		if load == nil {
			return false
		}
		load["writeDisposition"] = "WRITE_TRUNCATE"
		return true
	}) {
		return jobText{}, false
	}
	return jobText{writeDisposition: write}, true
}
