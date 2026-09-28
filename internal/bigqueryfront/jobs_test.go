package bigqueryfront

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
)

// TestJobsCheckTheTablesTheyMake (#881): a load, copy or query job's
// destination table is held to the table ID rule and a load's schema to
// the schema rules, 400 invalid, and nothing reaches the emulator; a job
// that passes is sent on as it came.
func TestJobsCheckTheTablesTheyMake(t *testing.T) {
	for _, c := range []struct {
		name, body string
		want       int
	}{
		{"load", `{"configuration":{"load":{"destinationTable":{"projectId":"p","datasetId":"d","tableId":"ok table-1"},` +
			`"schema":{"fields":[{"name":"a","type":"STRING"}]},"sourceFormat":"CSV"}}}`, 200},
		{"load into t!", `{"configuration":{"load":{"destinationTable":{"tableId":"t!"}}}}`, 400},
		{"load schema named twice", `{"configuration":{"load":{"destinationTable":{"tableId":"t"},` +
			`"schema":{"fields":[{"name":"a","type":"STRING"},{"name":"A","type":"STRING"}]}}}}`, 400},
		{"load a RECORD in a REPEATED RECORD", `{"configuration":{"load":{"destinationTable":{"tableId":"t"},"schema":{"fields":[{"name":"a",` +
			`"type":"RECORD","mode":"REPEATED","fields":[{"name":"b","type":"RECORD","fields":[{"name":"s","type":"STRING"}]}]}]}}}}`, 501},
		{"load a REPEATED RECORD in a RECORD", `{"configuration":{"load":{"destinationTable":{"tableId":"t"},"schema":{"fields":[{"name":"a",` +
			`"type":"RECORD","fields":[{"name":"b","type":"RECORD","mode":"REPEATED","fields":[{"name":"s","type":"STRING"}]}]}]},"sourceFormat":"CSV"}}}`, 200},
		{"load into an existing such table", `{"configuration":{"load":{"destinationTable":{"datasetId":"d","tableId":"deep"}}}}`, 501},
		{"copy into t!", `{"configuration":{"copy":{"destinationTable":{"tableId":"t!"}}}}`, 400},
		{"query into t!", `{"configuration":{"query":{"query":"SELECT 1","destinationTable":{"tableId":"t!"}}}}`, 400},
		{"DDL job", "{\"configuration\":{\"query\":{\"query\":\"CREATE TABLE d.`t!` (a STRING)\"}}}", 400},
		{"extract", `{"configuration":{"extract":{"sourceTable":{"tableId":"t!"}}}}`, 200},
	} {
		emu := &fakeEmulator{}
		if strings.Contains(c.body, `"deep"`) {
			emu.schema = `{"fields":[{"name":"a","type":"RECORD","mode":"REPEATED","fields":[{"name":"b","type":"RECORD","fields":[{"name":"s","type":"STRING"}]}]}]}`
		}
		// A job with a jobReference is sent on as it came (#973).
		c.body = `{"jobReference":{"projectId":"p","jobId":"j"},` + c.body[1:]
		code, got := do(t, Wrap(emu), "POST", base+"/jobs", c.body)
		if code != c.want {
			t.Errorf("%s: %d %v, want %d", c.name, code, got, c.want)
		}
		if sent := len(emu.writes) == 1; sent != (c.want == 200) || sent && emu.writes[0] != c.body {
			t.Errorf("%s: the emulator was sent %q", c.name, emu.writes)
		}
	}
}

// A load from a local file is a multipart upload: the job is its first
// part and the data follows. The front reads the job only, and the body
// the emulator gets is the one the client sent, byte for byte.
func TestLoadUploadIsCheckedByItsFirstPart(t *testing.T) {
	upload := func(table string) (*bytes.Buffer, string) {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		p, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json"}})
		_, _ = p.Write([]byte(`{"jobReference":{"jobId":"j"},"configuration":{"load":{"destinationTable":{"tableId":"` + table + `"},"sourceFormat":"NEWLINE_DELIMITED_JSON"}}}`))
		p, _ = mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/octet-stream"}})
		_, _ = p.Write([]byte(strings.Repeat(`{"a":"x"}`+"\n", 200000)))
		_ = mw.Close()
		return &b, "multipart/related; boundary=" + mw.Boundary()
	}
	for _, c := range []struct {
		table string
		want  int
	}{{"fine", 200}, {"t!", 400}} {
		emu := &fakeEmulator{}
		body, ct := upload(c.table)
		sent := body.String()
		r := httptest.NewRequest("POST", "/upload/bigquery/v2/projects/p/jobs?uploadType=multipart", body)
		r.Header.Set("Content-Type", ct)
		w := httptest.NewRecorder()
		Wrap(emu).ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("upload into %s: %d %s, want %d", c.table, w.Code, w.Body, c.want)
		}
		if c.want == http.StatusOK && (len(emu.writes) != 1 || emu.writes[0] != sent) {
			t.Errorf("upload into %s: the emulator was not sent the body as it came (%d writes)", c.table, len(emu.writes))
		}
		if c.want != http.StatusOK && len(emu.writes) != 0 {
			t.Errorf("upload into %s: the refused load reached the emulator", c.table)
		}
	}
}

