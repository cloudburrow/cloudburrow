package bigqueryfront

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scriptEmulator keeps tables by REST path, and answers each query with
// run's answer. Every request is logged, "METHOD path" or "QUERY text".
type scriptEmulator struct {
	tables map[string]string // path -> table resource JSON
	run    func(query string) (int, string)
	log    []string
	upload string // the last multipart upload's body
}

func (e *scriptEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && strings.Contains(path, "/tables/"):
		e.log = append(e.log, "GET "+path)
		t, ok := e.tables[path]
		if !ok {
			http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, t)
	case r.Method == http.MethodDelete:
		e.log = append(e.log, "DELETE "+path)
		delete(e.tables, path)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/upload/"):
		b, _ := io.ReadAll(r.Body)
		e.upload = string(b)
		e.log = append(e.log, "UPLOAD "+r.URL.RawQuery)
		_, _ = io.WriteString(w, `{"jobReference":{"projectId":"p","jobId":"up"},"status":{"state":"DONE"}}`)
	case r.Method == http.MethodPost:
		var body struct {
			Query         string `json:"query"`
			Configuration struct {
				Query struct {
					Query string `json:"query"`
				} `json:"query"`
			} `json:"configuration"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		q := body.Query
		if strings.HasSuffix(path, "/jobs") {
			q = body.Configuration.Query.Query
		}
		e.log = append(e.log, "QUERY "+q)
		status, answer := 200, `{"jobComplete":true}`
		if e.run != nil {
			status, answer = e.run(q)
		}
		if strings.HasSuffix(path, "/jobs") && status == 200 && !strings.Contains(answer, "jobReference") {
			answer = `{"jobReference":{"projectId":"p","jobId":"j1"},"status":{"state":"DONE"}}`
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	default:
		http.Error(w, "unexpected", http.StatusTeapot)
	}
}

const tablesBase = base + "/datasets/ds/tables/"

func queryBody(path, sql string) string {
	b, _ := json.Marshal(sql)
	if path == "/jobs" {
		return `{"jobReference":{"jobId":"j1"},"configuration":{"query":{"query":` + string(b) + `}}}`
	}
	return `{"query":` + string(b) + `}`
}

func errorReason(got map[string]any) string {
	if e, ok := got["error"].(map[string]any); ok {
		errs, _ := e["errors"].([]any)
		if len(errs) > 0 {
			r, _ := errs[0].(map[string]any)["reason"].(string)
			return r
		}
	}
	st, _ := got["status"].(map[string]any)
	er, _ := st["errorResult"].(map[string]any)
	r, _ := er["reason"].(string)
	return r
}

// TestViewsAreChecked (#916): a view's ID is held to the table ID rule
// and its query's columns to the column rules, through CREATE VIEW and
// tables.insert alike; CREATE MATERIALIZED VIEW, DROP MATERIALIZED VIEW
// and a view's column list, which the emulator does not support, are 501.
func TestViewsAreChecked(t *testing.T) {
	for _, c := range []struct {
		sql  string
		code int
		want string
	}{
		{"CREATE VIEW ds.`v!` AS SELECT 1 AS a", 400, `Invalid table ID "v!"`},
		{"CREATE OR REPLACE VIEW IF NOT EXISTS ds.v (`c!`, d) AS SELECT 1, 2", 400, `Invalid field name "c!"`},
		{"CREATE VIEW ds.v (c, d) AS SELECT 1, 2", 501, "CREATE VIEW with a column list"},
		{"CREATE MATERIALIZED VIEW ds.mv AS SELECT 1 AS a", 501, "CREATE MATERIALIZED VIEW"},
		{"CREATE MATERIALIZED VIEW ds.`mv!` AS SELECT 1 AS a", 400, `Invalid table ID "mv!"`},
		{"DROP MATERIALIZED VIEW ds.mv", 501, "DROP MATERIALIZED VIEW"},
		{"RAISE USING MESSAGE = 'x'", 501, "RAISE"},
		{"SELECT 'RAISE'", 0, ""},
	} {
		got := checkDDL(c.sql)
		if got.code != c.code || !strings.Contains(got.msg, c.want) {
			t.Errorf("%q: %d %q, want %d %q", c.sql, got.code, got.msg, c.code, c.want)
		}
	}
	v := checkDDL("CREATE OR REPLACE VIEW `p.ds.v` OPTIONS(description='AS') AS SELECT a AS `x!` FROM ds.t")
	if s := v.selects(); len(s) != 1 || !s[0].view || !s[0].replace || s[0].query != "SELECT a AS `x!` FROM ds.t" ||
		strings.Join(s[0].path, ".") != "p.ds.v" {
		t.Fatalf("the view's query: %+v", v)
	}

	emu := &ctasEmulator{schema: `{"fields":[{"name":"w!","type":"INTEGER"}]}`}
	code, got := do(t, Wrap(emu), "POST", base+"/datasets/ds/tables",
		`{"tableReference":{"tableId":"tv"},"view":{"query":"SELECT a AS `+"`w!`"+` FROM ds.t","useLegacySql":false}}`)
	if code != 400 || !strings.Contains(got["error"].(map[string]any)["message"].(string), `"w!"`) {
		t.Errorf("tables.insert of a view with a column w!: %d %v", code, got)
	}
	emu = &ctasEmulator{schema: `{"fields":[{"name":"w!","type":"INTEGER"}]}`}
	if code, _ := do(t, Wrap(emu), "POST", base+"/datasets/ds/tables",
		`{"tableReference":{"tableId":"tv"},"view":{"query":"SELECT a FROM [ds.t]","useLegacySql":true}}`); code != 200 {
		t.Errorf("a legacy SQL view was read: %d", code)
	}
}

// TestCreateOrReplaceExistingTable (#918): the front carries out a lone
// CREATE OR REPLACE of an existing table: the statement is run into a
// scratch table first, then the table is deleted and the statement is
// sent reading the scratch table, which is then deleted. When the scratch
// run fails, its error is the answer and the table is untouched. Inside a
// script of several statements it is 501. A view is deleted and the
// statement sent as it is.
func TestCreateOrReplaceExistingTable(t *testing.T) {
	for _, path := range []string{"/queries", "/jobs"} {
		emu := &scriptEmulator{tables: map[string]string{tablesBase + "t": `{"type":"TABLE"}`}}
		code, got := do(t, Wrap(emu), "POST", base+path, queryBody(path, "CREATE OR REPLACE TABLE ds.t AS SELECT * FROM ds.t WHERE a > 1"))
		if code != 200 {
			t.Fatalf("%s: %d %v", path, code, got)
		}
		log := strings.Join(emu.log, "\n")
		scratch := strings.SplitN(strings.SplitN(log, "ds._cloudburrow_replace_", 2)[1], "`", 2)[0]
		want := strings.Join([]string{
			"QUERY SELECT * FROM (\nSELECT * FROM ds.t WHERE a > 1\n) LIMIT 0",
			"GET " + tablesBase + "t",
			"QUERY CREATE OR REPLACE TABLE `ds._cloudburrow_replace_" + scratch + "` AS SELECT * FROM ds.t WHERE a > 1",
			"DELETE " + tablesBase + "t",
			"QUERY CREATE OR REPLACE TABLE ds.t AS SELECT * FROM `ds._cloudburrow_replace_" + scratch + "`",
			"DELETE " + tablesBase + "_cloudburrow_replace_" + scratch,
		}, "\n")
		if log != want {
			t.Errorf("%s: the requests were\n%s\nwant\n%s", path, log, want)
		}
	}

	emu := &scriptEmulator{tables: map[string]string{tablesBase + "t": `{"type":"TABLE"}`}, run: func(q string) (int, string) {
		if strings.Contains(q, "_cloudburrow_replace_") {
			return 400, `{"error":{"code":400,"message":"failed on ` + strings.Split(strings.Split(q, "`")[1], ".")[1] + `"}}`
		}
		return 200, `{"jobComplete":true}`
	}}
	code, got := do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", "CREATE OR REPLACE TABLE ds.t (a INT64)"))
	if msg, _ := got["error"].(map[string]any)["message"].(string); code != 400 || msg != "failed on t" {
		t.Errorf("a failing replacement: %d %v", code, got)
	}
	if _, ok := emu.tables[tablesBase+"t"]; !ok {
		t.Error("the table was deleted though its replacement failed")
	}

	emu = &scriptEmulator{tables: map[string]string{tablesBase + "t": `{"type":"TABLE"}`}}
	code, got = do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", "SELECT 1; CREATE OR REPLACE TABLE ds.t (a INT64)"))
	if code != 501 || errorReason(got) != "notImplemented" || len(emu.tables) != 1 {
		t.Errorf("a replacement inside a script: %d %v", code, got)
	}

	emu = &scriptEmulator{tables: map[string]string{tablesBase + "v": `{"type":"VIEW"}`}}
	if code, got := do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", "CREATE OR REPLACE VIEW ds.v AS SELECT 1 AS a")); code != 200 ||
		emu.log[len(emu.log)-2] != "DELETE "+tablesBase+"v" || emu.log[len(emu.log)-1] != "QUERY CREATE OR REPLACE VIEW ds.v AS SELECT 1 AS a" {
		t.Errorf("replacing a view: %d %v %q", code, got, emu.log)
	}

	emu = &scriptEmulator{tables: map[string]string{}}
	if code, _ := do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", "CREATE OR REPLACE TABLE ds.new AS SELECT 1 AS a")); code != 200 ||
		emu.log[len(emu.log)-1] != "QUERY CREATE OR REPLACE TABLE ds.new AS SELECT 1 AS a" {
		t.Errorf("a new table was not sent as it was: %q", emu.log)
	}
}

