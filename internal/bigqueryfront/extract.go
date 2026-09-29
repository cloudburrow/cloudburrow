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
//     object left. The front writes AVRO itself for the types in
//     avroExportTypes (writeExtract, #957); 501 for PARQUET and the others,
//     before anything is written.
//   - NEWLINE_DELIMITED_JSON: every value was written as a JSON string (a
//     FLOAT 1.5 as "1.5", a BOOL as "true"). The front writes it itself for
//     the types whose form BigQuery documents (writeExtract, #957); 501 for
//     the others.
//   - A compression (GZIP, DEFLATE, SNAPPY) was ignored: the object was
//     written uncompressed. GZIP, which BigQuery documents for CSV and
//     JSON, the front writes itself; 501 for the others.
//   - A fieldDelimiter was ignored: the file was comma-separated. The front
//     writes a CSV with any other one-character delimiter itself.
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
//     header row BigQuery writes: the front writes it itself unless
//     printHeader is false.
//   - A bucket that does not exist was created, and the file written to
//     it; BigQuery fails the job. With storage set, the bucket is looked
//     up first: 404 when it is not there.
//   - (#1015) It reads the table by its bare ID, which names the first
//     table of that ID made in any dataset (qualify.go): so when another
//     dataset has a table of the ID, the front writes the extract itself
//     (writeExtract), reading the table by its whole name.
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
	jsonFormat := format == "NEWLINE_DELIMITED_JSON"
	avroFormat := format == "AVRO"
	csvFormat := !jsonFormat && !avroFormat
	gz := strings.EqualFold(e.Compression, "GZIP")
	avroCodec := ""
	if avroFormat {
		switch strings.ToUpper(e.Compression) {
		case "DEFLATE":
			avroCodec = "deflate"
		case "SNAPPY":
			avroCodec = "snappy"
		}
	}
	notImplemented := func(what string) {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: an extract job "+what+
			" Nothing was written. A CSV or NEWLINE_DELIMITED_JSON extract of a table to one URI, uncompressed or "+
			"GZIP, or an AVRO extract, uncompressed, DEFLATE or SNAPPY, is supported (docs/compatibility.md lists the column types).")
	}
	switch {
	case len(e.SourceModel) > 0 && string(e.SourceModel) != "null":
		notImplemented("of a model. The emulator behind CloudBurrow has no models.")
		return
	case e.SourceTable == nil || len(uris) == 0:
		f.next.ServeHTTP(w, r)
		return
	case format == "PARQUET":
		notImplemented(fmt.Sprintf("to %s. BigQuery writes it, but the emulator behind CloudBurrow does not support it "+
			"(measured: 400 \"unsupported destination format %s\", and an empty object was left at the URI), and "+
			"CloudBurrow does not write %s files itself: it has no way to check them against BigQuery's.", format, format, format))
		return
	case format != "" && format != "CSV" && !jsonFormat && !avroFormat:
		f.next.ServeHTTP(w, r)
		return
	case avroFormat && e.Compression != "" && !strings.EqualFold(e.Compression, "NONE") && avroCodec == "":
		notImplemented(fmt.Sprintf("to AVRO with compression %s. BigQuery documents DEFLATE and SNAPPY for Avro, "+
			"which CloudBurrow writes.", e.Compression))
		return
	case !avroFormat && e.Compression != "" && !strings.EqualFold(e.Compression, "NONE") && !gz:
		notImplemented(fmt.Sprintf("with compression %s. BigQuery documents GZIP for CSV and JSON, and %s only for "+
			"Avro or Parquet; the emulator behind CloudBurrow ignores the compression (measured).", e.Compression, e.Compression))
		return
	case len(uris) > 1:
		notImplemented(fmt.Sprintf("to %d URIs. BigQuery shares the rows out between them, in a way its documentation "+
			"does not give, and the emulator behind CloudBurrow writes the whole table to each (measured).", len(uris)))
		return
	}
	delimiter := ','
	if d := e.FieldDelimiter; d != "" && d != "," && csvFormat {
		if len(d) != 1 || d[0] == '"' || d[0] == '\r' || d[0] == '\n' || d[0] != '\t' && (d[0] < 0x20 || d[0] > 0x7e) {
			notImplemented(fmt.Sprintf("with fieldDelimiter %q. CloudBurrow writes a CSV with a delimiter of one "+
				"printable ASCII character or a tab, as the emulator behind it ignores the delimiter (measured).", d))
			return
		}
		delimiter = rune(d[0])
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
		if avroFormat {
			notImplemented("to AVRO of a table with a RECORD or REPEATED column (" + loc + "). CloudBurrow writes " +
				"Avro only of a table of STRING, INT64, FLOAT64, BOOL and BYTES columns.")
			return
		}
		if jsonFormat {
			notImplemented("to NEWLINE_DELIMITED_JSON of a table with a RECORD or REPEATED column (" + loc + "). " +
				"BigQuery writes it, but its documentation does not give the form, so CloudBurrow does not write it.")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid", "Operation cannot be performed on a nested schema. Field: "+loc)
		return
	}
	for _, fl := range meta.Schema.Fields {
		typ := strings.ToUpper(fl.Type)
		switch {
		case avroFormat && avroExportTypes[typ] == "":
			notImplemented(fmt.Sprintf("to AVRO of a table with a %s column (%s). BigQuery's documentation gives the "+
				"Avro type of each BigQuery type, but CloudBurrow writes only STRING, INT64, FLOAT64, BOOL and BYTES "+
				"columns (as Avro string, long, double, boolean and bytes).", typ, fl.Name))
			return
		case jsonFormat && !jsonExportTypes[typ]:
			notImplemented(fmt.Sprintf("to NEWLINE_DELIMITED_JSON of a table with a %s column (%s). The emulator behind "+
				"CloudBurrow writes every value as a JSON string (measured: a FLOAT64 1.5 as \"1.5\", a BOOL as \"true\"), "+
				"and BigQuery's documentation gives the form only of INT64 (a JSON string) and STRING values, which "+
				"CloudBurrow writes.", typ, fl.Name))
			return
		case csvFormat && !csvExportTypes[typ]:
			notImplemented(fmt.Sprintf("to CSV of a table with a %s column (%s). The emulator behind CloudBurrow writes "+
				"its values in a form that differs from BigQuery's (measured: a TIMESTAMP as \"2020-01-02 03:04:05+00\", not "+
				"\"2020-01-02 03:04:05 UTC\") or that was not measured against it; STRING, INT64, BOOL, BYTES, DATE and "+
				"NUMERIC columns are written as BigQuery writes them.", typ, fl.Name))
			return
		}
	}
	if csvFormat {
		if col := f.emptyStringColumn(r, src.DatasetID, src.TableID, meta.Schema.Fields); col != "" {
			notImplemented(fmt.Sprintf("to CSV of a table whose %s column holds an empty value (''), which is not NULL. "+
				"The emulator behind CloudBurrow, and CloudBurrow's own CSV writer, write an empty value and a NULL alike, "+
				"as an empty field, and BigQuery's documentation does not give the form it writes an empty value in, "+
				"so CloudBurrow cannot write it as BigQuery does (#975). A NEWLINE_DELIMITED_JSON extract tells them apart.", col))
			return
		}
	}
	header := csvFormat && (e.PrintHeader == nil || *e.PrintHeader)
	emptyWithHeader := header && f.emptyTable(r, src.DatasetID, src.TableID, meta.NumRows)

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
	if jsonFormat || avroFormat || gz || delimiter != ',' || emptyWithHeader || f.storageHost != "" && f.sharedID(r, src.DatasetID, src.TableID) {
		// #957: the emulator writes these differently from BigQuery.
		// #1015: and it reads the table by its bare ID, which names the
		// first table of that ID made in any dataset (qualify.go).
		f.writeExtract(w, r, e, writtenExtract{json: jsonFormat, avro: avroFormat, avroCodec: avroCodec, gzip: gz, delimiter: delimiter, header: header,
			uri: sent, fields: meta.Schema.Fields})
		return
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

// avroExportTypes are the column types the front writes to an Avro file,
// with the Avro type BigQuery's documentation maps each to ("Avro export
// details", https://cloud.google.com/bigquery/docs/exporting-data#avro_export_details):
// INT64 as long, FLOAT64 as double, BOOL as boolean, STRING as string,
// BYTES as bytes (#957). The logical types (DATE, TIMESTAMP, NUMERIC, ...)
// depend on useAvroLogicalTypes and are 501.
var avroExportTypes = map[string]string{
	"STRING": "string", "INTEGER": "long", "INT64": "long", "FLOAT": "double", "FLOAT64": "double",
	"BOOLEAN": "boolean", "BOOL": "boolean", "BYTES": "bytes",
}

// jsonExportTypes are the column types whose values BigQuery's
// documentation gives the JSON form of: INT64 "encoded as JSON strings",
// and STRING (#957).
var jsonExportTypes = map[string]bool{"STRING": true, "INTEGER": true, "INT64": true}

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

// emptyStringColumn returns the first STRING or BYTES column of a table
// that holds an empty value, or "" (#975). A CSV file has one form for an
// empty field: Go's encoding/csv, which the emulator and the front write
// with, writes an empty value unquoted, as it writes a NULL. BigQuery's
// export documentation
// (https://cloud.google.com/bigquery/docs/exporting-data) says neither how
// it writes a NULL nor an empty value, so an extract that would write one
// is 501. The table is read with a query of the front's own; one that
// cannot be read is sent on (the emulator's own answer stands).
func (f front) emptyStringColumn(r *http.Request, dataset, table string, fields []field) string {
	var cols []string
	var conds []string
	for _, fl := range fields {
		switch strings.ToUpper(fl.Type) {
		case "STRING":
			cols = append(cols, fl.Name)
			conds = append(conds, "COUNTIF("+quoteName(fl.Name)+" = '') > 0")
		case "BYTES":
			cols = append(cols, fl.Name)
			conds = append(conds, "COUNTIF("+quoteName(fl.Name)+" = b'') > 0")
		}
	}
	if len(cols) == 0 {
		return ""
	}
	legacy := false
	req, err := json.Marshal(queryOptions{Query: "SELECT " + strings.Join(conds, ", ") + " FROM " +
		quotePath([]string{dataset, table}), UseLegacySQL: &legacy})
	if err != nil {
		return ""
	}
	status, got := f.send(r, http.MethodPost, "/queries", req)
	var res struct {
		Rows []struct {
			F []struct {
				V any `json:"v"`
			} `json:"f"`
		} `json:"rows"`
	}
	if status != http.StatusOK || json.Unmarshal(got, &res) != nil || len(res.Rows) != 1 || len(res.Rows[0].F) != len(cols) {
		return ""
	}
	for i, c := range res.Rows[0].F {
		if fmt.Sprint(c.V) == "true" {
			return cols[i]
		}
	}
	return ""
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
