package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	fv1 "cloud.google.com/go/functions/apiv1/functionspb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestConsoleFunctionsEditDownloadAndPermissions (#1491): a function's page
// offers Edit function, prefilled, which sends UpdateFunction through the
// API of its generation; Download source answers GenerateDownloadUrl's
// link; a 1st-gen function has a Permissions tab, through the v1 API's IAM
// methods, and a 2nd-gen one has none.
func TestConsoleFunctionsEditDownloadAndPermissions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	uploads := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer uploads.Close()
	s := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	fs := s.Functions(s.Builds(stubSource{}, failingBuilder{}), func() string { return uploads.URL })
	// UpdateFunction is answered here, done, with what it was sent: the
	// adapter's redeploy needs a cluster.
	var mu sync.Mutex
	var updates []proto.Message
	srv := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		if !strings.HasSuffix(info.FullMethod, "/UpdateFunction") {
			return h(ctx, req)
		}
		mu.Lock()
		updates = append(updates, req.(proto.Message))
		mu.Unlock()
		var fn proto.Message = &functionspb.Function{}
		if r, ok := req.(*fv1.UpdateFunctionRequest); ok {
			fn = r.GetFunction()
		}
		res, _ := anypb.New(fn)
		return &longrunningpb.Operation{Name: "op", Done: true, Result: &longrunningpb.Operation_Response{Response: res}}, nil
	}))
	fs.Register(srv)
	runadapter.NewOperationsServer(s).Register(srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	// Registered before the functions' cleanups, so it runs after them: a
	// deferred Stop ran first, their DeleteFunction failed unseen, and a
	// rerun found f1's policy from the run before (#1529).
	t.Cleanup(srv.Stop)
	p := functionsProvider{defaultProject: "console-edit", runAddr: func() string { return ln.Addr().String() }, storage: true}
	for _, env := range []string{functionGen1, functionGen2} {
		id := "f1"
		if env == functionGen2 {
			id = "f2"
		}
		_, _ = p.ActAtFile(ctx, "", nil, functionsCreateAction, map[string]string{"name": id, "environment": env, "region": "us-central1",
			"runtime": "go123", "entryPoint": "Hello", "memory": "256M"}, console.UploadedFile{Name: "src.zip", Reader: strings.NewReader("zip")})
		t.Cleanup(func() {
			if err := p.Delete(context.Background(), "", id); err != nil {
				t.Errorf("delete %s: %v", id, err)
			}
		})
	}

	fields := func(id string) map[string]string {
		t.Helper()
		for _, a := range p.DetailActions(ctx, "", []string{id}) {
			if a.ID == functionsEditAction {
				out := map[string]string{}
				for _, f := range a.Fields {
					out[f.Name] = f.Default
				}
				return out
			}
		}
		t.Fatalf("%s's page has no Edit function", id)
		return nil
	}
	if f := fields("f1"); f["memory"] != "256" {
		t.Errorf("a 1st-gen function's Edit function = %v; want its memory in MB", f)
	}
	if f := fields("f2"); f["memory"] != "256M" {
		t.Errorf("a 2nd-gen function's Edit function = %v; want its memory", f)
	}

	// Edit function sends UpdateFunction with the form's fields in its mask.
	if _, err := p.ActAtResult(ctx, "", []string{"f1"}, functionsEditAction, map[string]string{"description": "edited", "memory": "512",
		"timeout": "30", "maxInstances": "3", "env": "MODE=edit"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ActAtResult(ctx, "", []string{"f2"}, functionsEditAction, map[string]string{"description": "edited 2", "memory": "1G", "env": "A=1"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(updates) != 2 {
		t.Fatalf("UpdateFunction calls = %v", updates)
	}
	u1, u2 := updates[0].(*fv1.UpdateFunctionRequest), updates[1].(*functionspb.UpdateFunctionRequest)
	mu.Unlock()
	if f := u1.GetFunction(); f.GetDescription() != "edited" || f.GetAvailableMemoryMb() != 512 || f.GetMaxInstances() != 3 ||
		f.GetEnvironmentVariables()["MODE"] != "edit" || f.GetTimeout().GetSeconds() != 30 ||
		strings.Join(u1.GetUpdateMask().GetPaths(), ",") != "description,availableMemoryMb,maxInstances,environmentVariables,timeout" {
		t.Errorf("the v1 UpdateFunction = %v", u1)
	}
	if f := u2.GetFunction(); f.GetDescription() != "edited 2" || f.GetServiceConfig().GetAvailableMemory() != "1G" ||
		f.GetServiceConfig().GetEnvironmentVariables()["A"] != "1" || len(u2.GetUpdateMask().GetPaths()) != 4 {
		t.Errorf("the v2 UpdateFunction = %v", u2)
	}
	if _, err := p.ActAtResult(ctx, "", []string{"f2"}, functionsEditAction, map[string]string{"memory": "lots"}); err == nil {
		t.Error("Edit function took memory \"lots\"")
	}

	for _, id := range []string{"f1", "f2"} {
		res, err := p.ActAtResult(ctx, "", []string{id}, functionsDownloadAction, nil)
		if err != nil || len(res.Items) != 1 || !strings.Contains(res.Items[0].Link, "://") {
			t.Errorf("Download source of %s = %+v, %v; want GenerateDownloadUrl's link", id, res, err)
		}
	}

	if p.PolicyOn(ctx, []string{"f2"}) != nil {
		t.Error("a 2nd-gen function has a Permissions tab")
	}
	if p.PolicyOn(ctx, []string{"f1"}) == nil {
		t.Fatal("a 1st-gen function has no Permissions tab")
	}
	pol, err := p.GetPolicy(ctx, "", []string{"f1"})
	if err != nil {
		t.Fatal(err)
	}
	pol.Bindings = append(pol.Bindings, console.Binding{Role: "roles/cloudfunctions.invoker", Members: []string{"allUsers"}})
	if err := p.SetPolicy(ctx, "", []string{"f1"}, pol); err != nil {
		t.Fatal(err)
	}
	if got, err := p.GetPolicy(ctx, "", []string{"f1"}); err != nil || len(got.Bindings) != 1 || got.Bindings[0].Members[0] != "allUsers" {
		t.Errorf("GetIamPolicy after SetIamPolicy = %+v, %v", got, err)
	}
	if held, err := p.TestPermissions(ctx, "", []string{"f1"}, []string{"cloudfunctions.functions.invoke"}); err != nil || len(held) != 1 {
		t.Errorf("TestIamPermissions = %v, %v", held, err)
	}
}
