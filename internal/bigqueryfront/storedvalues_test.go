package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// valuesEmulator answers tables.get with schema (404 when it is empty)
// and numRows, and records each write: a multipart upload's job and data
// (whole, or cut short when the front cut the stream), and any other
// body. A job is answered with its configuration, as the emulator
// answers, and jobs.get with the last one.
type valuesEmulator struct {
	mu      sync.Mutex
	schema  string
	numRows string
	jobs    []map[string]any
	data    []string
	cut     int
	bodies  []string
}

func (e *valuesEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r.Method == http.MethodGet {
		switch {
		case strings.Contains(r.URL.Path, "/tables/") && e.schema != "":
			if e.numRows != "" {
				fmt.Fprintf(w, `{"schema":%s,"numRows":%q}`, e.schema, e.numRows)
				return
			}
			fmt.Fprintf(w, `{"schema":%s}`, e.schema)
		case strings.HasSuffix(r.URL.Path, "/jobs/j1") && len(e.jobs) > 0:
			_ = json.NewEncoder(w).Encode(e.answer(e.jobs[len(e.jobs)-1]))
		default:
			http.Error(w, `{"error":{"code":404,"message":"not found"}}`, http.StatusNotFound)
		}
		return
	}
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || params["boundary"] == "" {
		b, _ := io.ReadAll(r.Body)
		e.bodies = append(e.bodies, string(b))
		var job map[string]any
		if json.Unmarshal(b, &job) == nil && job["configuration"] != nil {
			e.jobs = append(e.jobs, job)
			_ = json.NewEncoder(w).Encode(e.answer(job))
			return
		}
		_, _ = io.WriteString(w, `{"kind":"bigquery#tableDataInsertAllResponse"}`)
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	var job map[string]any
	p, err := mr.NextPart()
	if err == nil {
		err = json.NewDecoder(p).Decode(&job)
	}
	var data []byte
	if err == nil {
		if p, err = mr.NextPart(); err == nil {
			data, err = io.ReadAll(p)
		}
	}
	if err != nil {
		e.cut++
		http.Error(w, `{"error":{"code":400,"message":"failed to read the data"}}`, http.StatusBadRequest)
		return
	}
	e.jobs = append(e.jobs, job)
	e.data = append(e.data, string(data))
	_ = json.NewEncoder(w).Encode(e.answer(job))
}

func (e *valuesEmulator) answer(job map[string]any) map[string]any {
	return map[string]any{"jobReference": map[string]any{"projectId": "p", "jobId": "j1"},
		"configuration": job["configuration"], "status": map[string]any{"state": "DONE"}}
}

const valuesSchema = `{"fields":[{"name":"id","type":"INTEGER"},{"name":"b","type":"BYTES"},` +
	`{"name":"r","type":"RECORD","fields":[{"name":"x","type":"BYTES"},{"name":"g","type":"FLOAT"}]},` +
	`{"name":"a","type":"BYTES","mode":"REPEATED"},{"name":"f","type":"FLOAT64"}]}`

func valuesLoad(format, extra string) string {
	return `{"jobReference":{"projectId":"p","jobId":"j1"},"configuration":{"load":{"sourceFormat":"` + format + `",` +
		`"destinationTable":{"datasetId":"ds","tableId":"t"},"schema":` + valuesSchema + extra + `}}}`
}

// TestJSONLoadSendsBytesAsTheirBytes (#1065): a JSON load's BYTES values,
// at the top level, in a RECORD and in a REPEATED column, reach the
// emulator as the strings of their bytes; a record with none is sent as it
// was, and the load's statistics count the data the client sent.
func TestJSONLoadSendsBytesAsTheirBytes(t *testing.T) {
	emu := &valuesEmulator{}
	h := Wrap(emu)
	const data = `{"id":1,"b":"YQBi","r":{"x":"4piD","g":1.5},"a":["YWJj","DQo="],"f":"Infinity"}` + "\n" +
		`{"id":2,"f":2.5}` + "\n\n" + `{"id":3,"B":"YWJj"}`
	w := upload(t, h, valuesLoad("NEWLINE_DELIMITED_JSON", ""), data)
	if w.Code != 200 || len(emu.data) != 1 {
		t.Fatalf("load: %d %s, %d loads", w.Code, w.Body, len(emu.data))
	}
	lines := strings.Split(emu.data[0], "\n")
	if len(lines) != 5 || lines[1] != `{"id":2,"f":2.5}` || lines[2] != "" || lines[4] != "" {
		t.Fatalf("sent %q", emu.data[0])
	}
	var first, third map[string]any
	if json.Unmarshal([]byte(lines[0]), &first) != nil || json.Unmarshal([]byte(lines[3]), &third) != nil {
		t.Fatalf("sent %q", emu.data[0])
	}
	if got, want := fmt.Sprint(first), "map[a:[abc \r\n] b:a\x00b f:Infinity id:1 r:map[g:1.5 x:☃]]"; got != want {
		t.Errorf("first record sent as %q, want %q", got, want)
	}
	if third["B"] != "abc" {
		t.Errorf("a field named in another case: %v", third)
	}
	load, _ := loadReport(decodeBody(t, w.Body.Bytes()))
	if load["inputFileBytes"] != fmt.Sprint(len(data)) || load["inputFiles"] != "1" {
		t.Errorf("statistics.load %v, want the client's %d bytes", load, len(data))
	}

	// Into a table that exists, its columns are the ones read: the
	// emulator loads into them.
	emu = &valuesEmulator{schema: `{"fields":[{"name":"b","type":"BYTES"}]}`}
	job := `{"configuration":{"load":{"sourceFormat":"NEWLINE_DELIMITED_JSON","destinationTable":{"datasetId":"ds","tableId":"t"},` +
		`"schema":{"fields":[{"name":"b","type":"STRING"}]}}}}`
	if w := upload(t, Wrap(emu), job, `{"b":"YWJj"}`+"\n"); w.Code != 200 || len(emu.data) != 1 || emu.data[0] != `{"b":"abc"}`+"\n" {
		t.Errorf("into a table with a BYTES column: %d %s, sent %q", w.Code, w.Body, emu.data)
	}

	// With no BYTES or FLOAT64 column the data is not read.
	emu = &valuesEmulator{}
	job = `{"configuration":{"load":{"sourceFormat":"NEWLINE_DELIMITED_JSON","destinationTable":{"datasetId":"ds","tableId":"t"},` +
		`"schema":{"fields":[{"name":"s","type":"STRING"}]}}}}`
	if w := upload(t, Wrap(emu), job, "not json at all"); w.Code != 200 || len(emu.data) != 1 || emu.data[0] != "not json at all" {
		t.Errorf("a load with no such column: %d %s, sent %q", w.Code, w.Body, emu.data)
	}
}

func decodeBody(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("answer %s: %v", b, err)
	}
	return m
}

