package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	if _, job := do(t, h, "GET", base+"/jobs/j1", ""); job["statistics"] != nil {
		t.Errorf("jobs.get after a load that was not counted: %v", job)
	}

	h = Wrap(jobsEmulator(&csvEmulator{}))
	if w := upload(t, h, loadJob(`,"maxBadRecords":1`), "1\n2\n3,x\n"); w.Code != 400 {
		t.Fatalf("too many bad records: %d %s", w.Code, w.Body)
	}
	if _, job := do(t, h, "GET", base+"/jobs/j1", ""); job["statistics"] != nil {
		t.Errorf("jobs.get of a failed load: %v", job)
	}
}
