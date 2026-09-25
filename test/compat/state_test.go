//go:build compat

package compat

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"cloud.google.com/go/storage"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestStateSaveResetLoadRestoresEverything.
//
// `cloudburrow state save` and `state load` (#289), end to end through the
// CLI against the CI instance: a queue, a task, a secret with two versions
// and a project are saved, removed, and loaded back, read through the
// official clients and identical. In CI, Secret Manager keeps its state in
// Kubernetes Secrets, so this also round-trips that backend.
func TestStateSaveResetLoadRestoresEverything(t *testing.T) {
	h := New(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	flags := strings.Fields(os.Getenv(EnvCLIArgs))
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(cli, append(args, flags...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("cloudburrow %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	var st struct {
		ConsoleURL string `json:"console_url"`
		ControlURL string `json:"control_url"`
	}
	out, _ := exec.Command(cli, append([]string{"status", "--format", "json"}, flags...)...).Output()
	if err := json.Unmarshal(out, &st); err != nil || st.ConsoleURL == "" {
		t.Fatalf("status gave no console URL (%v): %s", err, out)
	}
	console := strings.TrimPrefix(st.ConsoleURL, "http://")

	// State: a queue, a task in it, a secret with two versions, a project.
	tc := tasksClient(t, h)
	q := queue(t, h, tc, "state-queue")
	task, err := tc.CreateTask(h.Context(), &taskspb.CreateTaskRequest{Parent: q, Task: &taskspb.Task{
		Name:         q + "/tasks/state-task",
		ScheduleTime: timestamppb.New(time.Now().Add(24 * time.Hour)),
		MessageType:  &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{Url: "http://127.0.0.1:9/never", Body: []byte("kept")}},
	}})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	sc := secretsClient(t, h)
	sec, err := sc.CreateSecret(h.Context(), &secretmanagerpb.CreateSecretRequest{
		Parent: secretsParent(h), SecretId: "state-secret",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}},
	})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() { _ = sc.DeleteSecret(h.Context(), &secretmanagerpb.DeleteSecretRequest{Name: sec.Name}) })
	for _, v := range []string{"first", "second"} {
		if _, err := sc.AddSecretVersion(h.Context(), &secretmanagerpb.AddSecretVersionRequest{
			Parent: sec.Name, Payload: &secretmanagerpb.SecretPayload{Data: []byte(v)}}); err != nil {
			t.Fatal(err)
		}
	}
	project := "st-" + strings.TrimPrefix(h.Project(), "cb-test-")
	if code, body := consoleDo(t, console, http.MethodPost, "/api/resources/projects", fmt.Sprintf(`{"projectId":%q}`, project)); code != 200 {
		t.Fatalf("create project: %d %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, console, http.MethodDelete, "/api/resources/projects?name="+project, "") })

	// Save, and the CLI names what it left out and warns about secrets.
	file := filepath.Join(t.TempDir(), "state.tar.gz")
	saved := run("state", "save", file)
	for _, want := range []string{"captured:     tasks", "captured:     secretmanager", "captured:     projects",
		"captured:     storage", "not captured: pubsub", "contains secret values"} {
		if !strings.Contains(saved, want) {
			t.Errorf("state save output lacks %q:\n%s", want, saved)
		}
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the archive is %v (%v), want mode 0600", fi.Mode(), err)
	}

	// Remove it all.
	control := strings.TrimPrefix(st.ControlURL, "http://")
	if code, body := adminReset(t, control, "service=tasks,secretmanager&project="+h.Project()); code != 200 {
		t.Fatalf("reset: %d %s", code, body)
	}
	consoleDo(t, console, http.MethodDelete, "/api/resources/projects?name="+project, "")
	if _, err := tc.GetQueue(h.Context(), &taskspb.GetQueueRequest{Name: q}); err == nil {
		t.Fatal("the queue survived the reset; the test would prove nothing")
	}

	// Load: everything is back, as it was.
	loaded := run("state", "load", file)
	if !strings.Contains(loaded, "tasks") || !strings.Contains(loaded, "secretmanager") || !strings.Contains(loaded, "projects") {
		t.Errorf("state load output:\n%s", loaded)
	}
	got, err := tc.GetTask(h.Context(), &taskspb.GetTaskRequest{Name: task.Name})
	if err != nil {
		t.Fatalf("the task is not back: %v", err)
	}
	if !got.GetScheduleTime().AsTime().Equal(task.GetScheduleTime().AsTime()) || string(got.GetHttpRequest().GetBody()) != "kept" {
		t.Errorf("the task came back different: %v", got)
	}
	for n, want := range map[string]string{"1": "first", "2": "second", "latest": "second"} {
		v, err := sc.AccessSecretVersion(h.Context(), &secretmanagerpb.AccessSecretVersionRequest{Name: sec.Name + "/versions/" + n})
		if err != nil || string(v.GetPayload().GetData()) != want {
			t.Errorf("version %s: %q (%v), want %q", n, v.GetPayload().GetData(), err, want)
		}
	}
	code, body := consoleDo(t, console, http.MethodGet, "/api/resources/projects", "")
	if code != 200 || !strings.Contains(body, project) {
		t.Errorf("the project is not back: %d %s", code, body)
	}
}

// TestStateRestoresStorage (#290): buckets, objects with bytes and metadata,
// a 10 MiB object streamed through the archive, and a notification
// configuration, saved, reset and loaded back identical, CRC32C included.
func TestStateRestoresStorage(t *testing.T) {
	h := New(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	flags := strings.Fields(os.Getenv(EnvCLIArgs))
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(cli, append(args, flags...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("cloudburrow %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	var st struct {
		ControlURL string `json:"control_url"`
	}
	out, _ := exec.Command(cli, append([]string{"status", "--format", "json"}, flags...)...).Output()
	_ = json.Unmarshal(out, &st)
	control := strings.TrimPrefix(st.ControlURL, "http://")

	sc := storageClient(t, h)
	bucket := h.Project() + "-state"
	if err := sc.Bucket(bucket).Create(h.Context(), h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		it := sc.Bucket(bucket).Objects(h.Context(), nil)
		for o, err := it.Next(); err == nil; o, err = it.Next() {
			_ = sc.Bucket(bucket).Object(o.Name).Delete(h.Context())
		}
		_ = sc.Bucket(bucket).Delete(h.Context())
	})
	write := func(name, contentType string, meta map[string]string, data []byte) *storage.ObjectAttrs {
		t.Helper()
		w := sc.Bucket(bucket).Object(name).NewWriter(h.Context())
		w.ContentType, w.Metadata = contentType, meta
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return w.Attrs()
	}
	big := make([]byte, 10<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	small := write("notes/hello.txt", "text/plain", map[string]string{"owner": "state-test"}, []byte("hello"))
	large := write("blobs/big.bin", "application/octet-stream", nil, big)

	pc := pubsubClient(t, h)
	topicName := topic(t, h, pc, "state-notify")
	n, err := sc.Bucket(bucket).AddNotification(h.Context(), &storage.Notification{
		TopicProjectID: h.Project(), TopicID: topicName[strings.LastIndex(topicName, "/")+1:],
		PayloadFormat: storage.JSONPayload, ObjectNamePrefix: "notes/",
	})
	if err != nil {
		t.Fatalf("AddNotification: %v", err)
	}

	file := filepath.Join(t.TempDir(), "state.tar.gz")
	if saved := run("state", "save", file); !strings.Contains(saved, "captured:     storage") {
		t.Fatalf("storage was not captured:\n%s", saved)
	}
	if code, body := adminReset(t, control, "service=storage"); code != 200 {
		t.Fatalf("reset: %d %s", code, body)
	}
	if _, err := sc.Bucket(bucket).Attrs(h.Context()); err == nil {
		t.Fatal("the bucket survived the reset; the test would prove nothing")
	}
	run("state", "load", file)

	for _, want := range []*storage.ObjectAttrs{small, large} {
		got, err := sc.Bucket(bucket).Object(want.Name).Attrs(h.Context())
		if err != nil {
			t.Fatalf("%s is not back: %v", want.Name, err)
		}
		if got.CRC32C != want.CRC32C || got.Size != want.Size || got.ContentType != want.ContentType {
			t.Errorf("%s came back as %d bytes, %s, CRC32C %08x; saved %d, %s, %08x",
				want.Name, got.Size, got.ContentType, got.CRC32C, want.Size, want.ContentType, want.CRC32C)
		}
	}
	if got, _ := sc.Bucket(bucket).Object(small.Name).Attrs(h.Context()); got == nil || got.Metadata["owner"] != "state-test" {
		t.Errorf("object metadata not restored: %+v", got)
	}
	r, err := sc.Bucket(bucket).Object(large.Name).NewReader(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	back, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(back, big) {
		t.Error("the 10 MiB object's bytes differ after the round trip")
	}
	notes, err := sc.Bucket(bucket).Notifications(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	restored, ok := notes[n.ID]
	if !ok || restored.ObjectNamePrefix != "notes/" || !strings.HasSuffix(restored.TopicID, "state-notify") {
		t.Errorf("notification config not restored: %+v", notes)
	}
}

// TestStateRestoresVersionsAndHolds: on the builtin server (#511) a state
// save and load keeps every generation of an object and its holds, which
// the fake-gcs-server snapshot, re-uploading live objects, cannot.
func TestStateRestoresVersionsAndHolds(t *testing.T) {
	builtinOnly(t, "the fake-gcs-server snapshot re-uploads live objects only")
	h := New(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	flags := strings.Fields(os.Getenv(EnvCLIArgs))
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(cli, append(args, flags...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("cloudburrow %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	var st struct {
		ControlURL string `json:"control_url"`
	}
	out, _ := exec.Command(cli, append([]string{"status", "--format", "json"}, flags...)...).Output()
	_ = json.Unmarshal(out, &st)
	control := strings.TrimPrefix(st.ControlURL, "http://")

	sc := storageClient(t, h)
	ctx := h.Context()
	bh := sc.Bucket(h.Project() + "-state-versions")
	if err := bh.Create(ctx, h.Project(), &storage.BucketAttrs{VersioningEnabled: true}); err != nil {
		t.Fatal(err)
	}
	o := bh.Object("doc.txt")
	first := putObject(t, ctx, o, "one")
	second := putObject(t, ctx, o, "two")
	if _, err := o.Update(ctx, storage.ObjectAttrsToUpdate{TemporaryHold: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = o.Update(context.Background(), storage.ObjectAttrsToUpdate{TemporaryHold: false})
		emptyAndDelete(context.Background(), bh)
	})

	file := filepath.Join(t.TempDir(), "state.tar.gz")
	run("state", "save", file)
	if code, body := adminReset(t, control, "service=storage"); code != 200 {
		t.Fatalf("reset: %d %s", code, body)
	}
	run("state", "load", file)

	all := versionsOf(t, ctx, bh, true)
	if len(all) != 2 || all[0].Generation != first.Generation || all[1].Generation != second.Generation {
		t.Fatalf("after the load: %d versions; want generations %d and %d", len(all), first.Generation, second.Generation)
	}
	a, err := o.Attrs(ctx)
	if err != nil || !a.TemporaryHold || a.Metageneration != 2 {
		t.Errorf("the live version after the load = %+v, %v; want its hold and metageneration 2", a, err)
	}
}