// TestJSONLoadRefusesWhatItCannotWrite (#1065, #1066): a BYTES value
// whose bytes are not UTF-8 and a NaN in a FLOAT64 column, at any depth,
// are 501; a value that is not base64, and a line that is not one JSON
// object, fail the load 400 invalid. The emulator loads nothing, and
// jobs.get reads the job failed.
func TestJSONLoadRefusesWhatItCannotWrite(t *testing.T) {
	for _, c := range []struct {
		name, data, reason string
		code               int
	}{
		{"0xff", `{"id":1}` + "\n" + `{"b":"/w=="}`, "notImplemented", 501},
		{"0xff in a REPEATED column", `{"a":["YQ==","/w=="]}`, "notImplemented", 501},
		{"a NaN", `{"f":"NaN"}`, "notImplemented", 501},
		{"a nan in a RECORD", `{"r":{"g":"nan"}}`, "notImplemented", 501},
		{"not base64", `{"b":"%%%"}`, "invalid", 400},
		{"not an object", `[1,2]`, "invalid", 400},
		{"two objects on a line", `{"id":1} {"id":2}`, "invalid", 400},
	} {
		emu := &valuesEmulator{}
		h := Wrap(loadJobsEmulator(emu))
		w := upload(t, h, valuesLoad("NEWLINE_DELIMITED_JSON", ""), c.data+"\n")
		got := decodeBody(t, w.Body.Bytes())
		if e, _ := got["error"].(map[string]any); w.Code != c.code || e == nil || !strings.Contains(fmt.Sprint(e["errors"]), c.reason) {
			t.Errorf("%s: %d %s, want %d %s", c.name, w.Code, w.Body, c.code, c.reason)
		}
		if len(emu.data) != 0 {
			t.Errorf("%s: the emulator loaded %q", c.name, emu.data)
		}
		_, job := do(t, h, "GET", base+"/jobs/j1", "")
		if st, _ := job["status"].(map[string]any); st == nil || st["errorResult"] == nil {
			t.Errorf("%s: jobs.get: %v, want the job failed", c.name, job)
		}
	}
}