// TestFailedScriptWithAHandlerIs501 (#918): the emulator never runs an
// EXCEPTION handler, so a script with one that fails is 501, naming the
// emulator's error; through jobs.insert the job is failed so, and jobs.get
// and jobs.list report it. A script with a handler that succeeds is
// answered as it was.
func TestFailedScriptWithAHandlerIs501(t *testing.T) {
	sql := "BEGIN SELECT * FROM nope.nope; EXCEPTION WHEN ERROR THEN SELECT 1; END"
	emu := &scriptEmulator{run: func(string) (int, string) {
		return 400, `{"error":{"code":400,"message":"Table not found: nope.nope"}}`
	}}
	code, got := do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", sql))
	if msg, _ := got["error"].(map[string]any)["message"].(string); code != 501 || !strings.Contains(msg, "Table not found: nope.nope") {
		t.Errorf("jobs.query: %d %v", code, got)
	}

	emu = &scriptEmulator{run: func(string) (int, string) {
		return 200, `{"jobReference":{"jobId":"j1"},"status":{"state":"DONE","errorResult":{"reason":"jobInternalError","message":"Table not found: nope.nope"}}}`
	}}
	list := &listingEmulator{scriptEmulator: emu}
	h := Wrap(list)
	code, got = do(t, h, "POST", base+"/jobs", queryBody("/jobs", sql))
	if code != 200 || errorReason(got) != "notImplemented" {
		t.Errorf("jobs.insert: %d %v", code, got)
	}
	if _, job := do(t, h, "GET", base+"/jobs/j1", ""); errorReason(job) != "notImplemented" {
		t.Errorf("jobs.get: %v", job)
	}
	_, jobs := do(t, h, "GET", base+"/jobs", "")
	items, _ := jobs["jobs"].([]any)
	if len(items) != 2 || errorReason(items[0].(map[string]any)) != "notImplemented" || errorReason(items[1].(map[string]any)) != "" {
		t.Errorf("jobs.list: %v", jobs)
	}

	emu = &scriptEmulator{}
	if code, _ := do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", "BEGIN SELECT 1; EXCEPTION WHEN ERROR THEN SELECT 2; END")); code != 200 {
		t.Errorf("a script whose block succeeds: %d", code)
	}
}

