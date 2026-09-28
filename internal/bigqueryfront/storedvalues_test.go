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

// TestJSONLoadChecksBytes (#1065, #1075): a JSON load's BYTES values, at
// the top level, in a RECORD and in a REPEATED column, reach the emulator
// as base64 in the standard alphabet, which the emulator CloudBurrow builds
// decodes (#1061): one in the URL-safe alphabet is rewritten, bytes that are
// not UTF-8 (0xff) included; a record whose values need no change is sent
// as it was, NaN and ±Infinity with it; and the load's statistics count the
// data the client sent.
func TestJSONLoadChecksBytes(t *testing.T) {
	emu := &valuesEmulator{}
	h := Wrap(emu)
	const data = `{"id":1,"b":"YQBi","r":{"x":"4piD","g":"NaN"},"a":["YWJj","_w=="],"f":"Infinity"}` + "\n" +
		`{"id":2,"f":"NaN"}` + "\n\n" + `{"id":3,"B":"/w=="}`
	w := upload(t, h, valuesLoad("NEWLINE_DELIMITED_JSON", ""), data)
	if w.Code != 200 || len(emu.data) != 1 {
		t.Fatalf("load: %d %s, %d loads", w.Code, w.Body, len(emu.data))
	}
	lines := strings.Split(emu.data[0], "\n")
	if len(lines) != 5 || lines[1] != `{"id":2,"f":"NaN"}` || lines[2] != "" || lines[3] != `{"id":3,"B":"/w=="}` || lines[4] != "" {
		t.Fatalf("sent %q", emu.data[0])
	}
	var first map[string]any
	if json.Unmarshal([]byte(lines[0]), &first) != nil {
		t.Fatalf("sent %q", emu.data[0])
	}
	if got, want := fmt.Sprint(first), "map[a:[YWJj /w==] b:YQBi f:Infinity id:1 r:map[g:NaN x:4piD]]"; got != want {
		t.Errorf("first record sent as %q, want %q", got, want)
	}
	load, _ := loadReport(decodeBody(t, w.Body.Bytes()))
	if load["inputFileBytes"] != fmt.Sprint(len(data)) || load["inputFiles"] != "1" {
		t.Errorf("statistics.load %v, want the client's %d bytes", load, len(data))
	}

	// Into a table that exists, its columns are the ones read.
	emu = &valuesEmulator{schema: `{"fields":[{"name":"b","type":"BYTES"}]}`}
	job := `{"configuration":{"load":{"sourceFormat":"NEWLINE_DELIMITED_JSON","destinationTable":{"datasetId":"ds","tableId":"t"},` +
		`"schema":{"fields":[{"name":"b","type":"STRING"}]}}}}`
	if w := upload(t, Wrap(emu), job, `{"b":"_w=="}`+"\n"); w.Code != 200 || len(emu.data) != 1 || emu.data[0] != `{"b":"/w=="}`+"\n" {
		t.Errorf("into a table with a BYTES column: %d %s, sent %q", w.Code, w.Body, emu.data)
	}

	// With no BYTES column the data is not read, FLOAT64 or not.
	emu = &valuesEmulator{}
	job = `{"configuration":{"load":{"sourceFormat":"NEWLINE_DELIMITED_JSON","destinationTable":{"datasetId":"ds","tableId":"t"},` +
		`"schema":{"fields":[{"name":"f","type":"FLOAT64"}]}}}}`
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

// TestJSONLoadRefusesWhatItCannotWrite (#1065): a BYTES value that is not
// base64, and a line that is not one JSON object, fail the load 400
// invalid. The emulator loads nothing, and jobs.get reads the job failed.
func TestJSONLoadRefusesWhatItCannotWrite(t *testing.T) {
	for _, c := range []struct {
		name, data, reason string
		code               int
	}{
		{"not base64", `{"b":"%%%"}`, "invalid", 400},
		{"not base64 in a REPEATED column", `{"a":["YQ==","%%%"]}`, "invalid", 400},
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
// Storage into BYTES columns is read by the front and sent as one upload of
// its objects' records; with no Cloud Storage for the front to read, it is
// sent on as it came, for the emulator, which decodes BYTES itself since
// #1061, to read (it was 501 before).
func TestJSONLoadFromCloudStorageIsRead(t *testing.T) {
	st := &fakeStorage{objects: map[string]string{"b/d/one.json": `{"b":"_w=="}`, "b/d/two.json": `{"id":2}` + "\n"}}
	srv := httptest.NewServer(st)
	defer srv.Close()
	job := strings.Replace(valuesLoad("NEWLINE_DELIMITED_JSON", ""), `"destinationTable"`, `"sourceUris":["gs://b/d/*.json"],"destinationTable"`, 1)
	emu := &valuesEmulator{}
	if code, got := do(t, Wrap(emu, WithStorage(srv.URL)), "POST", base+"/jobs", job); code != 200 || len(emu.data) != 1 {
		t.Fatalf("load: %d %v, sent %q", code, got, emu.data)
	}
	if want := `{"b":"/w=="}` + "\n" + `{"id":2}` + "\n"; emu.data[0] != want {
		t.Errorf("sent %q, want %q", emu.data[0], want)
	}
	if load, _ := emu.jobs[0]["configuration"].(map[string]any)["load"].(map[string]any); load["sourceUris"] != nil {
		t.Errorf("sent with sourceUris: %v", load)
	}
	emu = &valuesEmulator{}
	if code, _ := do(t, Wrap(emu), "POST", base+"/jobs", job); code != 200 || len(emu.jobs) != 1 {
		t.Errorf("with no Cloud Storage: %d, %d jobs sent; want it sent on", code, len(emu.jobs))
	}
}

// TestCSVLoadChecksBytes (#1065, #1075): a CSV load's BYTES values reach
// the emulator as standard base64, which it decodes (#1061): 0xff and a
// carriage return included; NaN is sent as it is; a value that is not
// base64 fails the load 400 and nothing is loaded.
func TestCSVLoadChecksBytes(t *testing.T) {
	const schema = `{"fields":[{"name":"id","type":"INTEGER"},{"name":"b","type":"BYTES"},{"name":"f","type":"FLOAT"}]}`
	job := `{"jobReference":{"projectId":"p","jobId":"j1"},"configuration":{"load":{"sourceFormat":"CSV",` +
		`"destinationTable":{"datasetId":"ds","tableId":"t"},"schema":` + schema + `}}}`
	emu := &valuesEmulator{}
	if w := upload(t, Wrap(emu), job, "1,YWJj,1.5\n2,,inf\n3,_w==,NaN\n4,DQo=,nan\n"); w.Code != 200 || len(emu.data) != 1 {
		t.Fatalf("load: %d %s", w.Code, w.Body)
	}
	if want := "id,b,f\n1,YWJj,1.5\n2,,inf\n3,/w==,NaN\n4,DQo=,nan\n"; emu.data[0] != want {
		t.Errorf("sent %q, want %q", emu.data[0], want)
	}
	emu = &valuesEmulator{}
	if w := upload(t, Wrap(emu), job, "1,%%%,1\n"); w.Code != 400 || len(emu.data) != 0 {
		t.Errorf("not base64: %d %s, loaded %q; want 400", w.Code, w.Body, emu.data)
	}
}

// TestInsertAllChecksBytes (#1065, #1075): a streamed row's BYTES values
// reach the emulator as standard base64, which it decodes (#1061), 0xff
// included; a NaN is sent as it is; a value that is not base64 makes its
// row invalid (checkRow) and nothing is sent.
func TestInsertAllChecksBytes(t *testing.T) {
	emu := &valuesEmulator{schema: valuesSchema}
	path := base + "/datasets/ds/tables/t/insertAll"
	code, got := do(t, Wrap(emu), "POST", path, `{"rows":[{"json":{"id":1,"b":"YWJj","r":{"x":"_w==","g":"NaN"},"a":["AA=="],"f":"NaN"}}]}`)
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
	if got, want := fmt.Sprint(sent.Rows[0].JSON), "map[a:[AA==] b:YWJj f:NaN id:1 r:map[g:NaN x:/w==]]"; got != want {
		t.Errorf("sent %q, want %q", got, want)
	}
	emu = &valuesEmulator{schema: valuesSchema}
	if code, got := do(t, Wrap(emu), "POST", path, `{"rows":[{"json":{"id":1}},{"json":{"b":"%%%"}}]}`); code != 200 ||
		!strings.Contains(fmt.Sprint(got["insertErrors"]), "not a base64-encoded string") || len(emu.bodies) != 0 {
		t.Errorf("not base64: %d %v, sent %q; want the row invalid", code, got, emu.bodies)
	}
}

// TestNaNQueryParameters (#1066): a query whose parameters carry a FLOAT64
// NaN is sent on, through jobs.query and jobs.insert: the engine
// CloudBurrow builds keeps it (#1061); it was 501 before.
func TestNaNQueryParameters(t *testing.T) {
	params := `[{"name":"p","parameterType":{"type":"FLOAT64"},"parameterValue":{"value":"NaN"}}]`
	emu := &valuesEmulator{}
	h := Wrap(emu)
	if code, got := do(t, h, "POST", base+"/queries", `{"query":"SELECT @p","queryParameters":`+params+`}`); code != 200 {
		t.Errorf("jobs.query: %d %v, want it sent on", code, got)
	}
	if len(emu.bodies) == 0 {
		t.Errorf("nothing sent")
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

// TestQueryDestinationWriteDispositions (#1067, #1080): a query job with
// WRITE_EMPTY (the default) into a table with rows fails, duplicate, and
// its query is not run: the job sent runs nothing and names no table; into
// a new or empty table, or with WRITE_APPEND, it is sent as it is.
// WRITE_TRUNCATE and WRITE_TRUNCATE_DATA are TestQueryJobWriteDispositions.
func TestQueryDestinationWriteDispositions(t *testing.T) {
	for _, c := range []struct {
		name, write, schema, numRows string
		code                         int
	}{
		{"WRITE_EMPTY with rows", "WRITE_EMPTY", twoColumns, "3", 409},
		{"the default with rows", "", twoColumns, "3", 409},
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
		if c.code == 409 { // the job fails, duplicate
			status, _ := got["status"].(map[string]any)
			res, _ := status["errorResult"].(map[string]any)
			q, _ := got["configuration"].(map[string]any)["query"].(map[string]any)
			d, _ := q["destinationTable"].(map[string]any)
			if code != 200 || res["reason"] != "duplicate" || res["message"] != "Already Exists: Table p:ds.t" ||
				d["tableId"] != "t" || q["query"] != "SELECT 1 AS a" {
				t.Errorf("%s: %d %v, want the job failed duplicate", c.name, code, got)
			}
			if len(emu.jobs) != 1 || strings.Contains(emu.bodies[len(emu.bodies)-1], "destinationTable") ||
				strings.Contains(emu.bodies[len(emu.bodies)-1], "SELECT 1") {
				t.Errorf("%s: sent %v", c.name, emu.bodies)
			}
			continue
		}
		if code != c.code {
			t.Errorf("%s: %d %v, want %d", c.name, code, got, c.code)
		}
	}
}