// TestDDLNames (#881): CREATE TABLE and CREATE SCHEMA are held to the ID,
// column and field name rules; what the lexer cannot place is left to the
// engine.
func TestDDLNames(t *testing.T) {
	for _, c := range []struct {
		sql, want string
	}{
		{"SELECT 1", ""},
		{"CREATE TABLE ds.t (a STRING, `first name` INT64 NOT NULL OPTIONS(description='x, y'))", ""},
		{"CREATE TABLE ds.`t!` (a STRING)", `Invalid table ID "t!"`},
		{"create or replace temp table `p.ds.t!` as select 1 x", `Invalid table ID "t!"`},
		{"CREATE TABLE IF NOT EXISTS my-project.ds.`ok table-1` (a STRING)", ""},
		{"CREATE TABLE ds.t (`a!` STRING)", `Invalid field name "a!"`},
		{"CREATE TABLE ds.t (a STRING, b ARRAY<STRUCT<x INT64, `y.z` STRING>>)", `Invalid field name "b.y.z"`},
		{"CREATE TABLE ds.t (a STRUCT<x STRUCT<`_partition` INT64>>)", `Invalid field name "a.x._partition"`},
		{"CREATE TABLE ds.t (a STRUCT<INT64, STRING>, n NUMERIC(10, 2), r RANGE<DATE>, PRIMARY KEY (a) NOT ENFORCED)", ""},
		{"CREATE TABLE ds.t (a ARRAY<STRUCT<b STRUCT<s STRING>>>)", ""},
		{"CREATE SCHEMA `bad-name`", `Invalid dataset ID "bad-name"`},
		{"CREATE SCHEMA IF NOT EXISTS p.good_1", ""},
		{"CREATE TABLE FUNCTION ds.`f!`() AS SELECT 1", ""},
		{"SELECT 'CREATE TABLE ds.`t!` (a INT64)'; -- CREATE TABLE ds.`u!`\n/* CREATE SCHEMA `x-y` */ SELECT 2", ""},
		{"SELECT 1; CREATE TABLE ds.ok (a INT64); CREATE TABLE ds.`no!` (a INT64)", `Invalid table ID "no!"`},
		{`SELECT r"\"; CREATE TABLE ds.t (a INT64)`, ""},
		{"CREATE TABLE ds.`t", ""},
	} {
		got := checkDDL(c.sql).msg
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%q: %q, want %q", c.sql, got, c.want)
		}
	}
	emu := &fakeEmulator{}
	code, _ := do(t, Wrap(emu), "POST", base+"/queries", "{\"query\":\"CREATE SCHEMA `bad-name`\"}")
	if code != 400 || len(emu.writes) != 0 {
		t.Errorf("jobs.query with a bad CREATE SCHEMA: %d, %d writes", code, len(emu.writes))
	}
}

// TestLoadWithoutSourceFormatIsCSV (#919): a load that names no
// sourceFormat is a CSV load, as BigQuery reads it; the emulator, which
// has no default, is sent sourceFormat CSV, in a jobs.insert body and in
// a multipart upload's first part, whose data is sent as it came.
func TestLoadWithoutSourceFormatIsCSV(t *testing.T) {
	emu := &fakeEmulator{}
	body := `{"configuration":{"load":{"destinationTable":{"tableId":"t"},"sourceUris":["gs://b/o"],"skipLeadingRows":1,` +
		`"schema":{"fields":[{"name":"a","type":"STRING"}]}}}}`
	if code, _ := do(t, Wrap(emu), "POST", base+"/jobs", body); code != 200 || len(emu.writes) != 1 ||
		!strings.Contains(emu.writes[0], `"sourceFormat":"CSV"`) || !strings.Contains(emu.writes[0], `"gs://b/o"`) {
		t.Errorf("jobs.insert: %d %q", code, emu.writes)
	}
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	p, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json"}})
	_, _ = p.Write([]byte(`{"configuration":{"load":{"destinationTable":{"tableId":"t"}}}}`))
	data := strings.Repeat("a,b\n", 400000)
	p, _ = mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/octet-stream"}})
	_, _ = p.Write([]byte(data))
	_ = mw.Close()
	emu = &fakeEmulator{}
	r := httptest.NewRequest("POST", "/upload/bigquery/v2/projects/p/jobs?uploadType=multipart", &b)
	r.Header.Set("Content-Type", "multipart/related; boundary="+mw.Boundary())
	w := httptest.NewRecorder()
	Wrap(emu).ServeHTTP(w, r)
	if w.Code != 200 || len(emu.writes) != 1 {
		t.Fatalf("the upload: %d %s", w.Code, w.Body)
	}
	mr := multipart.NewReader(strings.NewReader(emu.writes[0]), mw.Boundary())
	var parts []string
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(p)
		parts = append(parts, string(b))
	}
	if len(parts) != 2 || !strings.Contains(parts[0], `"sourceFormat":"CSV"`) || parts[1] != data {
		t.Errorf("the emulator was sent %d parts, the first %q", len(parts), parts[0])
	}
}
