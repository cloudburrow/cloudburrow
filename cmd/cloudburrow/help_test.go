package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helpCommands are the commands whose --help CloudBurrow prints itself.
// terraform is not one: `cloudburrow terraform --help` is terraform's own.
var helpCommands = []string{
	"delete", "diagnose", "doctor", "env", "events", "gcloud-setup", "gcloud-teardown", "logs",
	"prefetch", "reset", "seed", "state", "status", "stop", "storage-server", "trust", "up", "wait",
}

// The help text is the first thing a new user reads, and nothing pinned it,
// so it drifted: --services said "(default all)", gcloud-setup named two of
// its overrides, -local-ai-image printed "make litert-lm" as its type and
// status did not mention --format (#708). Each command's --help, and the
// top-level usage, is a golden in testdata; `go test -update` rewrites them.
func TestHelpGoldens(t *testing.T) {
	check := func(name string, args ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if err := run(args, &stdout, &stderr); err != nil {
			t.Errorf("cloudburrow %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
			return
		}
		path := filepath.Join("testdata", "help_"+name+".golden")
		if *updateGolden {
			if err := os.WriteFile(path, stdout.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			return
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run go test -update to create it)", err)
		}
		if got := stdout.String(); got != string(want) {
			t.Errorf("cloudburrow %s changed; if deliberate, run go test -update.\ngot:\n%s", strings.Join(args, " "), got)
		}
	}
	check("usage", "--help")
	for _, c := range helpCommands {
		check(c, c, "--help")
	}
}

// The specific strings #708 corrected, so a golden update cannot quietly
// bring them back.
func TestHelpNamesDefaultsAndFormats(t *testing.T) {
	help := func(args ...string) string {
		var stdout, stderr bytes.Buffer
		if err := run(args, &stdout, &stderr); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return stdout.String()
	}
	up := help("up", "--help")
	if strings.Contains(up, "(default all)") || !strings.Contains(up, "default storage,pubsub,tasks,run,secretmanager") {
		t.Errorf("up --help does not name the default services:\n%s", up)
	}
	if strings.Contains(up, "-local-ai-image make") || !strings.Contains(up, "-local-ai-image string") {
		t.Errorf("up --help shows -local-ai-image's argument wrong")
	}
	for _, v := range gcloudVerified {
		if !strings.Contains(help("gcloud-setup", "--help"), v.property) {
			t.Errorf("gcloud-setup --help does not name the %s override", v.property)
		}
	}
	if !strings.Contains(help("status", "--help"), "-format text|json") {
		t.Error("status --help does not document --format")
	}
	if u := help("--help"); strings.Contains(u, "in development") {
		t.Errorf("the usage still calls storage-server in development:\n%s", u)
	}
}
