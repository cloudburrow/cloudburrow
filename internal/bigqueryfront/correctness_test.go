package bigqueryfront

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
)

// The tests in this file are #931, #932, #934 and #937.

// TestSkipRecords: a record ends at a newline outside quotes, and an empty
// line is not one, as encoding/csv reads them.
func TestSkipRecords(t *testing.T) {
	for _, c := range []struct {
		data string
		skip int64
		want string
	}{
		{"1,x\n2,y\n", 0, "1,x\n2,y\n"},
		{"h,h\n1,x\n", 1, "1,x\n"},
		{"h,h\r\n1,x\r\n", 1, "1,x\r\n"},
		{"\n\nh,h\n1,x\n", 1, "1,x\n"},
		{"\"a\nb\",h\n1,x\n", 1, "1,x\n"},
		{"\"a \"\"q\"\"\nb\",h\nsecond\n1,x", 2, "1,x"},
		{"h,h\n", 3, ""},
		{"only", 1, ""},
	} {
		got, err := io.ReadAll(&skipReader{r: bufio.NewReader(strings.NewReader(c.data)), skip: c.skip})
		if err != nil || string(got) != c.want {
			t.Errorf("%q skipping %d: %q %v, want %q", c.data, c.skip, got, err, c.want)
		}
	}
}

// csvEmulator reads a multipart load as the pinned emulator does
// (server/handler.go): the first CSV record is the header; when every
// name in it is a column, values go to the columns it names, else to the
// schema's columns in order. It answers a table read with schema.
type csvEmulator struct {
	schema string
	mu     sync.Mutex
	rows   []map[string]string
	loads  int
}

