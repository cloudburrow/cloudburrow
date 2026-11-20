//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	eventarc "cloud.google.com/go/eventarc/apiv1"
	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/workflows/apiv1/workflowspb"
	"cloud.google.com/go/workflows/executions/apiv1/executionspb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// (No covers line yet: not run against an instance, #1986.)
//
// TestGcloudWorkflows (#1986): `gcloud workflows deploy` with a source
// file, `gcloud workflows run` with --data, which waits for the result,
// and `gcloud workflows executions describe`, through the configuration
// `cloudburrow gcloud-setup` writes and with egress to Google blocked. The
// official client reads the workflow back.
func TestGcloudWorkflows(t *testing.T) {
	h := New(t)
	w := workflowsClients(t, h) // skips without CLOUDBURROW_TEST_WORKFLOWS
	ctx := h.Context()
	g := newGcloudSession(t, h)
	id := fmt.Sprintf("gcloud-wf-%d", time.Now().UnixNano()%1e6)
	src := filepath.Join(t.TempDir(), "workflow.yaml")
	if err := os.WriteFile(src, []byte("main:\n  params: [args]\n  steps:\n    - r:\n        return: ${\"hi \" + args.who}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loc := []string{"--location", "us-central1", "--project", g.project, "--quiet"}
	run := func(args ...string) string {
		t.Helper()
		out, err := g.run(nil, append(append([]string{"workflows"}, args...), loc...)...)
		if err != nil {
			t.Fatalf("gcloud workflows %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	run("deploy", id, "--source", src, "--description", "from gcloud")
	name := "projects/" + g.project + "/locations/us-central1/workflows/" + id
	t.Cleanup(func() { _, _ = g.run(nil, append([]string{"workflows", "delete", id}, loc...)...) })
	got, err := w.wf.GetWorkflow(ctx, &workflowspb.GetWorkflowRequest{Name: name})
	if err != nil || got.GetDescription() != "from gcloud" || got.GetState() != workflowspb.Workflow_ACTIVE {
		t.Fatalf("the workflow gcloud deployed: %v %v", got, err)
	}
	out := run("run", id, "--data", `{"who":"gcloud"}`, "--format", "json")
	var ex struct{ Name, State, Result string }
	if err := json.Unmarshal([]byte(out), &ex); err != nil {
		t.Fatalf("gcloud workflows run: %v\n%s", err, out)
	}
	if ex.State != "SUCCEEDED" || ex.Result != `"hi gcloud"` {
		t.Fatalf("gcloud workflows run = %+v", ex)
	}
	execID := ex.Name[strings.LastIndex(ex.Name, "/")+1:]
	desc := run("executions", "describe", execID, "--workflow", id, "--format", "value(state,result)")
	if !strings.Contains(desc, "SUCCEEDED") || !strings.Contains(desc, "hi gcloud") {
		t.Errorf("gcloud workflows executions describe: %s", desc)
	}
}

// (No covers line yet: not run against an instance, #1986.)
//
// TestTerraformWorkflowsWorkflow (#1986): Terraform's
// google_workflows_workflow creates a workflow with a description, labels,
// environment variables and its source, plans clean, updates the source
// (a new revision) and destroys it; the official client checks each step.
func TestTerraformWorkflowsWorkflow(t *testing.T) {
	h := New(t)
	w := workflowsClients(t, h) // skips without CLOUDBURROW_TEST_WORKFLOWS
	ctx := h.Context()
	m := newTFModule(t)
	id := fmt.Sprintf("tf-wf-%d", time.Now().UnixNano()%1e6)
	conf := func(ret string) string {
		return fmt.Sprintf(`
resource "google_workflows_workflow" "w" {
  project             = %[1]q
  region              = "us-central1"
  name                = %[2]q
  description         = "from terraform"
  labels              = { team = "burrow" }
  user_env_vars       = { GREETING = "hello" }
  deletion_protection = false
  source_contents     = <<-EOF
    main:
      steps:
        - r:
            return: $${sys.get_env("GREETING") + " %[3]s"}
  EOF
}
`, m.project, id, ret)
	}
	m.write(conf("one"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	name := "projects/" + m.project + "/locations/us-central1/workflows/" + id
	got, err := w.wf.GetWorkflow(ctx, &workflowspb.GetWorkflowRequest{Name: name})
	if err != nil || got.GetDescription() != "from terraform" || got.GetLabels()["team"] != "burrow" || got.GetUserEnvVars()["GREETING"] != "hello" {
		t.Fatalf("the workflow Terraform made: %v %v", got, err)
	}
	if ex := w.run(t, h, name, "", time.Minute); ex.GetResult() != `"hello one"` {
		t.Errorf("its execution: %v %q %v", ex.GetState(), ex.GetResult(), ex.GetError())
	}
	m.planClean()
	m.write(conf("two"))
	m.apply()
	got, err = w.wf.GetWorkflow(ctx, &workflowspb.GetWorkflowRequest{Name: name})
	if err != nil || !strings.HasPrefix(got.GetRevisionId(), "000002-") || !strings.Contains(got.GetSourceContents(), "two") {
		t.Fatalf("after the update: %v %v", got, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := w.wf.GetWorkflow(ctx, &workflowspb.GetWorkflowRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("after destroy: %v; want NOT_FOUND", err)
	}
}

// TestConsoleWorkflows (#1986): the console's Workflows page lists a
// workflow made by Create workflow, which the official client reads back;
// Execute on its page runs it, the Executions tab lists the execution, and
// the execution's page shows its Output and Steps; Edit deploys a new
// revision, Cancel stops a running execution and Delete removes the
// workflow.
func TestConsoleWorkflows(t *testing.T) {
	h := New(t)
	w := workflowsClients(t, h) // skips without CLOUDBURROW_TEST_WORKFLOWS
	addr := consoleAddr(t, h)
	ctx := h.Context()
	project := h.Project()
	id := fmt.Sprintf("console-wf-%d", time.Now().UnixNano()%1e6)
	name := workflowsParent(h) + "/workflows/" + id
	t.Cleanup(func() {
		if op, err := w.wf.DeleteWorkflow(context.Background(), &workflowspb.DeleteWorkflowRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if code, body := consoleCreate(t, addr, "workflows", project, map[string]string{"workflowId": id, "location": "us-central1",
		"description": "from the console", "sourceContents": "main:\n  params: [a]\n  steps:\n    - r:\n        return: ${a.n * 2}\n"}); code != http.StatusOK {
		t.Fatalf("Create workflow = %d: %s", code, body)
	}
	got, err := w.wf.GetWorkflow(ctx, &workflowspb.GetWorkflowRequest{Name: name})
	if err != nil || got.GetDescription() != "from the console" {
		t.Fatalf("the workflow the console made: %v %v", got, err)
	}
	if code, body := consoleAct(t, addr, "workflows", project, []string{name}, "execute", map[string]string{"argument": `{"n": 21}`}); code != http.StatusOK {
		t.Fatalf("Execute = %d: %s", code, body)
	}
	var exName string
	for it := w.ex.ListExecutions(ctx, &executionspb.ListExecutionsRequest{Parent: name}); ; {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		exName = e.GetName()
	}
	if exName == "" {
		t.Fatal("Execute made no execution")
	}
	if ex := w.await(t, h, exName, time.Minute); ex.GetResult() != "42" {
		t.Fatalf("the execution: %v %q", ex.GetState(), ex.GetResult())
	}
	q := url.Values{"project": {project}, "name": {name}}
	code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/workflows?"+q.Encode(), "")
	if code != http.StatusOK || !strings.Contains(body, exName[strings.LastIndex(exName, "/")+1:]) || !strings.Contains(body, `"Source"`) {
		t.Errorf("the workflow's page = %d: %s", code, body)
	}
	q["name"] = []string{name, exName}
	code, body = consoleDo(t, addr, http.MethodGet, "/api/detail/workflows?"+q.Encode(), "")
	if code != http.StatusOK || !strings.Contains(body, `"Steps"`) || !strings.Contains(body, "SUCCEEDED") || !strings.Contains(body, `"42"`) {
		t.Errorf("the execution's page = %d: %s", code, body)
	}
	if code, body := consoleAct(t, addr, "workflows", project, []string{name}, "edit", map[string]string{"description": "edited",
		"sourceContents": "- s:\n    call: sys.sleep\n    args:\n      seconds: 600\n"}); code != http.StatusOK {
		t.Fatalf("Edit = %d: %s", code, body)
	}
	got, _ = w.wf.GetWorkflow(ctx, &workflowspb.GetWorkflowRequest{Name: name})
	if !strings.HasPrefix(got.GetRevisionId(), "000002-") || got.GetDescription() != "edited" {
		t.Errorf("after Edit: %v", got)
	}
	ex, err := w.ex.CreateExecution(ctx, &executionspb.CreateExecutionRequest{Parent: name})
	if err != nil {
		t.Fatal(err)
	}
	if code, body := consoleAct(t, addr, "workflows", project, []string{name, ex.GetName()}, "cancel", nil); code != http.StatusOK {
		t.Fatalf("Cancel = %d: %s", code, body)
	}
	if got, _ := w.ex.GetExecution(ctx, &executionspb.GetExecutionRequest{Name: ex.GetName()}); got.GetState() != executionspb.Execution_CANCELLED {
		t.Errorf("after Cancel: %v", got.GetState())
	}
	if code, body := consoleAct(t, addr, "workflows", project, []string{name}, "delete", nil); code != http.StatusOK {
		t.Fatalf("Delete = %d: %s", code, body)
	}
	if _, err := w.wf.GetWorkflow(ctx, &workflowspb.GetWorkflowRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("after Delete: %v", err)
	}
}

// (No covers line yet: not run against an instance, #1986.)
//
// TestEventarcTriggerStartsAWorkflow (#1986), in the run shard: an
// Eventarc messagePublished trigger whose destination is a workflow starts
// an execution for a published message, with the CloudEvent as its
// argument; the workflow returns the message's data, decoded.
func TestEventarcTriggerStartsAWorkflow(t *testing.T) {
	h := New(t)
	w := workflowsClients(t, h)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	pc := pubsubClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ec, err := eventarc.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ec.Close() })
	n := time.Now().UnixNano() % 1e6
	wf := w.deploy(t, h, fmt.Sprintf("compat-ea-%d", n), `main:
  params: [event]
  steps:
    - r:
        return:
          type: ${event.type}
          data: ${text.decode(base64.decode(event.data.message.data))}
`)
	op, err := ec.CreateTrigger(ctx, &eventarcpb.CreateTriggerRequest{Parent: runParent(h), TriggerId: fmt.Sprintf("to-workflow-%d", n),
		Trigger: &eventarcpb.Trigger{EventFilters: []*eventarcpb.EventFilter{{Attribute: "type", Value: "google.cloud.pubsub.topic.v1.messagePublished"}},
			Destination: &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_Workflow{Workflow: wf.GetName()}}}})
	if err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}
	tr, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ec.DeleteTrigger(context.Background(), &eventarcpb.DeleteTriggerRequest{Name: tr.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	if tr.GetDestination().GetWorkflow() != wf.GetName() {
		t.Errorf("the trigger's destination = %v", tr.GetDestination())
	}
	if _, err := pc.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: tr.GetTransport().GetPubsub().GetTopic(),
		Messages: []*pubsubpb.PubsubMessage{{Data: []byte("to the workflow")}}}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		it := w.ex.ListExecutions(ctx, &executionspb.ListExecutionsRequest{Parent: wf.GetName()})
		if e, err := it.Next(); err == nil {
			got := w.await(t, h, e.GetName(), time.Minute)
			if got.GetState() != executionspb.Execution_SUCCEEDED || !strings.Contains(got.GetResult(), `"data":"to the workflow"`) ||
				!strings.Contains(got.GetResult(), "messagePublished") {
				t.Errorf("the execution the event started: %v %s %v", got.GetState(), got.GetResult(), got.GetError())
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no execution started for the published message")
		}
		time.Sleep(time.Second)
	}
}
