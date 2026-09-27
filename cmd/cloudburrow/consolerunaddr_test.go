package main

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"
)

// deleteRecorder is a Cloud Run Services API that answers DeleteService and
// records the names it was asked to delete.
type deleteRecorder struct {
	runpb.UnimplementedServicesServer
	mu      sync.Mutex
	deleted []string
}

func (d *deleteRecorder) DeleteService(_ context.Context, req *runpb.DeleteServiceRequest) (*longrunningpb.Operation, error) {
	d.mu.Lock()
	d.deleted = append(d.deleted, req.GetName())
	d.mu.Unlock()
	resp, err := anypb.New(&runpb.Service{Name: req.GetName()})
	if err != nil {
		return nil, err
	}
	return &longrunningpb.Operation{Name: "operations/delete", Done: true,
		Result: &longrunningpb.Operation_Response{Response: resp}}, nil
}

// boundLater is an adapter whose address is known only once it has bound,
// as with --port-run 0.
type boundLater struct {
	mu   sync.Mutex
	addr string
}

func (b *boundLater) Addr() string  { b.mu.Lock(); defer b.mu.Unlock(); return b.addr }
func (b *boundLater) bind(a string) { b.mu.Lock(); b.addr = a; b.mu.Unlock() }

// The console used to dial the configured Run port, fixed when the provider
// was built. With --port-run 0 that is 0, so every console deploy, delete and
// edit went to 127.0.0.1:0 (#646). The provider now asks the adapter for the
// address it bound, each time, so an adapter that binds after the console is
// built is reached at its real port.
func TestTheRunProviderReachesTheAdapterAtItsBoundPort(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	rec := &deleteRecorder{}
	runpb.RegisterServicesServer(srv, rec)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	adapter := &boundLater{}
	p := runProvider{runAddr: runAddrOf(adapter), defaultProject: "demo-project"}

	// Built before the adapter bound: nothing to dial yet.
	if err := p.Delete(context.Background(), "", "hello"); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("Delete before the adapter bound = %v, want it to say the adapter is not running", err)
	}

	adapter.bind(lis.Addr().String())
	if err := p.Delete(context.Background(), "", "hello"); err != nil {
		t.Fatalf("Delete after the adapter bound = %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.deleted) != 1 || !strings.HasSuffix(rec.deleted[0], "/services/hello") {
		t.Errorf("the adapter was asked to delete %v, want one .../services/hello", rec.deleted)
	}
}

// No adapter at all (Cloud Run not enabled) is "not running", never a dial
// to a configured port.
func TestTheRunProviderWithNoAdapterIsNotRunning(t *testing.T) {
	var none *runService
	p := runProvider{runAddr: runAddrOf(none)}
	if got := p.runEndpoint(); got != "" {
		t.Errorf("runEndpoint with no adapter = %q, want empty", got)
	}
	if got := (runProvider{}).runEndpoint(); got != "" {
		t.Errorf("runEndpoint of a zero provider = %q, want empty", got)
	}
}
