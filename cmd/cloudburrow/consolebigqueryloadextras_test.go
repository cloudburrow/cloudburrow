package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"strings"
	"testing"
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
