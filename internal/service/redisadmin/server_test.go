package redisadmin

import (
	"context"
	"errors"
	"testing"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeBackend struct {
	info ServerInfo
	err  error
}

func (f fakeBackend) Info(context.Context) (ServerInfo, error) { return f.info, f.err }

func newTestServer(err error) *Server {
	return NewServer(Instance{Project: "my-proj", Host: "127.0.0.1", Port: 9016, Created: time.Unix(100, 0)},
		fakeBackend{info: ServerInfo{Version: "7.2.4", MaxMemory: 1 << 30}, err: err})
}

func code(err error) codes.Code { return status.Code(err) }

func TestDescribesTheRunningServer(t *testing.T) {
	s := newTestServer(nil)
	ctx := context.Background()
	name := "projects/my-proj/locations/us-central1/instances/memorystore"
	got, err := s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetName() != name || got.GetHost() != "127.0.0.1" || got.GetPort() != 9016 ||
		got.GetState() != redispb.Instance_READY || got.GetRedisVersion() != "REDIS_7_2" || got.GetMemorySizeGb() != 1 {
		t.Errorf("GetInstance = %v", got)
	}
	for _, parent := range []string{"projects/my-proj/locations/us-central1", "projects/my-proj/locations/-"} {
		l, err := s.ListInstances(ctx, &redispb.ListInstancesRequest{Parent: parent})
		if err != nil || len(l.GetInstances()) != 1 {
			t.Errorf("ListInstances(%s) = %v, %v", parent, l, err)
		}
	}
	for _, parent := range []string{"projects/other/locations/us-central1", "projects/my-proj/locations/europe-west1"} {
		l, err := s.ListInstances(ctx, &redispb.ListInstancesRequest{Parent: parent})
		if err != nil || len(l.GetInstances()) != 0 {
			t.Errorf("ListInstances(%s) = %v, %v; want none", parent, l, err)
		}
	}
}

func TestRefusals(t *testing.T) {
	s := newTestServer(nil)
	ctx := context.Background()
	if _, err := s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: "projects/my-proj/locations/us-central1/instances/other"}); code(err) != codes.NotFound {
		t.Errorf("unknown instance: %v", err)
	}
	if _, err := s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: "bad"}); code(err) != codes.InvalidArgument {
		t.Errorf("bad name: %v", err)
	}
	if _, err := s.ListInstances(ctx, &redispb.ListInstancesRequest{Parent: "projects/my-proj"}); code(err) != codes.InvalidArgument {
		t.Errorf("bad parent: %v", err)
	}
	if _, err := s.GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: newTestServer(nil).inst.Name()}); code(err) != codes.FailedPrecondition {
		t.Errorf("auth string with AUTH off: %v", err)
	}
	if _, err := s.CreateInstance(ctx, &redispb.CreateInstanceRequest{}); code(err) != codes.Unimplemented {
		t.Errorf("CreateInstance: %v", err)
	}
	down := newTestServer(errors.New("connection refused"))
	if _, err := down.GetInstance(ctx, &redispb.GetInstanceRequest{Name: down.inst.Name()}); code(err) != codes.Unavailable {
		t.Errorf("server down: %v", err)
	}
}

func TestRedisVersion(t *testing.T) {
	for in, want := range map[string]string{"7.2.4": "REDIS_7_2", "6.2.14": "REDIS_6_X", "5.0.7": "REDIS_5_0", "x": "x"} {
		if got := RedisVersion(in); got != want {
			t.Errorf("RedisVersion(%q) = %q, want %q", in, got, want)
		}
	}
	if MemorySizeGB(1<<30+1) != 2 || MemorySizeGB(0) != 0 {
		t.Error("MemorySizeGB rounding")
	}
}
