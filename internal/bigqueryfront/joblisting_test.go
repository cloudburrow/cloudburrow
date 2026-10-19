package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// listedEmulator is a jobsEmulator whose jobs.list of every job answers
// body, written once, so a test measures what the front allocates and
// not what encoding the list costs the fake engine; listed counts the
// jobs it answered each jobs.list with.
type listedEmulator struct {
	*jobsEmulator
	body   []byte
	listed []int
	lists  []string
}

func (e *listedEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/jobs") {
		e.lists = append(e.lists, r.URL.RawQuery)
		q := r.URL.Query()
		if e.body != nil && !q.Has("cloudburrowAfterJobId") && !q.Has("cloudburrowJobIds") {
			e.listed = append(e.listed, strings.Count(string(e.body), `"jobReference"`))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(e.body)
			return
		}
		rec := httptest.NewRecorder()
		e.jobsEmulator.ServeHTTP(rec, r)
		e.listed = append(e.listed, strings.Count(rec.Body.String(), `"jobReference"`))
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return
	}
	e.jobsEmulator.ServeHTTP(w, r)
}

// manyJobs makes an emulator of n query jobs, each with its
// configuration and times, as the engine lists them (patches 0013, 0022).
func manyJobs(n int) *listedEmulator {
	e := &listedEmulator{jobsEmulator: &jobsEmulator{msTimes: true}}
	for i := 0; i < n; i++ {
		e.record(map[string]any{"configuration": map[string]any{"jobType": "QUERY", "query": map[string]any{
			"query": fmt.Sprintf("SELECT %d AS n, 'some text of the query' AS s", i), "useLegacySql": false}},
			"kind": "bigquery#job", "user_email": "dev@example.com"})
	}
	return e
}

// listBody is the emulator's list of every job, as it answers it.
func (e *listedEmulator) listBody(t *testing.T) []byte {
	t.Helper()
	var list []any
	for _, id := range e.order {
		job := e.jobs[id]
		item := map[string]any{"state": "DONE"}
		for k, v := range job {
			item[k] = v
		}
		list = append(list, item)
	}
	b, err := json.Marshal(map[string]any{"kind": "bigquery#jobList", "jobs": list})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// allocated returns the bytes the heap allocated, on average, for each of
// runs calls of f.
func allocated(runs int, f func()) uint64 {
	f() // warm: the first call fills what the front keeps
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < runs; i++ {
		f()
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / uint64(runs)
}

// TestJobListDecodesOnce (#1639): a jobs.list, and a query of
// INFORMATION_SCHEMA.JOBS that lists every job, decode the engine's list
// once, not once in each layer of the jobs.list path. Measured on 3,000
// jobs (arm64, 2026-10-19), the JOBS query allocated 45.9 times the
// list's JSON when each layer decoded the list again and the query read
// the top layer's answer, and 12.4 times decoding it once; a jobs.list
// page, which pages before the upper layers, 11.8 and 11.6 times.
func TestJobListDecodesOnce(t *testing.T) {
	const n = 3000
	emu := manyJobs(n)
	emu.body = emu.listBody(t)
	h := Wrap(emu)
	size := uint64(len(emu.body))

	page := allocated(5, func() {
		code, got := do(t, h, "GET", base+"/jobs?projection=full&maxResults=50", "")
		if jobs, _ := got["jobs"].([]any); code != 200 || len(jobs) != 50 {
			t.Fatalf("jobs.list: %d, %d jobs", code, len(jobs))
		}
	})
	// No job is a load: the filter keeps none, after reading every one.
	query := allocated(5, func() {
		code, got := do(t, h, "POST", base+"/queries",
			`{"query":"SELECT job_id FROM `+"`region-us`"+`.INFORMATION_SCHEMA.JOBS WHERE job_type = 'LOAD'","useLegacySql":false}`)
		if code != 200 {
			t.Fatalf("JOBS query: %d %v", code, got)
		}
	})
	t.Logf("%d jobs, a list of %d KB: a jobs.list page allocated %d KB (%.1f times), a JOBS query %d KB (%.1f times)",
		n, size/1024, page/1024, float64(page)/float64(size), query/1024, float64(query)/float64(size))
	if page > 16*size {
		t.Errorf("a jobs.list page allocated %d bytes, %.1f times the engine's list of %d: more than one decode of it",
			page, float64(page)/float64(size), size)
	}
	if query > 20*size {
		t.Errorf("a JOBS query allocated %d bytes, %.1f times the engine's list of %d: more than one decode of it",
			query, float64(query)/float64(size), size)
	}
}

// TestJobsQueryByIDListsThoseJobs (#1633): a query of
// INFORMATION_SCHEMA.JOBS that names its jobs (job_id = or IN) lists
// those and the jobs made since the newest the engine listed, not every
// job, and writes the rows a list of every job does.
func TestJobsQueryByIDListsThoseJobs(t *testing.T) {
	emu := manyJobs(2000)
	h := Wrap(emu)
	query := func(sql string) string {
		t.Helper()
		if code, got := do(t, h, "POST", base+"/queries", `{"query":"`+sql+`","useLegacySql":false}`); code != 200 {
			t.Fatalf("%s: %d %v", sql, code, got)
		}
		return emu.sentQuery(t)
	}
	const jobs = "SELECT job_id, state FROM `region-us`.INFORMATION_SCHEMA.JOBS"
	// The first lists every job: no newest job is known yet.
	first := query(jobs + " WHERE job_id = 'auto10'")
	if got := emu.listed[len(emu.listed)-1]; got < 2000 {
		t.Fatalf("the first query listed %d jobs, want every one", got)
	}
	// Jobs made since.
	for i := 0; i < 3; i++ {
		query("SELECT 1")
	}
	for _, c := range []struct {
		where string
		ids   string
		want  []string
	}{
		{" WHERE job_id = 'auto10'", "auto10", []string{"'auto10'"}},
		{" WHERE job_id IN ('auto7', 'auto1999', 'nosuchjob') AND state = 'DONE'", "auto1999,auto7,nosuchjob", []string{"'auto7'", "'auto1999'"}},
		{" WHERE job_id IN ('auto7', 'auto8') AND job_id = 'auto8'", "auto8", []string{"'auto8'"}},
	} {
		from := len(emu.listed)
		got := query(jobs + c.where)
		var listed int
		for _, n := range emu.listed[from:] {
			listed += n
		}
		// The jobs named, and the four made since the first list (three
		// queries and the JOBS query before this one, at most).
		if listed > len(c.want)+5 {
			t.Errorf("%s: the engine listed %d jobs, want those named and those made since (%q)", c.where, listed, emu.lists[from:])
		}
		if q := emu.lists[len(emu.lists)-1]; !strings.Contains(q, "cloudburrowJobIds="+strings.ReplaceAll(c.ids, ",", "%2C")) ||
			!strings.Contains(q, "cloudburrowAfterJobId=auto") {
			t.Errorf("%s: listed with %q, want the IDs named after the newest job", c.where, q)
		}
		for _, id := range c.want {
			if !strings.Contains(got, id) {
				t.Errorf("%s: no row of %s in %s", c.where, id, got)
			}
		}
		if strings.Count(got, "NULL, NULL, 'auto") != len(c.want) {
			t.Errorf("%s: rows of other jobs in %s", c.where, got)
		}
	}
	// The rows are those a list of every job gives.
	again := query(jobs + " WHERE job_id = 'auto10'")
	if again != first {
		t.Errorf("listed by ID, the query was\n%s\nlisted whole\n%s", again, first)
	}
}
