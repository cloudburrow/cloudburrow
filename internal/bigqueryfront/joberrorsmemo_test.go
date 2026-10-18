package bigqueryfront

import "testing"

// TestMappedJobErrorsKeepsADoneJobsMapping (#1628): a DONE job's errors
// are mapped once; a job not DONE, or with other errors, is not taken
// for it, and the emulator's restart forgets them.
func TestMappedJobErrorsKeepsADoneJobsMapping(t *testing.T) {
	var m mappedJobErrors
	job := func(state, msg string) (map[string]any, map[string]any) {
		status := map[string]any{"state": state, "errorResult": map[string]any{"reason": "notFound", "message": msg}}
		return map[string]any{"jobReference": map[string]any{"projectId": "p", "jobId": "j1"}, "status": status}, status
	}
	j, st := job("DONE", "engine words")
	key := m.key(j, st)
	if key == "" {
		t.Fatal("no key for a DONE job")
	}
	if kept, _ := m.apply(key, st); kept {
		t.Fatal("a mapping kept before any was put")
	}
	st["errorResult"] = map[string]any{"reason": "notFound", "message": "Not found: Dataset p:ds"}
	m.put(key, st, true)
	j2, st2 := job("DONE", "engine words")
	if kept, changed := m.apply(m.key(j2, st2), st2); !kept || !changed ||
		st2["errorResult"].(map[string]any)["message"] != "Not found: Dataset p:ds" {
		t.Errorf("the kept mapping: %v %v %v", kept, changed, st2)
	}
	if j3, st3 := job("RUNNING", "engine words"); m.key(j3, st3) != "" {
		t.Error("a key for a job not DONE")
	}
	if j4, st4 := job("DONE", "other words"); m.key(j4, st4) == key {
		t.Error("the same key for other errors")
	}
	m.reset()
	if kept, _ := m.apply(key, st2); kept {
		t.Error("a mapping kept after the reset")
	}
}
