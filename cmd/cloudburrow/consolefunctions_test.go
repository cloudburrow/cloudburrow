package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"google.golang.org/grpc"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
)

type failingBuilder struct{}

func (failingBuilder) Build(context.Context, runadapter.BuildSpec) error {
	return errors.New("no runtime detected")
}

// TestConsoleFunctionsPage (#1377): the Functions page lists a function the
// API created, with its state, region and runtime; its page shows the
// runtime, entry point, source and why it failed; Create function is
// offered only with Cloud Storage; Delete goes through DeleteFunction.
func TestConsoleFunctionsPage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	fs := s.Functions(s.Builds(stubSource{}, failingBuilder{}), func() string { return "http://127.0.0.1:1" })
	srv := grpc.NewServer()
	fs.Register(srv)
	runadapter.NewOperationsServer(s).Register(srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Stop()

	parent := "projects/console-fn/locations/us-central1"
	if _, err := fs.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: "page-fn", Function: &functionspb.Function{
		BuildConfig: &functionspb.BuildConfig{Runtime: "nodejs22", EntryPoint: "hello",
			Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: &functionspb.StorageSource{Bucket: "b", Object: "o.zip"}}}}}}); err != nil {
		t.Fatal(err)
	}
	p := functionsProvider{defaultProject: "console-fn", runAddr: func() string { return ln.Addr().String() }, storage: true}
	deadline := time.Now().Add(20 * time.Second)
	for {
		l, _ := p.List(ctx, "")
		if len(l.Items) == 1 && l.Items[0].Status == "error" {
			if it := l.Items[0]; it.Name != "page-fn" || it.Fields["Region"] != "us-central1" || it.Fields["Runtime"] != "nodejs22" {
				t.Errorf("the listed function = %+v", it)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the function never failed its build: %+v", l.Items)
		}
		time.Sleep(50 * time.Millisecond)
	}
	d, err := p.Detail(ctx, "", []string{"page-fn"})
	if err != nil {
		t.Fatal(err)
	}
	props := map[string]string{}
	for _, pr := range d.Summary {
		props[pr.Label] = pr.Value
	}
	if props["State"] != "FAILED" || props["Entry point"] != "hello" || props["Source"] != "gs://b/o.zip" ||
		!strings.Contains(props["Status detail"], "no runtime detected") || props["Memory"] != "256M" {
		t.Errorf("the function's page = %v", props)
	}
	if a := p.ListActions(ctx, ""); len(a) != 1 || a[0].ID != functionsCreateAction {
		t.Errorf("with Cloud Storage, ListActions = %+v; want Create function", a)
	}
	if a := (functionsProvider{}).ListActions(ctx, ""); len(a) != 0 {
		t.Errorf("without Cloud Storage, ListActions = %+v; want none", a)
	}
	if err := p.Delete(ctx, "", "page-fn"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if l, _ := p.List(ctx, ""); len(l.Items) != 0 {
		t.Errorf("after Delete, the list = %+v", l.Items)
	}
}