// TestJSONLoadFromCloudStorageIsRead (#1065): a JSON load from Cloud
// Storage into BYTES or FLOAT64 columns is read by the front and sent as
// one upload of its objects' records, the BYTES values decoded; with no
// Cloud Storage for the front to read, it is 501.
func TestJSONLoadFromCloudStorageIsRead(t *testing.T) {
	st := &fakeStorage{objects: map[string]string{"b/d/one.json": `{"b":"YWJj"}`, "b/d/two.json": `{"id":2}` + "\n"}}
	srv := httptest.NewServer(st)
	defer srv.Close()
	job := strings.Replace(valuesLoad("NEWLINE_DELIMITED_JSON", ""), `"destinationTable"`, `"sourceUris":["gs://b/d/*.json"],"destinationTable"`, 1)
	emu := &valuesEmulator{}
	if code, got := do(t, Wrap(emu, WithStorage(srv.URL)), "POST", base+"/jobs", job); code != 200 || len(emu.data) != 1 {
		t.Fatalf("load: %d %v, sent %q", code, got, emu.data)
	}
	if want := `{"b":"abc"}` + "\n" + `{"id":2}` + "\n"; emu.data[0] != want {
		t.Errorf("sent %q, want %q", emu.data[0], want)
	}
	if load, _ := emu.jobs[0]["configuration"].(map[string]any)["load"].(map[string]any); load["sourceUris"] != nil {
		t.Errorf("sent with sourceUris: %v", load)
	}
	emu = &valuesEmulator{}
	if code, _ := do(t, Wrap(emu), "POST", base+"/jobs", job); code != 501 || len(emu.jobs) != 0 {
		t.Errorf("with no Cloud Storage: %d, %d jobs sent; want 501", code, len(emu.jobs))
	}
}

// TestCSVLoadSendsBytesAsTheirBytes (#1065, #1066): a CSV load's BYTES
// values reach the emulator as the strings of their bytes; bytes that are
// not UTF-8, a carriage return and a NaN are 501, and nothing is loaded.
func TestCSVLoadSendsBytesAsTheirBytes(t *testing.T) {
	const schema = `{"fields":[{"name":"id","type":"INTEGER"},{"name":"b","type":"BYTES"},{"name":"f","type":"FLOAT"}]}`
	job := `{"jobReference":{"projectId":"p","jobId":"j1"},"configuration":{"load":{"sourceFormat":"CSV",` +
		`"destinationTable":{"datasetId":"ds","tableId":"t"},"schema":` + schema + `}}}`
	emu := &valuesEmulator{}
	if w := upload(t, Wrap(emu), job, "1,YWJj,1.5\n2,,inf\n3,4piD,\n"); w.Code != 200 || len(emu.data) != 1 {
		t.Fatalf("load: %d %s", w.Code, w.Body)
	}
	if want := "id,b,f\n1,abc,1.5\n2,,inf\n3,☃,\n"; emu.data[0] != want {
		t.Errorf("sent %q, want %q", emu.data[0], want)
	}
	for _, c := range []struct{ name, data string }{
		{"0xff", "1,/w==,1\n"},
		{"a carriage return", "1,DQo=,1\n"},
		{"a NaN", "1,,NaN\n"},
		{"a nan", "1,YQ==,nan\n"},
	} {
		emu := &valuesEmulator{}
		if w := upload(t, Wrap(emu), job, c.data); w.Code != 501 || len(emu.data) != 0 {
			t.Errorf("%s: %d %s, loaded %q; want 501", c.name, w.Code, w.Body, emu.data)
		}
	}
}

