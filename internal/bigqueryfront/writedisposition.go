package bigqueryfront

import (
	"encoding/json"
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
// into the table whatever writeDisposition says (its source,
// jobsInsertHandler.Handle, adds the rows to a table that exists; measured:
// WRITE_TRUNCATE_DATA, WRITE_TRUNCATE and WRITE_EMPTY each appended the
// result to a table with rows). BigQuery replaces the rows with
// WRITE_TRUNCATE_DATA and WRITE_TRUNCATE, and with WRITE_EMPTY (the
// default) fails the job when the table has rows. The front answers such
// a job 501 when the table exists, for WRITE_TRUNCATE_DATA and
// WRITE_TRUNCATE, and when it has rows, for WRITE_EMPTY; WRITE_APPEND and
// a table that does not exist are sent on as they are.

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

// queryDestination returns the 501 for a query job whose destination
// table, dest, the emulator would write otherwise than write says
// (above), or "".
func (f front) queryDestination(r *http.Request, dest *tableRef, write string) string {
	if dest == nil || dest.DatasetID == "" || dest.TableID == "" {
		return ""
	}
	write = strings.ToUpper(write)
	if write == "WRITE_APPEND" {
		return ""
	}
	if dest.ProjectID != "" && dest.ProjectID != projectOf(f.base) {
		return ""
	}
	status, got := f.get(r, tablePath(dest.DatasetID, dest.TableID))
	if status != http.StatusOK {
		// Not there (the emulator makes it), or unreadable: the
		// emulator's answer stands.
		return ""
	}
	disposition := write
	if disposition == "" {
		disposition = "WRITE_EMPTY (the default)"
	}
	why := "BigQuery then replaces the table's rows with the result, but the emulator behind CloudBurrow appends it " +
		"(measured, #1067)"
	if write == "" || write == "WRITE_EMPTY" {
		var meta struct {
			NumRows string `json:"numRows"`
		}
		if json.Unmarshal(got, &meta) == nil && (meta.NumRows == "" || meta.NumRows == "0") {
			return ""
		}
		why = "BigQuery then fails the job, as the table has rows, but the emulator behind CloudBurrow appends the " +
			"result (measured, #1067)"
	}
	return "Not implemented here: a query job with writeDisposition " + disposition + " into the table " +
		dest.DatasetID + "." + dest.TableID + ", which exists. " + why + ". Nothing was run. WRITE_APPEND is served, " +
		"and so is a destination table that does not exist yet."
}
