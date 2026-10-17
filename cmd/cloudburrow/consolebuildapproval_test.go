package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
)

// noopSteps is a builder whose arbitrary steps succeed at once.
type noopSteps struct{ stubBuilder }

func (noopSteps) OpenWorkspace(context.Context, string, io.Reader) (runadapter.BuildWorkspace, error) {
	return noopWorkspace{}, nil
}

type noopWorkspace struct{}

func (noopWorkspace) RunStep(context.Context, runadapter.StepSpec) error { return nil }
func (noopWorkspace) Archive(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (noopWorkspace) Close() {}

// TestConsoleBuildApproval (#1449): Create trigger with Require approval
// lists the trigger as Required; a run's build waits Awaiting approval with
// Approve, Reject and Cancel on its page; Approve runs it, and its page
// shows the approval and the comment.
func TestConsoleBuildApproval(t *testing.T) {
	ctx := context.Background()
	proj := freshProject("console-appr")
	s := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	g := grpc.NewServer()
	s.Builds(stubSource{}, noopSteps{}).AllowHostSteps(true).CloudBuild().Register(g)
	runadapter.NewOperationsServer(s).Register(g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	b := runBuildsProvider{defaultProject: proj, runAddr: func() string { return ln.Addr().String() }}
	p := buildTriggersProvider{b: b}
	if _, err := p.Create(ctx, "", map[string]string{"name": "gated", "region": "global", "image": "alpine:3.20",
		"command": "echo hi", "approval": "true"}); err != nil {
		t.Fatalf("Create trigger: %v", err)
	}
	l, _ := p.List(ctx, "")
	if len(l.Items) != 1 || l.Items[0].Fields["Approval"] != "Required" {
		t.Fatalf("triggers = %+v", l.Items)
	}
	if err := p.Act(ctx, "", "gated", "run"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var id string
	for _, x := range runadapter.CloudBuilds(proj, "") {
		id = x.GetId()
	}
	acts := b.DetailActions(ctx, "", []string{id})
	if len(acts) != 3 || acts[0].ID != "approve" || acts[1].ID != "reject" {
		t.Fatalf("a pending build's actions = %+v", acts)
	}
	if err := b.ActAt(ctx, "", []string{id}, "approve", map[string]string{"comment": "looks good"}); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if x := b.build("", id); x.GetStatus() == cbpb.Build_SUCCESS {
			break
		}
	}
	d, _ := b.Detail(ctx, "", []string{id})
	if sum := fmt.Sprint(d.Summary); !strings.Contains(sum, "APPROVED: looks good") || b.build("", id).GetStatus() != cbpb.Build_SUCCESS {
		t.Errorf("the approved build = %v", sum)
	}
}