// listingEmulator adds jobs.get and jobs.list, which report every job
// done and succeeded, as the emulator does (measured).
type listingEmulator struct{ *scriptEmulator }

func (e *listingEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/jobs"):
		_, _ = io.WriteString(w, `{"jobs":[{"jobReference":{"projectId":"p","jobId":"j1"},"status":{"state":"DONE"}},`+
			`{"jobReference":{"projectId":"p","jobId":"other"},"status":{"state":"DONE"}}]}`)
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/jobs/"):
		_, _ = io.WriteString(w, `{"jobReference":{"projectId":"p","jobId":"j1"},"status":{"state":"DONE"}}`)
	default:
		e.scriptEmulator.ServeHTTP(w, r)
	}
}

// TestCreateTableAsSelectCheckedAfterTheScript (#917): a CREATE TABLE ...
// AS SELECT whose query cannot run alone is checked after the script, in
// the table it made: a column BigQuery refuses deletes the table and
// fails the query, 400 through jobs.query and a failed job through
// jobs.insert, and the error names the statements after it that ran. A
// TEMP table, and a table that was there before, are not checked.
func TestCreateTableAsSelectCheckedAfterTheScript(t *testing.T) {
	sql := "DECLARE x INT64 DEFAULT 1; CREATE TABLE ds.s AS SELECT x AS `a!`; SELECT 2"
	for _, path := range []string{"/queries", "/jobs"} {
		emu := &scriptEmulator{tables: map[string]string{}}
		emu.run = func(q string) (int, string) {
			if strings.HasPrefix(q, "SELECT * FROM (") {
				return 400, `{"error":{"code":400,"message":"Unrecognized name: x"}}`
			}
			emu.tables[tablesBase+"s"] = `{"type":"TABLE","schema":{"fields":[{"name":"a!","type":"INTEGER"}]}}`
			return 200, `{"jobComplete":true}`
		}
		code, got := do(t, Wrap(emu), "POST", base+path, queryBody(path, sql))
		wantCode := 400
		if path == "/jobs" {
			wantCode = 200
		}
		msg := ""
		if e, ok := got["error"].(map[string]any); ok {
			msg, _ = e["message"].(string)
		} else if st, ok := got["status"].(map[string]any); ok {
			msg, _ = st["errorResult"].(map[string]any)["message"].(string)
		}
		if code != wantCode || errorReason(got) != "invalidQuery" || !strings.Contains(msg, `"a!"`) ||
			!strings.Contains(msg, "The 1 statements after it") {
			t.Errorf("%s: %d %v", path, code, got)
		}
		if _, ok := emu.tables[tablesBase+"s"]; ok {
			t.Errorf("%s: the table with a refused column is still there", path)
		}
	}

	emu := &scriptEmulator{tables: map[string]string{tablesBase + "s": `{"type":"TABLE","schema":{"fields":[{"name":"a!","type":"INTEGER"}]}}`},
		run: func(q string) (int, string) {
			if strings.HasPrefix(q, "SELECT * FROM (") {
				return 400, `{"error":{"code":400}}`
			}
			return 200, `{"jobComplete":true}`
		}}
	if code, _ := do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", "DECLARE x INT64; CREATE TEMP TABLE tt AS SELECT x AS `b!`; CREATE TABLE ds.s AS SELECT x AS `a!`")); code != 200 ||
		len(emu.tables) != 1 {
		t.Errorf("a TEMP table, and one there before, were checked: %d %v", code, emu.log)
	}
}

