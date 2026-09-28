package bigqueryfront

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// extractConfig is the part of an extract job's configuration the front
// reads.
// https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobConfigurationExtract
type extractConfig struct {
	SourceTable *struct {
		ProjectID string `json:"projectId"`
		DatasetID string `json:"datasetId"`
		TableID   string `json:"tableId"`
	} `json:"sourceTable"`
	SourceModel       json.RawMessage `json:"sourceModel"`
	DestinationURIs   []string        `json:"destinationUris"`
	DestinationURI    string          `json:"destinationUri"`
	DestinationFormat string          `json:"destinationFormat"`
	Compression       string          `json:"compression"`
	FieldDelimiter    string          `json:"fieldDelimiter"`
	PrintHeader       *bool           `json:"printHeader"`
}

// csvExportTypes are the column types whose values the emulator writes to
// a CSV file as BigQuery does (measured against the pinned image: 1,
// true, a base64 BYTES value YWI=, 2020-01-02, a NUMERIC 1.25; a NULL as
// an empty field). The emulator wrote a TIMESTAMP as "2020-01-02
// 03:04:05+00", where BigQuery writes "2020-01-02 03:04:05 UTC"; the rest
// were not measured against BigQuery's form.
var csvExportTypes = map[string]bool{
	"STRING": true, "INTEGER": true, "INT64": true, "BOOLEAN": true, "BOOL": true,
	"BYTES": true, "DATE": true, "NUMERIC": true,
}

// wildcardShard is the name BigQuery gives the first file of a wildcard
// URI: "BigQuery replaces the asterisk with a 12-digit number, starting
// with 000000000000"
// (https://cloud.google.com/bigquery/docs/exporting-data#exporting_data_into_one_or_more_files).
const wildcardShard = "000000000000"

