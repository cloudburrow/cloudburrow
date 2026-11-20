package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	workflows "cloud.google.com/go/workflows/apiv1"
	"cloud.google.com/go/workflows/apiv1/workflowspb"
	executions "cloud.google.com/go/workflows/executions/apiv1"
	"cloud.google.com/go/workflows/executions/apiv1/executionspb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func startServer(t *testing.T, s *Server) (*workflows.Client, *executions.Client, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	s.Register(g)
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	rs := httptest.NewServer(NewRESTHandler(s))
	t.Cleanup(rs.Close)
	s.opts.Addr = func() string { return strings.TrimPrefix(rs.URL, "http://") }
	opts := []option.ClientOption{option.WithEndpoint(ln.Addr().String()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	ctx := context.Background()
	wc, err := workflows.NewClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	ec, err := executions.NewClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wc.Close(); ec.Close() })
	return wc, ec, rs.URL
}

const parent = "projects/p/locations/us-central1"

func deploy(t *testing.T, wc *workflows.Client, id, src string) *workflowspb.Workflow {
	t.Helper()
	op, err := wc.CreateWorkflow(context.Background(), &workflowspb.CreateWorkflowRequest{Parent: parent, WorkflowId: id,
		Workflow: &workflowspb.Workflow{SourceCode: &workflowspb.Workflow_SourceContents{SourceContents: src}}})
	if err != nil {
		t.Fatal(err)
	}
	w, err := op.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func runAndWait(t *testing.T, s *Server, ec *executions.Client, wf, arg string) *executionspb.Execution {
	t.Helper()
	ex, err := ec.CreateExecution(context.Background(), &executionspb.CreateExecutionRequest{Parent: wf, Execution: &executionspb.Execution{Argument: arg}})
	if err != nil {
		t.Fatal(err)
	}
	s.Wait(ex.GetName())
	got, err := ec.GetExecution(context.Background(), &executionspb.GetExecutionRequest{Name: ex.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestServerLifecycleAndPersistence(t *testing.T) {
	state := filepath.Join(t.TempDir(), "workflows.json")
	s, err := NewServer(state, Options{})
	if err != nil {
		t.Fatal(err)
	}
	wc, ec, base := startServer(t, s)
	ctx := context.Background()
	w := deploy(t, wc, "greet", "main:\n  params: [args]\n  steps:\n    - r:\n        return: ${\"hello \" + args.name}\n")
	if w.GetState() != workflowspb.Workflow_ACTIVE || !strings.HasPrefix(w.GetRevisionId(), "000001-") {
		t.Fatalf("deployed %v", w)
	}
	// A bad source is refused at deployment.
	_, err = wc.CreateWorkflow(ctx, &workflowspb.CreateWorkflowRequest{Parent: parent, WorkflowId: "bad",
		Workflow: &workflowspb.Workflow{SourceCode: &workflowspb.Workflow_SourceContents{SourceContents: "- a:\n    next: nowhere\n"}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad source: %v", err)
	}
	ex := runAndWait(t, s, ec, w.GetName(), `{"name": "burrow"}`)
	if ex.GetState() != executionspb.Execution_SUCCEEDED || ex.GetResult() != `"hello burrow"` {
		t.Fatalf("execution %v", ex)
	}
	// An update of the source is a new revision.
	op, err := wc.UpdateWorkflow(ctx, &workflowspb.UpdateWorkflowRequest{Workflow: &workflowspb.Workflow{Name: w.GetName(),
		SourceCode: &workflowspb.Workflow_SourceContents{SourceContents: "- r:\n    raise: oops\n"}}})
	if err != nil {
		t.Fatal(err)
	}
	w2, err := op.Wait(ctx)
	if err != nil || !strings.HasPrefix(w2.GetRevisionId(), "000002-") {
		t.Fatalf("update: %v %v", w2, err)
	}
	revs := wc.ListWorkflowRevisions(ctx, &workflowspb.ListWorkflowRevisionsRequest{Name: w.GetName()})
	n := 0
	for {
		_, err := revs.Next()
		if err != nil {
			break
		}
		n++
	}
	if n != 2 {
		t.Errorf("%d revisions", n)
	}
	failed := runAndWait(t, s, ec, w.GetName(), "")
	if failed.GetState() != executionspb.Execution_FAILED || failed.GetError().GetPayload() != `"oops"` ||
		failed.GetError().GetStackTrace().GetElements()[0].GetStep() != "r" {
		t.Fatalf("failed execution %v", failed)
	}
	// Step entries over REST.
	resp, err := http.Get(base + "/v1/" + failed.GetName() + "/stepEntries")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"STEP_RAISE"`) || !strings.Contains(string(b), `"STATE_FAILED"`) {
		t.Errorf("stepEntries: %s", b)
	}
	// The records survive a restart.
	s.Close()
	s2, err := NewServer(state, Options{})
	if err != nil {
		t.Fatal(err)
	}
	wc2, ec2, _ := startServer(t, s2)
	got, err := wc2.GetWorkflow(ctx, &workflowspb.GetWorkflowRequest{Name: w.GetName()})
	if err != nil || got.GetRevisionId() != w2.GetRevisionId() {
		t.Fatalf("after restart: %v %v", got, err)
	}
	it := ec2.ListExecutions(ctx, &executionspb.ListExecutionsRequest{Parent: w.GetName()})
	n = 0
	for {
		if _, err := it.Next(); err != nil {
			break
		}
		n++
	}
	if n != 2 {
		t.Errorf("%d executions after restart", n)
	}
}

func TestServerCallbackAndCancel(t *testing.T) {
	s, _ := NewServer("", Options{})
	defer s.Close()
	wc, ec, base := startServer(t, s)
	ctx := context.Background()
	w := deploy(t, wc, "cb", `
- create:
    call: events.create_callback_endpoint
    args:
      http_callback_method: POST
    result: cb
- log:
    call: sys.log
    args:
      text: ${cb.url}
- wait:
    call: events.await_callback
    args:
      callback: ${cb}
      timeout: 30
    result: got
- done:
    return: ${got.http_request.body}
`)
	ex, err := ec.CreateExecution(ctx, &executionspb.CreateExecutionRequest{Parent: w.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	var cbName string
	for i := 0; i < 100 && cbName == ""; i++ {
		cbs, _ := s.Callbacks(ex.GetName())
		for _, c := range cbs {
			cbName = c.Name
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cbName == "" {
		t.Fatal("no callback endpoint")
	}
	resp, err := http.Post(base+"/v1/"+cbName, "application/json", strings.NewReader(`{"approved": true}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("callback: %v %v", resp, err)
	}
	s.Wait(ex.GetName())
	got, _ := ec.GetExecution(ctx, &executionspb.GetExecutionRequest{Name: ex.GetName()})
	if got.GetResult() != `{"approved":true}` {
		t.Fatalf("result %v", got)
	}
	// Cancel.
	w2 := deploy(t, wc, "sleepy", "- s:\n    call: sys.sleep\n    args:\n      seconds: 60\n")
	ex2, err := ec.CreateExecution(ctx, &executionspb.CreateExecutionRequest{Parent: w2.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	c, err := ec.CancelExecution(ctx, &executionspb.CancelExecutionRequest{Name: ex2.GetName()})
	if err != nil || c.GetState() != executionspb.Execution_CANCELLED {
		t.Fatalf("cancel: %v %v", c, err)
	}
	if _, err := ec.CancelExecution(ctx, &executionspb.CancelExecutionRequest{Name: ex2.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("second cancel: %v", err)
	}
}

func TestRESTSurface(t *testing.T) {
	s, _ := NewServer("", Options{})
	defer s.Close()
	_, _, base := startServer(t, s)
	body := `{"sourceContents": "- r:\n    return: 1\n"}`
	resp, err := http.Post(base+"/v1/"+parent+"/workflows?workflowId=rest", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var op map[string]any
	_ = json.Unmarshal(b, &op)
	if op["done"] != true {
		t.Fatalf("create: %s", b)
	}
	resp, err = http.Post(base+"/v1/"+parent+"/workflows/rest/executions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var ex map[string]any
	_ = json.Unmarshal(b, &ex)
	name, _ := ex["name"].(string)
	if !strings.Contains(name, "/executions/") {
		t.Fatalf("execution: %s", b)
	}
	s.Wait(name)
	resp, _ = http.Get(base + "/v1/" + name)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"SUCCEEDED"`) {
		t.Fatalf("get: %s", b)
	}
	resp, _ = http.Get(base + "/v1/projects/p/locations")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "us-central1") {
		t.Fatalf("locations: %s", b)
	}
	fmt.Fprint(io.Discard, b)
}
