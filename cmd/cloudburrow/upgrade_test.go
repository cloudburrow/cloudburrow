package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/scheduler"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// State written by v0.1.0, the latest release (#1605), must load in this
// one. testdata/state-v0.1.0 holds what that release wrote for an instance
// seeded through the official clients (test/upgrade): the host side of its
// persistent state directory, and a `state save` archive. These tests read
// both back through this release's stores; test/upgrade does the whole
// upgrade against real instances.

const v010Project = "w-ejx-up"

func copyFixtureDir(t *testing.T, name string) store.Store {
	t.Helper()
	src := filepath.Join("testdata", "state-v0.1.0", "statedir", name)
	dst := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(src, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "metadata.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenDurable(dst)
	if err != nil {
		t.Fatalf("open v0.1.0's %s state: %v", name, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func checkV010Tasks(t *testing.T, db store.Store) {
	t.Helper()
	ts := tasks.NewStore(db)
	q := "projects/" + v010Project + "/locations/us-central1/queues/upgrade-queue"
	got, err := ts.GetQueue(q)
	if err != nil {
		t.Fatalf("v0.1.0's queue: %v", err)
	}
	if got.State != tasks.StatePaused || got.RetryConfig.MaxAttempts != 7 {
		t.Errorf("v0.1.0's queue = %+v", got)
	}
	task, err := ts.GetTask(tasks.TaskName(q, "upgrade-task"))
	if err != nil {
		t.Fatalf("v0.1.0's task: %v", err)
	}
	if task.HTTPRequest == nil || string(task.HTTPRequest.Body) != "payload" {
		t.Errorf("v0.1.0's task = %+v", task)
	}
	// The queue takes new tasks, beside v0.1.0's, which carry no tombstones.
	if _, err := ts.CreateTask(tasks.Task{Name: tasks.TaskName(q, "after-upgrade"),
		HTTPRequest: &tasks.HTTPRequest{URL: "http://127.0.0.1:9/x"}}); err != nil {
		t.Errorf("a task after the upgrade: %v", err)
	}
	if err := ts.DeleteTask(tasks.TaskName(q, "upgrade-task")); err != nil {
		t.Errorf("delete v0.1.0's task: %v", err)
	}
}

func checkV010Projects(t *testing.T, db store.Store) {
	t.Helper()
	p, err := resourcemanager.New(db).Get("cb-upgrade-proj")
	if err != nil {
		t.Fatalf("v0.1.0's project: %v", err)
	}
	if p.DisplayName != "Upgrade" {
		t.Errorf("v0.1.0's project = %+v", p)
	}
}

func TestStateDirFromV010Loads(t *testing.T) {
	checkV010Tasks(t, copyFixtureDir(t, "tasks"))
	checkV010Projects(t, copyFixtureDir(t, "projects"))
	j, err := scheduler.NewStore(copyFixtureDir(t, "scheduler")).Get("projects/" + v010Project + "/locations/us-central1/jobs/upgrade-job")
	if err != nil {
		t.Fatalf("v0.1.0's job: %v", err)
	}
	if j.State != scheduler.StatePaused || j.Schedule != "0 3 * * *" {
		t.Errorf("v0.1.0's job = %+v", j)
	}
}

// v010Archive reads the v0.1.0 archive's entries.
func v010Archive(t *testing.T) memEntries {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "state-v0.1.0", "state-save.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := memEntries{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = b
	}
}

// service is the entries of one service, as an import reads them.
func (m memEntries) service(name string) memEntries {
	out := memEntries{}
	for k, v := range m {
		if rest, ok := strings.CutPrefix(k, "services/"+name+"/"); ok {
			out[rest] = v
		}
	}
	return out
}

func TestStateArchiveFromV010Loads(t *testing.T) {
	a := v010Archive(t)
	ctx := context.Background()
	load := func(name string) store.Store {
		t.Helper()
		db := store.NewMemory()
		if err := (&kvSnapshotter{name: name, db: func() store.Store { return db }}).Import(ctx, a.service(name)); err != nil {
			t.Fatalf("load v0.1.0's %s: %v", name, err)
		}
		return db
	}
	checkV010Tasks(t, load("tasks"))
	checkV010Projects(t, load("projects"))

	ss := secrets.NewStore(load("secretmanager"))
	vs, err := ss.ListVersions(v010Project, "upgrade-secret")
	if err != nil {
		t.Fatal(err)
	}
	states := map[int]secrets.VersionState{}
	for _, v := range vs {
		states[v.Number] = v.State
	}
	if states[1] != secrets.StateDisabled || states[2] != secrets.StateDestroyed || states[3] != secrets.StateEnabled {
		t.Errorf("v0.1.0's versions = %v", states)
	}
	if v, err := ss.AccessVersion(v010Project, "upgrade-secret", "3"); err != nil || string(v.Payload) != "third" {
		t.Errorf("v0.1.0's version 3 = %q, %v", v.Payload, err)
	}
	// The archive was saved with five versions, the last two destroyed.
	if v, err := ss.AddVersion(v010Project, "upgrade-secret", []byte("sixth")); err != nil || v.Number != len(vs)+1 {
		t.Errorf("a version after the upgrade = %+v, %v", v, err)
	}

	// Cloud Storage, from the layout of the archive v0.1.0 wrote (#290),
	// into the builtin server (#519).
	srv, call := builtinAt(t)
	call("POST", "/storage/v1/b?project=p", strings.NewReader(`{"name":"before-the-load"}`))
	s := &builtinStorageSnapshotter{tunnel: forwarderAt(t, srv.Listener.Addr().String()), project: v010Project}
	if err := s.Import(ctx, a.service("storage")); err != nil {
		t.Fatalf("load v0.1.0's storage: %v", err)
	}
	if code, body := call("GET", "/download/storage/v1/b/cb-upgrade-1605/o/dir%2Fb.json?alt=media", nil); code != 200 || body != `{"n": 1}` {
		t.Errorf("v0.1.0's object = %d %q", code, body)
	}
	if _, body := call("GET", "/storage/v1/b/cb-upgrade-1605/o/a.txt", nil); !strings.Contains(body, `"seeded": "yes"`) ||
		!strings.Contains(body, `"contentType": "text/plain"`) {
		t.Errorf("v0.1.0's object metadata = %s", body)
	}
	if code, _ := call("GET", "/storage/v1/b/before-the-load", nil); code != 404 {
		t.Errorf("a bucket made before the load is still there: %d", code)
	}
}

// TestEarlierStorageSpoolLoads is the migration of a pre-#519 volume (#1606)
// without the cluster: the spool is read through a Cloud Storage JSON API
// and loaded into another builtin server, metadata and bytes intact.
func TestEarlierStorageSpoolLoads(t *testing.T) {
	src, call := builtinAt(t)
	call("POST", "/storage/v1/b?project=p", strings.NewReader(`{"name":"earlier","labels":{"k":"v"}}`))
	call("POST", "/upload/storage/v1/b/earlier/o?uploadType=media&name=dir%2Fone.txt", strings.NewReader("one"))
	call("PATCH", "/storage/v1/b/earlier/o/dir%2Fone.txt", strings.NewReader(`{"metadata":{"m":"1"},"cacheControl":"no-store"}`))
	e := &earlierStorage{instance: t.TempDir(), project: "p"}
	dir := e.spool()
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	n, err := e.download(context.Background(), src.URL, dir)
	if err != nil || n != 1 {
		t.Fatalf("download = %d, %v", n, err)
	}
	dst, dcall := builtinAt(t)
	s := &builtinStorageSnapshotter{tunnel: forwarderAt(t, dst.Listener.Addr().String()), project: "p"}
	if err := s.importLegacy(context.Background(), dirEntries(dir)); err != nil {
		t.Fatal(err)
	}
	if code, body := dcall("GET", "/download/storage/v1/b/earlier/o/dir%2Fone.txt?alt=media", nil); code != 200 || body != "one" {
		t.Errorf("migrated object = %d %q", code, body)
	}
	if _, body := dcall("GET", "/storage/v1/b/earlier/o/dir%2Fone.txt", nil); !strings.Contains(body, `"m": "1"`) ||
		!strings.Contains(body, `"cacheControl": "no-store"`) {
		t.Errorf("migrated metadata = %s", body)
	}
	if _, body := dcall("GET", "/storage/v1/b/earlier", nil); !strings.Contains(body, `"k": "v"`) {
		t.Errorf("migrated bucket = %s", body)
	}
}
