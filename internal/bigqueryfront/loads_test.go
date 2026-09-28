package bigqueryfront

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
)

// The tests in this file are #944, #945 and #946.

const twoColumns = `{"fields":[{"name":"a","type":"INTEGER"},{"name":"b","type":"STRING"}]}`

func loadJob(extra string) string {
	return `{"jobReference":{"projectId":"p","jobId":"j1"},"configuration":{"load":{"sourceFormat":"CSV",` +
		`"destinationTable":{"datasetId":"ds","tableId":"t"},"schema":` + twoColumns + extra + `}}}`
}

// TestCSVDialect (#945): a load's fieldDelimiter, quote, allowJaggedRows
// and nullMarker are read by the front, which writes the data on as the
// comma-separated, "-quoted CSV the emulator reads.
func TestCSVDialect(t *testing.T) {
	for _, c := range []struct {
		name, extra, data, want string
	}{
		{"tab", `,"fieldDelimiter":"\t"`, "1\tx,y\n2\tz\n", `[{"a":"1","b":"x,y"},{"a":"2","b":"z"}]`},
		{"tab escape", `,"fieldDelimiter":"\\t"`, "1\tx\n", `[{"a":"1","b":"x"}]`},
		{"pipe, CRLF", `,"fieldDelimiter":"|"`, "1|x\r\n2|\"q|r\"\r\n", `[{"a":"1","b":"x"},{"a":"2","b":"q|r"}]`},
		{"quote '", `,"quote":"'"`, "1,'x,''y'''\n2,\"z\"\n", `[{"a":"1","b":"x,'y'"},{"a":"2","b":"\"z\""}]`},
		{"no quote", `,"quote":""`, "1,\"x\"\n", `[{"a":"1","b":"\"x\""}]`},
		{"quoted newline", `,"fieldDelimiter":";","allowQuotedNewlines":true`, "1;\"a\nb\"\n", `[{"a":"1","b":"a\nb"}]`},
		{"jagged", `,"allowJaggedRows":true`, "1\n2,y\n", `[{"a":"1","b":""},{"a":"2","b":"y"}]`},
		{"null marker", `,"nullMarker":"\\N"`, "1,\\N\n2,\"\\N\"\n\\N,y\n", `[{"a":"1","b":""},{"a":"2","b":"\\N"},{"a":"","b":"y"}]`},
		{"skip with a dialect", `,"fieldDelimiter":"|","skipLeadingRows":1`, "b|a\n1|x\n", `[{"a":"1","b":"x"}]`},
		{"null marker, jagged", `,"nullMarker":"NA","allowJaggedRows":true`, "1\n", `[{"a":"1","b":""}]`},
	} {
		emu := &csvEmulator{}
		w := upload(t, Wrap(emu), loadJob(c.extra), c.data)
		if w.Code != 200 {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
			continue
		}
		if got, _ := json.Marshal(emu.rows); string(got) != c.want {
			t.Errorf("%s: loaded %s, want %s", c.name, got, c.want)
		}
	}

	// Failures while the data passes: the load fails, and jobs.get says so.
	for _, c := range []struct {
		name, extra, data string
		code              int
		reason            string
	}{
		{"empty STRING with a null marker", `,"nullMarker":"\\N"`, "1,\n", 501, "notImplemented"},
		{"empty INTEGER with a null marker", `,"nullMarker":"\\N"`, ",x\n", 400, "invalid"},
		{"quote not closed", `,"quote":"'"`, "1,'x\n", 400, "invalid"},
		{"data after a closing quote", `,"quote":"'"`, "1,'x'y\n", 400, "invalid"},
	} {
		emu := &csvEmulator{}
		h := Wrap(emu)
		w := upload(t, h, loadJob(c.extra), c.data)
		var got struct {
			Error struct {
				Errors []rowError `json:"errors"`
			} `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code != c.code || len(got.Error.Errors) != 1 || got.Error.Errors[0].Reason != c.reason || len(emu.rows) != 0 {
			t.Errorf("%s: %d %s, %d rows; want %d %s", c.name, w.Code, w.Body, len(emu.rows), c.code, c.reason)
		}
	}

	// A delimiter or quote the front does not read is 501 before the data
	// is sent.
	for _, extra := range []string{`,"fieldDelimiter":"||"`, `,"fieldDelimiter":"é"`, `,"quote":"<<"`, `,"quote":"|","fieldDelimiter":"|"`} {
		emu := &csvEmulator{}
		if w := upload(t, Wrap(emu), loadJob(extra), "1,x\n"); w.Code != 501 || emu.loads != 0 {
			t.Errorf("%s: %d %s, %d loads; want 501", extra, w.Code, w.Body, emu.loads)
		}
	}
}

// TestCSVDialectFailureReadsBackFailed (#945): a load the front failed
// while its data passed reads back failed from jobs.get.
func TestCSVDialectFailureReadsBackFailed(t *testing.T) {
	emu := &csvEmulator{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+base+"/jobs/j1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"jobReference":{"projectId":"p","jobId":"j1"},"status":{"state":"DONE"}}`)
	})
	mux.Handle("/", emu)
	h := Wrap(mux)
	if w := upload(t, h, loadJob(`,"nullMarker":"\\N"`), ",x\n"); w.Code != 400 {
		t.Fatalf("load: %d %s", w.Code, w.Body)
	}
	_, job := do(t, h, "GET", base+"/jobs/j1", "")
	st, _ := job["status"].(map[string]any)
	er, _ := st["errorResult"].(map[string]any)
	if er["reason"] != "invalid" {
		t.Errorf("jobs.get: %v, want the job failed invalid", job)
	}
}

