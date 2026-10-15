package sqladmin

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// fakeProv runs each created instance as a fake engine.
type fakeProv struct {
	mu       sync.Mutex
	started  map[string]Spec
	removed  []string
	restarts int
	pruned   []string
	failWith error
}

func newFakeProv() *fakeProv { return &fakeProv{started: map[string]Spec{}} }

type namedFake struct {
	*fake
	name, version string
}

func (n namedFake) Instance() string        { return n.name }
func (n namedFake) DatabaseVersion() string { return n.version }

func (p *fakeProv) Engine(s Spec) Engine {
	return namedFake{fake: newFake(), name: s.Name, version: s.DatabaseVersion}
}
func (p *fakeProv) Start(_ context.Context, s Spec) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failWith != nil {
		return p.failWith
	}
	p.started[s.Name] = s
	return nil
}
func (p *fakeProv) Restart(context.Context, Spec) error {
	p.mu.Lock()
	p.restarts++
	p.mu.Unlock()
	return nil
}
func (p *fakeProv) Remove(_ context.Context, name string) error {
	p.mu.Lock()
	p.removed = append(p.removed, name)
	delete(p.started, name)
	p.mu.Unlock()
	return nil
}
func (p *fakeProv) Prune(_ context.Context, keep []string) error {
	p.mu.Lock()
	p.pruned = keep
	p.mu.Unlock()
	return nil
}