// extractJob checks an extract job to Cloud Storage (#939), whose job is
// job, and sends on the ones the emulator carries out as BigQuery does.
//
// The emulator writes an extract to the instance's Cloud Storage, which
// STORAGE_EMULATOR_HOST names in its container (#919): never
// storage.googleapis.com. Measured against the pinned image with the
// official Go client (Table.ExtractorTo), it differs from BigQuery so:
//
//   - With no destinationFormat, which the Go client sends for a CSV
//     extract, it failed "unsupported destination format " and left an
//     empty object at the URI. BigQuery's default is CSV, so CSV is sent.
//   - AVRO and PARQUET: 400 "unsupported destination format", and an empty
//     object left: 501, before anything is written.
//   - NEWLINE_DELIMITED_JSON: every value was written as a JSON string (a
//     FLOAT 1.5 as "1.5", a BOOL as "true"), where BigQuery writes numbers
//     and booleans as JSON ones: 501.
//   - A compression (GZIP, DEFLATE, SNAPPY) was ignored: the object was
//     written uncompressed. 501.
//   - A fieldDelimiter was ignored: the file was comma-separated. 501 for
//     any other than ",".
//   - printHeader true, as a client may send it, wrote no header (the
//     emulator writes one only when printHeader is absent): it is left
//     out. printHeader false writes none, as in BigQuery.
//   - A wildcard URI (gs://b/out-*.csv) was written to an object named
//     "out-*.csv". BigQuery writes out-000000000000.csv for a table this
//     size, so the URI is sent with that name; jobs.get shows the
//     client's URI (jobTexts).
//   - Several URIs were each written the whole table, where BigQuery
//     shares the rows out between them: 501.
//   - A table with a RECORD or REPEATED column was written with JSON text
//     in the field; BigQuery refuses a nested schema in CSV: 400.
//   - A view was written with its query's rows; BigQuery extracts only
//     tables: 400.
//   - A column of a type not in csvExportTypes (TIMESTAMP was written
//     "2020-01-02 03:04:05+00", not BigQuery's "... UTC"): 501.
//   - A table with no rows was written as an empty object, without the
//     header row BigQuery writes: 501 unless printHeader is false.
//   - A bucket that does not exist was created, and the file written to
//     it; BigQuery fails the job. With storage set, the bucket is looked
//     up first: 404 when it is not there.
//
// Only destinationUris is read by the emulator: a job with the older
// destinationUri alone was not run as an extract, so it is sent as
// destinationUris.
func (f front) extractJob(w http.ResponseWriter, r *http.Request, e *extractConfig) {
	uris := e.DestinationURIs
	if len(uris) == 0 && e.DestinationURI != "" {
		uris = []string{e.DestinationURI}
	}
	format := strings.ToUpper(e.DestinationFormat)
	notImplemented := func(what string) {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: an extract job "+what+
			" Nothing was written. A CSV extract of a table to one URI, without compression, is supported.")
	}
	switch {
	case len(e.SourceModel) > 0 && string(e.SourceModel) != "null":
		notImplemented("of a model. The emulator behind CloudBurrow has no models.")
		return
	case e.SourceTable == nil || len(uris) == 0:
		f.next.ServeHTTP(w, r)
		return
	case format == "AVRO" || format == "PARQUET":
		notImplemented(fmt.Sprintf("to %s. BigQuery writes it, but the emulator behind CloudBurrow does not support it "+
			"(measured: 400 \"unsupported destination format %s\", and an empty object was left at the URI).", format, format))
		return
	case format == "NEWLINE_DELIMITED_JSON":
		notImplemented("to NEWLINE_DELIMITED_JSON. The emulator behind CloudBurrow writes every value as a JSON string " +
			"(measured: a FLOAT64 1.5 as \"1.5\", a BOOL as \"true\"), where BigQuery writes numbers and booleans as JSON " +
			"numbers and booleans.")
		return
	case format != "" && format != "CSV":
		f.next.ServeHTTP(w, r)
		return
	case e.Compression != "" && !strings.EqualFold(e.Compression, "NONE"):
		notImplemented(fmt.Sprintf("with compression %s. BigQuery compresses the file, but the emulator behind CloudBurrow "+
			"ignores the compression and writes it uncompressed (measured).", e.Compression))
		return
	case e.FieldDelimiter != "" && e.FieldDelimiter != ",":
		notImplemented(fmt.Sprintf("with fieldDelimiter %q. The emulator behind CloudBurrow ignores it and separates "+
			"the fields with commas (measured).", e.FieldDelimiter))
		return
	case len(uris) > 1:
		notImplemented(fmt.Sprintf("to %d URIs. BigQuery shares the rows out between them, but the emulator behind "+
			"CloudBurrow writes the whole table to each (measured).", len(uris)))
		return
	}
	uri := uris[0]
	if strings.Count(uri, "*") > 1 {
		writeError(w, http.StatusBadRequest, "invalid", fmt.Sprintf("Invalid destination URI %s: a URI may have at most one wildcard (*).", uri))
		return
	}

	// The table: BigQuery extracts tables, not views, and CSV has no
	// nested values.
	src := e.SourceTable
	status, got := f.get(r, tablePath(src.DatasetID, src.TableID))
	var meta struct {
		Type    string      `json:"type"`
		NumRows any         `json:"numRows"`
		Schema  tableSchema `json:"schema"`
	}
	if status != http.StatusOK || json.Unmarshal(got, &meta) != nil {
		// Not found, or not readable: the emulator's own answer stands.
		f.next.ServeHTTP(w, r)
		return
	}
	name := src.DatasetID + "." + src.TableID
	if src.ProjectID != "" {
		name = src.ProjectID + ":" + name
	}
	if t := strings.ToUpper(meta.Type); t == "VIEW" || t == "MATERIALIZED_VIEW" {
		writeError(w, http.StatusBadRequest, "invalid", fmt.Sprintf("%s is not allowed for this operation because it is "+
			"currently a %s.", name, t))
		return
	}
	if loc := nestedField(meta.Schema.Fields); loc != "" {
		writeError(w, http.StatusBadRequest, "invalid", "Operation cannot be performed on a nested schema. Field: "+loc)
		return
	}
	for _, fl := range meta.Schema.Fields {
		if !csvExportTypes[strings.ToUpper(fl.Type)] {
			notImplemented(fmt.Sprintf("to CSV of a table with a %s column (%s). The emulator behind CloudBurrow writes "+
				"its values in a form that differs from BigQuery's (measured: a TIMESTAMP as \"2020-01-02 03:04:05+00\", not "+
				"\"2020-01-02 03:04:05 UTC\") or that was not measured against it; STRING, INT64, BOOL, BYTES, DATE and "+
				"NUMERIC columns are written as BigQuery writes them.", strings.ToUpper(fl.Type), fl.Name))
			return
		}
	}
	header := e.PrintHeader == nil || *e.PrintHeader
	if header && f.emptyTable(r, src.DatasetID, src.TableID, meta.NumRows) {
		notImplemented("of an empty table with a header row. BigQuery writes a file holding the header row, but the " +
			"emulator behind CloudBurrow writes an empty object (measured). With printHeader false, it is supported.")
		return
	}

	// The bucket must exist: the emulator creates one that does not.
	bucket, _, _ := strings.Cut(strings.TrimPrefix(uri, "gs://"), "/")
	if strings.HasPrefix(uri, "gs://") && f.storageHost != "" && !f.bucketExists(r.Context(), bucket) {
		writeError(w, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: bucket %s of destination URI %s", bucket, uri))
		return
	}

	sent := uri
	if strings.Contains(uri, "*") {
		sent = strings.Replace(uri, "*", wildcardShard, 1)
	}
	if !setExtract(r, format == "", sent != uri || len(e.DestinationURIs) == 0, sent, header && e.PrintHeader != nil) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the extract job")
		return
	}
	t := jobText{}
	if sent != uri {
		t.uris = []string{uri}
	}
	f.forward(w, r, t)
}

