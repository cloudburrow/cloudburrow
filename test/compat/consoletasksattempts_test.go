//go:build compat

package compat

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/protobuf/types/known/durationpb"
)

// TestConsoleTasksAttemptHistory (#1576): each dispatch attempt of a task
// made with the official Cloud Tasks client is a line in the console's
// logs, under the queue and the queue's project (the filter the queue's
// page and a project's Logs Explorer use), saying the attempt's number and
// outcome: a 500 a WARNING, a 200 INFO.
// covers: google.cloud.tasks.v2.CloudTasks/CreateTask
func TestConsoleTasksAttemptHistory(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	q := queue(t, h, c, "attempts-q")
	if _, err := c.UpdateQueue(ctx, &taskspb.UpdateQueueRequest{Queue: &taskspb.Queue{Name: q,
		RetryConfig: &taskspb.RetryConfig{MaxAttempts: 2, MinBackoff: durationpb.New(100 * time.Millisecond), MaxBackoff: durationpb.New(100 * time.Millisecond)}}}); err != nil {
		t.Fatalf("UpdateQueue: %v", err)
	}

	failing, ok := newTarget(t, http.StatusInternalServerError), newTarget(t, http.StatusOK)
	bad, good := q+"/tasks/fails-twice", q+"/tasks/succeeds"
	for name, tg := range map[string]*target{bad: failing, good: ok} {
		if _, err := c.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: q, Task: &taskspb.Task{Name: name,
			MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{Url: tg.srv.URL + "/", HttpMethod: taskspb.HttpMethod_POST}}}}); err != nil {
			t.Fatalf("CreateTask %s: %v", name, err)
		}
	}
	failing.await(t, 2, 20*time.Second)
	ok.await(t, 1, 20*time.Second)

	want := map[string]string{
		"attempt 1 of " + bad + " returned 500":  "WARNING",
		"attempt 2 of " + bad + " returned 500":  "WARNING",
		"attempt 1 of " + good + " returned 200": "INFO",
	}
	query := url.Values{"project": {h.Project()}, "resource": {q}, "source": {"tasks"}}
	var got map[string]string
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		code, body := consoleDo(t, addr, http.MethodGet, "/api/logs?"+query.Encode(), "")
		var resp struct {
			Entries []struct {
				Severity, Source, Project, Resource, Message string
			} `json:"entries"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &resp) != nil {
			t.Fatalf("/api/logs = %d %s", code, body)
		}
		got = map[string]string{}
		for _, e := range resp.Entries {
			if e.Source == "tasks" && e.Resource == q && e.Project == h.Project() && strings.HasPrefix(e.Message, "attempt ") {
				got[e.Message] = e.Severity
			}
		}
		missing := false
		for m, sev := range want {
			missing = missing || got[m] != sev
		}
		if !missing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the queue's attempt history in the console under project %s = %v, want %v", h.Project(), got, want)
		}
	}
}
