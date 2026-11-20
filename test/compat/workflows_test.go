//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/run/apiv2/runpb"
	"cloud.google.com/go/storage"
	workflows "cloud.google.com/go/workflows/apiv1"
	"cloud.google.com/go/workflows/apiv1/workflowspb"
	executions "cloud.google.com/go/workflows/executions/apiv1"
	"cloud.google.com/go/workflows/executions/apiv1/executionspb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/api/workflowexecutions/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// EnvWorkflows is the host address of Workflows and Workflow Executions
// (workflows.googleapis.com and workflowexecutions.googleapis.com, #1986),
// gRPC and JSON on one port.
const EnvWorkflows = "CLOUDBURROW_TEST_WORKFLOWS"

type workflowsAPI struct {
	addr string
	wf   *workflows.Client
	ex   *executions.Client
	// rest is the official REST client of the Workflow Executions API,
	// for the step entries and callbacks the gRPC API has no methods for.
	rest *workflowexecutions.Service
}

// workflowsClients are the official clients; they skip without
// CLOUDBURROW_TEST_WORKFLOWS.
func workflowsClients(t *testing.T, h *Harness) *workflowsAPI {
	t.Helper()
	ctx := h.Context()
	addr := h.Endpoint(EnvWorkflows)
	opts := []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	wf, err := workflows.NewClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	ex, err := executions.NewClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	rest, err := workflowexecutions.NewService(ctx, option.WithEndpoint("http://"+addr+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wf.Close(); _ = ex.Close() })
	return &workflowsAPI{addr: addr, wf: wf, ex: ex, rest: rest}
}

func workflowsParent(h *Harness) string { return "projects/" + h.Project() + "/locations/us-central1" }

