package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

const seedDoc = `{"components": {
  "tasks": {"queues": ["projects/seed-proj/locations/us-central1/queues/seeded"]},
  "secretmanager": {"secrets": [{"name": "projects/seed-proj/secrets/seeded", "versions": [{"data": "v1"}]}]}
}}`

func seedFileConfig(t *testing.T, doc string, services ...config.Service) config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seed.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Services = services
	cfg.SeedFile = path
	return cfg
}

// An invalid seed file is refused with the field named, before anything is
// created, and so is a component for a service that is not enabled.
func TestAnInvalidSeedFileIsRefusedUpFront(t *testing.T) {
	for doc, want := range map[string]string{
		`{"components": {"tasks": {"queues": ["not-a-queue-name"]}}}`:   "queues[0]",
		`{"components": {"storage": {"buckets": [{"name": "b-one"}]}}}`: `unknown component "storage"`,
		`{"components": {}, "extra": true}`:                             "extra",
		`not json`:                                                      "malformed",
	} {
		_, err := planSeedFile(seedFileConfig(t, doc, config.ServiceTasks))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("planSeedFile(%s) = %v, want an error naming %q", doc, err, want)
		}
	}
	cfg := config.Default()
	if plan, err := planSeedFile(cfg); plan != nil || err != nil {
		t.Errorf("no seed file gave %v %v", plan, err)
	}
}

// fixtureAdmin is the admin API over in-memory Cloud Tasks and Secret Manager,
// registered the way mountAdmin registers them.
func fixtureAdmin(t *testing.T) (*admin.API, *tasks.Store, *secrets.Store) {
	t.Helper()
	ts := tasks.NewStore(store.NewMemory())
	ss := secrets.NewStore(store.NewMemory())
	tsvc, ssvc := &tasksService{store: ts}, &secretsService{store: ss}
	api := admin.NewAPI(admin.NewRecorder(10, nil))
	api.RegisterResetter(&tasksResetter{svc: tsvc}, &secretsResetter{svc: ssvc})
	api.RegisterSeeder(&tasksSeeder{svc: tsvc})
	api.RegisterSeeder(&secretsSeeder{svc: ssvc})
	return api, ts, ss
}

// TestResetReseedLeavesExactlyTheSeed: the startup seed is applied, more is
// created afterwards, and reset?reseed=true leaves the seeded resources and
// nothing else.
func TestResetReseedLeavesExactlyTheSeed(t *testing.T) {
	cfg := seedFileConfig(t, seedDoc, config.ServiceTasks, config.ServiceSecrets)
	plan, err := planSeedFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	api, ts, ss := fixtureAdmin(t)
	api.SetStartupSeed(plan)
	sc := &seedComponent{api: api, plan: plan, file: cfg.SeedFile, out: &strings.Builder{}}
	if err := sc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A persistent instance restarted with the same file: already there is
	// not an error.
	if err := sc.Start(context.Background()); err != nil {
		t.Fatalf("re-applying the startup seed failed: %v", err)
	}

	// Created after startup, so reseeding must remove it.
	if _, err := ts.CreateQueue(tasks.Queue{Name: "projects/seed-proj/locations/us-central1/queues/later"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.CreateSecret("seed-proj", "later", nil, nil, ""); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	api.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/admin/reset?reseed=true", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Reset, Reseeded []string
		Failed          map[string]string
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(body.Failed) != 0 {
		t.Fatalf("reset?reseed=true: %d %+v", resp.StatusCode, body)
	}
	sort.Strings(body.Reseeded)
	if strings.Join(body.Reseeded, ",") != "secretmanager,tasks" {
		t.Errorf("reseeded %v", body.Reseeded)
	}

	queues, _ := ts.AllQueues()
	var qs []string
	for _, q := range queues {
		qs = append(qs, q.Name[strings.LastIndex(q.Name, "/")+1:])
	}
	if strings.Join(qs, ",") != "seeded" {
		t.Errorf("queues after reseed: %v, want only the seeded one", qs)
	}
	secretsLeft, _ := ss.ListSecrets("seed-proj")
	var sn []string
	for _, s := range secretsLeft {
		sn = append(sn, s.Name[strings.LastIndex(s.Name, "/")+1:])
	}
	if strings.Join(sn, ",") != "seeded" {
		t.Errorf("secrets after reseed: %v, want only the seeded one", sn)
	}
	if v, err := ss.AccessVersion("seed-proj", "seeded", "latest"); err != nil || string(v.Payload) != "v1" {
		t.Errorf("the reseeded secret's payload: %q %v", v.Payload, err)
	}
}

// reseed=true is refused, before anything is reset, without a startup seed
// or with a project scope.
func TestReseedIsRefusedBeforeResetting(t *testing.T) {
	api, ts, _ := fixtureAdmin(t)
	if _, err := ts.CreateQueue(tasks.Queue{Name: "projects/seed-proj/locations/us-central1/queues/keep"}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	api.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	for _, q := range []string{"reseed=true", "reseed=true&project=seed-proj"} {
		if q == "reseed=true&project=seed-proj" {
			plan, err := planSeedFile(seedFileConfig(t, seedDoc, config.ServiceTasks, config.ServiceSecrets))
			if err != nil {
				t.Fatal(err)
			}
			api.SetStartupSeed(plan)
		}
		resp, err := http.Post(srv.URL+"/admin/reset?"+q, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, resp.StatusCode)
		}
		if all, _ := ts.AllQueues(); len(all) != 1 {
			t.Errorf("%s: a refused reseed reset things: %d queues left", q, len(all))
		}
	}
}

// `up` with an invalid seed file exits non-zero with the validation error,
// before any cluster step: no kind configuration or credentials are written.
func TestUpRefusesAnInvalidSeedFileBeforeCreatingAnything(t *testing.T) {
	stateDir := t.TempDir()
	seed := filepath.Join(t.TempDir(), "seed.json")
	if err := os.WriteFile(seed, []byte(`{"components": {"tasks": {"queues": ["bad"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "up", "--name", "seedcheck", "--state-dir", stateDir, "--services", "tasks", "--seed-file", seed)
	cmd.Env = append(os.Environ(), runCLIEnv+"=1")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() == 0 {
		t.Fatalf("up with an invalid seed file exited %v:\n%s", err, out)
	}
	if !strings.Contains(string(out), "queues[0]") || !strings.Contains(string(out), "nothing was seeded") {
		t.Errorf("the error does not name the field:\n%s", out)
	}
	for _, f := range []string{"kind.yaml", "credentials.json"} {
		if _, err := os.Stat(filepath.Join(stateDir, "seedcheck", f)); err == nil {
			t.Errorf("%s was written before the seed file was rejected", f)
		}
	}
}
