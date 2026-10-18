package bigqueryfront

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func memoJob(id, state, msg string) (map[string]any, map[string]any) {
	status := map[string]any{"state": state, "errorResult": map[string]any{"reason": "notFound", "message": msg}}
	return map[string]any{"jobReference": map[string]any{"projectId": "p", "jobId": id}, "status": status}, status
}

func putMapping(t *testing.T, m *mappedJobErrors, key jobErrorsKey, status map[string]any, changed bool) {
	t.Helper()
	e, ok := m.mapping(key, status, changed)
	if !ok {
		t.Fatal("no mapping")
	}
	m.put([]jobErrorsMapping{e})
}

// TestMappedJobErrorsKeepsADoneJobsMapping (#1628): a DONE job's errors
// are mapped once; a job not DONE, or with other errors, is not taken
// for it, and the emulator's restart forgets them.
func TestMappedJobErrorsKeepsADoneJobsMapping(t *testing.T) {
	var m mappedJobErrors
	j, st := memoJob("j1", "DONE", "engine words")
	key, ok := m.key(j, st)
	if !ok {
		t.Fatal("no key for a DONE job")
	}
	if kept, _ := m.apply(key, st); kept {
		t.Fatal("a mapping kept before any was put")
	}
	st["errorResult"] = map[string]any{"reason": "notFound", "message": "Not found: Dataset p:ds"}
	putMapping(t, &m, key, st, true)
	j2, st2 := memoJob("j1", "DONE", "engine words")
	k2, _ := m.key(j2, st2)
	if kept, changed := m.apply(k2, st2); !kept || !changed ||
		st2["errorResult"].(map[string]any)["message"] != "Not found: Dataset p:ds" {
		t.Errorf("the kept mapping: %v %v %v", kept, changed, st2)
	}
	if j3, st3 := memoJob("j1", "RUNNING", "engine words"); func() bool { _, ok := m.key(j3, st3); return ok }() {
		t.Error("a key for a job not DONE")
	}
	if j4, st4 := memoJob("j1", "DONE", "other words"); func() bool { k, _ := m.key(j4, st4); return k == key }() {
		t.Error("the same key for other errors")
	}
	if _, ok := m.key(map[string]any{"jobReference": map[string]any{"projectId": "p", "jobId": "j5"}},
		map[string]any{"state": "DONE"}); ok {
		t.Error("a key for a job with no errors, which has nothing to map")
	}
	m.reset()
	if kept, _ := m.apply(key, st2); kept {
		t.Error("a mapping kept after the reset")
	}
}

// TestMappedJobErrorsOutliveAFrontRestart (#1628): the mappings are kept
// in the state directory, so a front started again maps none of them
// again; the emulator's restart empties the file too.
func TestMappedJobErrorsOutliveAFrontRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "joberrors.json")
	var m mappedJobErrors
	m.keep(path, t.Logf)
	var keys []jobErrorsKey
	for i := range 3 {
		j, st := memoJob(fmt.Sprint("j", i), "DONE", "engine words")
		k, _ := m.key(j, st)
		keys = append(keys, k)
		if i == 0 {
			putMapping(t, &m, k, st, false) // mapped to itself
			continue
		}
		st["errorResult"] = map[string]any{"reason": "notFound", "message": fmt.Sprint("mapped ", i)}
		putMapping(t, &m, k, st, true)
	}

	var again mappedJobErrors
	again.keep(path, t.Logf)
	if again.len() != 3 {
		t.Fatalf("restored %d mappings, want 3", again.len())
	}
	_, st := memoJob("j0", "DONE", "engine words")
	if kept, changed := again.apply(keys[0], st); !kept || changed {
		t.Errorf("j0: kept %v changed %v, want kept and unchanged", kept, changed)
	}
	_, st = memoJob("j2", "DONE", "engine words")
	if kept, _ := again.apply(keys[2], st); !kept || st["errorResult"].(map[string]any)["message"] != "mapped 2" {
		t.Errorf("j2: kept %v, %v", kept, st)
	}

	again.reset()
	var third mappedJobErrors
	third.keep(path, t.Logf)
	if third.len() != 0 {
		t.Errorf("%d mappings restored after the emulator's restart, want none", third.len())
	}
}

// TestMappedJobErrorsStayBounded (#1628, #1632's memory): however many
// jobs are listed, the front keeps maxMappedJobErrors mappings, the most
// recent, and its file stays within twice their lines.
func TestMappedJobErrorsStayBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "joberrors.json")
	var m mappedJobErrors
	m.keep(path, nil)
	const total = maxMappedJobErrors + 5000
	var first, last jobErrorsKey
	for i := 0; i < total; i += 500 {
		var es []jobErrorsMapping
		for n := i; n < i+500 && n < total; n++ {
			j, st := memoJob(fmt.Sprint("j", n), "DONE", "engine words")
			k, _ := m.key(j, st)
			if n == 0 {
				first = k
			}
			last = k
			st["errorResult"] = map[string]any{"reason": "notFound", "message": "Not found: Table p:ds.t"}
			e, _ := m.mapping(k, st, true)
			es = append(es, e)
		}
		m.put(es)
	}
	if got := m.len(); got != maxMappedJobErrors {
		t.Errorf("%d mappings kept, want %d", got, maxMappedJobErrors)
	}
	if got := len(m.order); got != maxMappedJobErrors || cap(m.order) > 2*maxMappedJobErrors {
		t.Errorf("order: len %d cap %d", got, cap(m.order))
	}
	_, st := memoJob("j0", "DONE", "engine words")
	if kept, _ := m.apply(first, st); kept {
		t.Error("the oldest mapping is still kept")
	}
	if kept, _ := m.apply(last, st); !kept {
		t.Error("the newest mapping is not kept")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := 0
	for _, c := range b {
		if c == '\n' {
			lines++
		}
	}
	if lines > 2*maxMappedJobErrors {
		t.Errorf("the file has %d lines, past twice the %d kept", lines, maxMappedJobErrors)
	}
}
