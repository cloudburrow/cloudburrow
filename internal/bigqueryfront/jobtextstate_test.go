package bigqueryfront

import (
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