// TestCSVHeaderDetection (#919): BigQuery takes a CSV's first row as its
// header only when it holds only strings and another row does not; the
// emulator always does, so a load whose table says otherwise is 501, and
// so is skipLeadingRows other than 1, which the emulator ignores.
func TestCSVHeaderDetection(t *testing.T) {
	for _, c := range []struct {
		fields  string
		differs bool
	}{
		{`[{"name":"a","type":"STRING"},{"name":"b","type":"STRING"}]`, true},
		{`[{"name":"a","type":"INTEGER"},{"name":"b","type":"STRING"}]`, false},
		{`[{"name":"1","type":"INTEGER"},{"name":"2","type":"INTEGER"}]`, true},
		{`[{"name":"true","type":"STRING"},{"name":"b","type":"INTEGER"}]`, true},
		{`[{"name":"2024-01-01","type":"DATE"}]`, true},
		{`[{"name":"first name","type":"STRING"},{"name":"n","type":"INTEGER"}]`, false},
	} {
		var fields []field
		if err := json.Unmarshal([]byte(c.fields), &fields); err != nil {
			t.Fatal(err)
		}
		if got := headerDiffers(fields); (got != "") != c.differs {
			t.Errorf("%s: %q, want differs=%v", c.fields, got, c.differs)
		}
	}
	for skip, code := range map[string]int{`"0"`: 501, `2`: 501, `"1"`: 200} {
		emu := &loadEmulator{detected: `{"fields":[{"name":"a","type":"INTEGER"}]}`, tables: map[string]string{}}
		body := `{"jobReference":{"jobId":"j"},"configuration":{"load":{"destinationTable":{"datasetId":"ds","tableId":"t"},` +
			`"autodetect":true,"skipLeadingRows":` + skip + `}}}`
		if got, _ := do(t, Wrap(emu), "POST", base+"/jobs", body); got != code {
			t.Errorf("skipLeadingRows %s: %d, want %d", skip, got, code)
		}
	}
	emu := &loadEmulator{detected: `{"fields":[{"name":"a","type":"STRING"},{"name":"b","type":"STRING"}]}`, tables: map[string]string{}}
	h := Wrap(emu)
	body := `{"jobReference":{"jobId":"j"},"configuration":{"load":{"destinationTable":{"datasetId":"ds","tableId":"t"},"autodetect":true}}}`
	if code, _ := do(t, h, "POST", base+"/jobs", body); code != 501 || len(emu.tables) != 0 {
		t.Errorf("an all-string CSV: %d, tables %v", code, emu.tables)
	}
}

