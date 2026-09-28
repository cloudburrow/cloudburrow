//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// consoleJobDetail is what the console's detail route answers for a Cloud
// Run job or execution, as much of it as these tests read.
type consoleJobDetail struct {
	Unavailable string
	Summary     []struct{ Label, Value string }
	Actions     []struct{ ID string }
	Sections    []struct {
		ID, Text, Unavailable string
		Listing               struct {
			Items []struct {
				Name, Status string
				Actions      []struct{ ID string }
			}
		}
	}
}

func (d consoleJobDetail) summary(label string) string {
	for _, p := range d.Summary {
		if p.Label == label {
			return p.Value
		}
	}
	return ""
}

func (d consoleJobDetail) offers(id string) bool {
	for _, a := range d.Actions {
		if a.ID == id {
			return true
		}
	}
	return false
}

func consoleDetailOf(t *testing.T, addr, service, project string, path ...string) consoleJobDetail {
	t.Helper()
	q := url.Values{"project": {project}}
	for _, seg := range path {
		q.Add("name", seg)
	}
	code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/"+service+"?"+q.Encode(), "")
	if code != http.StatusOK {
		t.Fatalf("console detail %s %v = %d: %s", service, path, code, body)
	}
	var d consoleJobDetail
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("decode detail: %v: %s", err, body)
	}
	if d.Unavailable != "" {
		t.Fatalf("console detail %s %v is unavailable: %s", service, path, d.Unavailable)
	}
	return d
}

func consoleActAt(t *testing.T, addr, service, project, action string, path ...string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"Path": path, "Action": action})
	if code, resp := consoleDo(t, addr, http.MethodPost, "/api/actions/"+service+"?project="+project, string(body)); code != http.StatusOK {
		t.Fatalf("console %s on %v = %d: %s", action, path, code, resp)
	}
}