func wait(t *testing.T, svc *sqladmin.Service, project string, op *sqladmin.Operation) *sqladmin.Operation {
	t.Helper()
	for i := 0; i < 200; i++ {
		got, err := svc.Operations.Get(project, op.Name).Do()
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == "DONE" {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("operation %s never finished", op.Name)
	return nil
}

func TestInstanceWritesThroughTheGoClient(t *testing.T) {
	prov := newFakeProv()
	dir := t.TempDir()
	srv := NewServer("", newFake())
	if err := srv.EnableConnector("127.0.0.1:0", dir); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "instances.json")
	srv.EnableInstanceWrites(prov, state)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []*sqladmin.DatabaseInstance{
		{Name: "xy", DatabaseVersion: "POSTGRES_15", Settings: &sqladmin.Settings{Tier: "db-f1-micro"}},
		{Name: "xy", DatabaseVersion: "SQLSERVER_2019_STANDARD", Settings: &sqladmin.Settings{Tier: "db-f1-micro"}},
		{Name: "xy", DatabaseVersion: "POSTGRES_17", Settings: &sqladmin.Settings{Tier: "db-f1-micro"}, MasterInstanceName: "pg"},
		{Name: "xy", DatabaseVersion: "POSTGRES_17", Settings: &sqladmin.Settings{Tier: "t",
			DatabaseFlags: []*sqladmin.DatabaseFlags{{Name: "cloudsql.iam_authentication", Value: "on"}}}},
	} {
		if _, err := svc.Instances.Insert("proj", bad).Do(); err == nil || !strings.Contains(err.Error(), "501") {
			t.Errorf("insert %s %s: %v, want 501", bad.DatabaseVersion, bad.MasterInstanceName, err)
		}
	}
	if _, err := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "xy", DatabaseVersion: "MYSQL_8_4",
		Settings: &sqladmin.Settings{Tier: "t"}}).Do(); err == nil || !strings.Contains(err.Error(), "412") {
		t.Errorf("a MySQL instance without --services cloudsql-mysql: %v, want 412", err)
	}
	op, err := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "orders", DatabaseVersion: "POSTGRES_17",
		Settings: &sqladmin.Settings{Tier: "db-custom-2-7680", UserLabels: map[string]string{"team": "a"},
			DeletionProtectionEnabled: true,
			DatabaseFlags:             []*sqladmin.DatabaseFlags{{Name: "max_connections", Value: "50"}}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil || got.OperationType != "CREATE" {
		t.Fatalf("create: %+v", got)
	}
	in, err := svc.Instances.Get("proj", "orders").Do()
	if err != nil {
		t.Fatal(err)
	}
	if in.State != "RUNNABLE" || in.Settings.Tier != "db-custom-2-7680" || in.Settings.UserLabels["team"] != "a" ||
		len(in.Settings.DatabaseFlags) != 1 || !in.Settings.DeletionProtectionEnabled || in.ConnectionName != "proj:us-central1:orders" {
		t.Errorf("instance: %+v %+v", in, in.Settings)
	}
	if _, err := svc.Connect.Get("proj", "orders").Do(); err != nil {
		t.Errorf("connectSettings of a created instance: %v", err)
	}
	if _, err := svc.Instances.Get("other", "orders").Do(); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("another project's: %v, want 404", err)
	}
	if _, err := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "orders", DatabaseVersion: "POSTGRES_17",
		Settings: &sqladmin.Settings{Tier: "t"}}).Do(); err == nil || !strings.Contains(err.Error(), "409") {
		t.Errorf("second insert: %v, want 409", err)
	}
	if _, err := svc.Instances.Delete("proj", "orders").Do(); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Errorf("delete of a protected instance: %v", err)
	}
	if _, err := svc.Instances.Delete("proj", "pg").Do(); err == nil || !strings.Contains(err.Error(), "cloudburrow up") {
		t.Errorf("delete of the instance up runs: %v", err)
	}
	// A patch of labels alone does not restart; one of flags does.
	op, err = svc.Instances.Patch("proj", "orders", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		UserLabels: map[string]string{"team": "b"}, DeletionProtectionEnabled: false, ForceSendFields: []string{"DeletionProtectionEnabled"}}}).Do()
	if err != nil || op.Status != "DONE" {
		t.Fatalf("patch: %v %+v", err, op)
	}
	op, err = svc.Instances.Patch("proj", "orders", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		DatabaseFlags: []*sqladmin.DatabaseFlags{{Name: "max_connections", Value: "60"}}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, svc, "proj", op)
	if f := prov.started["orders"].Flags; len(f) != 1 || f[0].Value != "60" {
		t.Errorf("flags given to the server: %+v", f)
	}
	in, _ = svc.Instances.Get("proj", "orders").Do()
	if in.Settings.UserLabels["team"] != "b" || in.Settings.DeletionProtectionEnabled || in.Settings.SettingsVersion != 3 {
		t.Errorf("after patch: %+v", in.Settings)
	}
	op, err = svc.Instances.Restart("proj", "orders").Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, svc, "proj", op)
	if prov.restarts != 1 {
		t.Errorf("restarts: %d", prov.restarts)
	}
	// The record survives a restart of the API server.
	b, err := os.ReadFile(state)
	if err != nil || !strings.Contains(string(b), `"orders"`) || !strings.Contains(string(b), "max_connections") {
		t.Errorf("records: %v %s", err, b)
	}
	srv2 := NewServer("", newFake())
	prov2 := newFakeProv()
	srv2.EnableInstanceWrites(prov2, state)
	srv2.addr = "127.0.0.1:0"
	if err := srv2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer srv2.Stop(context.Background())
	for i := 0; i < 200 && srv2.engine("orders") != nil && len(prov2.startedNames()) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if names := prov2.startedNames(); len(names) != 1 || names[0] != "orders" {
		t.Errorf("restored: %v", names)
	}

	op, err = svc.Instances.Delete("proj", "orders").Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil {
		t.Fatalf("delete: %+v", got.Error)
	}
	if _, err := svc.Instances.Get("proj", "orders").Do(); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("get after delete: %v", err)
	}
	if len(prov.removed) != 1 {
		t.Errorf("removed: %v", prov.removed)
	}

	// A server that does not start fails the operation and the instance.
	prov.failWith = errors.New("mysqld: unknown variable 'bogus=1'")
	op, err = svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "broken", DatabaseVersion: "POSTGRES_17",
		Settings: &sqladmin.Settings{Tier: "t"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error == nil || !strings.Contains(got.Error.Errors[0].Message, "bogus") {
		t.Errorf("failed create: %+v", got.Error)
	}
	if in, _ := svc.Instances.Get("proj", "broken").Do(); in == nil || in.State != "FAILED" {
		t.Errorf("failed instance: %+v", in)
	}
}

func (p *fakeProv) startedNames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for n := range p.started {
		out = append(out, n)
	}
	return out
}
