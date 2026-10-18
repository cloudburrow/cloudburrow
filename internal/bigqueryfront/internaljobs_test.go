package bigqueryfront

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTheFrontsOwnJobsStayUnlistedAfterARestart (#1555): the front's own
// queries are named as its own on the engine (internaljobs.go), so a new
// front, which has no record of them, leaves them out of jobs.list too,
// and answers a client's read of one 404.
func TestTheFrontsOwnJobsStayUnlistedAfterARestart(t *testing.T) {
	emu := &jobsEmulator{run: func(q string) (int, string) {
		return 200, `{"jobComplete":true,"schema":{"fields":[{"name":"a","type":"INTEGER"}]}}`
	}}
	h := Wrap(emu)
	// A CREATE TABLE ... AS SELECT's columns are read with a query of the
	// front's own.
	if code, got := do(t, h, "POST", base+"/jobs", `{"jobReference":{"jobId":"ctas"},"configuration":{"query":{"query":"CREATE TABLE ds.x AS SELECT 1 AS a","useLegacySql":false}}}`); code != 200 {
		t.Fatalf("CREATE TABLE AS SELECT: %d %v", code, got)
	}
	var own []string
	for _, id := range emu.order {
		if id != "ctas" {
			own = append(own, id)
		}
	}
	if len(own) == 0 {
		t.Fatalf("the front ran no query of its own: %v", emu.log)
	}
	for _, id := range own {
		if !strings.HasPrefix(id, internalJobPrefix) {
			t.Errorf("the front's own job %q is not named as its own", id)
		}
	}
	listed := func(h http.Handler, query string) []string {
		t.Helper()
		_, list := do(t, h, "GET", base+"/jobs"+query, "")
		var ids []string
		jobs, _ := list["jobs"].([]any)
		for _, j := range jobs {
			ref, _ := j.(map[string]any)["jobReference"].(map[string]any)
			id, _ := ref["jobId"].(string)
			ids = append(ids, id)
		}
		return ids
	}
	for _, front := range []struct {
		name string
		h    http.Handler
	}{{"the front", h}, {"a restarted front", Wrap(emu)}} {
		for _, q := range []string{"", "?projection=full", "?allUsers=true&projection=full"} {
			if got := listed(front.h, q); len(got) != 1 || got[0] != "ctas" {
				t.Errorf("%s: jobs.list%s: %v, want [ctas]", front.name, q, got)
			}
		}
		for _, req := range []struct{ method, path string }{
			{"GET", "/jobs/" + own[0]},
			{"GET", "/jobs/" + own[0] + "?location=EU"},
			{"GET", "/queries/" + own[0]},
			{"POST", "/jobs/" + own[0] + "/cancel"},
			{"DELETE", "/jobs/" + own[0] + "/delete"},
		} {
			code, got := do(t, front.h, req.method, base+req.path, "")
			if code != 404 {
				t.Errorf("%s: %s %s: %d %v, want 404", front.name, req.method, req.path, code, got)
			}
		}
		if code, got := do(t, front.h, "GET", base+"/jobs/ctas", ""); code != 200 {
			t.Errorf("%s: jobs.get of the client's job: %d %v", front.name, code, got)
		}
	}

	// A client may not name a job as the front's.
	n := len(emu.order)
	if code, got := do(t, h, "POST", base+"/jobs", `{"jobReference":{"jobId":"`+internalJobPrefix+`mine"},"configuration":{"query":{"query":"SELECT 1","useLegacySql":false}}}`); code != 400 {
		t.Errorf("jobs.insert of a reserved job ID: %d %v, want 400", code, got)
	}
	if len(emu.order) != n {
		t.Errorf("a refused jobs.insert reached the engine: %v", emu.order[n:])
	}
}

// TestJobIDPrefixHeader (#1555): only the front's own requests reach the
// engine with the job ID prefix; a client's header is dropped.
func TestJobIDPrefixHeader(t *testing.T) {
	var got []string
	h := JobIDPrefix(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get(jobIDPrefixHeader))
	}))
	client := httptest.NewRequest("POST", base+"/queries", nil)
	client.Header.Set(jobIDPrefixHeader, "_cloudburrow_")
	h.ServeHTTP(httptest.NewRecorder(), client)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", base+"/queries", nil))
	own := httptest.NewRequest("POST", base+"/queries", nil)
	h.ServeHTTP(httptest.NewRecorder(), own.WithContext(asInternal(own.Context())))
	if len(got) != 3 || got[0] != "" || got[1] != "" || got[2] != internalJobPrefix {
		t.Errorf("prefixes sent: %q, want a client's dropped and the front's %q", got, internalJobPrefix)
	}
	if !isInternalJobID(jobIDFor(asInternal(own.Context()))) || isInternalJobID(jobIDFor(client.Context())) {
		t.Error("jobIDFor: the front's job IDs are its own, a client's not")
	}
}

// TestTheFrontDeletesItsOwnFinishedJobs (#1609): the engine kept every job
// of the front's own, eight for each of a client's in the soak (every
// streamed insertAll is two), though no client can read one. Each that is
// complete and fully read is deleted on the engine; a client's job, and one
// whose rows are paged, are kept.
func TestTheFrontDeletesItsOwnFinishedJobs(t *testing.T) {
	for _, c := range []struct {
		name   string
		answer string
		kept   bool
	}{
		{"complete", `{"jobComplete":true,"schema":{"fields":[{"name":"a","type":"INTEGER"}]}}`, false},
		{"paged", `{"jobComplete":true,"pageToken":"next","schema":{"fields":[{"name":"a","type":"INTEGER"}]}}`, true},
		{"running", `{"jobComplete":false}`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			emu := &jobsEmulator{run: func(q string) (int, string) { return 200, c.answer }}
			h := Wrap(emu)
			do(t, h, "POST", base+"/jobs", `{"jobReference":{"jobId":"ctas"},"configuration":{"query":{"query":"CREATE TABLE ds.x AS SELECT 1 AS a","useLegacySql":false}}}`)
			var own []string
			for _, id := range emu.order {
				if isInternalJobID(id) {
					own = append(own, id)
				}
			}
			if len(own) == 0 {
				t.Fatalf("the front ran no query of its own: %v", emu.log)
			}
			deleted := map[string]bool{}
			for _, id := range emu.deleted {
				deleted[id] = true
			}
			if deleted["ctas"] {
				t.Error("the client's job was deleted")
			}
			for _, id := range own {
				if deleted[id] == c.kept {
					t.Errorf("the front's own job %s: deleted %v, want %v (deleted: %v)", id, deleted[id], !c.kept, emu.deleted)
				}
			}
		})
	}
}