// TestConsoleRunJobsCreateExecuteAndCancel (#785): a job created on the
// console's Jobs page is read back by the official JobsClient with the
// form's settings; Execute from the console starts an execution the official
// ExecutionsClient lists and reads to success, whose console page shows its
// task counts and its tasks' output. Edited from the console to fail, its next
// execution's page shows the failed condition and the pod's own message.
// Edited to run until stopped, its execution offers Cancel and not Delete
// while it runs; Cancel from the console leaves it cancelled as the client
// reads it, and it then offers Delete and not Cancel. The console's delete of
// the execution and of the job are NOT_FOUND through the clients.
func TestConsoleRunJobsCreateExecuteAndCancel(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	jc, xc := runJobClients(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)
	project := h.Project()
	id := "compat-console-job"
	name := runParent(h) + "/jobs/" + id
	t.Cleanup(func() { _, _ = jc.DeleteJob(context.Background(), &runpb.DeleteJobRequest{Name: name}) })

	form := map[string]string{
		"name": id, "image": jobImage, "command": "sh -c",
		"args":      `'echo "console task $CLOUD_RUN_TASK_INDEX of $CLOUD_RUN_TASK_COUNT"'`,
		"env":       `{"GREETING":"console"}`,
		"taskCount": "2", "maxRetries": "0", "timeout": "180",
	}
	body, _ := json.Marshal(form)
	if code, resp := consoleDo(t, addr, http.MethodPost, "/api/resources/run-jobs?project="+project, string(body)); code != http.StatusOK {
		t.Fatalf("console create job = %d: %s", code, resp)
	}
	job, err := jc.GetJob(ctx, &runpb.GetJobRequest{Name: name})
	if err != nil {
		t.Fatalf("GetJob of the console's job: %v", err)
	}
	c := job.GetTemplate().GetTemplate().GetContainers()[0]
	if c.GetImage() != jobImage || job.GetTemplate().GetTaskCount() != 2 ||
		strings.Join(c.GetCommand(), " ") != "sh -c" || len(c.GetArgs()) != 1 ||
		c.GetArgs()[0] != `echo "console task $CLOUD_RUN_TASK_INDEX of $CLOUD_RUN_TASK_COUNT"` ||
		job.GetTemplate().GetTemplate().GetMaxRetries() != 0 ||
		job.GetTemplate().GetTemplate().GetTimeout().AsDuration() != 180*time.Second {
		t.Errorf("the console's job reads back as %v", job)
	}

	// Execute, and follow the execution through the official client.
	run := func() *runpb.Execution {
		t.Helper()
		before := map[string]bool{}
		for it := xc.ListExecutions(ctx, &runpb.ListExecutionsRequest{Parent: name}); ; {
			e, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("ListExecutions: %v", err)
			}
			before[e.GetName()] = true
		}
		consoleActAt(t, addr, "run-jobs", project, "execute", id)
		for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
			it := xc.ListExecutions(ctx, &runpb.ListExecutionsRequest{Parent: name})
			for {
				e, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatalf("ListExecutions: %v", err)
				}
				if !before[e.GetName()] {
					return e
				}
			}
		}
		t.Fatal("the console's Execute started no execution the ExecutionsClient lists")
		return nil
	}
	waitFor := func(e *runpb.Execution, what string, done func(*runpb.Execution) bool) *runpb.Execution {
		t.Helper()
		for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(2 * time.Second) {
			got, err := xc.GetExecution(ctx, &runpb.GetExecutionRequest{Name: e.GetName()})
			if err != nil {
				t.Fatalf("GetExecution: %v", err)
			}
			if done(got) {
				return got
			}
			if time.Now().After(deadline) {
				t.Fatalf("execution %s never %s: %v", e.GetName(), what, got)
			}
		}
	}
	short := func(e *runpb.Execution) string { return e.GetName()[strings.LastIndex(e.GetName(), "/")+1:] }

	first := waitFor(run(), "succeeded", func(e *runpb.Execution) bool { return e.GetCompletionTime() != nil })
	if first.GetSucceededCount() != 2 || completedCondition(first).GetState() != runpb.Condition_CONDITION_SUCCEEDED {
		t.Fatalf("the console's first execution = %v; want both tasks succeeded", first)
	}
	page := consoleDetailOf(t, addr, "run-jobs", project, id, short(first))
	if page.summary("Status") != "Succeeded" || page.summary("Succeeded") != "2" || page.summary("Tasks") != "2" {
		t.Errorf("the execution page's summary = %v", page.Summary)
	}
	if !page.offers("delete") || page.offers("cancel") {
		t.Errorf("a finished execution offers %v; want Delete and not Cancel", page.Actions)
	}
	for _, s := range page.Sections {
		if s.ID == "logs" && !strings.Contains(s.Text, "console task 0 of 2") {
			t.Errorf("the execution's Logs tab lacks task 0's output (unavailable %q):\n%s", s.Unavailable, s.Text)
		}
	}
	jobPage := consoleDetailOf(t, addr, "run-jobs", project, id)
	if jobPage.summary("Status") != "Succeeded" || !jobPage.offers("execute") {
		t.Errorf("the job page reads %v, offering %v", jobPage.Summary, jobPage.Actions)
	}

	// Edited from the console to fail: the page says why, in the pod's words.
	edit := func(args, tasks string) {
		t.Helper()
		values := map[string]string{}
		for k, v := range form {
			if k != "name" {
				values[k] = v
			}
		}
		values["args"], values["taskCount"] = args, tasks
		b, _ := json.Marshal(map[string]any{"Path": []string{id}, "Values": values})
		if code, resp := consoleDo(t, addr, http.MethodPatch, "/api/resources/run-jobs?project="+project, string(b)); code != http.StatusOK {
			t.Fatalf("console edit job = %d: %s", code, resp)
		}
	}
	edit(`'echo "console job failed: table users is missing"; exit 3'`, "1")
	if job, err := jc.GetJob(ctx, &runpb.GetJobRequest{Name: name}); err != nil || job.GetGeneration() != 2 ||
		job.GetTemplate().GetTaskCount() != 1 {
		t.Errorf("after the console's edit GetJob = %v, %v; want generation 2 with one task", job, err)
	}
	failed := waitFor(run(), "finished", func(e *runpb.Execution) bool { return e.GetCompletionTime() != nil })
	if completedCondition(failed).GetState() != runpb.Condition_CONDITION_FAILED {
		t.Fatalf("the failing execution = %v", failed)
	}
	page = consoleDetailOf(t, addr, "run-jobs", project, id, short(failed))
	if page.summary("Status") != "Failed" || !strings.Contains(page.summary("Failure"), "exited with code 3") ||
		!strings.Contains(page.summary("Failure"), "table users is missing") {
		t.Errorf("the failed execution's page = %v; want Failed with the pod's message", page.Summary)
	}
	t.Logf("failed execution's page says: %s", page.summary("Failure"))

	// Edited to run until stopped: Cancel while it runs.
	edit(`'trap "exit 143" TERM; sleep 300 & wait'`, "1")
	running := waitFor(run(), "ran", func(e *runpb.Execution) bool { return e.GetRunningCount() > 0 })
	page = consoleDetailOf(t, addr, "run-jobs", project, id, short(running))
	if !page.offers("cancel") || page.offers("delete") {
		t.Errorf("a running execution offers %v; want Cancel and not Delete", page.Actions)
	}
	consoleActAt(t, addr, "run-jobs", project, "cancel", id, short(running))
	cancelled := waitFor(running, "stopped", func(e *runpb.Execution) bool { return e.GetCompletionTime() != nil })
	if completedCondition(cancelled).GetExecutionReason() != runpb.Condition_CANCELLED || cancelled.GetCancelledCount() != 1 {
		t.Errorf("after the console's Cancel the execution reads %v; want it cancelled", cancelled)
	}
	page = consoleDetailOf(t, addr, "run-jobs", project, id, short(cancelled))
	if page.summary("Status") != "Cancelled" || page.offers("cancel") || !page.offers("delete") {
		t.Errorf("the cancelled execution's page reads %v offering %v", page.Summary, page.Actions)
	}

	// Deletes, from the console, are gone through the clients.
	consoleActAt(t, addr, "run-jobs", project, "delete", id, short(cancelled))
	if _, err := xc.GetExecution(ctx, &runpb.GetExecutionRequest{Name: cancelled.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetExecution after the console's delete = %v, want NotFound", err)
	}
	if code, resp := consoleDo(t, addr, http.MethodDelete, "/api/resources/run-jobs?project="+project+"&name="+id, ""); code != http.StatusOK {
		t.Fatalf("console delete job = %d: %s", code, resp)
	}
	if _, err := jc.GetJob(ctx, &runpb.GetJobRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetJob after the console's delete = %v, want NotFound", err)
	}
}
