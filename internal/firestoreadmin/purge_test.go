package firestoreadmin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
)

// TestResetProjectDropsOnlyThatProject (#1540): a project's reset clears
// the documents of each of its databases in the emulator and drops its
// records, and leaves another project's alone.
func TestResetProjectDropsOnlyThatProject(t *testing.T) {
	var mu sync.Mutex
	var cleared []string
	emu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/emulator/v1/") {
			mu.Lock()
			cleared = append(cleared, strings.TrimPrefix(r.URL.Path, "/emulator/v1/"))
			mu.Unlock()
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(emu.Close)
	a := New(strings.TrimPrefix(emu.URL, "http://"))
	a.mu.Lock()
	for _, p := range []string{"gone", "kept"} {
		a.seed(p)
		db := "projects/" + p + "/databases/second"
		a.databases[db] = &adminpb.Database{Name: db}
		a.indexes[db+"/collectionGroups/c/indexes/i"] = &adminpb.Index{}
		a.schedules[db+"/backupSchedules/s"] = &adminpb.BackupSchedule{}
		a.backups["projects/"+p+"/locations/nam5/backups/b"] = &backup{meta: &adminpb.Backup{}}
		op := db + "/operations/o"
		a.ops[op] = &longrunningpb.Operation{Name: op}
		a.opOrder = append(a.opOrder, op)
	}
	a.mu.Unlock()

	if err := a.ResetProject(context.Background(), "gone"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := strings.Join(cleared, ",")
	mu.Unlock()
	if got != "projects/gone/databases/(default)/documents,projects/gone/databases/second/documents" {
		t.Errorf("cleared %s", got)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.databases) != 2 || len(a.indexes) != 1 || len(a.schedules) != 1 || len(a.backups) != 1 || len(a.ops) != 1 || len(a.opOrder) != 1 {
		t.Errorf("records left: databases %v, indexes %v, schedules %v, backups %v, ops %v", a.databases, a.indexes, a.schedules, a.backups, a.opOrder)
	}
	if a.seeded["gone"] || !a.seeded["kept"] || a.databases["projects/kept/databases/second"] == nil {
		t.Errorf("seeded %v, databases %v", a.seeded, a.databases)
	}
}

// TestResetProjectClearsDatabasesCallsNamed (#1550): a database the Admin
// API never made, that a documents call named, has its documents cleared
// with its project's, though the API was restarted in between; another
// project's is left.
func TestResetProjectClearsDatabasesCallsNamed(t *testing.T) {
	var mu sync.Mutex
	var cleared []string
	emu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/emulator/v1/") {
			mu.Lock()
			cleared = append(cleared, strings.TrimPrefix(r.URL.Path, "/emulator/v1/"))
			mu.Unlock()
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(emu.Close)
	dir := t.TempDir()
	logf := quietLogf(t)
	ctx, cancel := context.WithCancel(context.Background())
	a := New(strings.TrimPrefix(emu.URL, "http://"))
	if err := a.KeepState(ctx, dir, logf); err != nil {
		t.Fatal(err)
	}
	a.SawResource("projects/gone/databases/never-made")
	a.SawResource("projects/gone/databases/never-made/documents/c/d")
	a.SawResource("projects/kept/databases/other")
	a.SawResource("projects/gone")
	cancel()

	bctx, bcancel := context.WithCancel(context.Background())
	t.Cleanup(bcancel)
	b := New(strings.TrimPrefix(emu.URL, "http://"))
	if err := b.KeepState(bctx, dir, logf); err != nil {
		t.Fatal(err)
	}
	if err := b.ResetProject(context.Background(), "gone"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := strings.Join(cleared, ",")
	mu.Unlock()
	if got != "projects/gone/databases/(default)/documents,projects/gone/databases/never-made/documents" {
		t.Errorf("cleared %s", got)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.seen) != 1 || !b.seen["projects/kept/databases/other"] {
		t.Errorf("seen after the reset: %v", b.seen)
	}
}

// quietLogf is t.Logf until the test's cleanups start: the state's saver
// writes once more when its context ends, and may run after the test.
func quietLogf(t *testing.T) func(string, ...any) {
	var mu sync.Mutex
	done := false
	t.Cleanup(func() { mu.Lock(); done = true; mu.Unlock() })
	return func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			t.Logf(format, args...)
		}
	}
}
