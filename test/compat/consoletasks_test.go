//go:build compat

package compat

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/api/iterator"
)

// consoleQueueEditForm reads a queue's page from the console and returns the
// values its Edit queue form would submit: every field's default except the
// immutable ones, which the browser does not send.
func consoleQueueEditForm(t *testing.T, addr, project, queue string) map[string]string {
	t.Helper()
	code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/tasks?project="+project+"&name="+queue, "")
	if code != http.StatusOK {
		t.Fatalf("console detail = %d: %s", code, body)
	}
	var detail struct {
		Unavailable string
		Actions     []struct{ ID string }
		Edit        *struct {
			Label  string
			Fields []struct {
				Name, Default string
				Immutable     bool
			}
		}
	}
	if err := json.Unmarshal([]byte(body), &detail); err != nil {
		t.Fatalf("decode detail: %v: %s", err, body)
	}
	if detail.Edit == nil || detail.Edit.Label != "Edit queue" {
		t.Fatalf("the queue page offers no Edit queue (unavailable: %q): %s", detail.Unavailable, body)
	}
	offered := false
	for _, a := range detail.Actions {
		offered = offered || a.ID == "createtask"
	}
	if !offered {
		t.Errorf("the queue page does not offer Create task: %s", body)
	}
	values := map[string]string{}
	for _, f := range detail.Edit.Fields {
		if !f.Immutable {
			values[f.Name] = f.Default
		}
	}
	return values
}

// TestConsoleTasksEditQueueAndCreateTask (#784): the console's Edit queue
// changes the rate limits and retry parameters of a queue the official client
// created, and the official client's GetQueue reads back what was saved; a
// max attempts below -1 is refused with UpdateQueue's own message and changes
// nothing. The console's Create task on the queue's page makes a task the
// official client's ListTasks returns with the URL, method, headers and body
// given, and one with no schedule time is dispatched to a test HTTP target
// with them.
func TestConsoleTasksEditQueueAndCreateTask(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := tasksClient(t, h)
	ctx := h.Context()
	project := h.Project()
	name := queue(t, h, c, "console-edit-q")

	values := consoleQueueEditForm(t, addr, project, name)
	if values["maxDispatchesPerSecond"] == "" || values["maxAttempts"] == "" {
		t.Fatalf("the edit form is not prefilled: %v", values)
	}
	if _, ok := values["maxBurstSize"]; ok {
		t.Error("the edit form submits max burst size, which is output only")
	}
	edit := func(v map[string]string) (int, string) {
		body, _ := json.Marshal(map[string]any{"Path": []string{name}, "Values": v})
		return consoleDo(t, addr, http.MethodPatch, "/api/resources/tasks?project="+project, string(body))
	}

	values["maxDispatchesPerSecond"] = "3.5"
	values["maxConcurrentDispatches"] = "4"
	values["maxAttempts"] = "6"
	values["minBackoff"] = "2s"
	values["maxBackoff"] = "20s"
	values["maxDoublings"] = "2"
	values["maxRetryDuration"] = "5m"
	if code, body := edit(values); code != http.StatusOK {
		t.Fatalf("console edit = %d: %s", code, body)
	}
	got, err := c.GetQueue(ctx, &taskspb.GetQueueRequest{Name: name})
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if rl := got.GetRateLimits(); rl.GetMaxDispatchesPerSecond() != 3.5 || rl.GetMaxConcurrentDispatches() != 4 {
		t.Errorf("GetQueue reads rate limits %v; want 3.5 per second, 4 concurrent", rl)
	}
	if rc := got.GetRetryConfig(); rc.GetMaxAttempts() != 6 || rc.GetMinBackoff().AsDuration() != 2*time.Second ||
		rc.GetMaxBackoff().AsDuration() != 20*time.Second || rc.GetMaxDoublings() != 2 ||
		rc.GetMaxRetryDuration().AsDuration() != 5*time.Minute {
		t.Errorf("GetQueue reads retry config %v; want what the console saved", rc)
	}

	values["maxAttempts"] = "-2"
	code, body := edit(values)
	var refusal struct{ Error, Operation string }
	_ = json.Unmarshal([]byte(body), &refusal)
	if code != http.StatusBadRequest || refusal.Error != "InvalidArgument: retry_config.max_attempts -2 must be -1 (unlimited) or greater" {
		t.Errorf("max attempts -2 = %d %s; want 400 with UpdateQueue's INVALID_ARGUMENT message", code, body)
	}
	if got, err := c.GetQueue(ctx, &taskspb.GetQueueRequest{Name: name}); err != nil || got.GetRetryConfig().GetMaxAttempts() != 6 {
		t.Errorf("after a refused edit GetQueue reads max_attempts %d (%v), want 6", got.GetRetryConfig().GetMaxAttempts(), err)
	}

	// Create task: one scheduled an hour out, which ListTasks returns.
	createTask := func(v map[string]string) (int, string) {
		body, _ := json.Marshal(map[string]any{"Path": []string{name}, "Action": "createtask", "Values": v})
		return consoleDo(t, addr, http.MethodPost, "/api/actions/tasks?project="+project, string(body))
	}
	later := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if code, body := createTask(map[string]string{
		"url": "http://127.0.0.1:9/later", "httpMethod": "PATCH",
		"headers": `{"X-Console":"yes"}`, "body": `{"n":1}`,
		"scheduleTime": later.Format(time.RFC3339), "taskId": "console-later",
	}); code != http.StatusOK {
		t.Fatalf("console Create task = %d: %s", code, body)
	}
	var listed *taskspb.Task
	it := c.ListTasks(ctx, &taskspb.ListTasksRequest{Parent: name, ResponseView: taskspb.Task_FULL})
	for {
		tk, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListTasks: %v", err)
		}
		if tk.GetName() == name+"/tasks/console-later" {
			listed = tk
		}
	}
	if listed == nil {
		t.Fatalf("ListTasks does not return the console-created task %s/tasks/console-later", name)
	}
	hr := listed.GetHttpRequest()
	if hr.GetUrl() != "http://127.0.0.1:9/later" || hr.GetHttpMethod() != taskspb.HttpMethod_PATCH ||
		hr.GetHeaders()["X-Console"] != "yes" || string(hr.GetBody()) != `{"n":1}` ||
		!listed.GetScheduleTime().AsTime().Equal(later) {
		t.Errorf("ListTasks returns the console-created task as %v", listed)
	}

	// And one due now, which is dispatched with what was given.
	tg := newTarget(t, http.StatusOK)
	if code, body := createTask(map[string]string{
		"url": tg.srv.URL + "/now", "httpMethod": "PUT",
		"headers": `{"X-Console":"now"}`, "body": "dispatched",
	}); code != http.StatusOK {
		t.Fatalf("console Create task = %d: %s", code, body)
	}
	d := tg.await(t, 1, 20*time.Second)[0]
	if d.method != http.MethodPut || d.path != "/now" || d.body != "dispatched" || d.header.Get("X-Console") != "now" {
		t.Errorf("the console-created task was delivered as %s %s %q %v", d.method, d.path, d.body, d.header)
	}

	// What CreateTask refuses is refused with its message.
	code, body = createTask(map[string]string{"url": tg.srv.URL, "httpMethod": "GET", "body": "x"})
	if code != http.StatusBadRequest || !strings.Contains(body, "httpRequest.body must be empty") {
		t.Errorf("a GET task with a body = %d %s; want CreateTask's refusal", code, body)
	}
}
