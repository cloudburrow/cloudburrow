package bigqueryfront

import (
	"bytes"
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

// The tests in this file are #960.

// jobsEmulator answers jobs.get of j1 and jobs.list as the emulator does,
// with no statistics and status DONE, and hands the rest to next.
func jobsEmulator(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+base+"/jobs/j1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"jobReference":{"projectId":"p","jobId":"j1"},"status":{"state":"DONE"}}`)
	})
	mux.HandleFunc("GET "+base+"/jobs", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"jobs":[{"jobReference":{"projectId":"p","jobId":"j1"},"state":"DONE"},`+
			`{"jobReference":{"projectId":"p","jobId":"other"},"state":"DONE"}]}`)
	})
	mux.Handle("/", next)
	return mux
}

// loadReport reads a Job's statistics.load and status.errors.
func loadReport(job map[string]any) (load map[string]any, errs []map[string]any) {
	stats, _ := job["statistics"].(map[string]any)
	load, _ = stats["load"].(map[string]any)
	status, _ := job["status"].(map[string]any)
	list, _ := status["errors"].([]any)
	for _, e := range list {
		m, _ := e.(map[string]any)
		errs = append(errs, m)
	}
	return load, errs
}

// TestCSVLoadReportsBadRecords (#960): a CSV load the front left bad
// records out of reports them in statistics.load.badRecords and
// status.errors, with its output rows, input files and input bytes, in
// the jobs.insert answer, jobs.get and jobs.list.
func TestCSVLoadReportsBadRecords(t *testing.T) {
	const data = "1,x\n2\n3,y,z\n4,w\n"
	emu := &csvEmulator{}
	h := Wrap(jobsEmulator(emu))
	w := upload(t, h, loadJob(`,"maxBadRecords":2`), data)
	if w.Code != 200 {
		t.Fatalf("load: %d %s", w.Code, w.Body)
	}
	var inserted map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &inserted); err != nil {
		t.Fatalf("jobs.insert answer: %v", err)
	}
	_, got := do(t, h, "GET", base+"/jobs/j1", "")
	_, list := do(t, h, "GET", base+"/jobs", "")
	jobs, _ := list["jobs"].([]any)
	if len(jobs) != 2 {
		t.Fatalf("jobs.list: %v", list)
	}
	listed, _ := jobs[0].(map[string]any)
	for what, job := range map[string]map[string]any{"jobs.insert": inserted, "jobs.get": got, "jobs.list": listed} {
		load, errs := loadReport(job)
		want := map[string]any{"badRecords": "2", "outputRows": "2", "inputFiles": "1", "inputFileBytes": fmt.Sprint(len(data))}
		for k, v := range want {
			if load[k] != v {
				t.Errorf("%s: statistics.load.%s = %v, want %v (%v)", what, k, load[k], v, job)
			}
		}
		if _, ok := load["outputBytes"]; ok {
			t.Errorf("%s: outputBytes reported: %v", what, load)
		}
		if len(errs) != 2 || errs[0]["reason"] != "invalid" || !strings.Contains(fmt.Sprint(errs[0]["message"]), "Line 2") ||
			!strings.Contains(fmt.Sprint(errs[1]["message"]), "Line 3") {
			t.Errorf("%s: status.errors = %v", what, errs)
		}
		status, _ := job["status"].(map[string]any)
		if status["errorResult"] != nil {
			t.Errorf("%s: the load failed: %v", what, status)
		}
	}
	other, _ := jobs[1].(map[string]any)
	if other["statistics"] != nil {
		t.Errorf("jobs.list: another job was given statistics: %v", other)
	}
}

// TestCSVLoadReportsNoBadRecords (#960): a load with none reports 0 and
// no errors; ignoreUnknownValues trims values and leaves out no record.
func TestCSVLoadReportsNoBadRecords(t *testing.T) {
	for _, c := range []struct {
		name, extra, data, rows string
	}{
		{"maxBadRecords, none bad", `,"maxBadRecords":5`, "1,x\n2,y\n", "2"},
		{"ignoreUnknownValues", `,"ignoreUnknownValues":true`, "1,x,extra\n2,y\n3,z,more,still\n", "3"},
		{"skipLeadingRows", `,"skipLeadingRows":1`, "a,b\n1,x\n", "1"},
	} {
		h := Wrap(jobsEmulator(&csvEmulator{}))
		w := upload(t, h, loadJob(c.extra), c.data)
		if w.Code != 200 {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
			continue
		}
		// The emulator's answer has no status; the Go client reads the
		// statistics only with one.
		var inserted map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &inserted)
		if st, _ := inserted["status"].(map[string]any); st["state"] != "DONE" {
			t.Errorf("%s: jobs.insert: %v, want status DONE", c.name, inserted)
		}
		_, job := do(t, h, "GET", base+"/jobs/j1", "")
		load, errs := loadReport(job)
		if load["badRecords"] != "0" || load["outputRows"] != c.rows || load["inputFileBytes"] != fmt.Sprint(len(c.data)) || len(errs) != 0 {
			t.Errorf("%s: jobs.get: %v", c.name, job)
		}
	}
}

// TestCSVLoadBadRecordsListed (#960): at most maxListedBadRecords bad
// records are listed in status.errors; badRecords counts them all.
func TestCSVLoadBadRecordsListed(t *testing.T) {
	var b strings.Builder
	const bad = maxListedBadRecords + 20
	for i := 0; i < bad; i++ {
		b.WriteString("1\n")
	}
	b.WriteString("2,y\n")
	h := Wrap(jobsEmulator(&csvEmulator{}))
	if w := upload(t, h, loadJob(fmt.Sprintf(`,"maxBadRecords":%d`, bad)), b.String()); w.Code != 200 {
		t.Fatalf("load: %d %s", w.Code, w.Body)
	}
	_, job := do(t, h, "GET", base+"/jobs/j1", "")
	load, errs := loadReport(job)
	if load["badRecords"] != fmt.Sprint(bad) || load["outputRows"] != "1" || len(errs) != maxListedBadRecords {
		t.Errorf("jobs.get: badRecords %v, outputRows %v, %d errors", load["badRecords"], load["outputRows"], len(errs))
	}
}

// TestCSVLoadFromCloudStorageReportsBadRecords (#960): a load from gs://
// URIs counts each file, and each bad record is located at its file.
func TestCSVLoadFromCloudStorageReportsBadRecords(t *testing.T) {
	st := &fakeStorage{objects: map[string]string{"b/one.csv": "1,x\n2\n", "b/two.csv": "3,y\n4,z,q\n5,w\n"}}
	srv := httptest.NewServer(st)
	defer srv.Close()
	job := `{"jobReference":{"projectId":"p","jobId":"j1"},"configuration":{"load":{"sourceUris":["gs://b/one.csv","gs://b/two.csv"],` +
		`"destinationTable":{"datasetId":"ds","tableId":"t"},"schema":` + twoColumns + `,"maxBadRecords":2}}}`
	h := Wrap(jobsEmulator(&uploadRecorder{}), WithStorage(srv.URL))
	if code, got := do(t, h, "POST", base+"/jobs", job); code != 200 {
		t.Fatalf("load: %d %v", code, got)
	}
	_, got := do(t, h, "GET", base+"/jobs/j1", "")
	load, errs := loadReport(got)
	if load["badRecords"] != "2" || load["outputRows"] != "3" || load["inputFiles"] != "2" ||
		load["inputFileBytes"] != fmt.Sprint(len(st.objects["b/one.csv"])+len(st.objects["b/two.csv"])) {
		t.Errorf("jobs.get: statistics.load %v", load)
	}
	if len(errs) != 2 || errs[0]["location"] != "gs://b/one.csv" || errs[1]["location"] != "gs://b/two.csv" ||
		!strings.Contains(fmt.Sprint(errs[1]["message"]), "Line 2") {
		t.Errorf("jobs.get: status.errors %v", errs)
	}

	// With autodetect, the first file's header is not a row loaded.
	st.objects["b/auto.csv"], st.objects["b/auto2.csv"] = "a,b\n8,s\n", "a,b\n9,u\n"
	auto := `{"jobReference":{"projectId":"p","jobId":"j1"},"configuration":{"load":{"sourceFormat":"CSV","autodetect":true,` +
		`"sourceUris":["gs://b/auto*"],"destinationTable":{"datasetId":"ds","tableId":"t"},"maxBadRecords":1}}}`
	emu := &uploadRecorder{}
	h = Wrap(jobsEmulator(&autodetectRecorder{uploadRecorder: emu}), WithStorage(srv.URL))
	if code, got := do(t, h, "POST", base+"/jobs", auto); code != 200 || len(emu.rows) != 2 {
		t.Fatalf("autodetect load: %d %v, %d rows", code, got, len(emu.rows))
	}
	_, got = do(t, h, "GET", base+"/jobs/j1", "")
	if load, _ := loadReport(got); load["outputRows"] != "2" || load["inputFiles"] != "2" || load["badRecords"] != "0" {
		t.Errorf("autodetect: jobs.get: statistics.load %v", load)
	}
}

// TestCSVLoadReportForgotten (#960): a load of the same job ID that the
// front does not count, and a failed one, are not given another load's
// counts.
func TestCSVLoadReportForgotten(t *testing.T) {
	h := Wrap(jobsEmulator(&csvEmulator{}))
	if w := upload(t, h, loadJob(`,"maxBadRecords":1`), "1,x\n2\n"); w.Code != 200 {
		t.Fatalf("load: %d %s", w.Code, w.Body)
	}
	// Plain: allowQuotedNewlines and preserveAsciiControlCharacters, no
	// other option, so the front passes the records on unread.
	if w := upload(t, h, loadJob(`,"allowQuotedNewlines":true,"preserveAsciiControlCharacters":true`), "1,x\n"); w.Code != 200 {
		t.Fatalf("plain load: %d %s", w.Code, w.Body)
	}
	// Its records are not read: it has its own bytes, and no bad records
	// or errors of the first load's (#966).
	_, plain := do(t, h, "GET", base+"/jobs/j1", "")
	if load, errs := loadReport(plain); load["badRecords"] != nil || load["inputFileBytes"] != "4" || len(errs) != 0 {
		t.Errorf("jobs.get after a load whose records were not read: %v", plain)
	}

	h = Wrap(jobsEmulator(&csvEmulator{}))
	if w := upload(t, h, loadJob(`,"maxBadRecords":1`), "1\n2\n3,x\n"); w.Code != 400 {
		t.Fatalf("too many bad records: %d %s", w.Code, w.Body)
	}
	if _, job := do(t, h, "GET", base+"/jobs/j1", ""); job["statistics"] != nil {
		t.Errorf("jobs.get of a failed load: %v", job)
	}
}

// rowsEmulator is the emulator for a load whose data the front does not
// read (#966): it counts the table's rows, answers tables.get with them in
// numRows (left out at 0, as the emulator leaves it out), loads an
// upload's JSON lines (all of them, or none: the emulator fails a load on
// a bad one), and a load from Cloud Storage with gsRows rows. A load into
// a table with a column "fail" fails, 400.
type rowsEmulator struct {
	mu     sync.Mutex
	rows   int64
	exists bool
	gsRows int64
	jobs   int
}

func (e *rowsEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r.Method == http.MethodGet {
		if !e.exists {
			http.Error(w, `{"error":{"code":404,"message":"not found"}}`, http.StatusNotFound)
			return
		}
		if e.rows == 0 {
			_, _ = io.WriteString(w, `{"schema":`+twoColumns+`}`)
			return
		}
		fmt.Fprintf(w, `{"schema":%s,"numRows":"%d"}`, twoColumns, e.rows)
		return
	}
	var job jobBody
	var data []byte
	if _, params, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && params["boundary"] != "" {
		mr := multipart.NewReader(r.Body, params["boundary"])
		p, err := mr.NextPart()
		if err == nil {
			err = json.NewDecoder(p).Decode(&job)
		}
		if err == nil {
			p, err = mr.NextPart()
		}
		if err == nil {
			data, err = io.ReadAll(p)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	l := job.Configuration.Load
	if bytes.Contains(data, []byte(`"fail"`)) {
		http.Error(w, `{"error":{"code":400,"message":"bad value"}}`, http.StatusBadRequest)
		return
	}
	if l.WriteDisposition == "WRITE_TRUNCATE" {
		e.rows = 0
	}
	if len(l.SourceURIs) > 0 {
		e.rows += e.gsRows
	} else {
		e.rows += int64(bytes.Count(data, []byte("\n")))
	}
	e.exists = true
	e.jobs++
	ref := `{"projectId":"p","jobId":"j1"}`
	if len(l.SourceURIs) > 0 {
		fmt.Fprintf(w, `{"jobReference":%s,"status":{"state":"DONE"},"statistics":{"creationTime":"1"}}`, ref)
		return
	}
	fmt.Fprintf(w, `{"jobReference":%s}`, ref)
}

func jsonLoad(extra string) string {
	return `{"jobReference":{"projectId":"p","jobId":"j1"},"configuration":{"load":{"sourceFormat":"NEWLINE_DELIMITED_JSON",` +
		`"destinationTable":{"datasetId":"ds","tableId":"t"}` + extra + `}}}`
}

// loadStats returns statistics.load of the jobs.insert answer w, of
// jobs.get and of jobs.list, each with its status.
func loadStats(t *testing.T, h http.Handler, inserted []byte) map[string]map[string]any {
	t.Helper()
	var ins map[string]any
	if err := json.Unmarshal(inserted, &ins); err != nil {
		t.Fatalf("jobs.insert answer: %v", err)
	}
	_, got := do(t, h, "GET", base+"/jobs/j1", "")
	_, list := do(t, h, "GET", base+"/jobs", "")
	jobs, _ := list["jobs"].([]any)
	listed := map[string]any{}
	if len(jobs) > 0 {
		listed, _ = jobs[0].(map[string]any)
	}
	out := map[string]map[string]any{}
	for what, job := range map[string]map[string]any{"jobs.insert": ins, "jobs.get": got, "jobs.list": listed} {
		load, _ := loadReport(job)
		if st, _ := job["status"].(map[string]any); st["state"] != "DONE" || st["errorResult"] != nil {
			t.Errorf("%s: status %v, want DONE", what, job["status"])
		}
		out[what] = load
	}
	return out
}

// TestJSONLoadReportsCounts (#966): an upload whose records the front does
// not read reports its rows, counted in the table before and after it
// (all of them after a WRITE_TRUNCATE), and its data's bytes as one file,
// and no badRecords, in the jobs.insert answer, jobs.get and jobs.list.
// The body reaches the emulator as it was sent.
func TestJSONLoadReportsCounts(t *testing.T) {
	const data = "{\"a\":1}\n{\"a\":2}\n{\"a\":3}\n"
	for _, c := range []struct {
		name, extra string
		before      int64
		exists      bool
		rows        string
	}{
		{"new table", "", 0, false, "3"},
		{"append", "", 5, true, "3"},
		{"truncate", `,"writeDisposition":"WRITE_TRUNCATE"`, 5, true, "3"},
		{"empty table", "", 0, true, "3"},
	} {
		emu := &rowsEmulator{rows: c.before, exists: c.exists}
		h := Wrap(jobsEmulator(emu))
		w := upload(t, h, jsonLoad(c.extra), data)
		if w.Code != 200 {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
			continue
		}
		for what, load := range loadStats(t, h, w.Body.Bytes()) {
			want := map[string]any{"outputRows": c.rows, "inputFiles": "1", "inputFileBytes": fmt.Sprint(len(data))}
			for k, v := range want {
				if load[k] != v {
					t.Errorf("%s: %s: statistics.load.%s = %v, want %v", c.name, what, k, load[k], v)
				}
			}
			for _, k := range []string{"badRecords", "outputBytes"} {
				if _, ok := load[k]; ok {
					t.Errorf("%s: %s: statistics.load.%s reported: %v", c.name, what, k, load)
				}
			}
		}
	}

	// A load that fails reports no counts.
	emu := &rowsEmulator{exists: true}
	h := Wrap(jobsEmulator(emu))
	if w := upload(t, h, jsonLoad(""), "{\"a\":\"fail\"}\n"); w.Code != 400 {
		t.Fatalf("a failing load: %d %s", w.Code, w.Body)
	}
	if _, job := do(t, h, "GET", base+"/jobs/j1", ""); job["statistics"] != nil {
		t.Errorf("jobs.get of a failed load: %v", job)
	}

	// A load into another project's table has no outputRows: the front
	// reads the tables of the project in the path.
	emu = &rowsEmulator{exists: true}
	h = Wrap(jobsEmulator(emu))
	other := strings.Replace(jsonLoad(""), `"destinationTable":{`, `"destinationTable":{"projectId":"other",`, 1)
	w := upload(t, h, other, data)
	if w.Code != 200 {
		t.Fatalf("a load into another project: %d %s", w.Code, w.Body)
	}
	if load := loadStats(t, h, w.Body.Bytes())["jobs.get"]; load["outputRows"] != nil || load["inputFileBytes"] != fmt.Sprint(len(data)) {
		t.Errorf("a load into another project: statistics.load %v", load)
	}
}

// TestCloudStorageLoadReportsCounts (#966): a load from Cloud Storage that
// the emulator reads itself reports its rows, and, with the front given
// the instance's Cloud Storage, the objects the emulator reads and their
// sizes: a wildcard matches as the emulator matches it. With no Cloud
// Storage, the files and bytes are left out.
func TestCloudStorageLoadReportsCounts(t *testing.T) {
	st := &fakeStorage{objects: map[string]string{"b/d/one.json": "{}\n", "b/d/two.json": "{}\n{}\n", "b/d/x.csv": "1\n"}}
	srv := httptest.NewServer(st)
	defer srv.Close()
	job := `{"jobReference":{"projectId":"p","jobId":"j1"},"configuration":{"load":{"sourceFormat":"NEWLINE_DELIMITED_JSON",` +
		`"sourceUris":["gs://b/d/*.json","gs://b/d/x.csv"],"destinationTable":{"datasetId":"ds","tableId":"t"}}}}`
	for _, c := range []struct {
		name  string
		opts  []Option
		files any
		bytes any
	}{
		{"with Cloud Storage", []Option{WithStorage(srv.URL)}, "3", fmt.Sprint(3 + 6 + 2)},
		{"without Cloud Storage", nil, nil, nil},
	} {
		emu := &rowsEmulator{rows: 1, exists: true, gsRows: 4}
		h := Wrap(jobsEmulator(emu), c.opts...)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", base+"/jobs", strings.NewReader(job)))
		if w.Code != 200 {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
			continue
		}
		for what, load := range loadStats(t, h, w.Body.Bytes()) {
			if load["outputRows"] != "4" || load["inputFiles"] != c.files || load["inputFileBytes"] != c.bytes || load["badRecords"] != nil {
				t.Errorf("%s: %s: statistics.load %v, want 4 rows, %v files of %v bytes", c.name, what, load, c.files, c.bytes)
			}
		}
		if len(st.reads) != 0 {
			t.Errorf("%s: the front read the objects: %v", c.name, st.reads)
		}
	}
}
