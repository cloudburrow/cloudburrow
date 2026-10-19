package bigqueryfront

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
)

// The pushed-down state and parent_job_id filters of INFORMATION_SCHEMA.JOBS
// (jobsopen.go, #1633): the rows are those a list of every job gives, and
// the engine lists the jobs that can match and those made since, not
// every job.

const openJobsSelect = "SELECT job_id, state, parent_job_id FROM `region-us`.INFORMATION_SCHEMA.JOBS"

// setState gives the emulator's job id the state.
func (e *listedEmulator) setState(id, state string) {
	e.jobs[id]["status"] = map[string]any{"state": state}
}

// sendJobs sends sql to h, and returns the query the front sent the engine
// and how many jobs the engine listed for it.
func sendJobs(t *testing.T, h http.Handler, emu *listedEmulator, sql string) (string, int) {
	t.Helper()
	from := len(emu.listed)
	if code, got := do(t, h, "POST", base+"/queries", `{"query":"`+sql+`","useLegacySql":false}`); code != 200 {
		t.Fatalf("%s: %d %v", sql, code, got)
	}
	n := 0
	for _, c := range emu.listed[from:] {
		n += c
	}
	return emu.sentQuery(t), n
}

// wantRows checks that the query h sent for sql is the one a front that
// lists every job sends (a new front, which knows no cursor), and that the
// engine listed at most most jobs for it.
func wantRows(t *testing.T, h http.Handler, emu *listedEmulator, sql string, most int, records *jobRecords) string {
	t.Helper()
	got, listed := sendJobs(t, h, emu, sql)
	if listed > most {
		t.Errorf("%s: the engine listed %d jobs, want %d at most", sql, listed, most)
	}
	fresh := Wrap(emu, func(o *options) { o.records = records })
	want, all := sendJobs(t, fresh, emu, sql)
	if all < 1000 {
		t.Fatalf("%s: the reference listed %d jobs, not every one", sql, all)
	}
	if got != want {
		t.Errorf("%s: pushed down, the query was\n%s\nlisted whole\n%s", sql, got, want)
	}
	return got
}

func TestJobsQueryOfStateListsOpenJobs(t *testing.T) {
	emu := manyJobs(2000)
	emu.setState("auto5", "RUNNING")
	emu.setState("auto1500", "PENDING")
	dir := t.TempDir()
	records := &jobRecords{}
	wrap := func() http.Handler {
		return Wrap(emu, func(o *options) { o.stateDir = dir; o.records = records })
	}
	h := wrap()
	running := openJobsSelect + " WHERE state = 'RUNNING'"
	// The first lists every job: nothing is kept yet.
	if _, n := sendJobs(t, h, emu, running); n < 2000 {
		t.Fatalf("the first query listed %d jobs, want every one", n)
	}
	// Jobs made since, one of them running; one running before is DONE now.
	for i := 0; i < 3; i++ {
		sendJobs(t, h, emu, "SELECT 1")
	}
	emu.setState(emu.order[len(emu.order)-2], "RUNNING")
	emu.setState("auto1500", "DONE")
	// Those kept (2), the cursor, and the jobs made since (3 queries,
	// the JOBS query, and the reference's queries of each check).
	got := wantRows(t, h, emu, running, 12, records)
	if !strings.Contains(got, "'auto5'") || strings.Contains(got, "'auto1500'") {
		t.Errorf("rows %s, want auto5's and not auto1500's", got)
	}
	wantRows(t, h, emu, openJobsSelect+" WHERE state IN ('PENDING', 'RUNNING')", 12, records)
	wantRows(t, h, emu, "SELECT job_id FROM `region-us`.INFORMATION_SCHEMA.JOBS_BY_PROJECT WHERE state = 'PENDING'", 12, records)
	wantRows(t, h, emu, "SELECT job_id FROM `region-us`.INFORMATION_SCHEMA.JOBS_BY_USER WHERE state = 'RUNNING' AND job_type = 'QUERY'", 12, records)

	// A restart of the front: what it kept is in its state directory.
	emu.setState("auto5", "DONE")
	h = wrap()
	got = wantRows(t, h, emu, running, 12, records)
	if strings.Contains(got, "'auto5'") {
		t.Errorf("rows %s, want none of auto5, DONE", got)
	}

	// A client's jobs.list of every job makes it anew.
	emu.setState("auto11", "RUNNING")
	if code, _ := do(t, h, "GET", base+"/jobs?allUsers=true", ""); code != 200 {
		t.Fatalf("jobs.list: %d", code)
	}
	got = wantRows(t, h, emu, running, 12, records)
	if !strings.Contains(got, "'auto11'") {
		t.Errorf("rows %s, want auto11's", got)
	}
	// A job gone from the engine is no longer named.
	delete(emu.jobs, "auto11")
	emu.order = slices.DeleteFunc(emu.order, func(id string) bool { return id == "auto11" })
	wantRows(t, h, emu, running, 12, records)
	if b, err := os.ReadFile(jobsOpenStateFile(dir)); err != nil || strings.Contains(string(b), "auto11") || !strings.Contains(string(b), "auto2") {
		t.Errorf("kept %s (%v), want no auto11", b, err)
	}
}

