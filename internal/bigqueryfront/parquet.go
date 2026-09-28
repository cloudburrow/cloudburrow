package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// A Parquet load whose job gives no schema (#970).
//
// BigQuery reads a Parquet file's schema from the file: "the schema is
// automatically retrieved from the self-describing source data"
// (https://cloud.google.com/bigquery/docs/loading-data-cloud-storage-parquet).
// The emulator reads a Parquet file's columns only from the job's
// configuration.load.schema (its source, server/handler.go,
// uploadContentHandler.Handle, `case "PARQUET"`: load.Schema.Fields, and
// importFromGCS the same way), so such a load failed 500 "runtime error:
// invalid memory address or nil pointer dereference" (measured against the
// pinned image through the official Go client: an upload, into a new
// table and into one that exists; a load from Cloud Storage, which the Go
// client retried until its deadline).
//
// So the front sends a Parquet load with no schema into a table that
// exists with the table's schema, which the file's columns are read by:
// the emulator then loads the file's columns of those names. One into a
// table that does not exist, or that replaces the table (WRITE_TRUNCATE)
// or may change its schema (schemaUpdateOptions), takes its schema from
// the file in BigQuery, which the front does not read: 501, before
// anything is sent.

// parquetSchema gives a Parquet load with no schema its destination
// table's (above), in r's body and in job, and reports false when it has
// answered the request.
func (f front) parquetSchema(w http.ResponseWriter, r *http.Request, job *jobBody) bool {
	l := job.Configuration.Load
	if l == nil || l.Schema != nil || !strings.EqualFold(l.SourceFormat, "PARQUET") || l.DestinationTable == nil {
		return true
	}
	dest := l.DestinationTable
	name := dest.DatasetID + "." + dest.TableID
	notImplemented := func(why string) bool {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a Parquet load with no schema "+why+
			". BigQuery reads the schema from the file, but the emulator behind CloudBurrow reads a Parquet file's columns "+
			"only from the job's schema (measured: 500 \"nil pointer dereference\"), and CloudBurrow does not read the "+
			"file's schema. Nothing was loaded. Give the load its schema (Schema in the Go client's ReaderSource or "+
			"GCSReference), or create the table first and append to it.")
		return false
	}
	switch {
	case strings.EqualFold(l.WriteDisposition, "WRITE_TRUNCATE"):
		return notImplemented("that replaces " + name + " (WRITE_TRUNCATE), whose schema then comes from the file")
	case len(l.SchemaUpdateOptions) > 0:
		return notImplemented("and schemaUpdateOptions " + strings.Join(l.SchemaUpdateOptions, ", ") +
			", which change " + name + "'s schema to the file's")
	case dest.ProjectID != "" && dest.ProjectID != projectOf(f.base):
		// Another project's table: the emulator's own answer stands.
		return true
	}
	status, got := f.get(r, "/datasets/"+url.PathEscape(dest.DatasetID)+"/tables/"+url.PathEscape(dest.TableID))
	if status == http.StatusNotFound {
		return notImplemented(fmt.Sprintf("into %s, which does not exist, so its schema would come from the file", name))
	}
	var meta struct {
		Schema *struct {
			Fields json.RawMessage `json:"fields"`
		} `json:"schema"`
	}
	if status != http.StatusOK || json.Unmarshal(got, &meta) != nil || meta.Schema == nil || len(meta.Schema.Fields) == 0 ||
		string(meta.Schema.Fields) == "null" {
		return true
	}
	var fields []field
	if json.Unmarshal(meta.Schema.Fields, &fields) != nil {
		return true
	}
	var raw any
	if json.Unmarshal(meta.Schema.Fields, &raw) != nil {
		return true
	}
	if !editJob(r, func(j map[string]any) bool {
		conf, _ := j["configuration"].(map[string]any)
		load, _ := conf["load"].(map[string]any)
		if load == nil {
			return false
		}
		load["schema"] = map[string]any{"fields": raw}
		return true
	}) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not give the Parquet load its table's schema")
		return false
	}
	l.Schema = &tableSchema{Fields: fields}
	return true
}
