package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestJobTextsOutliveAFrontRestart (#1028): a DML job's counts and the
// client's text, kept on the state directory, are restored by the next
// front, and the emulator's restart empties them there too.
func TestJobTextsOutliveAFrontRestart(t *testing.T) {
	path := jobTextsStateFile(t.TempDir())
	a := &jobTexts{}
	a.keep(path, t.Logf)
	want := jobText{query: "UPDATE t SET a = 1 WHERE b", names: map[string]string{"p_d_t": "d.t"},
		dml: &dmlCounts{statementType: "UPDATE", updated: 3}, dest: &tableRef{"p", "d", "t"}}
	a.add("p", "job1", want)
	b := &jobTexts{}
	b.keep(path, t.Logf)
	got, ok := b.get("p", "job1")
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("restored %+v %v, want %+v", got, ok, want)
	}
	b.reset()
	c := &jobTexts{}
	c.keep(filepath.Clean(path), t.Logf)
	if !c.none() {
		t.Errorf("the emulator's restart left job texts in the file")
	}
}

// TestJobTextsKeepAnEarlierJobsText (#1629): a client's job keeps its text
// after thousands of later jobs of texts of their own (every DML job has
// one), before and after a front restart. At 1,000 kept, a time-travel
// query's job 1,000 DML jobs on read back the front's rewrite of it, which
// jobs.list listed among the jobs since a test began once the instance's
// clock was ahead. The file is a line per job added, written again before
// it holds twice the lines of the jobs kept, and the array an earlier
// front wrote is read.
func TestJobTextsKeepAnEarlierJobsText(t *testing.T) {
	path := jobTextsStateFile(t.TempDir())
	a := &jobTexts{}
	a.keep(path, t.Logf)
	want := jobText{query: "SELECT n FROM d.t FOR SYSTEM_TIME AS OF TIMESTAMP_MICROS(1)"}
	a.add("p", "travel", want)
	const later = 5000
	for i := range later {
		a.add("p", fmt.Sprintf("dml%d", i), jobText{query: "UPDATE d.t SET n = 1 WHERE TRUE",
			dml: &dmlCounts{statementType: "UPDATE", updated: int64(i)}})
	}
	if got, ok := a.get("p", "travel"); !ok || got.query != want.query {
		t.Fatalf("after %d later jobs: %+v %v, want the client's text", later, got, ok)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if lines := bytes.Count(b, []byte("\n")); lines > 2*(later+1) {
		t.Errorf("the file has %d lines for %d jobs", lines, later+1)
	}
	again := &jobTexts{}
	again.keep(path, t.Logf)
	if got, ok := again.get("p", "travel"); !ok || got.query != want.query {
		t.Errorf("after a restart: %+v %v, want the client's text", got, ok)
	}
	if got, _ := again.get("p", fmt.Sprint("dml", later-1)); got.dml == nil || got.dml.updated != later-1 {
		t.Errorf("after a restart, the last DML job: %+v", got)
	}

	// The most recent maxJobTexts are kept, the file no longer than twice.
	for i := range maxJobTexts {
		again.add("p", fmt.Sprintf("more%d", i), jobText{query: "DELETE d.t WHERE TRUE"})
	}
	if _, ok := again.get("p", "travel"); ok {
		t.Errorf("kept past %d later jobs", maxJobTexts)
	}
	if b, err = os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	if lines := bytes.Count(b, []byte("\n")); lines > 2*maxJobTexts {
		t.Errorf("the file has %d lines for %d jobs kept", lines, maxJobTexts)
	}

	// An earlier front's file, one JSON array.
	old := filepath.Join(t.TempDir(), "jobtexts.json")
	arr, _ := json.Marshal([]savedJobText{saveJobText("p/old", want)})
	if err := os.WriteFile(old, arr, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &jobTexts{}
	c.keep(old, t.Logf)
	c.add("p", "new", jobText{query: "SELECT 1"})
	d := &jobTexts{}
	d.keep(old, t.Logf)
	if got, ok := d.get("p", "old"); !ok || got.query != want.query {
		t.Errorf("the array an earlier front wrote: %+v %v", got, ok)
	}
	if _, ok := d.get("p", "new"); !ok {
		t.Errorf("a job added after the array was read is not kept")
	}
}
