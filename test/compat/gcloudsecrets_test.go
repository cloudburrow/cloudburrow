//go:build compat

package compat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGcloudSecrets (#590): through the configuration `cloudburrow
// gcloud-setup` writes and nothing else, the real gcloud creates a secret,
// adds a version (gcloud sends its CRC32C), reads it back (gcloud verifies
// the returned CRC32C), updates labels, disables the version and deletes
// the secret.
func TestGcloudSecrets(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvSecrets)
	g := newGcloudSession(t, h)
	id := "gcloud-secret"
	t.Cleanup(func() { _, _ = g.run(nil, "secrets", "delete", id, "--quiet") })
	g.must("secrets", "create", id, "--replication-policy=automatic", "--labels=env=dev")
	payload := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(payload, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	g.must("secrets", "versions", "add", id, "--data-file="+payload)
	if got := strings.TrimSpace(g.must("secrets", "versions", "access", "latest", "--secret="+id)); got != "s3cret" {
		t.Errorf("versions access = %q", got)
	}
	g.must("secrets", "update", id, "--update-labels=team=a")
	if got := g.must("secrets", "describe", id, "--format=value(labels)"); !strings.Contains(got, "env=dev") || !strings.Contains(got, "team=a") {
		t.Errorf("labels after update = %q", got)
	}
	g.must("secrets", "versions", "disable", "1", "--secret="+id)
	if got := g.must("secrets", "versions", "list", id, "--format=value(name,state)"); !strings.Contains(got, "disabled") {
		t.Errorf("versions list after disable = %q", got)
	}
	g.must("secrets", "delete", id, "--quiet")
}

// TestGcloudTasksQueues (#590): gcloud tasks queues through gcloud-setup's
// configuration; creating a task is UNIMPLEMENTED on the JSON surface, and
// says so rather than reaching Google.
func TestGcloudTasksQueues(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvTasks)
	g := newGcloudSession(t, h)
	q := "gcloud-queue"
	loc := "--location=us-central1"
	t.Cleanup(func() { _, _ = g.run(nil, "tasks", "queues", "delete", q, loc, "--quiet") })
	g.must("tasks", "queues", "create", q, loc)
	g.must("tasks", "queues", "pause", q, loc)
	if got := g.must("tasks", "queues", "describe", q, loc, "--format=value(state)"); !strings.Contains(got, "PAUSED") {
		t.Errorf("state after pause = %q", got)
	}
	g.must("tasks", "queues", "resume", q, loc)
	if got := g.must("tasks", "queues", "list", loc, "--format=value(name)"); !strings.Contains(got, q) {
		t.Errorf("queues list = %q", got)
	}
	out, err := g.run(nil, "tasks", "create-http-task", "--queue="+q, loc, "--url=http://127.0.0.1:1/")
	if err == nil || !strings.Contains(out, "501") {
		t.Errorf("create-http-task = %v %q; want a 501 from the local JSON surface", err, out)
	}
	g.must("tasks", "queues", "delete", q, loc, "--quiet")
}
