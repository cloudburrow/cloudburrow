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

// covers: google.cloud.tasks.v2.CloudTasks/CreateQueue, google.cloud.tasks.v2.CloudTasks/PauseQueue, google.cloud.tasks.v2.CloudTasks/GetQueue, google.cloud.tasks.v2.CloudTasks/ListQueues, google.cloud.tasks.v2.CloudTasks/CreateTask, google.cloud.tasks.v2.CloudTasks/ListTasks, google.cloud.tasks.v2.CloudTasks/GetTask, google.cloud.tasks.v2.CloudTasks/DeleteTask, google.cloud.tasks.v2.CloudTasks/ResumeQueue, google.cloud.tasks.v2.CloudTasks/DeleteQueue, google.cloud.tasks.v2.CloudTasks/UpdateQueue
//
// TestGcloudTasks (#590, #591, #692): gcloud tasks through gcloud-setup's
// configuration: queues by the hand-written JSON routes, updated in place
// through the transcoder's PATCH, and tasks, created
// with and without a name, listed, described and deleted, through the
// transcoder. `gcloud tasks run` is RunTask, which is not implemented, and
// gcloud retries its 501 for minutes, so it is not called.
func TestGcloudTasks(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvTasks)
	g := newGcloudSession(t, h)
	q := "gcloud-queue"
	loc := "--location=us-central1"
	t.Cleanup(func() { _, _ = g.run(nil, "tasks", "queues", "delete", q, loc, "--quiet") })
	g.must("tasks", "queues", "create", q, loc)
	// In place, by PATCH with an updateMask of the flags given (#692).
	g.must("tasks", "queues", "update", q, loc, "--max-attempts=7", "--max-concurrent-dispatches=3")
	if got := g.must("tasks", "queues", "describe", q, loc,
		"--format=value(retryConfig.maxAttempts,rateLimits.maxConcurrentDispatches,retryConfig.minBackoff)"); !strings.HasPrefix(strings.TrimSpace(got), "7\t3\t0.100s") {
		t.Errorf("describe after update = %q; want max attempts 7, 3 concurrent dispatches and min backoff unchanged", got)
	}
	g.must("tasks", "queues", "pause", q, loc)
	if got := g.must("tasks", "queues", "describe", q, loc, "--format=value(state)"); !strings.Contains(got, "PAUSED") {
		t.Errorf("state after pause = %q", got)
	}
	if got := g.must("tasks", "queues", "list", loc, "--format=value(name)"); !strings.Contains(got, q) {
		t.Errorf("queues list = %q", got)
	}
	// Paused, so the tasks stay put for the reads.
	g.must("tasks", "create-http-task", "t1", "--queue="+q, loc, "--url=http://127.0.0.1:1/",
		"--method=PUT", "--body-content=hi", "--header=X-Test:1")
	g.must("tasks", "create-http-task", "--queue="+q, loc, "--url=http://127.0.0.1:1/")
	if got := g.must("tasks", "list", "--queue="+q, loc, "--format=value(name)"); len(strings.Fields(got)) != 2 || !strings.Contains(got, "t1") {
		t.Errorf("tasks list = %q; want t1 and a generated name", got)
	}
	got := g.must("tasks", "describe", "t1", "--queue="+q, loc, "--response-view=full",
		"--format=value(httpRequest.httpMethod,httpRequest.body,httpRequest.headers)")
	if !strings.Contains(got, "PUT	aGk=") || !strings.Contains(got, "X-Test") {
		t.Errorf("describe t1 = %q", got)
	}
	g.must("tasks", "delete", "t1", "--queue="+q, loc, "--quiet")
	if out, err := g.run(nil, "tasks", "describe", "t1", "--queue="+q, loc); err == nil || !strings.Contains(out, "NOT_FOUND") {
		t.Errorf("describe after delete = %v %q; want NOT_FOUND", err, out)
	}
	g.must("tasks", "queues", "resume", q, loc)
	g.must("tasks", "queues", "delete", q, loc, "--quiet")
}
