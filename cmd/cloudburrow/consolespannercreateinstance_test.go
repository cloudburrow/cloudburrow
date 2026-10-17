package main

import (
	"context"
	"net"
	"testing"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"
)

// fakeInstanceAdmin answers CreateInstance done, recording the request.
type fakeInstanceAdmin struct {
	instancepb.UnimplementedInstanceAdminServer
	got *instancepb.CreateInstanceRequest
}

func (f *fakeInstanceAdmin) CreateInstance(_ context.Context, r *instancepb.CreateInstanceRequest) (*longrunningpb.Operation, error) {
	f.got = r
	res, _ := anypb.New(r.GetInstance())
	return &longrunningpb.Operation{Name: "op", Done: true, Result: &longrunningpb.Operation_Response{Response: res}}, nil
}

// TestConsoleSpannerCreateInstance (#1491): the Spanner list screen offers
// Create instance, which sends CreateInstance with the form's ID, display
// name, configuration and processing units, and refuses a compute capacity
// Spanner refuses before any call.
func TestConsoleSpannerCreateInstance(t *testing.T) {
	f := &fakeInstanceAdmin{}
	g := grpc.NewServer()
	instancepb.RegisterInstanceAdminServer(g, f)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	p := spannerProvider{endpoint: ln.Addr().String()}
	ctx := context.Background()
	acts := p.ListActions(ctx, "proj")
	if len(acts) != 1 || acts[0].ID != spannerCreateInstanceAction || acts[0].Label != "Create instance" {
		t.Fatalf("the list screen's actions = %+v", acts)
	}
	if len(p.ListActions(ctx, "")) != 0 {
		t.Error("Create instance is offered with no project")
	}
	if err := p.ActAt(ctx, "proj", nil, spannerCreateInstanceAction, map[string]string{"instance": "orders", "displayName": "Orders",
		"config": "emulator-config", "processingUnits": "300"}); err != nil {
		t.Fatal(err)
	}
	if r := f.got; r.GetParent() != "projects/proj" || r.GetInstanceId() != "orders" || r.GetInstance().GetDisplayName() != "Orders" ||
		r.GetInstance().GetConfig() != "projects/proj/instanceConfigs/emulator-config" || r.GetInstance().GetProcessingUnits() != 300 {
		t.Errorf("CreateInstance = %v", r)
	}
	f.got = nil
	for _, pu := range []string{"150", "1500", "0", "many"} {
		if err := p.ActAt(ctx, "proj", nil, spannerCreateInstanceAction, map[string]string{"instance": "bad", "displayName": "Badly",
			"processingUnits": pu}); err == nil || f.got != nil {
			t.Errorf("Create instance with %s processing units = %v, sent %v", pu, err, f.got)
		}
	}
}
