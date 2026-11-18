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

	pubsub "cloud.google.com/go/pubsub/v2"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/scheduler"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// State written by an earlier release must load in this one (#1605, #1807):
// v0.2.0, the latest release, and v0.1.0 before it. testdata/state-<release>
// holds what that release wrote for a persistent instance seeded through the
// official clients (test/upgrade's TestSeedRunning): the host side of its
// state directory (the tasks, projects and scheduler stores; the other
// services keep theirs on cluster volumes), and a `state save` archive taken
// before `stop`. These tests read both back through this release's stores;
// test/upgrade does the whole upgrade against real instances.
//
// To capture a release's fixture: bring that release's published binary up
// with `--mode persistent` and test/upgrade's flags, run TestSeedRunning
// against it, `state save` it to state-save.tar.gz, `stop` it, and copy
// <state-dir>/<name>/{tasks,projects,scheduler}/metadata.json under statedir.

// stateFixture is one earlier release's testdata, and the project its
// instance ran as.
type stateFixture struct {
	release, project string
}

var (
	v010 = stateFixture{release: "v0.1.0", project: "w-ejx-up"}
	v020 = stateFixture{release: "v0.2.0", project: "w-emdb"}
)

func (f stateFixture) path(elem ...string) string {
	return filepath.Join(append([]string{"testdata", "state-" + f.release}, elem...)...)
}