// emptyTable reports whether a table has no rows. The emulator gives a
// table no numRows until rows are written to it (measured: a CREATE TABLE
// with a column list had none), so a table without one is read.
func (f front) emptyTable(r *http.Request, dataset, table string, numRows any) bool {
	switch n := fmt.Sprint(numRows); {
	case n == "0":
		return true
	case numRows != nil && n != "":
		return false
	}
	legacy := false
	req, err := json.Marshal(queryOptions{Query: "SELECT 1 FROM " + quotePath([]string{dataset, table}) + " LIMIT 1", UseLegacySQL: &legacy})
	if err != nil {
		return false
	}
	status, got := f.send(r, http.MethodPost, "/queries", req)
	var res struct {
		Rows []json.RawMessage `json:"rows"`
	}
	return status == http.StatusOK && json.Unmarshal(got, &res) == nil && len(res.Rows) == 0
}

// nestedField returns the first RECORD or REPEATED column, or "".
func nestedField(fields []field) string {
	for _, fl := range fields {
		typ := strings.ToUpper(fl.Type)
		if typ == "RECORD" || typ == "STRUCT" || strings.EqualFold(fl.Mode, "REPEATED") {
			return fl.Name
		}
	}
	return ""
}

// bucketExists looks a bucket up in the instance's Cloud Storage. A bucket
// that cannot be looked up is taken to exist: the emulator's own answer
// then stands.
func (f front) bucketExists(ctx context.Context, bucket string) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+f.storageHost+"/storage/v1/b/"+url.PathEscape(bucket), nil)
	if err != nil {
		return true
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return true
	}
	_ = resp.Body.Close()
	return resp.StatusCode != http.StatusNotFound
}

// setExtract changes an extract job's configuration in r's body: CSV as
// its destinationFormat (csv), uri as its one destination URI (setURI),
// and no printHeader (dropHeader).
func setExtract(r *http.Request, csv, setURI bool, uri string, dropHeader bool) bool {
	if !csv && !setURI && !dropHeader {
		return true
	}
	b, err := readBody(r)
	if err != nil {
		return false
	}
	var job map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if dec.Decode(&job) != nil {
		return false
	}
	conf, _ := job["configuration"].(map[string]any)
	ex, _ := conf["extract"].(map[string]any)
	if ex == nil {
		return false
	}
	if csv {
		ex["destinationFormat"] = "CSV"
	}
	if setURI {
		ex["destinationUris"] = []string{uri}
		delete(ex, "destinationUri")
	}
	if dropHeader {
		delete(ex, "printHeader")
	}
	out, err := json.Marshal(job)
	if err != nil {
		return false
	}
	setBody(r, out)
	return true
}