// TestJobsQueryOfStateAfterEngineRestart: the engine restarted while the
// front was down, empty, and made jobs again; the cursor the front kept is
// no job of it, or another job of the same ID, and the query lists every
// job.
func TestJobsQueryOfStateAfterEngineRestart(t *testing.T) {
	running := openJobsSelect + " WHERE state = 'RUNNING'"
	for _, c := range []struct {
		name string
		jobs int
	}{
		{"no job of the cursor's ID", 1000},
		{"another job of the cursor's ID", 1600},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			emu := manyJobs(1500)
			h := Wrap(emu, func(o *options) { o.stateDir = dir })
			sendJobs(t, h, emu, running)
			if _, n := sendJobs(t, h, emu, running); n > 5 {
				t.Fatalf("the second query listed %d jobs", n)
			}
			again := manyJobs(c.jobs)
			for _, id := range again.order {
				stats := again.jobs[id]["statistics"].(map[string]any)
				stats["creationTime"] = fmt.Sprint(1800000000000 + len(id)) // made after the restart
			}
			again.setState("auto3", "RUNNING") // before the cursor, and never listed
			h = Wrap(again, func(o *options) { o.stateDir = dir })
			got, n := sendJobs(t, h, again, running)
			if !strings.Contains(got, "'auto3'") {
				t.Errorf("rows %s, want auto3's", got)
			}
			if n < c.jobs {
				t.Errorf("listed %d jobs, want every one (%d)", n, c.jobs)
			}
		})
	}
}

// TestJobsQueryOfParentListsChildren: parent_job_id = lists the children the
// front recorded of the parent, and the jobs made since the newest listed.
func TestJobsQueryOfParentListsChildren(t *testing.T) {
	emu := manyJobs(2000)
	records := &jobRecords{}
	h := Wrap(emu, func(o *options) { o.records = records })
	adopt := func(parent string, kids ...string) {
		rec := newRecorder()
		writeJSON(rec, 200, map[string]any{"jobReference": map[string]any{"projectId": "p", "jobId": parent}})
		var children []scriptChild
		for _, k := range kids {
			children = append(children, scriptChild{project: "p", id: k, kind: "STATEMENT", text: "SELECT 1"})
		}
		records.adoptScript(httptest.NewRecorder(), rec, children, false, "p")
	}
	adopt("auto100", "auto98", "auto99")
	adopt("auto200", "auto197", "auto198", "auto199")
	// The first lists every job: no newest job is known yet.
	if _, n := sendJobs(t, h, emu, openJobsSelect+" WHERE parent_job_id = 'auto100'"); n < 2000 {
		t.Fatalf("the first query listed %d jobs, want every one", n)
	}
	sendJobs(t, h, emu, "SELECT 1")
	got := wantRows(t, h, emu, openJobsSelect+" WHERE parent_job_id = 'auto100'", 8, records)
	if !strings.Contains(got, "'auto98'") || !strings.Contains(got, "'auto99'") || strings.Contains(got, "'auto197'") {
		t.Errorf("rows %s, want auto100's children alone", got)
	}
	wantRows(t, h, emu, openJobsSelect+" WHERE parent_job_id IN ('auto100', 'auto200') AND state = 'DONE'", 10, records)
	wantRows(t, h, emu, openJobsSelect+" WHERE parent_job_id = 'nosuchjob'", 6, records)
	// With state too: the children, the narrower.
	emu.setState("auto199", "RUNNING")
	wantRows(t, h, emu, openJobsSelect+" WHERE parent_job_id = 'auto200' AND state = 'RUNNING'", 10, records)
}

// TestOpenJobsForget: a purge, and the emulator's restart, forget what is
// kept, and the file keeps that.
func TestOpenJobsForget(t *testing.T) {
	dir := t.TempDir()
	o := &openJobs{}
	o.keep(jobsOpenStateFile(dir), nil)
	job := func(id, state string) any {
		return map[string]any{"jobReference": map[string]any{"jobId": id}, "status": map[string]any{"state": state},
			"statistics": map[string]any{"creationTime": "1"}}
	}
	o.listed("p", nil, []any{job("a", "RUNNING"), job("b", "DONE")}, false)
	o.listed("q", nil, []any{job("c", "DONE")}, false)
	again := &openJobs{}
	again.keep(jobsOpenStateFile(dir), nil)
	if l, ok := again.listing("p"); !ok || l.after != "b" || strings.Join(l.ids, ",") != "a,b" {
		t.Fatalf("restored listing %+v %v, want after b of a and b", l, ok)
	}
	again.forget("p")
	if _, ok := again.listing("p"); ok {
		t.Error("p kept after forget")
	}
	again.reset()
	third := &openJobs{}
	third.keep(jobsOpenStateFile(dir), nil)
	if _, ok := third.listing("q"); ok {
		t.Error("q kept after reset")
	}
}