func (f stateFixture) copyDir(t *testing.T, name string) store.Store {
	t.Helper()
	src := f.path("statedir", name)
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
		t.Fatalf("open %s's %s state: %v", f.release, name, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func (f stateFixture) checkTasks(t *testing.T, db store.Store) {
	t.Helper()
	ts := tasks.NewStore(db)
	q := "projects/" + f.project + "/locations/us-central1/queues/upgrade-queue"
	got, err := ts.GetQueue(q)
	if err != nil {
		t.Fatalf("%s's queue: %v", f.release, err)
	}
	if got.State != tasks.StatePaused || got.RetryConfig.MaxAttempts != 7 {
		t.Errorf("%s's queue = %+v", f.release, got)
	}
	task, err := ts.GetTask(tasks.TaskName(q, "upgrade-task"))
	if err != nil {
		t.Fatalf("%s's task: %v", f.release, err)
	}
	if task.HTTPRequest == nil || string(task.HTTPRequest.Body) != "payload" {
		t.Errorf("%s's task = %+v", f.release, task)
	}
	// The queue takes new tasks beside the older release's (v0.1.0's carry
	// no tombstones).
	if _, err := ts.CreateTask(tasks.Task{Name: tasks.TaskName(q, "after-upgrade"),
		HTTPRequest: &tasks.HTTPRequest{URL: "http://127.0.0.1:9/x"}}); err != nil {
		t.Errorf("a task after the upgrade: %v", err)
	}
	if err := ts.DeleteTask(tasks.TaskName(q, "upgrade-task")); err != nil {
		t.Errorf("delete %s's task: %v", f.release, err)
	}
}

func (f stateFixture) checkProjects(t *testing.T, db store.Store) {
	t.Helper()
	p, err := resourcemanager.New(db).Get("cb-upgrade-proj")
	if err != nil {
		t.Fatalf("%s's project: %v", f.release, err)
	}
	if p.DisplayName != "Upgrade" {
		t.Errorf("%s's project = %+v", f.release, p)
	}
}

func (f stateFixture) checkScheduler(t *testing.T, db store.Store) {
	t.Helper()
	j, err := scheduler.NewStore(db).Get("projects/" + f.project + "/locations/us-central1/jobs/upgrade-job")
	if err != nil {
		t.Fatalf("%s's job: %v", f.release, err)
	}
	if j.State != scheduler.StatePaused || j.Schedule != "0 3 * * *" {
		t.Errorf("%s's job = %+v", f.release, j)
	}
}

func (f stateFixture) checkStateDir(t *testing.T) {
	f.checkTasks(t, f.copyDir(t, "tasks"))
	f.checkProjects(t, f.copyDir(t, "projects"))
	f.checkScheduler(t, f.copyDir(t, "scheduler"))
}

func TestStateDirFromV010Loads(t *testing.T) { v010.checkStateDir(t) }
func TestStateDirFromV020Loads(t *testing.T) { v020.checkStateDir(t) }

// archive reads the release's `state save` archive's entries.
func (f stateFixture) archive(t *testing.T) memEntries {
	t.Helper()
	file, err := os.Open(f.path("state-save.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
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

// loadKV loads one key-value service of the archive into a fresh store.
func (f stateFixture) loadKV(t *testing.T, a memEntries, name string) store.Store {
	t.Helper()
	db := store.NewMemory()
	if err := (&kvSnapshotter{name: name, db: func() store.Store { return db }}).Import(context.Background(), a.service(name)); err != nil {
		t.Fatalf("load %s's %s: %v", f.release, name, err)
	}
	return db
}

// checkArchive loads what every release's archive holds: Cloud Tasks,
// projects, Secret Manager and Cloud Storage.
func (f stateFixture) checkArchive(t *testing.T, a memEntries) {
	t.Helper()
	ctx := context.Background()
	f.checkTasks(t, f.loadKV(t, a, "tasks"))
	f.checkProjects(t, f.loadKV(t, a, "projects"))

	ss := secrets.NewStore(f.loadKV(t, a, "secretmanager"))
	vs, err := ss.ListVersions(f.project, "upgrade-secret")
	if err != nil {
		t.Fatal(err)
	}
	states := map[int]secrets.VersionState{}
	for _, v := range vs {
		states[v.Number] = v.State
	}
	if states[1] != secrets.StateDisabled || states[2] != secrets.StateDestroyed || states[3] != secrets.StateEnabled {
		t.Errorf("%s's versions = %v", f.release, states)
	}
	if v, err := ss.AccessVersion(f.project, "upgrade-secret", "3"); err != nil || string(v.Payload) != "third" {
		t.Errorf("%s's version 3 = %q, %v", f.release, v.Payload, err)
	}
	// A new version is numbered after every saved one, destroyed ones too.
	if v, err := ss.AddVersion(f.project, "upgrade-secret", []byte("next")); err != nil || v.Number != len(vs)+1 {
		t.Errorf("a version after the upgrade = %+v, %v", v, err)
	}

	srv, call := builtinAt(t)
	call("POST", "/storage/v1/b?project=p", strings.NewReader(`{"name":"before-the-load"}`))
	s := &builtinStorageSnapshotter{tunnel: forwarderAt(t, srv.Listener.Addr().String()), project: f.project}
	if err := s.Import(ctx, a.service("storage")); err != nil {
		t.Fatalf("load %s's storage: %v", f.release, err)
	}
	if code, body := call("GET", "/download/storage/v1/b/cb-upgrade-1605/o/dir%2Fb.json?alt=media", nil); code != 200 || body != `{"n": 1}` {
		t.Errorf("%s's object = %d %q", f.release, code, body)
	}
	if _, body := call("GET", "/storage/v1/b/cb-upgrade-1605/o/a.txt", nil); !strings.Contains(body, `"seeded": "yes"`) ||
		!strings.Contains(body, `"contentType": "text/plain"`) {
		t.Errorf("%s's object metadata = %s", f.release, body)
	}
	if code, _ := call("GET", "/storage/v1/b/before-the-load", nil); code != 404 {
		t.Errorf("a bucket made before the load is still there: %d", code)
	}
}

// v0.1.0's archive keeps Cloud Storage in the layout of the server it ran
// before the builtin one (#290), which a load creates through the builtin
// server's JSON API (#519).
func TestStateArchiveFromV010Loads(t *testing.T) {
	v010.checkArchive(t, v010.archive(t))
}

// v0.2.0's archive keeps the builtin server's own Cloud Storage state, and
// adds Cloud Scheduler and Pub/Sub's resources (#880).
func TestStateArchiveFromV020Loads(t *testing.T) {
	a := v020.archive(t)
	if _, ok := a["services/storage/state.json"]; !ok {
		t.Fatal("v0.2.0's archive has no services/storage/state.json")
	}
	v020.checkArchive(t, a)
	v020.checkScheduler(t, v020.loadKV(t, a, "scheduler"))

	_, addr := pubsubBehindFront(t)
	ctx := context.Background()
	p := &pubsubSnapshotter{tunnel: forwarderAt(t, addr), projects: func() []string { return nil }}
	if err := p.Import(ctx, a.service("pubsub")); err != nil {
		t.Fatalf("load v0.2.0's pubsub: %v", err)
	}
	c, err := pubsub.NewClient(ctx, v020.project, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sub, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{
		Subscription: "projects/" + v020.project + "/subscriptions/upgrade-sub"})
	if err != nil {
		t.Fatalf("v0.2.0's subscription: %v", err)
	}
	if sub.Topic != "projects/"+v020.project+"/topics/upgrade-topic" || sub.AckDeadlineSeconds != 30 {
		t.Errorf("v0.2.0's subscription = %v", sub)
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