func (e *csvEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if e.schema == "" {
			http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"schema":`+e.schema+`}`)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.loads++
	_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	mr := multipart.NewReader(r.Body, params["boundary"])
	p, err := mr.NextPart()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var job jobBody
	if err := json.NewDecoder(p).Decode(&job); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	schema := job.Configuration.Load.Schema
	if schema == nil {
		var meta struct{ Schema *tableSchema }
		_ = json.Unmarshal([]byte(`{"schema":`+e.schema+`}`), &meta)
		schema = meta.Schema
	}
	if p, err = mr.NextPart(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	records, err := csv.NewReader(p).ReadAll()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(records) < 2 {
		_, _ = io.WriteString(w, `{}`)
		return
	}
	cols := records[0]
	known := map[string]bool{}
	for _, f := range schema.Fields {
		known[f.Name] = true
	}
	for _, c := range cols {
		if !known[c] {
			cols = nil
			for _, f := range schema.Fields {
				cols = append(cols, f.Name)
			}
			break
		}
	}
	for _, rec := range records[1:] {
		row := map[string]string{}
		for i, v := range rec {
			row[cols[i]] = v
		}
		e.rows = append(e.rows, row)
	}
	_, _ = io.WriteString(w, `{}`)
}

// upload sends a load job and its data as the Go client does, a
// multipart upload.
func upload(t *testing.T, h http.Handler, job, data string) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	p, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json"}})
	_, _ = p.Write([]byte(job))
	p, _ = mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/octet-stream"}})
	_, _ = p.Write([]byte(data))
	_ = mw.Close()
	r := httptest.NewRequest("POST", "/upload"+base+"/jobs?uploadType=multipart", &b)
	r.Header.Set("Content-Type", "multipart/related; boundary="+mw.Boundary())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// TestCSVLoadWithColumnsLoadsEveryRow (#931): a CSV load whose schema is
// given, or whose table exists, loads every row the load does not skip,
// each value to the column at its position, through an emulator that
// always takes the first record as the header.
func TestCSVLoadWithColumnsLoadsEveryRow(t *testing.T) {
	const schema = `{"fields":[{"name":"a","type":"INTEGER"},{"name":"b","type":"STRING"}]}`
	load := func(extra string) string {
		return `{"configuration":{"load":{"destinationTable":{"datasetId":"ds","tableId":"t"},"schema":` + schema + extra + `}}}`
	}
	for _, c := range []struct {
		name, job, table, data string
		want                   []map[string]string
	}{
		{"no header", load(""), "", "1,x\n2,y\n3,z\n",
			[]map[string]string{{"a": "1", "b": "x"}, {"a": "2", "b": "y"}, {"a": "3", "b": "z"}}},
		{"one row, no newline", load(""), "", "1,x", []map[string]string{{"a": "1", "b": "x"}}},
		{"skipLeadingRows 0", load(`,"skipLeadingRows":"0"`), "", "1,x\n2,y\n",
			[]map[string]string{{"a": "1", "b": "x"}, {"a": "2", "b": "y"}}},
		{"skipLeadingRows 1 with the columns named out of order", load(`,"skipLeadingRows":1`), "", "b,a\n1,x\n",
			[]map[string]string{{"a": "1", "b": "x"}}},
		{"skipLeadingRows 2 with a quoted newline", load(`,"skipLeadingRows":2,"allowQuotedNewlines":true`), "", "\"a\nb\"\nsecond,row\n1,x\n",
			[]map[string]string{{"a": "1", "b": "x"}}},
		{"into an existing table with no load schema",
			`{"configuration":{"load":{"sourceFormat":"CSV","destinationTable":{"datasetId":"ds","tableId":"t"}}}}`,
			schema, "1,x\n2,y\n", []map[string]string{{"a": "1", "b": "x"}, {"a": "2", "b": "y"}}},
	} {
		emu := &csvEmulator{schema: c.table}
		w := upload(t, Wrap(emu), c.job, c.data)
		if w.Code != 200 || emu.loads != 1 {
			t.Errorf("%s: %d %s, %d loads", c.name, w.Code, w.Body, emu.loads)
			continue
		}
		got, _ := json.Marshal(emu.rows)
		want, _ := json.Marshal(c.want)
		if !bytes.Equal(got, want) {
			t.Errorf("%s: loaded %s, want %s", c.name, got, want)
		}
	}

	// With autodetect and columns given (#945), skipLeadingRows says how
	// many rows are not data; unset, BigQuery decides, and it is 501.
	emu := &csvEmulator{schema: schema}
	upload(t, Wrap(emu), `{"configuration":{"load":{"autodetect":true,"skipLeadingRows":1,"destinationTable":{"datasetId":"ds","tableId":"t"}}}}`, "b,a\n1,x\n")
	if got, _ := json.Marshal(emu.rows); string(got) != `[{"a":"1","b":"x"}]` {
		t.Errorf("autodetect, skipLeadingRows 1: loaded %s", got)
	}
	emu = &csvEmulator{schema: schema}
	upload(t, Wrap(emu), `{"configuration":{"load":{"autodetect":true,"skipLeadingRows":"0","destinationTable":{"datasetId":"ds","tableId":"t"}}}}`, "1,x\n")
	if got, _ := json.Marshal(emu.rows); string(got) != `[{"a":"1","b":"x"}]` {
		t.Errorf("autodetect, skipLeadingRows 0: loaded %s", got)
	}
	emu = &csvEmulator{schema: schema}
	if w := upload(t, Wrap(emu), `{"configuration":{"load":{"autodetect":true,"destinationTable":{"datasetId":"ds","tableId":"t"}}}}`, "1,x\n"); w.Code != 501 || emu.loads != 0 {
		t.Errorf("autodetect into an existing table, no skipLeadingRows: %d %s, %d loads; want 501", w.Code, w.Body, emu.loads)
	}
}

// TestCSVLoadFromCloudStorage (#931): with no Cloud Storage to read a
// gs:// load's data from (#944), the emulator reads it itself, so one with
// columns given is 501 unless skipLeadingRows is 1; a load with no columns
// given is left alone. With autodetect and columns given, skipLeadingRows
// must be set (#945).
func TestCSVLoadFromCloudStorage(t *testing.T) {
	const schema = `"schema":{"fields":[{"name":"a","type":"STRING"}]}`
	for _, c := range []struct {
		job  string
		want int
	}{
		{`{"configuration":{"load":{"sourceUris":["gs://b/o"],"destinationTable":{"datasetId":"ds","tableId":"t"},` + schema + `}}}`, 501},
		{`{"configuration":{"load":{"sourceUris":["gs://b/o"],"skipLeadingRows":"2","destinationTable":{"datasetId":"ds","tableId":"t"},` + schema + `}}}`, 501},
		{`{"configuration":{"load":{"sourceUris":["gs://b/o"],"skipLeadingRows":1,"destinationTable":{"datasetId":"ds","tableId":"t"},` + schema + `}}}`, 200},
		{`{"configuration":{"load":{"sourceUris":["gs://b/o"],"autodetect":true,"destinationTable":{"datasetId":"ds","tableId":"t"},` + schema + `}}}`, 501},
		{`{"configuration":{"load":{"sourceUris":["gs://b/o"],"autodetect":true,"skipLeadingRows":1,"destinationTable":{"datasetId":"ds","tableId":"t"},` + schema + `}}}`, 200},
		{`{"configuration":{"load":{"sourceUris":["gs://b/o"],"sourceFormat":"NEWLINE_DELIMITED_JSON","destinationTable":{"datasetId":"ds","tableId":"t"},` + schema + `}}}`, 200},
		{`{"configuration":{"load":{"sourceUris":["gs://b/o"],"destinationTable":{"datasetId":"ds","tableId":"t"}}}}`, 200},
	} {
		emu := &fakeEmulator{}
		code, got := do(t, Wrap(emu), "POST", base+"/jobs", c.job)
		if code != c.want || (code == 501) != (len(emu.writes) == 0) {
			t.Errorf("%s: %d %v, %d writes; want %d", c.job, code, got, len(emu.writes), c.want)
		}
	}
}

// jobEmulator answers jobs.insert with answer and status, and jobs.get and
// jobs.list as the pinned emulator does after it: the job done, with no
// error.
type jobEmulator struct {
	status int
	answer string
}

func (e *jobEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost:
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(e.status)
		_, _ = io.WriteString(w, e.answer)
	case strings.HasSuffix(r.URL.Path, "/jobs"):
		_, _ = io.WriteString(w, `{"jobs":[{"jobReference":{"projectId":"p","jobId":"j1"}}]}`)
	default:
		_, _ = io.WriteString(w, `{"jobReference":{"projectId":"p","jobId":"j1"},"status":{"state":"DONE"}}`)
	}
}

// TestFailedJobsReadBackFailed (#934): a job the emulator failed in its
// jobs.insert answer, with errorResult or with an error status, reads back
// failed from jobs.get, jobs.getQueryResults and jobs.list; one it then
// answers without an error reads back as the emulator has it.
func TestFailedJobsReadBackFailed(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		answer string
		reason string
	}{
		{"errorResult", 200, `{"jobReference":{"projectId":"p","jobId":"j1"},"status":{"state":"DONE","errorResult":{"reason":"invalidQuery","message":"Table not found: nope.nope"}}}`, "invalidQuery"},
		{"400", 400, `{"error":{"code":400,"message":"failed to read csv","errors":[{"reason":"jobInternalError","message":"failed to read csv"}]}}`, "jobInternalError"},
	} {
		emu := &jobEmulator{status: c.status, answer: c.answer}
		h := Wrap(emu)
		do(t, h, "POST", base+"/jobs", `{"jobReference":{"jobId":"j1"},"configuration":{"query":{"query":"SELECT * FROM nope.nope"}}}`)
		_, job := do(t, h, "GET", base+"/jobs/j1", "")
		status, _ := job["status"].(map[string]any)
		res, _ := status["errorResult"].(map[string]any)
		if res["reason"] != c.reason {
			t.Errorf("%s: jobs.get: %v, want errorResult %s", c.name, job, c.reason)
		}
		if code, _ := do(t, h, "GET", base+"/queries/j1", ""); code != 400 {
			t.Errorf("%s: jobs.getQueryResults: %d, want 400", c.name, code)
		}
		_, list := do(t, h, "GET", base+"/jobs", "")
		if b, _ := json.Marshal(list); !strings.Contains(string(b), `"reason":"`+c.reason+`"`) {
			t.Errorf("%s: jobs.list: %s", c.name, b)
		}

		// The same ID answered without an error, as a retried insert can
		// be, is no longer failed.
		emu.status, emu.answer = 200, `{"jobReference":{"projectId":"p","jobId":"j1"},"status":{"state":"DONE"}}`
		do(t, h, "POST", base+"/jobs", `{"jobReference":{"jobId":"j1"},"configuration":{"query":{"query":"SELECT 1"}}}`)
		if _, job := do(t, h, "GET", base+"/jobs/j1", ""); job["status"].(map[string]any)["errorResult"] != nil {
			t.Errorf("%s: jobs.get after a success: %v", c.name, job)
		}
	}

	// A 409 is about an ID that is another job's: that job is not failed.
	emu := &jobEmulator{status: 409, answer: `{"error":{"code":409,"message":"already exists","errors":[{"reason":"duplicate"}]}}`}
	h := Wrap(emu)
	do(t, h, "POST", base+"/jobs", `{"jobReference":{"jobId":"j1"},"configuration":{"query":{"query":"SELECT 1"}}}`)
	if _, job := do(t, h, "GET", base+"/jobs/j1", ""); job["status"].(map[string]any)["errorResult"] != nil {
		t.Errorf("jobs.get after a 409: %v", job)
	}
}

// existsEmulator has the tables in tables; it answers every query with
// no rows, and records the queries it is sent.
type existsEmulator struct {
	tables  map[string]bool
	queries []string
}

func (e *existsEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) < 2 || !e.tables[parts[len(parts)-3]+"."+parts[len(parts)-1]] {
			http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"type":"TABLE","schema":{"fields":[{"name":"a","type":"INTEGER"}]}}`)
		return
	}
	var body struct {
		Query         string `json:"query"`
		Configuration struct {
			Query struct {
				Query string `json:"query"`
			} `json:"query"`
		} `json:"configuration"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	q := body.Query + body.Configuration.Query.Query
	if !strings.HasPrefix(q, "SELECT * FROM (\n") {
		e.queries = append(e.queries, q)
	}
	_, _ = io.WriteString(w, `{"jobComplete":true,"schema":{"fields":[{"name":"a","type":"INTEGER"}]}}`)
}

// TestCreateIfNotExistsOfAnExistingOneDoesNothing (#932): CREATE TABLE or
// VIEW ... IF NOT EXISTS of one that exists, or that an earlier statement
// of the script makes, is sent as a statement that does nothing; after a
// DROP of it, of for one that does not exist, as it is.
func TestCreateIfNotExistsOfAnExistingOneDoesNothing(t *testing.T) {
	noop := "DROP TABLE IF EXISTS `ds._cloudburrow_replace_"
	for _, c := range []struct {
		sql  string
		want []string // what each statement is sent as: "" as it is, or noop
	}{
		{"CREATE TABLE IF NOT EXISTS ds.t AS SELECT 2 AS a", []string{noop}},
		{"CREATE TABLE IF NOT EXISTS ds.t (z STRING)", []string{noop}},
		{"create view if not exists ds.t as select 1 as a", []string{noop}},
		{"CREATE TABLE IF NOT EXISTS ds.new (a INT64)", []string{""}},
		{"CREATE TABLE ds.t2 (a INT64); CREATE TABLE IF NOT EXISTS ds.t2 (a INT64); SELECT 1", []string{"", noop, ""}},
		{"DROP TABLE ds.t; CREATE TABLE IF NOT EXISTS ds.t (a INT64)", []string{"", ""}},
		{"DROP TABLE ds.other; CREATE TABLE IF NOT EXISTS ds.t (a INT64)", []string{"", noop}},
		{"CREATE TEMP TABLE IF NOT EXISTS t AS SELECT 1 AS a", []string{""}},
		{"CREATE TABLE IF NOT EXISTS t (a INT64)", []string{""}},
	} {
		for _, insert := range []bool{false, true} {
			emu := &existsEmulator{tables: map[string]bool{"ds.t": true}}
			path, body := base+"/queries", `{"query":`+quote(c.sql)+`}`
			if insert {
				path, body = base+"/jobs", `{"configuration":{"query":{"query":`+quote(c.sql)+`}}}`
			}
			if code, got := do(t, Wrap(emu), "POST", path, body); code != 200 || len(emu.queries) != 1 {
				t.Errorf("%q: %d %v, sent %q", c.sql, code, got, emu.queries)
				continue
			}
			stmts := strings.Split(emu.queries[0], ";")
			orig := strings.Split(c.sql, ";")
			if len(stmts) != len(c.want) {
				t.Errorf("%q was sent as %q", c.sql, emu.queries[0])
				continue
			}
			for i, w := range c.want {
				if w == "" && stmts[i] != orig[i] || w != "" && !strings.HasPrefix(strings.TrimSpace(stmts[i]), w) {
					t.Errorf("%q: statement %d was sent as %q", c.sql, i+1, stmts[i])
				}
			}
		}
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestCreateTableLikeCopyCloneAreNotImplemented (#937): the emulator
// supports none of them (400 "not supported", measured), so the front
// answers 501 naming the statement, before anything is run. A LIKE in a
// query is not one.
func TestCreateTableLikeCopyCloneAreNotImplemented(t *testing.T) {
	for _, c := range []struct {
		sql, want string
	}{
		{"CREATE TABLE ds.c LIKE ds.t", "CREATE TABLE LIKE"},
		{"CREATE TABLE IF NOT EXISTS ds.c LIKE ds.t", "CREATE TABLE LIKE"},
		{"create table ds.c like ds.t as select 5 as a", "CREATE TABLE LIKE"},
		{"CREATE TABLE ds.c COPY ds.t", "CREATE TABLE COPY"},
		{"CREATE OR REPLACE TABLE ds.c COPY ds.t", "CREATE TABLE COPY"},
		{"CREATE TABLE ds.c CLONE ds.t FOR SYSTEM_TIME AS OF CURRENT_TIMESTAMP()", "CREATE TABLE CLONE"},
		{"SELECT 1; CREATE TABLE ds.c PARTITION BY d OPTIONS(description='x') CLONE ds.t", "CREATE TABLE CLONE"},
		{"CREATE SNAPSHOT TABLE ds.s CLONE ds.t", "CREATE SNAPSHOT TABLE"},
		{"DROP SNAPSHOT TABLE IF EXISTS ds.s", "DROP SNAPSHOT TABLE"},
		{"CREATE TABLE ds.c AS SELECT * FROM ds.t WHERE b LIKE 'x%'", ""},
		{"CREATE VIEW ds.v AS SELECT b LIKE 'x%' AS m FROM ds.t", ""},
		{"CREATE TABLE ds.`t!` LIKE ds.t", `Invalid table ID "t!"`},
	} {
		v := checkDDL(c.sql)
		switch {
		case c.want == "" && v.code != 0,
			strings.HasPrefix(c.want, "Invalid") && (v.code != 400 || !strings.Contains(v.msg, c.want)),
			c.want != "" && !strings.HasPrefix(c.want, "Invalid") && (v.code != 501 || v.reason != "notImplemented" ||
				!strings.Contains(v.msg, "Not implemented here: "+c.want+".")):
			t.Errorf("%q: %d %s %q, want %q", c.sql, v.code, v.reason, v.msg, c.want)
		}
	}
	emu := &fakeEmulator{}
	for _, path := range []string{base + "/queries", base + "/jobs"} {
		body := `{"query":"CREATE TABLE ds.c CLONE ds.t"}`
		if strings.HasSuffix(path, "/jobs") {
			body = `{"configuration":{"query":{"query":"CREATE TABLE ds.c CLONE ds.t"}}}`
		}
		if code, got := do(t, Wrap(emu), "POST", path, body); code != 501 {
			t.Errorf("%s: %d %v, want 501", path, code, got)
		}
	}
	if len(emu.writes) != 0 {
		t.Errorf("the emulator was sent %q", emu.writes)
	}
}
