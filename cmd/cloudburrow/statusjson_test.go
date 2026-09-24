package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files in testdata")

func goldenConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Load(config.Options{
		Args:   []string{"--name", "golden", "--state-dir", t.TempDir(), "--services", "storage,pubsub,tasks,secretmanager,bigquery"},
		Getenv: func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func goldenCase(t *testing.T, name string, r statusReport, code int) {
	t.Helper()
	var buf bytes.Buffer
	err := writeStatusJSON(&buf, r, code)
	path := filepath.Join("testdata", "status_"+name+".golden.json")
	if *updateGolden {
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("%v (run go test -update to create it)", rerr)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("%s: the report changed shape. If deliberate, bump statusSchemaVersion and run -update.\ngot:\n%s\nwant:\n%s",
			name, buf.String(), want)
	}
	var exit *exitError
	switch {
	case code == statusExitReady && err != nil:
		t.Errorf("%s: a ready report returned %v", name, err)
	case code != statusExitReady && (!errors.As(err, &exit) || exit.code != code || !exit.quiet):
		t.Errorf("%s: returned %v, want a quiet exit %d", name, err, code)
	}
}

// TestStatusJSONGolden pins the schema, schema_version included, in each
// state a caller branches on, with the exit status each maps to.
func TestStatusJSONGolden(t *testing.T) {
	cfg := goldenConfig(t)
	live := func(rd *readiness) *liveState {
		return &liveState{info: runtimeInfo{PID: 4242, Control: "127.0.0.1:49152", Endpoints: map[string]string{
			"control": "127.0.0.1:49152", "console": "127.0.0.1:9090", "storage": "127.0.0.1:49153", "pubsub": "127.0.0.1:49154",
			"tasks": "127.0.0.1:9003", "secretmanager": "127.0.0.1:9004", "bigquery": "127.0.0.1:9014",
		}}, readiness: rd}
	}
	for _, c := range []struct {
		name string
		live *liveState
		code int
	}{
		{"not_running", nil, statusExitNotRunning},
		{"starting", live(&readiness{State: "starting", Components: map[string]bool{
			"control": true, "cluster": true, "components": false, "forward:storage": false, "tasks": true, "secretmanager": true,
		}}), statusExitNotReady},
		{"failed", live(&readiness{State: "failed", Error: "components: pubsub did not become ready", Components: map[string]bool{
			"control": true, "cluster": true, "components": false,
		}}), statusExitNotReady},
		{"ready", live(&readiness{Ready: true, State: "running", Components: map[string]bool{
			"control": true, "cluster": true, "components": true, "forward:storage": true, "storage-notify": true,
			"forward:pubsub": true, "tasks": true, "secretmanager": true, "forward:bigquery": true, "forward:bigquery-storage": true,
		}}), statusExitReady},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := "running"
			if c.live == nil {
				state = "stopped"
			}
			r, code := buildStatusReport(cfg, c.live, state, "")
			if code != c.code {
				t.Errorf("exit status %d, want %d", code, c.code)
			}
			goldenCase(t, c.name, r, code)
		})
	}
}

// A service is only as ready as the components it depends on, and says which
// one it is waiting for.
func TestStatusJSONNamesWhatAServiceIsWaitingFor(t *testing.T) {
	cfg := goldenConfig(t)
	r, _ := buildStatusReport(cfg, &liveState{info: runtimeInfo{PID: 1, Control: "x"}, readiness: &readiness{
		State: "starting", Components: map[string]bool{"cluster": true, "components": true, "forward:storage": true, "storage-notify": false, "tasks": true},
	}}, "running", "")
	byID := map[string]statusService{}
	for _, s := range r.Services {
		byID[s.ID] = s
	}
	if s := byID["storage"]; s.Ready || s.Reason != "not ready: storage-notify" {
		t.Errorf("storage = %+v, want not ready, waiting for storage-notify", s)
	}
	if s := byID["tasks"]; !s.Ready {
		t.Errorf("tasks = %+v, want ready", s)
	}
}

// The human form is unchanged by default: the same lines, and --format text
// is the same as no flag at all.
func TestStatusHumanOutputIsUnchangedByDefault(t *testing.T) {
	dir := t.TempDir()
	args := []string{"--name", "human", "--state-dir", dir}
	var plain, text strings.Builder
	_ = runStatus(args, &plain, &strings.Builder{})
	_ = runStatus(append([]string{"--format", "text"}, args...), &text, &strings.Builder{})
	if plain.String() != text.String() {
		t.Errorf("--format text differs from the default:\n%s\n---\n%s", plain.String(), text.String())
	}
	for _, want := range []string{"instance:   human\n", "project:    ", "service persistence:\n", "endpoints (as configured):\n"} {
		if !strings.Contains(plain.String(), want) {
			t.Errorf("the human output lost %q:\n%s", want, plain.String())
		}
	}
	if strings.Contains(plain.String(), "schema_version") {
		t.Error("the default output is JSON")
	}
	if err := runStatus(append([]string{"--format", "yaml"}, args...), &strings.Builder{}, &strings.Builder{}); !errors.Is(err, errUsage) {
		t.Errorf("--format yaml returned %v, want a usage error", err)
	}
}

// Persistence is what this instance keeps: volume only in persistent mode.
func TestStatusPersistenceFollowsTheMode(t *testing.T) {
	for mode, want := range map[config.Mode]string{config.ModePersistent: "volume", config.ModeEphemeral: "none"} {
		cfg := config.Default()
		cfg.Mode = mode
		cfg.StateDir = t.TempDir()
		cfg.Services = []config.Service{config.ServiceStorage, config.ServiceDatastore}
		r, _ := buildStatusReport(cfg, nil, "unknown", "")
		for _, s := range r.Services {
			if s.ID == "datastore" && s.Persistence != want {
				t.Errorf("%s mode: datastore persistence = %q, want %q", mode, s.Persistence, want)
			}
		}
	}
}

func TestReadySummaryListsTheSlowComponents(t *testing.T) {
	c := lifecycle.New(time.Second)
	c.Register(sleepy{"cluster", 1100 * time.Millisecond}, sleepy{"fast", 0}, sleepy{"components", 1200 * time.Millisecond})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := readySummary(c)
	if !strings.HasPrefix(got, "ready in 2s: components 1s, cluster 1s") || strings.Contains(got, "fast") {
		t.Errorf("summary = %q", got)
	}
}

type sleepy struct {
	name string
	d    time.Duration
}

func (s sleepy) Name() string                { return s.name }
func (s sleepy) Start(context.Context) error { time.Sleep(s.d); return nil }
func (s sleepy) Stop(context.Context) error  { return nil }
