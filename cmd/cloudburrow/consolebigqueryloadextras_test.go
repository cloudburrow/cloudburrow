package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryLoadFormSendsHiveAndGeoJSONOptions (#1547): the load form's
// hive partitioning and JSON extension reach the job's configuration.load,
// in a JSON jobs.insert and in a multipart upload's metadata; a mode with
// no prefix, a prefix with no mode, hive partitioning of a file and GEOJSON
// on CSV are refused before anything is sent.
func TestBigQueryLoadFormSendsHiveAndGeoJSONOptions(t *testing.T) {
	x, err := loadExtrasOf(map[string]string{"format": "NEWLINE_DELIMITED_JSON", "jsonExtension": "GEOJSON",
		"hiveMode": "AUTO", "hiveSourceUriPrefix": "gs://b/t"}, true)
	if err != nil || x == nil {
		t.Fatalf("loadExtrasOf: %v %v", x, err)
	}
	if x, err := loadExtrasOf(map[string]string{"format": "CSV", "jsonExtension": "NONE", "hiveMode": "NONE"}, true); x != nil || err != nil {
		t.Errorf("an unset form: %v %v", x, err)
	}
	for why, v := range map[string]map[string]string{
		"gs://":                  {"format": "NEWLINE_DELIMITED_JSON", "hiveMode": "AUTO"},
		"needs a source data":    {"format": "NEWLINE_DELIMITED_JSON", "hiveSourceUriPrefix": "gs://b/t"},
		"NEWLINE_DELIMITED_JSON": {"format": "CSV", "jsonExtension": "GEOJSON"},
	} {
		if _, err := loadExtrasOf(v, true); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%v: got %v, want %q", v, err, why)
		}
	}
	if _, err := loadExtrasOf(map[string]string{"format": "NEWLINE_DELIMITED_JSON", "hiveMode": "AUTO",
		"hiveSourceUriPrefix": "gs://b/t"}, false); err == nil || !strings.Contains(err.Error(), "Load from Cloud Storage") {
		t.Errorf("hive partitioning of a file: %v", err)
	}
	check := func(what string, job []byte) {
		t.Helper()
		var j struct {
			Configuration struct {
				Load struct {
					SourceFormat  string            `json:"sourceFormat"`
					JSONExtension string            `json:"jsonExtension"`
					Hive          map[string]string `json:"hivePartitioningOptions"`
				} `json:"load"`
			} `json:"configuration"`
		}
		if err := json.Unmarshal(job, &j); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		l := j.Configuration.Load
		if l.SourceFormat != "NEWLINE_DELIMITED_JSON" || l.JSONExtension != "GEOJSON" || l.Hive["mode"] != "AUTO" ||
			l.Hive["sourceUriPrefix"] != "gs://b/t" {
			t.Errorf("%s: %+v", what, l)
		}
	}
	job := []byte(`{"configuration":{"load":{"sourceFormat":"NEWLINE_DELIMITED_JSON"}}}`)
	out, err := x.edit("application/json", job)
	if err != nil {
		t.Fatal(err)
	}
	check("JSON", out)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	w, _ := mw.CreatePart(map[string][]string{"Content-Type": {"application/json"}})
	w.Write(job)
	w, _ = mw.CreatePart(map[string][]string{"Content-Type": {"application/octet-stream"}})
	w.Write([]byte("{}\n"))
	mw.Close()
	out, err = x.edit("multipart/related; boundary="+mw.Boundary(), buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(bytes.NewReader(out), mw.Boundary())
	p, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := io.ReadAll(p)
	check("multipart", meta)
	p, err = mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := io.ReadAll(p); string(data) != "{}\n" {
		t.Errorf("the upload's data: %q", data)
	}
}

// TestBigQueryLoadFormSendsFileSetSpecType (#1547): the load form's "Source
// URIs are" reaches the job's configuration.load as fileSetSpecType; the
// default sends nothing, and a manifest of a file is refused.
func TestBigQueryLoadFormSendsFileSetSpecType(t *testing.T) {
	const spec = "FILE_SET_SPEC_TYPE_NEW_LINE_DELIMITED_MANIFEST"
	x, err := loadExtrasOf(map[string]string{"format": "CSV", "fileSetSpecType": spec}, true)
	if err != nil || x == nil {
		t.Fatalf("loadExtrasOf: %v %v", x, err)
	}
	out, err := x.edit("application/json", []byte(`{"configuration":{"load":{"sourceFormat":"CSV"}}}`))
	if err != nil || !strings.Contains(string(out), `"fileSetSpecType":"`+spec+`"`) {
		t.Errorf("the job: %s %v", out, err)
	}
	if x, err := loadExtrasOf(map[string]string{"format": "CSV", "fileSetSpecType": "FILE_SET_SPEC_TYPE_FILE_SYSTEM_MATCH"}, true); x != nil || err != nil {
		t.Errorf("the default: %v %v", x, err)
	}
	if _, err := loadExtrasOf(map[string]string{"format": "CSV", "fileSetSpecType": spec}, false); err == nil {
		t.Error("a manifest of a file was not refused")
	}
	if _, err := loadExtrasOf(map[string]string{"format": "CSV", "fileSetSpecType": "X"}, true); err == nil {
		t.Error("an unknown fileSetSpecType was not refused")
	}
}

// TestBigQueryLoadFormCharacterMap (#1547): the load forms send
// columnNameCharacterMap through the Go client's loader.
func TestBigQueryLoadFormCharacterMap(t *testing.T) {
	for in, want := range map[string]bigquery.ColumnNameCharacterMap{"": "", "DEFAULT": "", "v2": "V2", "STRICT": "STRICT", "V1": "V1"} {
		if got, err := loadCharacterMap(map[string]string{"columnNameCharacterMap": in}); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	if _, err := loadCharacterMap(map[string]string{"columnNameCharacterMap": "V3"}); err == nil {
		t.Error("V3 was not refused")
	}
	found := false
	for _, f := range bigqueryLoadFields(true) {
		found = found || f.Name == "columnNameCharacterMap"
	}
	if !found {
		t.Error("the load form has no columnNameCharacterMap")
	}
}
