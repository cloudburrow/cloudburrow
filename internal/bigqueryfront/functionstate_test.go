package bigqueryfront

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The functions the front knows of, and whether it scanned a project's
// jobs, outlive a restart of the front given a state directory; the
// emulator's restart empties them there too; an unreadable file is
// replaced (#1115).
func TestKnownFunctionsOutliveAFrontRestart(t *testing.T) {
	path := functionsStateFile(filepath.Join(t.TempDir(), "front"))
	k := &knownFunctions{started: 1}
	k.keep(path, nil)
	k.add("p", []string{"ds", "F"})
	k.add("p", []string{"ds", "tf"})
	k.mu.Lock()
	k.scanned = map[string]bool{"p": true}
	k.saveLocked()
	k.mu.Unlock()

	again := &knownFunctions{started: 2}
	again.keep(path, nil)
	if got, want := len(again.in("p", "ds")), 2; got != want {
		t.Errorf("restored %d functions of p/ds, want %d", got, want)
	}
	if !reflect.DeepEqual(again.scanned, map[string]bool{"p": true}) || again.started != 1 {
		t.Errorf("restored scanned %v, started %d; want p scanned by the front that started at 1", again.scanned, again.started)
	}
	again.forget("p", "ds")
	again.reset()
	third := &knownFunctions{started: 3}
	third.keep(path, nil)
	if len(third.in("p", "ds")) != 0 || third.scanned["p"] {
		t.Errorf("after reset: %v, scanned %v; want none", third.funcs, third.scanned)
	}

	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logged int
	fourth := &knownFunctions{started: 4}
	fourth.keep(path, func(string, ...any) { logged++ })
	if logged == 0 || fourth.started != 4 || len(fourth.funcs) != 0 {
		t.Errorf("an unreadable file: logged %d, started %d, funcs %v", logged, fourth.started, fourth.funcs)
	}
}
