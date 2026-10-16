package run

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// fromLoopback is a gRPC call from the host: dialed at 127.0.0.1, from
// loopback, as the SDK, gcloud through the harness and the console call.
func fromLoopback() context.Context { return grpcCaller("127.0.0.1:9004", "127.0.0.1:50123") }

func grpcCaller(authority, remote string) context.Context {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(":authority", authority))
	addr, _ := net.ResolveTCPAddr("tcp", remote)
	return peer.NewContext(ctx, &peer.Peer{Addr: addr})
}

// jsonCaller is a JSON request's context as SourceDeployHandler hands it on.
func jsonCaller(host, remote string) context.Context {
	r := httptest.NewRequest(http.MethodPost, "/v1/projects/p/builds", nil)
	r.Host, r.RemoteAddr = host, remote
	return withCaller(r).Context()
}

func stepsBuild() *cbpb.CreateBuildRequest {
	return &cbpb.CreateBuildRequest{ProjectId: "proj-gate", Build: &cbpb.Build{
		Steps: []*cbpb.BuildStep{{Name: "alpine", Args: []string{"echo", "hi"}}}}}
}

// TestHostStepsGateOff (#1382): without --cloudbuild-host-steps an
// arbitrary step is refused before anything runs, naming the flag and the
// Docker socket; a docker build still runs.
func TestHostStepsGateOff(t *testing.T) {
	s := NewServer(&Knative{}, "t", time.Minute)
	b := &stepBuilder{fakeBuilder: fakeBuilder{got: make(chan BuildSpec, 1)}}
	f := fakeFetcher{objects: map[string]string{"b/src.tgz": "archive"}}
	cb := s.Builds(f, b).CloudBuild()
	before := len(CloudBuilds("proj-gate", ""))
	_, err := cb.CreateBuild(fromLoopback(), stepsBuild())
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "--cloudbuild-host-steps") ||
		!strings.Contains(err.Error(), "Docker socket") {
		t.Fatalf("CreateBuild with the gate off = %v; want FAILED_PRECONDITION naming the flag and the socket", err)
	}
	if b.opened != "" || len(CloudBuilds("proj-gate", "")) != before {
		t.Error("a refused build opened a workspace or was recorded")
	}
	op, err := cb.CreateBuild(fromLoopback(), &cbpb.CreateBuildRequest{ProjectId: "proj-gate", Build: &cbpb.Build{Source: cbSource(),
		Steps: []*cbpb.BuildStep{{Name: "gcr.io/cloud-builders/docker", Args: []string{"build", "-t", "gcr.io/p/img", "."}}}}})
	if err != nil {
		t.Fatalf("a docker build with the gate off: %v", err)
	}
	<-b.got
	if res, err := buildResult(t, s, op.GetName()); err != nil || res.GetStatus() != cbpb.Build_SUCCESS {
		t.Errorf("docker build = %v, %v", res, err)
	}
}

// TestHostStepsGateFromPod (#1382): with the flag on, a build with an
// arbitrary step from a pod (through cloudburrow-host or
// host.docker.internal, or from another address) is refused with the
// reason, over gRPC and JSON alike, and RetryBuild from a pod too.
func TestHostStepsGateFromPod(t *testing.T) {
	s := NewServer(&Knative{}, "t", time.Minute)
	b := &stepBuilder{fakeBuilder: fakeBuilder{got: make(chan BuildSpec, 1)}}
	cb := s.Builds(fakeFetcher{}, b).AllowHostSteps(true).CloudBuild()
	for name, tc := range map[string]struct {
		ctx  context.Context
		want string
	}{
		"grpc cloudburrow-host": {grpcCaller("cloudburrow-host:9004", "127.0.0.1:50000"), "cloudburrow-host"},
		"json cloudburrow-host": {jsonCaller("cloudburrow-host.ns.svc.cluster.local:9004", "127.0.0.1:50000"), "cloudburrow-host"},
		"host.docker.internal":  {grpcCaller("host.docker.internal:9004", "127.0.0.1:50000"), "host.docker.internal"},
		"gateway address":       {jsonCaller("172.18.0.1:9004", "127.0.0.1:50000"), "not a loopback address"},
		"remote peer":           {grpcCaller("127.0.0.1:9004", "192.168.1.20:50000"), "not the host's loopback"},
		"no caller":             {context.Background(), "no address"},
	} {
		_, err := cb.CreateBuild(tc.ctx, stepsBuild())
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "only from the host's loopback") ||
			!strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v; want FAILED_PRECONDITION saying %q", name, err, tc.want)
		}
	}
	if b.opened != "" {
		t.Error("a refused build opened a workspace")
	}
	op, err := cb.CreateBuild(fromLoopback(), stepsBuild())
	if err != nil {
		t.Fatal(err)
	}
	first, _ := buildResult(t, s, op.GetName())
	if _, err := cb.RetryBuild(grpcCaller("cloudburrow-host:9004", "127.0.0.1:1"), &cbpb.RetryBuildRequest{Name: first.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("RetryBuild from a pod = %v; want FAILED_PRECONDITION", err)
	}
}

// TestHostStepsGateFromLoopback (#1382): with the flag on, a build from the
// host's loopback runs its arbitrary steps, by any loopback name.
func TestHostStepsGateFromLoopback(t *testing.T) {
	s := NewServer(&Knative{}, "t", time.Minute)
	b := &stepBuilder{fakeBuilder: fakeBuilder{got: make(chan BuildSpec, 1)}}
	cb := s.Builds(fakeFetcher{}, b).AllowHostSteps(true).CloudBuild()
	for name, ctx := range map[string]context.Context{
		"grpc 127.0.0.1": fromLoopback(),
		"json localhost": jsonCaller("localhost:9004", "127.0.0.1:50000"),
		"json [::1]":     jsonCaller("[::1]:9004", "[::1]:50000"),
	} {
		op, err := cb.CreateBuild(ctx, stepsBuild())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res, err := buildResult(t, s, op.GetName()); err != nil || res.GetStatus() != cbpb.Build_SUCCESS {
			t.Errorf("%s: build = %v, %v", name, res, err)
		}
	}
}
