package bigqueryfront

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

// takenJobsEmulator answers jobs.get for the jobs it has, with their locations,
// and counts the inserts it is sent.
type takenJobsEmulator struct {
	mu      sync.Mutex
	jobs    map[string]string // id -> location
	inserts int
}

func (e *takenJobsEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/jobs/") {
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		location, ok := e.jobs[id]
		if !ok {
			writeError(w, http.StatusNotFound, "notFound", "Not found: Job p:"+id)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"jobReference": map[string]any{"projectId": "p", "jobId": id, "location": location}})
		return
	}
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs") {
		e.inserts++
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": map[string]any{"state": "DONE"}})
}

// TestInsertOfAnExistingJobIs409 (#1622): a client's jobs.insert of a job
// ID the project has is 409 duplicate, BigQuery's "Already Exists: Job
// project:LOCATION.id", and nothing is sent to the emulator; an insert
// with no job ID, or of an ID not taken, is sent on.
func TestInsertOfAnExistingJobIs409(t *testing.T) {
	emu := &takenJobsEmulator{jobs: map[string]string{"taken": "EU", "nowhere": ""}}
	h := Wrap(emu)
	insert := func(ref string) (int, map[string]any) {
		return do(t, h, "POST", base+"/jobs", `{`+ref+`"configuration":{"query":{"query":"SELECT 1","useLegacySql":false}}}`)
	}
	for _, c := range []struct {
		ref, want string
	}{
		{`"jobReference":{"projectId":"p","jobId":"taken"},`, "Already Exists: Job p:EU.taken"},
		{`"jobReference":{"jobId":"nowhere"},`, "Already Exists: Job p:US.nowhere"},
	} {
		code, got := insert(c.ref)
		e, _ := got["error"].(map[string]any)
		errs, _ := e["errors"].([]any)
		reason := ""
		if len(errs) > 0 {
			reason, _ = errs[0].(map[string]any)["reason"].(string)
		}
		if code != http.StatusConflict || reason != "duplicate" || e["message"] != c.want {
			t.Errorf("insert %s: %d %v, want 409 duplicate %q", c.ref, code, got, c.want)
		}
	}
	if emu.inserts != 0 {
		t.Errorf("the emulator was sent %d inserts of existing jobs, want none", emu.inserts)
	}
	for _, ref := range []string{`"jobReference":{"jobId":"fresh"},`, ``} {
		if code, got := insert(ref); code != http.StatusOK {
			t.Errorf("insert %q: %d %v, want 200", ref, code, got)
		}
	}
	if emu.inserts != 2 {
		t.Errorf("the emulator was sent %d inserts of new jobs, want 2", emu.inserts)
	}
}
