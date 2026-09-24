//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
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
		"not captured: storage", "not captured: pubsub", "contains secret values"} {
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