// fakeStorage serves objects over the Cloud Storage JSON API: metadata,
// media (alt=media) and lists by prefix, a page of two at a time.
type fakeStorage struct {
	mu      sync.Mutex
	objects map[string]string // bucket/name -> data
	reads   []string
}

func (s *fakeStorage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/storage/v1/b/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	bucket, obj, _ := strings.Cut(rest, "/o")
	bucket, _ = url.PathUnescape(bucket)
	if obj == "" {
		var names []string
		for k := range s.objects {
			if b, n, _ := strings.Cut(k, "/"); b == bucket && strings.HasPrefix(n, r.URL.Query().Get("prefix")) {
				names = append(names, n)
			}
		}
		if len(names) == 0 && !s.bucketExists(bucket) {
			http.NotFound(w, r)
			return
		}
		sort.Strings(names)
		start := 0
		if tok := r.URL.Query().Get("pageToken"); tok != "" {
			start = sort.SearchStrings(names, tok)
		}
		end := min(start+2, len(names))
		out := map[string]any{}
		var items []map[string]string
		for _, n := range names[start:end] {
			items = append(items, map[string]string{"name": n})
		}
		out["items"] = items
		if end < len(names) {
			out["nextPageToken"] = names[end]
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	name, _ := url.PathUnescape(strings.TrimPrefix(obj, "/"))
	data, ok := s.objects[bucket+"/"+name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.URL.Query().Get("alt") == "media" {
		s.reads = append(s.reads, name)
		_, _ = io.WriteString(w, data)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"name": name})
}

func (s *fakeStorage) bucketExists(bucket string) bool {
	for k := range s.objects {
		if strings.HasPrefix(k, bucket+"/") {
			return true
		}
	}
	return false
}

// uploadRecorder records the uploads a front sends on, as csvEmulator
// reads them, and the path each was sent to.
type uploadRecorder struct {
	csvEmulator
	paths []string
}

func (u *uploadRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		u.paths = append(u.paths, r.URL.Path+"?"+r.URL.RawQuery)
	}
	u.csvEmulator.ServeHTTP(w, r)
}

// TestCSVLoadFromCloudStorageIsRead (#944): a CSV load from gs:// URIs is
// read from the instance's Cloud Storage by the front and sent on as an
// upload of the same job: every row loaded, each file's leading rows
// skipped, the CSV options applied, wildcards expanded.
func TestCSVLoadFromCloudStorageIsRead(t *testing.T) {
	st := &fakeStorage{objects: map[string]string{
		"b/one.csv":      "1,x\n2,y",
		"b/two.csv":      "3,z\n",
		"b/dir/a.csv":    "h,h\n4,p\n",
		"b/dir/b.csv":    "h,h\n5,q\n",
		"b/dir/c.csv":    "h,h\n6,r\n",
		"b/dir/skip.txt": "junk",
		"b/tab.tsv":      "7\tt\n",
		"b/auto.csv":     "a,b\n8,s\n",
		"b/auto2.csv":    "a,b\n9,u\n",
	}}
	srv := httptest.NewServer(st)
	defer srv.Close()
	gcs := func(uris, extra string) string {
		return `{"jobReference":{"projectId":"p","jobId":"j1"},"configuration":{"load":{"sourceUris":[` + uris + `],` +
			`"destinationTable":{"datasetId":"ds","tableId":"t"},"schema":` + twoColumns + extra + `}}}`
	}
	for _, c := range []struct {
		name, job, want string
	}{
		{"one file, no header", gcs(`"gs://b/one.csv"`, ""), `[{"a":"1","b":"x"},{"a":"2","b":"y"}]`},
		{"two files", gcs(`"gs://b/one.csv","gs://b/two.csv"`, ""), `[{"a":"1","b":"x"},{"a":"2","b":"y"},{"a":"3","b":"z"}]`},
		{"a wildcard, a header in each", gcs(`"gs://b/dir/*.csv"`, `,"skipLeadingRows":"1"`),
			`[{"a":"4","b":"p"},{"a":"5","b":"q"},{"a":"6","b":"r"}]`},
		{"tab-delimited", gcs(`"gs://b/tab.tsv"`, `,"fieldDelimiter":"\t"`), `[{"a":"7","b":"t"}]`},
	} {
		emu := &uploadRecorder{}
		code, got := do(t, Wrap(emu, WithStorage(srv.URL)), "POST", base+"/jobs", c.job)
		if code != 200 || emu.loads != 1 {
			t.Errorf("%s: %d %v, %d loads", c.name, code, got, emu.loads)
			continue
		}
		if rows, _ := json.Marshal(emu.rows); string(rows) != c.want {
			t.Errorf("%s: loaded %s, want %s", c.name, rows, c.want)
		}
		if emu.paths[0] != "/upload/bigquery/v2/projects/p/jobs?uploadType=multipart" {
			t.Errorf("%s: sent to %s", c.name, emu.paths[0])
		}
	}

	// With autodetect and no schema, into a new table: the emulator takes
	// the first file's header; the later files' are dropped.
	emu := &uploadRecorder{}
	job := `{"jobReference":{"projectId":"p","jobId":"j1"},"configuration":{"load":{"sourceFormat":"CSV","autodetect":true,` +
		`"sourceUris":["gs://b/auto*"],"destinationTable":{"datasetId":"ds","tableId":"t"}}}}`
	auto := &autodetectRecorder{uploadRecorder: emu}
	if code, got := do(t, Wrap(auto, WithStorage(srv.URL)), "POST", base+"/jobs", job); code != 200 {
		t.Errorf("autodetect: %d %v", code, got)
	} else if rows, _ := json.Marshal(emu.rows); string(rows) != `[{"a":"8","b":"s"},{"a":"9","b":"u"}]` {
		t.Errorf("autodetect: loaded %s", rows)
	}

	for _, c := range []struct {
		name, uris string
		code       int
	}{
		{"a missing object", `"gs://b/none.csv"`, 404},
		{"a missing bucket", `"gs://nobucket/one.csv"`, 404},
		{"a wildcard that matches nothing", `"gs://b/nothing*"`, 404},
		{"two wildcards", `"gs://b/*/*.csv"`, 400},
		{"not gs://", `"s3://b/one.csv"`, 400},
		{"one of two missing", `"gs://b/one.csv","gs://b/none.csv"`, 404},
	} {
		emu := &uploadRecorder{}
		st.reads = nil
		if code, got := do(t, Wrap(emu, WithStorage(srv.URL)), "POST", base+"/jobs", gcs(c.uris, "")); code != c.code || emu.loads != 0 || len(st.reads) != 0 {
			t.Errorf("%s: %d %v, %d loads, read %v; want %d and nothing read", c.name, code, got, emu.loads, st.reads, c.code)
		}
	}

	// Cloud Storage that does not answer: 404, as the objects cannot be
	// found, and nothing is loaded.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	emu = &uploadRecorder{}
	if code, got := do(t, Wrap(emu, WithStorage(dead.URL)), "POST", base+"/jobs", gcs(`"gs://b/one.csv"`, "")); code != 404 || emu.loads != 0 {
		t.Errorf("unreachable Cloud Storage: %d %v, %d loads", code, got, emu.loads)
	}
}

// autodetectRecorder answers a table read 404 until a load has made the
// table, and then with the columns the emulator would detect from the
// test's files.
type autodetectRecorder struct {
	*uploadRecorder
}

func (a *autodetectRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && a.loads > 0 {
		_, _ = io.WriteString(w, `{"schema":{"fields":[{"name":"a","type":"INTEGER"},{"name":"b","type":"STRING"}]}}`)
		return
	}
	if r.Method == http.MethodPost && a.schema == "" {
		a.schema = twoColumns
		defer func() { a.schema = "" }()
	}
	a.uploadRecorder.ServeHTTP(w, r)
}

// TestCreateSchemaOfAnExistingDataset (#946): CREATE SCHEMA of a dataset
// that exists fails as BigQuery fails it; IF NOT EXISTS, another dataset,
// or one an earlier DROP SCHEMA removes, pass; a later statement of a
// script that does so is 501.
func TestCreateSchemaOfAnExistingDataset(t *testing.T) {
	for _, c := range []struct {
		sql    string
		code   int
		reason string
	}{
		{"CREATE SCHEMA taken", 409, "duplicate"},
		{"create schema `p.taken` OPTIONS(description='x')", 409, "duplicate"},
		{"CREATE SCHEMA p.taken; SELECT 1", 409, "duplicate"},
		{"CREATE SCHEMA IF NOT EXISTS taken", 200, ""},
		{"CREATE SCHEMA fresh", 200, ""},
		{"CREATE SCHEMA other.taken", 200, ""},
		{"DROP SCHEMA taken CASCADE; CREATE SCHEMA taken", 501, "notImplemented"}, // #951
		{"SELECT 1; CREATE SCHEMA taken", 501, "notImplemented"},
		{"CREATE SCHEMA fresh; CREATE SCHEMA fresh", 501, "notImplemented"},
		{"CREATE SCHEMA fresh; CREATE SCHEMA IF NOT EXISTS fresh", 200, ""},
		{"SELECT 'CREATE SCHEMA taken'", 200, ""},
	} {
		emu := &fakeEmulator{datasets: map[string]bool{"taken": true}}
		body, _ := json.Marshal(map[string]any{"query": c.sql, "useLegacySql": false})
		code, got := do(t, Wrap(emu), "POST", base+"/queries", string(body))
		if code != c.code {
			t.Errorf("%s: %d %v, want %d", c.sql, code, got, c.code)
			continue
		}
		if c.code != 200 {
			if len(emu.writes) != 0 {
				t.Errorf("%s: sent %v", c.sql, emu.writes)
			}
			errs, _ := got["error"].(map[string]any)["errors"].([]any)
			if len(errs) != 1 || errs[0].(map[string]any)["reason"] != c.reason {
				t.Errorf("%s: %v, want reason %s", c.sql, got, c.reason)
			}
		}
	}

	// A query job: the job is recorded, and fails with duplicate.
	emu := &fakeEmulator{datasets: map[string]bool{"taken": true},
		answer: `{"jobReference":{"projectId":"p","jobId":"q1"},"configuration":{"query":{"query":"x"}},"status":{"state":"DONE"}}`}
	h := Wrap(emu)
	code, got := do(t, h, "POST", base+"/jobs",
		`{"jobReference":{"projectId":"p","jobId":"q1"},"configuration":{"query":{"query":"CREATE SCHEMA taken","useLegacySql":false}}}`)
	st, _ := got["status"].(map[string]any)
	er, _ := st["errorResult"].(map[string]any)
	if code != 200 || er["reason"] != "duplicate" || er["message"] != "Already Exists: Dataset p:taken" {
		t.Errorf("jobs.insert: %d %v, want the job failed duplicate", code, got)
	}
	if len(emu.writes) != 1 || strings.Contains(emu.writes[0], "CREATE SCHEMA") {
		t.Errorf("jobs.insert sent %v, want one statement that does nothing", emu.writes)
	}
	if q := got["configuration"].(map[string]any)["query"].(map[string]any)["query"]; q != "CREATE SCHEMA taken" {
		t.Errorf("the job's query reads %v", q)
	}
}