// TestResumableUploads (#919): the front serves a resumable upload itself
// and sends the whole of it to the emulator as one multipart upload, on
// the last chunk, whose answer is the job. The chunks before it are
// answered 308, or 200 with X-Http-Status-Code-Override when the client
// asks with X-GUploader-No-308.
func TestResumableUploads(t *testing.T) {
	emu := &scriptEmulator{tables: map[string]string{}}
	h := Wrap(emu)
	job := `{"jobReference":{"jobId":"up"},"configuration":{"load":{"destinationTable":{"datasetId":"ds","tableId":"t"},"sourceFormat":"CSV"}}}`
	r := httptest.NewRequest("POST", "/upload"+base+"/jobs?uploadType=resumable", strings.NewReader(job))
	r.Host = "bq.test:9050"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	loc := w.Header().Get("Location")
	if w.Code != 200 || !strings.HasPrefix(loc, "http://bq.test:9050/upload"+base+"/jobs?") || !strings.Contains(loc, "upload_id=") {
		t.Fatalf("the first request: %d, Location %q", w.Code, loc)
	}
	chunk := func(body, cr string, no308 bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", strings.TrimPrefix(loc, "http://bq.test:9050"), strings.NewReader(body))
		r.Header.Set("Content-Range", cr)
		if no308 {
			r.Header.Set("X-GUploader-No-308", "yes")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := chunk("a,b\n", "bytes 0-3/*", false); w.Code != 308 || w.Header().Get("Range") != "bytes=0-3" {
		t.Errorf("the first chunk: %d %v", w.Code, w.Header())
	}
	if w := chunk("1,", "bytes 4-5/*", true); w.Code != 200 || w.Header().Get("X-Http-Status-Code-Override") != "308" ||
		w.Header().Get("Range") != "bytes=0-5" {
		t.Errorf("a chunk with X-GUploader-No-308: %d %v", w.Code, w.Header())
	}
	if len(emu.log) != 0 {
		t.Errorf("the emulator was sent a chunk: %q", emu.log)
	}
	w = chunk("2\n", "bytes 6-7/8", true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"jobId":"up"`) {
		t.Fatalf("the last chunk: %d %s", w.Code, w.Body)
	}
	if len(emu.log) == 0 || emu.log[len(emu.log)-1] != "UPLOAD uploadType=multipart" {
		t.Fatalf("the emulator was sent %q", emu.log)
	}
	parts := readParts(t, emu.upload)
	if len(parts) != 2 || parts[0] != job || parts[1] != "a,b\n1,2\n" {
		t.Errorf("the multipart upload: %q", parts)
	}
	if w := chunk("x", "bytes 8-8/9", false); w.Code != 404 {
		t.Errorf("a chunk after the last: %d", w.Code)
	}
}

func readParts(t *testing.T, body string) []string {
	t.Helper()
	i := strings.Index(body, "\r\n")
	boundary := strings.TrimPrefix(body[:i], "--")
	_, params, _ := mime.ParseMediaType("multipart/related; boundary=" + boundary)
	mr := multipart.NewReader(strings.NewReader(body), params["boundary"])
	var out []string
	for {
		p, err := mr.NextPart()
		if err != nil {
			return out
		}
		b, _ := io.ReadAll(p)
		out = append(out, string(b))
	}
}