// deploy creates a workflow and waits for its operation.
func (w *workflowsAPI) deploy(t *testing.T, h *Harness, id, src string) *workflowspb.Workflow {
	t.Helper()
	ctx := h.Context()
	op, err := w.wf.CreateWorkflow(ctx, &workflowspb.CreateWorkflowRequest{Parent: workflowsParent(h), WorkflowId: id,
		Workflow: &workflowspb.Workflow{SourceCode: &workflowspb.Workflow_SourceContents{SourceContents: src}}})
	if err != nil {
		t.Fatalf("CreateWorkflow %s: %v", id, err)
	}
	got, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateWorkflow %s: %v", id, err)
	}
	t.Cleanup(func() {
		if op, err := w.wf.DeleteWorkflow(context.Background(), &workflowspb.DeleteWorkflowRequest{Name: got.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	return got
}

// run executes a workflow and polls GetExecution until it ends, as gcloud
// workflows run does.
func (w *workflowsAPI) run(t *testing.T, h *Harness, wf, arg string, within time.Duration) *executionspb.Execution {
	t.Helper()
	ctx := h.Context()
	ex, err := w.ex.CreateExecution(ctx, &executionspb.CreateExecutionRequest{Parent: wf, Execution: &executionspb.Execution{Argument: arg}})
	if err != nil {
		t.Fatalf("CreateExecution: %v", err)
	}
	return w.await(t, h, ex.GetName(), within)
}

func (w *workflowsAPI) await(t *testing.T, h *Harness, name string, within time.Duration) *executionspb.Execution {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got, err := w.ex.GetExecution(h.Context(), &executionspb.GetExecutionRequest{Name: name})
		if err != nil {
			t.Fatalf("GetExecution: %v", err)
		}
		if got.GetState() != executionspb.Execution_ACTIVE {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("execution %s still ACTIVE after %v", name, within)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// errorTags is the tags of a failed execution's error payload.
func errorTags(t *testing.T, ex *executionspb.Execution) (map[string]any, string) {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal([]byte(ex.GetError().GetPayload()), &p); err != nil {
		t.Fatalf("error payload %q: %v", ex.GetError().GetPayload(), err)
	}
	tags, _ := json.Marshal(p["tags"])
	return p, string(tags)
}

// (No covers line yet: not run against an instance, #1986.)
//
// TestWorkflowsLifecycleTryRetryParallelAndLimits (#1986), with the official
// workflows and executions clients: a workflow deploys, lists, gets, runs
// with an argument and returns its result; an update with a new source is
// revision 2, and both revisions list. Against an endpoint served by this
// test that answers 503 twice, http.default_retry retries and succeeds; a
// 404 is caught by try/except as an HttpError whose code is 404. Parallel
// branches update a shared variable. An http.get to a host outside the
// machine is refused before anything is sent, as ConnectionFailedError
// naming why. A source over 128 KB and a call to an undefined subworkflow
// are refused at deployment; a loop past 100,000 steps fails with
// ResourceLimitError; an unhandled raise fails the execution with its
// payload and a stack trace.
func TestWorkflowsLifecycleTryRetryParallelAndLimits(t *testing.T) {
	h := New(t)
	w := workflowsClients(t, h)
	ctx := h.Context()
	n := time.Now().UnixNano() % 1e6

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/flaky":
			if hits.Add(1) <= 2 {
				http.Error(rw, "try again", http.StatusServiceUnavailable)
				return
			}
			rw.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(rw, `{"ok": true, "attempts": 3}`)
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(srv.Close)

	src := `main:
  params: [args]
  steps:
    - init:
        assign:
          - total: 0
          - items: [1, 2, 3, 4]
    - flaky:
        try:
          call: http.get
          args:
            url: ${args.base + "/flaky"}
          result: flaky
        retry: ${http.default_retry}
    - missing:
        try:
          call: http.get
          args:
            url: ${args.base + "/missing"}
        except:
          as: e
          steps:
            - caught:
                assign:
                  - missingCode: ${e.code}
                  - missingTags: ${e.tags}
    - outside:
        try:
          call: http.get
          args:
            url: https://example.com/
        except:
          as: e
          steps:
            - refused:
                assign:
                  - outsideTags: ${e.tags}
                  - outsideMessage: ${e.message}
    - sum:
        parallel:
          shared: [total]
          for:
            value: v
            in: ${items}
            steps:
              - add:
                  assign:
                    - total: ${total + v}
    - done:
        return:
          greeting: ${"hello " + args.name}
          flaky: ${flaky.body}
          missingCode: ${missingCode}
          missingTags: ${missingTags}
          outsideTags: ${outsideTags}
          outsideMessage: ${outsideMessage}
          total: ${total}
`
	id := fmt.Sprintf("compat-wf-%d", n)
	wf := w.deploy(t, h, id, src)
	if wf.GetState() != workflowspb.Workflow_ACTIVE || !strings.HasPrefix(wf.GetRevisionId(), "000001-") {
		t.Fatalf("deployed %v", wf)
	}
	got, err := w.wf.GetWorkflow(ctx, &workflowspb.GetWorkflowRequest{Name: wf.GetName()})
	if err != nil || got.GetSourceContents() != src {
		t.Fatalf("GetWorkflow: %v %v", got, err)
	}
	found := false
	for it := w.wf.ListWorkflows(ctx, &workflowspb.ListWorkflowsRequest{Parent: workflowsParent(h)}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListWorkflows: %v", err)
		}
		found = found || x.GetName() == wf.GetName()
	}
	if !found {
		t.Error("ListWorkflows omitted the workflow")
	}

	arg, _ := json.Marshal(map[string]string{"base": srv.URL, "name": "burrow"})
	ex := w.run(t, h, wf.GetName(), string(arg), 2*time.Minute)
	if ex.GetState() != executionspb.Execution_SUCCEEDED {
		t.Fatalf("execution %v: %v", ex.GetState(), ex.GetError())
	}
	var res struct {
		Greeting       string
		Flaky          map[string]any
		MissingCode    float64
		MissingTags    []string
		OutsideTags    []string
		OutsideMessage string
		Total          float64
	}
	if err := json.Unmarshal([]byte(ex.GetResult()), &res); err != nil {
		t.Fatalf("result %q: %v", ex.GetResult(), err)
	}
	if res.Greeting != "hello burrow" || res.Flaky["ok"] != true || hits.Load() != 3 || res.MissingCode != 404 ||
		!strings.Contains(strings.Join(res.MissingTags, ","), "HttpError") || res.Total != 10 {
		t.Errorf("result %s (flaky hits %d)", ex.GetResult(), hits.Load())
	}
	if !strings.Contains(strings.Join(res.OutsideTags, ","), "ConnectionFailedError") || !strings.Contains(res.OutsideMessage, "outside this CloudBurrow instance") {
		t.Errorf("an outside URL: tags %v, %q; want ConnectionFailedError naming the refusal", res.OutsideTags, res.OutsideMessage)
	}
	if ex.GetWorkflowRevisionId() != wf.GetRevisionId() || ex.GetDuration() == nil {
		t.Errorf("execution revision %q duration %v", ex.GetWorkflowRevisionId(), ex.GetDuration())
	}

	// The step entries, through the official REST client.
	entries, err := w.rest.Projects.Locations.Workflows.Executions.StepEntries.List(ex.GetName()).Context(ctx).Do()
	if err != nil {
		t.Fatalf("stepEntries.list: %v", err)
	}
	steps := map[string]bool{}
	for _, e := range entries.StepEntries {
		steps[e.Step] = true
	}
	for _, s := range []string{"init", "flaky", "missing", "sum", "done"} {
		if !steps[s] {
			t.Errorf("stepEntries has no %q: %v", s, steps)
		}
	}

	// A new source is revision 2; an unhandled raise fails the execution.
	op, err := w.wf.UpdateWorkflow(ctx, &workflowspb.UpdateWorkflowRequest{Workflow: &workflowspb.Workflow{Name: wf.GetName(),
		SourceCode: &workflowspb.Workflow_SourceContents{SourceContents: "- fail:\n    raise:\n      code: 42\n      message: deliberate\n"}}})
	if err != nil {
		t.Fatalf("UpdateWorkflow: %v", err)
	}
	wf2, err := op.Wait(ctx)
	if err != nil || !strings.HasPrefix(wf2.GetRevisionId(), "000002-") {
		t.Fatalf("UpdateWorkflow: %v %v", wf2, err)
	}
	revs := 0
	for it := w.wf.ListWorkflowRevisions(ctx, &workflowspb.ListWorkflowRevisionsRequest{Name: wf.GetName()}); ; revs++ {
		if _, err := it.Next(); err == iterator.Done {
			break
		} else if err != nil {
			t.Fatalf("ListWorkflowRevisions: %v", err)
		}
	}
	if revs != 2 {
		t.Errorf("%d revisions; want 2", revs)
	}
	failed := w.run(t, h, wf.GetName(), "", time.Minute)
	if failed.GetState() != executionspb.Execution_FAILED || !strings.Contains(failed.GetError().GetPayload(), `"deliberate"`) ||
		len(failed.GetError().GetStackTrace().GetElements()) == 0 || failed.GetError().GetStackTrace().GetElements()[0].GetStep() != "fail" {
		t.Errorf("a raise: %v %v", failed.GetState(), failed.GetError())
	}
	listed := 0
	for it := w.ex.ListExecutions(ctx, &executionspb.ListExecutionsRequest{Parent: wf.GetName()}); ; listed++ {
		if _, err := it.Next(); err == iterator.Done {
			break
		} else if err != nil {
			t.Fatalf("ListExecutions: %v", err)
		}
	}
	if listed != 2 {
		t.Errorf("ListExecutions = %d; want 2", listed)
	}

	// Limits: deployment refuses a source over 128 KB and a call to a
	// subworkflow that does not exist; a loop past 100,000 steps fails.
	big := "- a:\n    return: \"" + strings.Repeat("x", 130*1024) + "\"\n"
	for name, bad := range map[string]string{"a source over 128 KB": big,
		"an undefined subworkflow": "main:\n  steps:\n    - a:\n        call: nowhere\n"} {
		_, err := w.wf.CreateWorkflow(ctx, &workflowspb.CreateWorkflowRequest{Parent: workflowsParent(h), WorkflowId: fmt.Sprintf("bad-%d", n),
			Workflow: &workflowspb.Workflow{SourceCode: &workflowspb.Workflow_SourceContents{SourceContents: bad}}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v; want INVALID_ARGUMENT", name, err)
		}
	}
	loop := w.deploy(t, h, fmt.Sprintf("compat-loop-%d", n), `- init:
    assign:
      - i: 0
- inc:
    assign:
      - i: ${i + 1}
    next: inc
`)
	lx := w.run(t, h, loop.GetName(), "", 2*time.Minute)
	if lx.GetState() != executionspb.Execution_FAILED {
		t.Fatalf("an endless loop: %v", lx.GetState())
	}
	if _, tags := errorTags(t, lx); !strings.Contains(tags, "ResourceLimitError") {
		t.Errorf("an endless loop failed with %s; want ResourceLimitError", lx.GetError().GetPayload())
	}
}

// (No covers line yet: not run against an instance, #1986.)
//
// TestWorkflowsCallbackAndCancel (#1986): a workflow creates a callback
// endpoint and awaits it; the callback lists through the official REST
// client, a POST to it delivers the request, and the workflow returns its
// body. A workflow sleeping for an hour is cancelled by CancelExecution
// and is CANCELLED; cancelling it again is FAILED_PRECONDITION.
func TestWorkflowsCallbackAndCancel(t *testing.T) {
	h := New(t)
	w := workflowsClients(t, h)
	ctx := h.Context()
	n := time.Now().UnixNano() % 1e6
	cb := w.deploy(t, h, fmt.Sprintf("compat-cb-%d", n), `- create:
    call: events.create_callback_endpoint
    args:
      http_callback_method: POST
    result: endpoint
- wait:
    call: events.await_callback
    args:
      callback: ${endpoint}
      timeout: 120
    result: request
- done:
    return: ${request.http_request.body}
`)
	ex, err := w.ex.CreateExecution(ctx, &executionspb.CreateExecutionRequest{Parent: cb.GetName()})
	if err != nil {
		t.Fatalf("CreateExecution: %v", err)
	}
	var callback string
	for deadline := time.Now().Add(30 * time.Second); callback == "" && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		list, err := w.rest.Projects.Locations.Workflows.Executions.Callbacks.List(ex.GetName()).Context(ctx).Do()
		if err != nil {
			t.Fatalf("callbacks.list: %v", err)
		}
		for _, c := range list.Callbacks {
			if c.Method == "POST" && c.Waiters == 1 {
				callback = c.Name
			}
		}
	}
	if callback == "" {
		t.Fatal("the execution listed no callback with a waiter")
	}
	resp, err := http.Post("http://"+w.addr+"/v1/"+callback, "application/json", strings.NewReader(`{"approved": true}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST to the callback = %d", resp.StatusCode)
	}
	if got := w.await(t, h, ex.GetName(), time.Minute); got.GetState() != executionspb.Execution_SUCCEEDED || got.GetResult() != `{"approved":true}` {
		t.Errorf("after the callback: %v %q %v", got.GetState(), got.GetResult(), got.GetError())
	}

	sleepy := w.deploy(t, h, fmt.Sprintf("compat-sleep-%d", n), "- s:\n    call: sys.sleep\n    args:\n      seconds: 3600\n")
	sx, err := w.ex.CreateExecution(ctx, &executionspb.CreateExecutionRequest{Parent: sleepy.GetName()})
	if err != nil {
		t.Fatalf("CreateExecution: %v", err)
	}
	c, err := w.ex.CancelExecution(ctx, &executionspb.CancelExecutionRequest{Name: sx.GetName()})
	if err != nil || c.GetState() != executionspb.Execution_CANCELLED {
		t.Fatalf("CancelExecution: %v %v", c.GetState(), err)
	}
	if _, err := w.ex.CancelExecution(ctx, &executionspb.CancelExecutionRequest{Name: sx.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a second cancel: %v; want FAILED_PRECONDITION", err)
	}
}

// TestWorkflowsCallRunStorageAndPubSub (#1986), in the run shard: a
// workflow calls a Cloud Run service of this instance with http.post and
// an OIDC token, writes the service's response to Cloud Storage with the
// googleapis.storage.v1.objects.insert connector and publishes it with
// googleapis.pubsub.v1.projects.topics.publish; the official Cloud
// Storage and Pub/Sub clients read both back. The Cloud Run URL is the
// service's own, which the execution reaches through the instance's
// ingress.
func TestWorkflowsCallRunStorageAndPubSub(t *testing.T) {
	h := New(t)
	w := workflowsClients(t, h)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	rc := runClient(t, h)
	ctx := runWait(t)
	n := time.Now().UnixNano() % 1e6

	svcID := fmt.Sprintf("wf-hello-%d", n)
	op, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: svcID,
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: "ghcr.io/knative/helloworld-go:latest",
			Env: []*runpb.EnvVar{{Name: "TARGET", Values: &runpb.EnvVar_Value{Value: "Workflows"}}}}}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() {
		_, _ = rc.DeleteService(context.Background(), &runpb.DeleteServiceRequest{Name: runParent(h) + "/services/" + svcID})
	})
	svc, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("the service: %v", err)
	}

	sc, err := storage.NewClient(ctx, option.WithoutAuthentication(),
		option.WithEndpoint(runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	bucketName := fmt.Sprintf("%s-wf-%d", h.Project(), n)
	bh := sc.Bucket(bucketName)
	if err := bh.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { emptyAndDelete(context.Background(), bh) })
	pc := pubsubClient(t, h)
	topicName := topic(t, h, pc, fmt.Sprintf("wf-topic-%d", n))
	subName := fmt.Sprintf("projects/%s/subscriptions/wf-sub-%d", h.Project(), n)
	if _, err := pc.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: subName, Topic: topicName}); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	t.Cleanup(func() {
		_ = pc.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: subName})
	})

	wf := w.deploy(t, h, fmt.Sprintf("compat-run-%d", n), `main:
  params: [args]
  steps:
    - call_run:
        call: http.post
        args:
          url: ${args.url}
          auth:
            type: OIDC
          body:
            from: workflow
        result: reply
    - save:
        call: googleapis.storage.v1.objects.insert
        args:
          bucket: ${args.bucket}
          name: reply.txt
          uploadType: media
          body: ${reply.body}
        result: object
    - publish:
        call: googleapis.pubsub.v1.projects.topics.publish
        args:
          topic: ${args.topic}
          body:
            messages:
              - data: ${base64.encode(text.encode(reply.body))}
        result: published
    - done:
        return:
          code: ${reply.code}
          object: ${object.name}
          messageIds: ${published.messageIds}
`)
	arg, _ := json.Marshal(map[string]string{"url": svc.GetUri(), "bucket": bucketName, "topic": topicName})
	ex := w.run(t, h, wf.GetName(), string(arg), 3*time.Minute)
	if ex.GetState() != executionspb.Execution_SUCCEEDED {
		t.Fatalf("execution %v: %v", ex.GetState(), ex.GetError())
	}
	var res struct {
		Code       float64
		Object     string
		MessageIDs []string `json:"messageIds"`
	}
	if err := json.Unmarshal([]byte(ex.GetResult()), &res); err != nil || res.Code != 200 || res.Object != "reply.txt" || len(res.MessageIDs) != 1 {
		t.Fatalf("result %s (%v)", ex.GetResult(), err)
	}
	r, err := bh.Object("reply.txt").NewReader(ctx)
	if err != nil {
		t.Fatalf("read the object: %v", err)
	}
	body, _ := io.ReadAll(r)
	r.Close()
	if !strings.Contains(string(body), "Hello Workflows") {
		t.Errorf("the object the workflow wrote = %q; want the service's reply", body)
	}
	pull, err := pc.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: subName, MaxMessages: 1})
	if err != nil || len(pull.GetReceivedMessages()) != 1 || !strings.Contains(string(pull.GetReceivedMessages()[0].GetMessage().GetData()), "Hello Workflows") {
		t.Errorf("the message the workflow published: %v %v", pull, err)
	}
}
