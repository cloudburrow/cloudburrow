package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	bqv2 "google.golang.org/api/bigquery/v2"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestBigQueryLoadFormBecomesTheLoadsSource (#993): the form's values are
// the load's source as the official client sends it. An option left at
// BigQuery's default is not set, an empty quote is no quoting, the null
// markers are one per line, and a CSV option changed on a load of another
// format, a count that is not a whole number and a value no select offers
// are refused before anything is sent.
func TestBigQueryLoadFormBecomesTheLoadsSource(t *testing.T) {
	defaults := map[string]string{
		"uris": "gs://b/a.csv\n\n gs://b/parts/*.csv ", "format": "CSV", "writeDisposition": "WRITE_APPEND",
		"autodetect": "false", "schema": "", "maxBadRecords": "", "ignoreUnknownValues": "false",
		"skipLeadingRows": "", "fieldDelimiter": ",", "quote": `"`, "nullMarker": "", "nullMarkers": "",
		"encoding": "UTF-8", "sourceColumnMatch": "POSITION", "allowJaggedRows": "false",
		"allowQuotedNewlines": "false", "preserveAsciiControlCharacters": "false",
	}
	with := func(changes map[string]string) map[string]string {
		v := map[string]string{}
		for k, x := range defaults {
			v[k] = x
		}
		for k, x := range changes {
			v[k] = x
		}
		return v
	}

	src, err := loadSource(defaults)
	if err != nil {
		t.Fatal(err)
	}
	want := bigquery.NewGCSReference("gs://b/a.csv", "gs://b/parts/*.csv")
	want.SourceFormat = bigquery.CSV
	if !reflect.DeepEqual(src, want) {
		t.Errorf("the form as it opens gives %+v, want only the URIs and CSV", src)
	}

	src, err = loadSource(with(map[string]string{
		"schema": `[{"name":"id","type":"INTEGER","mode":"REQUIRED"}]`, "autodetect": "true",
		"maxBadRecords": "3", "ignoreUnknownValues": "true", "skipLeadingRows": "1", "fieldDelimiter": `\t`,
		"quote": "", "nullMarkers": "NA\n\\N\n", "encoding": "ISO-8859-1", "sourceColumnMatch": "NAME",
		"allowJaggedRows": "true", "allowQuotedNewlines": "true", "preserveAsciiControlCharacters": "true",
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := src.FileConfig
	if !got.AutoDetect || got.MaxBadRecords != 3 || !got.IgnoreUnknownValues || got.SkipLeadingRows != 1 ||
		got.FieldDelimiter != `\t` || got.Quote != "" || !got.ForceZeroQuote ||
		!reflect.DeepEqual(got.NullMarkers, []string{"NA", `\N`}) || got.Encoding != bigquery.ISO_8859_1 ||
		got.SourceColumnMatch != "NAME" || !got.AllowJaggedRows || !got.AllowQuotedNewlines ||
		!got.PreserveASCIIControlCharacters || len(got.Schema) != 1 || !got.Schema[0].Required {
		t.Errorf("every option set gives %+v", got)
	}

	src, err = loadSource(with(map[string]string{"format": "PARQUET", "maxBadRecords": "2"}))
	if err != nil || src.SourceFormat != bigquery.Parquet || src.MaxBadRecords != 2 {
		t.Errorf("a Parquet load gives %+v, %v", src, err)
	}
	for values, why := range map[*map[string]string]string{
		ptr(with(map[string]string{"format": "NEWLINE_DELIMITED_JSON", "fieldDelimiter": "|", "allowJaggedRows": "true"})): "allowJaggedRows, fieldDelimiter applies to CSV only",
		ptr(with(map[string]string{"format": "AVRO"})):                                                                     `not "AVRO"`,
		ptr(with(map[string]string{"skipLeadingRows": "-1"})):                                                              "whole number",
		ptr(with(map[string]string{"maxBadRecords": "two"})):                                                               "whole number",
		ptr(with(map[string]string{"encoding": "UTF-16BE"})):                                                               `not "UTF-16BE"`,
		ptr(with(map[string]string{"uris": " \n "})):                                                                       "at least one gs:// URI",
		ptr(with(map[string]string{"schema": "not json"})):                                                                 "JSON array",
		ptr(with(map[string]string{"sourceColumnMatch": "WHATEVER"})):                                                      `not "WHATEVER"`,
		ptr(with(map[string]string{"format": "PARQUET", "quote": "'"})):                                                    "quote applies to CSV only",
	} {
		if _, err := loadSource(*values); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%v = %v, want %q", *values, err, why)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// TestBigQueryJobPageShowsWhatTheJobReported (#993): a job's page is its
// state, what it was asked to do and what it reported. A load's counts are
// shown only when jobs.get reports them, since the generated client decodes
// one left out as 0; its bad records are the Errors tab, and a failed job's
// errorResult is its Error. The configuration is shown as the API gave it.
func TestBigQueryJobPageShowsWhatTheJobReported(t *testing.T) {
	const body = `{"jobReference":{"projectId":"p","jobId":"j1","location":"US"},
		"configuration":{"jobType":"LOAD","load":{"sourceUris":["gs://b/a.csv"],"sourceFormat":"CSV",
		"destinationTable":{"projectId":"p","datasetId":"d","tableId":"t"},"skipLeadingRows":1}},
		"statistics":{"creationTime":"1790000000000","startTime":"1790000000000","endTime":"1790000001500",
		"load":{"outputRows":"2","inputFiles":"1","inputFileBytes":"30"}},
		"status":{"state":"DONE","errors":[{"reason":"invalid","location":"gs://b/a.csv","message":"Too many values"}]}}`
	page := jobPageOf(t, body)
	props := propertiesOf(page)
	for label, want := range map[string]string{
		"Job/Job ID": "p:j1", "Job/Type": "LOAD", "Job/State": "DONE", "Job/Location": "US",
		"Job/Created": "2026-09-21T14:13:20Z", "Job/Duration": "1.5s",
		"Load/Source URIs": "gs://b/a.csv", "Load/Destination table": "p.d.t", "Load/Header rows to skip": "1",
		"Statistics/Output rows": "2", "Statistics/Input files": "1", "Statistics/Input file bytes": "30",
	} {
		if props[label] != want {
			t.Errorf("%s = %q, want %q", label, props[label], want)
		}
	}
	if _, ok := props["Statistics/Bad records"]; ok {
		t.Error("the page shows a bad records count jobs.get did not report")
	}
	if _, ok := props["Job/Error"]; ok {
		t.Error("a job that succeeded shows an error")
	}
	if got := page.Summary; len(got) < 2 || got[1].Value != "Succeeded" {
		t.Errorf("the summary is %+v", got)
	}
	errs := sectionOf(t, page, "errors").Listing
	if len(errs.Items) != 1 || errs.Items[0].Fields["Message"] != "Too many values" ||
		errs.Items[0].Fields["Location"] != "gs://b/a.csv" || !strings.Contains(errs.Note, "left out") {
		t.Errorf("the Errors tab is %+v", errs)
	}
	if text := sectionOf(t, page, "configuration").Text; !strings.Contains(text, `"skipLeadingRows": 1`) {
		t.Errorf("the Configuration tab is %q", text)
	}

	failed := jobPageOf(t, `{"jobReference":{"projectId":"p","jobId":"j2"},
		"configuration":{"extract":{"sourceTable":{"projectId":"p","datasetId":"d","tableId":"t"},
		"destinationUris":["gs://b/o.json"],"destinationFormat":"NEWLINE_DELIMITED_JSON"}},
		"statistics":{"load":{"badRecords":"0"}},
		"status":{"state":"DONE","errorResult":{"reason":"notImplemented","message":"Not implemented here"}}}`)
	props = propertiesOf(failed)
	if props["Job/Type"] != "EXTRACT" || props["Job/Error"] != "notImplemented: Not implemented here" ||
		props["Export/Destination URIs"] != "gs://b/o.json" || props["Statistics/Bad records"] != "0" {
		t.Errorf("a failed export's page is %v", props)
	}
	if failed.Summary[1].Value != "Failed" {
		t.Errorf("a failed job's state is %q", failed.Summary[1].Value)
	}
}

func jobPageOf(t *testing.T, body string) console.Detail {
	t.Helper()
	var job bqv2.Job
	var raw rawJob
	if err := json.Unmarshal([]byte(body), &job); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	return bigqueryJobPage("p", &job, raw)
}

func propertiesOf(d console.Detail) map[string]string {
	out := map[string]string{}
	for _, s := range d.Sections {
		for _, g := range s.Groups {
			for _, p := range g.Properties {
				out[g.Heading+"/"+p.Label] = p.Value
			}
		}
	}
	return out
}

func sectionOf(t *testing.T, d console.Detail, id string) console.Section {
	t.Helper()
	for _, s := range d.Sections {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no %s section", id)
	return console.Section{}
}

// TestBigQueryJobActionsFollowTheJobsState (#993): a job's page offers
// Cancel job only while the job is not DONE, and Delete job always; each is
// the API's call (jobs.cancel, jobs.delete) through the generated client, and
// a refusal is shown in the API's words.
func TestBigQueryJobActionsFollowTheJobsState(t *testing.T) {
	var calls []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/jobs/running"):
			_, _ = io.WriteString(w, `{"jobReference":{"jobId":"running"},"status":{"state":"RUNNING"}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/jobs/done"):
			_, _ = io.WriteString(w, `{"jobReference":{"jobId":"done"},"status":{"state":"DONE"}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs/running/cancel"):
			_, _ = io.WriteString(w, `{"job":{"jobReference":{"jobId":"running"},"status":{"state":"DONE"}}}`)
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/jobs/done/delete"):
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":404,"message":"Not found: Job p:gone","errors":[{"reason":"notFound"}]}}`)
		}
	}))
	defer api.Close()
	p := bigqueryJobsProvider{bq: bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}}
	ctx := context.Background()
	ids := func(id string) (out []string) {
		for _, a := range p.DetailActions(ctx, "p", []string{id}) {
			out = append(out, a.ID)
		}
		return out
	}
	if got := ids("running"); !reflect.DeepEqual(got, []string{"cancel", "delete"}) {
		t.Errorf("a running job offers %v", got)
	}
	if got := ids("done"); !reflect.DeepEqual(got, []string{"delete"}) {
		t.Errorf("a done job offers %v", got)
	}
	calls = nil
	if err := p.ActAt(ctx, "p", []string{"running"}, "cancel", nil); err != nil {
		t.Errorf("cancel: %v", err)
	}
	if err := p.ActAt(ctx, "p", []string{"done"}, "delete", nil); err != nil {
		t.Errorf("delete: %v", err)
	}
	if want := []string{"POST /projects/p/jobs/running/cancel", "DELETE /projects/p/jobs/done/delete"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("the actions called %v, want %v", calls, want)
	}
	if err := p.Delete(ctx, "p", "gone"); err == nil || err.Error() != "Not found: Job p:gone" {
		t.Errorf("deleting a job that is gone = %v, want the API's words", err)
	}
	if d, _ := p.Detail(ctx, "p", []string{"gone"}); !strings.Contains(d.Unavailable, "Not found: Job p:gone") {
		t.Errorf("a job that is gone opens as %+v", d)
	}
}

// TestBigQueryJobPageShowsAQueryJobsDMLRows (#1041): a query job's page
// shows its statement type and, for DML, numDmlAffectedRows and dmlStats'
// inserted, updated and deleted rows, each only when jobs.get reported it:
// a zero dmlStats leaves out is not shown as 0, and a SELECT shows no row
// counts at all.
func TestBigQueryJobPageShowsAQueryJobsDMLRows(t *testing.T) {
	merge := propertiesOf(jobPageOf(t, `{"jobReference":{"projectId":"p","jobId":"m"},
		"configuration":{"jobType":"QUERY","query":{"query":"MERGE d.t USING d.s ON t.id = s.id ..."}},
		"statistics":{"query":{"statementType":"MERGE","numDmlAffectedRows":"3",
		"dmlStats":{"insertedRowCount":"1","updatedRowCount":"2"}}},"status":{"state":"DONE"}}`))
	for label, want := range map[string]string{
		"Statistics/Statement type": "MERGE", "Statistics/Affected rows": "3",
		"Statistics/Inserted rows": "1", "Statistics/Updated rows": "2",
	} {
		if merge[label] != want {
			t.Errorf("a MERGE job's %s = %q, want %q", label, merge[label], want)
		}
	}
	if v, ok := merge["Statistics/Deleted rows"]; ok {
		t.Errorf("a MERGE that deleted nothing shows Deleted rows %q; dmlStats left it out", v)
	}

	none := propertiesOf(jobPageOf(t, `{"jobReference":{"projectId":"p","jobId":"d"},
		"configuration":{"query":{"query":"DELETE FROM d.t WHERE false"}},
		"statistics":{"query":{"statementType":"DELETE","numDmlAffectedRows":"0"}},"status":{"state":"DONE"}}`))
	if none["Statistics/Statement type"] != "DELETE" || none["Statistics/Affected rows"] != "0" {
		t.Errorf("a DELETE of nothing shows %v, want DELETE and 0 affected rows", none)
	}

	sel := propertiesOf(jobPageOf(t, `{"jobReference":{"projectId":"p","jobId":"s"},
		"configuration":{"query":{"query":"SELECT 1"}},
		"statistics":{"query":{"statementType":"SELECT"}},"status":{"state":"DONE"}}`))
	if sel["Statistics/Statement type"] != "SELECT" {
		t.Errorf("a SELECT job shows %v", sel)
	}
	for _, label := range []string{"Affected rows", "Inserted rows", "Updated rows", "Deleted rows"} {
		if _, ok := sel["Statistics/"+label]; ok {
			t.Errorf("a SELECT job shows %s", label)
		}
	}
}

// The export form offers AVRO and PARQUET, which the front writes, each
// compression only with the formats BigQuery's export documentation lists
// it for, Avro logical types for Avro only, and More destination URIs,
// all sent in one extract job (#957).
func TestBigQueryExportFormOffersAvroAndParquet(t *testing.T) {
	var format, compression []string
	names := map[string]bool{}
	for _, f := range bigqueryExportFields() {
		names[f.Name] = true
		switch f.Name {
		case "format":
			format = f.Options
		case "compression":
			compression = f.Options
		}
	}
	if !slices.Contains(format, "AVRO") || !slices.Contains(format, "PARQUET") ||
		!slices.Contains(compression, "DEFLATE") || !slices.Contains(compression, "SNAPPY") || !slices.Contains(compression, "ZSTD") ||
		!names["useAvroLogicalTypes"] || !names["moreUris"] {
		t.Errorf("the export form offers formats %v, compressions %v, fields %v", format, compression, names)
	}
	p := bigqueryProvider{endpoint: "127.0.0.1:1", project: "p"}
	for _, c := range []struct{ format, compression, logical, want string }{
		{"AVRO", "GZIP", "", "DEFLATE or SNAPPY or not at all"},
		{"AVRO", "ZSTD", "", "DEFLATE or SNAPPY or not at all"},
		{"CSV", "SNAPPY", "", "GZIP or not at all"},
		{"NEWLINE_DELIMITED_JSON", "DEFLATE", "", "GZIP or not at all"},
		{"PARQUET", "DEFLATE", "", "SNAPPY or GZIP or ZSTD"},
		{"PARQUET", "NONE", "true", "Avro only"},
		{"ORC", "NONE", "", "CSV, NEWLINE_DELIMITED_JSON, AVRO or PARQUET"},
	} {
		_, err := p.exportJob(context.Background(), "p", "d", "t",
			map[string]string{"uri": "gs://b/o", "format": c.format, "compression": c.compression, "useAvroLogicalTypes": c.logical})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s with %s: %v, want %q", c.format, c.compression, err, c.want)
		}
	}

	var sent []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs") {
			b, _ := io.ReadAll(r.Body)
			sent = append(sent, string(b))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jobReference":{"projectId":"p","jobId":"e1","location":"US"},
			"configuration":{"extract":{"sourceTable":{"projectId":"p","datasetId":"d","tableId":"t"},
			"destinationUris":["gs://b/a-*.avro","gs://b/b-*.avro"]}},"status":{"state":"DONE"}}`)
	}))
	defer api.Close()
	p = bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}
	if _, err := p.exportJob(context.Background(), "p", "d", "t", map[string]string{"uri": "gs://b/a-*.avro",
		"moreUris": "gs://b/b-*.avro\n\n", "format": "AVRO", "compression": "SNAPPY", "useAvroLogicalTypes": "true"}); err != nil {
		t.Fatal(err)
	}
	if len(sent) == 0 {
		t.Fatal("no extract job was sent")
	}
	for _, want := range []string{`"destinationUris":["gs://b/a-*.avro","gs://b/b-*.avro"]`, `"destinationFormat":"AVRO"`,
		`"compression":"SNAPPY"`, `"useAvroLogicalTypes":true`} {
		if !strings.Contains(sent[0], want) {
			t.Errorf("the extract job sent %s; want %s in it", sent[0], want)
		}
	}
	if _, err := p.exportJob(context.Background(), "p", "d", "t", map[string]string{"uri": "gs://b/o",
		"moreUris": "b/o2", "format": "CSV"}); err == nil {
		t.Error("a More destination URI that is not gs:// was sent")
	}
}

// Copy table (#782) sends a copy job of the table to the destination the form
// names, with its write preference, and refuses a copy onto itself.
func TestBigQueryCopyTableFormRunsACopyJob(t *testing.T) {
	var sent string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs") {
			b, _ := io.ReadAll(r.Body)
			sent = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jobReference":{"projectId":"p","jobId":"c1","location":"US"},
			"configuration":{"copy":{"sourceTable":{"projectId":"p","datasetId":"d","tableId":"t"},
			"destinationTable":{"projectId":"p","datasetId":"d2","tableId":"t2"}}},"status":{"state":"DONE"}}`)
	}))
	defer api.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}
	ctx := context.Background()
	if _, err := p.ActAtResult(ctx, "p", []string{"d", "t"}, actCopyTable,
		map[string]string{"datasetId": "d2", "tableId": "t2", "writeDisposition": "WRITE_TRUNCATE"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"copy"`, `"datasetId":"d2"`, `"tableId":"t2"`, `"tableId":"t"`, `"writeDisposition":"WRITE_TRUNCATE"`} {
		if !strings.Contains(sent, want) {
			t.Errorf("the copy job sent %s; want %s in it", sent, want)
		}
	}
	if _, err := p.ActAtResult(ctx, "p", []string{"d", "t"}, actCopyTable,
		map[string]string{"datasetId": "d", "tableId": "t"}); err == nil {
		t.Error("a copy of a table onto itself was sent")
	}
}

// TestBigQueryCopyTableSendsEveryMoreSource (#782): Copy table's More source
// tables are sent as one copy job's sourceTables, this table first; a
// source named twice, one that is this table or the destination, or one
// that is not dataset.table is refused before anything is sent.
func TestBigQueryCopyTableSendsEveryMoreSource(t *testing.T) {
	var sent []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs") {
			b, _ := io.ReadAll(r.Body)
			sent = append(sent, string(b))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jobReference":{"projectId":"p","jobId":"c1","location":"US"},"status":{"state":"DONE"}}`)
	}))
	defer api.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}
	ctx := context.Background()
	if _, err := p.ActAtResult(ctx, "p", []string{"d", "t"}, actCopyTable, map[string]string{
		"datasetId": "d2", "tableId": "all", "moreSources": "d.u\n\n  e.v  \n"}); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 {
		t.Fatalf("%d jobs sent, want one", len(sent))
	}
	var job struct {
		Configuration struct {
			Copy struct {
				SourceTables []struct{ DatasetID, TableID string } `json:"sourceTables"`
			} `json:"copy"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal([]byte(sent[0]), &job); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range job.Configuration.Copy.SourceTables {
		got = append(got, s.DatasetID+"."+s.TableID)
	}
	if strings.Join(got, " ") != "d.t d.u e.v" {
		t.Errorf("the copy job's sourceTables = %v, want d.t d.u e.v: %s", got, sent[0])
	}
	for _, more := range []string{"d.u\nd.u", "d.t", "d2.all", "nodot", "a.b.c", ".x"} {
		if _, err := p.ActAtResult(ctx, "p", []string{"d", "t"}, actCopyTable, map[string]string{
			"datasetId": "d2", "tableId": "all", "moreSources": more}); err == nil {
			t.Errorf("More source tables %q was sent", more)
		}
	}
	if len(sent) != 1 {
		t.Errorf("a refused copy was sent: %v", sent[1:])
	}
}

// TestBigQueryCopyTableSendsTheOperation (#1002): Copy table's Operation is
// sent as the copy job's operationType (COPY is left out, the default); an
// unknown one, or CLONE, SNAPSHOT or RESTORE with More source tables, is
// refused before anything is sent.
func TestBigQueryCopyTableSendsTheOperation(t *testing.T) {
	var sent []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs") {
			b, _ := io.ReadAll(r.Body)
			sent = append(sent, string(b))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jobReference":{"projectId":"p","jobId":"c1","location":"US"},"status":{"state":"DONE"}}`)
	}))
	defer api.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}
	ctx := context.Background()
	for _, op := range []string{"CLONE", "SNAPSHOT", "RESTORE"} {
		sent = nil
		if _, err := p.ActAtResult(ctx, "p", []string{"d", "t"}, actCopyTable,
			map[string]string{"datasetId": "d2", "tableId": "t2", "operationType": op}); err != nil {
			t.Fatal(err)
		}
		if len(sent) != 1 || !strings.Contains(sent[0], `"operationType":"`+op+`"`) {
			t.Errorf("%s sent %v", op, sent)
		}
	}
	sent = nil
	if _, err := p.ActAtResult(ctx, "p", []string{"d", "t"}, actCopyTable,
		map[string]string{"datasetId": "d2", "tableId": "t2", "operationType": "COPY"}); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || strings.Contains(sent[0], "operationType") {
		t.Errorf("COPY sent %v", sent)
	}
	sent = nil
	for _, values := range []map[string]string{
		{"datasetId": "d2", "tableId": "t2", "operationType": "MOVE"},
		{"datasetId": "d2", "tableId": "t2", "operationType": "CLONE", "moreSources": "d.u"},
	} {
		if _, err := p.ActAtResult(ctx, "p", []string{"d", "t"}, actCopyTable, values); err == nil {
			t.Errorf("%v was sent", values)
		}
	}
	if len(sent) != 0 {
		t.Errorf("a refused copy was sent: %v", sent)
	}
}
