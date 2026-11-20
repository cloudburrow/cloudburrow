package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/workflows/apiv1/workflowspb"
	"cloud.google.com/go/workflows/executions/apiv1/executionspb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
	"github.com/cloudburrow/cloudburrow/internal/service/workflows"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"google.golang.org/grpc"
)

// workflowsConsole is the Workflows page over an in-process Workflows
// whose executions log to an in-process Cloud Logging through its JSON
// API, as `up` wires them.
func workflowsConsole(t *testing.T) (*httptest.Server, *workflows.Server) {
	t.Helper()
	st := logging.NewStore(1000)
	logs := &loggingService{store: st, api: logging.NewServer(st), admin: logging.NewAdmin(store.NewMemory())}
	logs.api.UseAdmin(logs.admin)
	logREST := httptest.NewServer(logging.NewRESTHandler(func(g grpc.ServiceRegistrar) { logs.api.Register(g) }))
	t.Cleanup(logREST.Close)
	local := func(api string) string {
		if api == "logging" {
			return logREST.URL
		}
		return ""
	}
	api, err := workflows.NewServer("", workflows.Options{Local: local, Log: workflowsLogger(local)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(api.Close)
	srv := httptest.NewServer(console.New("127.0.0.1:0", nil, workflowsProvider{svc: &workflowsService{api: api}, logs: logs}).Handler())
	t.Cleanup(srv.Close)
	return srv, api
}

func workflowsSection(d console.Detail, id string) *console.Section {
	for i := range d.Sections {
		if d.Sections[i].ID == id {
			return &d.Sections[i]
		}
	}
	return nil
}

// TestConsoleWorkflows (#1986, #2004): Create workflow's form has Google's
// fields in its order (the editor last, as a "workflowsource" field), and
// what it submits is what the API holds, encryption, call log level,
// execution history and environment variables included; the Source tab is
// the "workflows" language; Edit changes them; an execution has Google's
// Summary (with its input and output), Steps (the step entries with the
// revision's source for the graph) and Logs (its entries from Cloud
// Logging) tabs.
func TestConsoleWorkflows(t *testing.T) {
	srv, api := workflowsConsole(t)
	ctx := context.Background()
	const project = "proj-wf"

	title, fields := workflowsProvider{}.CreateForm()
	var names []string
	for _, f := range fields {
		names = append(names, f.Name)
	}
	if want := "workflowId location description serviceAccount encryption cryptoKeyName callLogLevel executionHistoryLevel userEnvVars labels sourceContents"; title != "Create workflow" || strings.Join(names, " ") != want {
		t.Fatalf("Create workflow form = %q %v; want %s", title, names, want)
	}
	if src := fields[len(fields)-1]; src.Type != "workflowsource" || !strings.Contains(src.Default, "main:") {
		t.Errorf("the editor field = %+v", src)
	}

	source := "main:\n  params: [input]\n  steps:\n    - log:\n        call: sys.log\n        args:\n          text: ${\"n is \" + string(input.n)}\n    - double:\n        return: ${input.n * 2}\n"
	base := srv.URL + "/api/resources/workflows?project=" + project
	if code, out := consolePost(t, base, map[string]string{"workflowId": "wf1", "location": "us-east1", "description": "d",
		"encryption": "google", "callLogLevel": "LOG_ALL_CALLS", "executionHistoryLevel": "EXECUTION_HISTORY_DETAILED",
		"userEnvVars": `{"COLOR":"blue"}`, "labels": `{"team":"a"}`, "sourceContents": source}); code != http.StatusOK {
		t.Fatalf("create = %d %s", code, out)
	}
	name := "projects/" + project + "/locations/us-east1/workflows/wf1"
	w, err := api.Workflows().GetWorkflow(ctx, &workflowspb.GetWorkflowRequest{Name: name})
	if err != nil || w.GetCallLogLevel() != workflowspb.Workflow_LOG_ALL_CALLS ||
		w.GetExecutionHistoryLevel() != workflowspb.ExecutionHistoryLevel_EXECUTION_HISTORY_DETAILED ||
		w.GetUserEnvVars()["COLOR"] != "blue" || w.GetLabels()["team"] != "a" || w.GetCryptoKeyName() != "" {
		t.Fatalf("the API reads %v, %v", w, err)
	}
	if code, out := consolePost(t, base, map[string]string{"workflowId": "wf2", "location": "us-east1", "encryption": "cmek",
		"sourceContents": source}); code == http.StatusOK || !strings.Contains(out, "Cloud KMS key") {
		t.Errorf("Cloud KMS key chosen with no key = %d %s; want refused", code, out)
	}

	detail := func(path ...string) console.Detail {
		t.Helper()
		var d console.Detail
		getJSON200(t, srv.URL+"/api/detail/workflows?"+url.Values{"project": {project}, "name": path}.Encode(), &d)
		return d
	}
	d := detail(name)
	var tabs []string
	for _, s := range d.Sections {
		tabs = append(tabs, s.Label)
	}
	if got := strings.Join(tabs, ","); got != "Details,Executions,Source,Revisions,Logs" {
		t.Errorf("a workflow's tabs = %s", got)
	}
	if s := workflowsSection(d, "source"); s == nil || s.Language != "workflows" || s.Text != source {
		t.Errorf("the Source tab = %+v", s)
	}
	b, _ := json.Marshal(workflowsSection(d, "details"))
	for _, want := range []string{"All calls", "Detailed", "Google-managed encryption key", "COLOR=blue"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the Details tab lacks %q: %s", want, b)
		}
	}

	// Edit: the identity is shown and not changed; the rest is the API's.
	if code, out := consolePost(t, srv.URL+"/api/actions/workflows?project="+project, map[string]any{"Path": []string{name}, "Action": "edit",
		"Values": map[string]string{"workflowId": "wf1", "location": "us-east1", "description": "d2", "encryption": "google",
			"callLogLevel": "LOG_ERRORS_ONLY", "executionHistoryLevel": "EXECUTION_HISTORY_BASIC", "userEnvVars": `{"COLOR":"red"}`,
			"labels": `{}`, "sourceContents": source}}); code != http.StatusOK {
		t.Fatalf("edit = %d %s", code, out)
	}
	w, _ = api.Workflows().GetWorkflow(ctx, &workflowspb.GetWorkflowRequest{Name: name})
	if w.GetDescription() != "d2" || w.GetCallLogLevel() != workflowspb.Workflow_LOG_ERRORS_ONLY ||
		w.GetExecutionHistoryLevel() != workflowspb.ExecutionHistoryLevel_EXECUTION_HISTORY_BASIC || w.GetUserEnvVars()["COLOR"] != "red" {
		t.Errorf("after Edit the API reads %v", w)
	}
	var edit *console.Action
	for _, a := range d.Actions {
		if a.ID == "edit" {
			edit = &a
		}
	}
	if edit == nil || len(edit.Fields) == 0 || !edit.Fields[0].Immutable || edit.Fields[len(edit.Fields)-1].Type != "workflowsource" {
		t.Errorf("Edit = %+v", edit)
	}

	if code, out := consolePost(t, srv.URL+"/api/actions/workflows?project="+project, map[string]any{"Path": []string{name}, "Action": "execute",
		"Values": map[string]string{"argument": `{"n": 21}`, "callLogLevel": "LOG_ALL_CALLS"}}); code != http.StatusOK {
		t.Fatalf("execute = %d %s", code, out)
	}
	execs, err := api.Executions().ListExecutions(ctx, &executionspb.ListExecutionsRequest{Parent: name})
	if err != nil || len(execs.GetExecutions()) != 1 {
		t.Fatalf("executions = %v, %v", execs, err)
	}
	exec := execs.GetExecutions()[0].GetName()
	deadline := time.Now().Add(10 * time.Second)
	var ed console.Detail
	for {
		ed = detail(name, exec)
		logs := workflowsSection(ed, "logs")
		// The end of the execution is logged after its state is set.
		if strings.Contains(ed.Summary[0].Value, "SUCCEEDED") && logs != nil && len(logs.Listing.Items) >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the execution's page = %+v", ed)
		}
		time.Sleep(50 * time.Millisecond)
	}
	tabs = nil
	for _, s := range ed.Sections {
		tabs = append(tabs, s.Label)
	}
	if got := strings.Join(tabs, ","); got != "Summary,Steps,Logs" {
		t.Errorf("an execution's tabs = %s; want Google's Summary, Steps, Logs", got)
	}
	b, _ = json.Marshal(workflowsSection(ed, "summary"))
	for _, want := range []string{`"Input"`, `{\"n\": 21}`, `"Output"`, `"42"`, "Execution duration", "All calls"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the Summary tab lacks %s: %s", want, b)
		}
	}
	steps := workflowsSection(ed, "steps")
	if steps == nil || steps.Graph != source || len(steps.Listing.Items) < 2 {
		t.Fatalf("the Steps tab = %+v", steps)
	}
	var stepNames []string
	for _, it := range steps.Listing.Items {
		stepNames = append(stepNames, it.Name)
	}
	if got := strings.Join(stepNames, " "); !strings.Contains(got, "log") || !strings.Contains(got, "double") {
		t.Errorf("step entries = %s", got)
	}
	b, _ = json.Marshal(workflowsSection(ed, "logs"))
	for _, want := range []string{"n is 21", "executions_system_log"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the Logs tab lacks %q: %s", want, b)
		}
	}
	// The workflow's own Logs tab holds its executions' entries too.
	b, _ = json.Marshal(workflowsSection(detail(name), "logs"))
	if !strings.Contains(string(b), "n is 21") {
		t.Errorf("the workflow's Logs tab = %s", b)
	}
}

// TestConsoleWorkflowsLogsWithoutLogging: without Cloud Logging the Logs
// tab says why it is empty.
func TestConsoleWorkflowsLogsWithoutLogging(t *testing.T) {
	s := workflowsProvider{}.logsSection(context.Background(), "p", "x")
	if s.Unavailable == "" {
		t.Errorf("logs without Cloud Logging = %+v", s)
	}
}