// TestInsertAllSendsBytesAsTheirBytes (#1065, #1066): a streamed row's
// BYTES values reach the emulator as the strings of their bytes; bytes
// that are not UTF-8 and a NaN are 501, and nothing is sent.
func TestInsertAllSendsBytesAsTheirBytes(t *testing.T) {
	emu := &valuesEmulator{schema: valuesSchema}
	path := base + "/datasets/ds/tables/t/insertAll"
	code, got := do(t, Wrap(emu), "POST", path, `{"rows":[{"json":{"id":1,"b":"YWJj","r":{"x":"4piD"},"a":["AA=="],"f":"-Infinity"}}]}`)
	if code != 200 || len(emu.bodies) != 1 {
		t.Fatalf("insertAll: %d %v", code, got)
	}
	var sent struct {
		Rows []struct {
			JSON map[string]any `json:"json"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(emu.bodies[0]), &sent); err != nil || len(sent.Rows) != 1 {
		t.Fatalf("sent %s", emu.bodies[0])
	}
	if got, want := fmt.Sprint(sent.Rows[0].JSON), "map[a:[\x00] b:abc f:-Infinity id:1 r:map[x:☃]]"; got != want {
		t.Errorf("sent %q, want %q", got, want)
	}
	for _, c := range []struct{ name, row string }{
		{"0xff", `{"b":"/w=="}`},
		{"0xff in a RECORD", `{"r":{"x":"/w=="}}`},
		{"a NaN", `{"f":"NaN"}`},
		{"a NaN in a RECORD", `{"r":{"g":"nan"}}`},
	} {
		emu := &valuesEmulator{schema: valuesSchema}
		if code, got := do(t, Wrap(emu), "POST", path, `{"rows":[{"json":{"id":1}},{"json":`+c.row+`}]}`); code != 501 || len(emu.bodies) != 0 {
			t.Errorf("%s: %d %v, sent %q; want 501", c.name, code, got, emu.bodies)
		}
	}
}

// TestNaNQueryParameters (#1066): a query whose parameters carry a
// FLOAT64 NaN, at any depth, is 501, through jobs.query and jobs.insert;
// one with ±Infinity is sent on.
func TestNaNQueryParameters(t *testing.T) {
	for _, c := range []struct {
		name, params string
		nan          bool
	}{
		{"scalar", `[{"name":"p","parameterType":{"type":"FLOAT64"},"parameterValue":{"value":"NaN"}}]`, true},
		{"positional", `[{"parameterType":{"type":"FLOAT64"},"parameterValue":{"value":"nan"}}]`, true},
		{"in an array", `[{"name":"p","parameterType":{"type":"ARRAY","arrayType":{"type":"FLOAT64"}},` +
			`"parameterValue":{"arrayValues":[{"value":"1"},{"value":"NaN"}]}}]`, true},
		{"in a struct", `[{"name":"p","parameterType":{"type":"STRUCT","structTypes":[{"name":"x","type":{"type":"FLOAT64"}}]},` +
			`"parameterValue":{"structValues":{"x":{"value":"NaN"}}}}]`, true},
		{"infinity", `[{"name":"p","parameterType":{"type":"FLOAT64"},"parameterValue":{"value":"-Infinity"}}]`, false},
		{"a string NaN", `[{"name":"p","parameterType":{"type":"STRING"},"parameterValue":{"value":"NaN"}}]`, false},
		{"a NULL", `[{"name":"p","parameterType":{"type":"FLOAT64"},"parameterValue":{}}]`, false},
	} {
		if got := nanParameter(json.RawMessage(c.params)); (got != "") != c.nan {
			t.Errorf("%s: nanParameter = %q, want a NaN %v", c.name, got, c.nan)
		}
		if !c.nan {
			continue
		}
		emu := &valuesEmulator{}
		h := Wrap(emu)
		if code, got := do(t, h, "POST", base+"/queries", `{"query":"SELECT @p","queryParameters":`+c.params+`}`); code != 501 {
			t.Errorf("%s: jobs.query: %d %v, want 501", c.name, code, got)
		}
		if code, got := do(t, h, "POST", base+"/jobs", `{"configuration":{"query":{"query":"INSERT INTO ds.t (f) VALUES (@p)",`+
			`"queryParameters":`+c.params+`}}}`); code != 501 {
			t.Errorf("%s: jobs.insert: %d %v, want 501", c.name, code, got)
		}
		if len(emu.bodies) != 0 {
			t.Errorf("%s: sent %q", c.name, emu.bodies)
		}
	}
}

// TestWriteTruncateDataIsSentAsWriteTruncate (#1067): a load with
// WRITE_TRUNCATE_DATA reaches the emulator with WRITE_TRUNCATE, whose load
// replaces the rows and keeps the table's schema, and reads back with
// WRITE_TRUNCATE_DATA in jobs.insert's answer and jobs.get; its outputRows
// are all the table's rows after it.
func TestWriteTruncateDataIsSentAsWriteTruncate(t *testing.T) {
	emu := &rowsEmulator{rows: 5, exists: true}
	h := Wrap(loadJobsEmulator(emu))
	w := upload(t, h, jsonLoad(`,"writeDisposition":"WRITE_TRUNCATE_DATA"`), "{\"a\":1}\n{\"a\":2}\n")
	if w.Code != 200 {
		t.Fatalf("load: %d %s", w.Code, w.Body)
	}
	if emu.rows != 2 {
		t.Errorf("the table has %d rows after the load, want 2", emu.rows)
	}
	for what, load := range loadStats(t, h, w.Body.Bytes()) {
		if load["outputRows"] != "2" {
			t.Errorf("%s: outputRows %v, want 2", what, load["outputRows"])
		}
	}

	for _, format := range []string{"NEWLINE_DELIMITED_JSON", "CSV"} {
		emu := &valuesEmulator{}
		h := Wrap(emu)
		data := `{"id":1}` + "\n"
		if format == "CSV" {
			data = "1\n"
		}
		job := strings.Replace(valuesLoad(format, `,"writeDisposition":"WRITE_TRUNCATE_DATA"`), `"schema":`+valuesSchema,
			`"schema":{"fields":[{"name":"id","type":"INTEGER"}]}`, 1)
		w := upload(t, h, job, data)
		if w.Code != 200 || len(emu.jobs) != 1 {
			t.Fatalf("%s: %d %s", format, w.Code, w.Body)
		}
		sent := emu.jobs[0]["configuration"].(map[string]any)["load"].(map[string]any)
		if sent["writeDisposition"] != "WRITE_TRUNCATE" {
			t.Errorf("%s: sent writeDisposition %v, want WRITE_TRUNCATE", format, sent["writeDisposition"])
		}
		answered := decodeBody(t, w.Body.Bytes())["configuration"].(map[string]any)["load"].(map[string]any)
		if answered["writeDisposition"] != "WRITE_TRUNCATE_DATA" {
			t.Errorf("%s: jobs.insert answered %v, want WRITE_TRUNCATE_DATA", format, answered["writeDisposition"])
		}
		_, got := do(t, h, "GET", base+"/jobs/j1", "")
		if conf, _ := got["configuration"].(map[string]any); conf == nil || conf["load"].(map[string]any)["writeDisposition"] != "WRITE_TRUNCATE_DATA" {
			t.Errorf("%s: jobs.get: %v, want WRITE_TRUNCATE_DATA", format, got)
		}
	}
}

// TestQueryDestinationWriteDispositions (#1067): a query job into a table
// that exists, with WRITE_TRUNCATE_DATA or WRITE_TRUNCATE, or with
// WRITE_EMPTY (the default) into one with rows, is 501 and not sent; into
// a new or empty table, or with WRITE_APPEND, it is sent.
func TestQueryDestinationWriteDispositions(t *testing.T) {
	for _, c := range []struct {
		name, write, schema, numRows string
		code                         int
	}{
		{"WRITE_TRUNCATE_DATA", "WRITE_TRUNCATE_DATA", twoColumns, "3", 501},
		{"WRITE_TRUNCATE of an empty table", "WRITE_TRUNCATE", twoColumns, "", 501},
		{"WRITE_EMPTY with rows", "WRITE_EMPTY", twoColumns, "3", 501},
		{"the default with rows", "", twoColumns, "3", 501},
		{"the default into an empty table", "", twoColumns, "", 200},
		{"WRITE_APPEND", "WRITE_APPEND", twoColumns, "3", 200},
		{"WRITE_TRUNCATE_DATA into a new table", "WRITE_TRUNCATE_DATA", "", "", 200},
	} {
		emu := &valuesEmulator{schema: c.schema, numRows: c.numRows}
		write := ""
		if c.write != "" {
			write = `,"writeDisposition":"` + c.write + `"`
		}
		job := `{"jobReference":{"projectId":"p","jobId":"j1"},"configuration":{"query":{"query":"SELECT 1 AS a",` +
			`"useLegacySql":false,"destinationTable":{"projectId":"p","datasetId":"ds","tableId":"t"}` + write + `}}}`
		code, got := do(t, Wrap(emu), "POST", base+"/jobs", job)
		if code != c.code {
			t.Errorf("%s: %d %v, want %d", c.name, code, got, c.code)
		}
		if c.code == 501 && len(emu.jobs) != 0 {
			t.Errorf("%s: sent %v", c.name, emu.jobs)
		}
	}
}
