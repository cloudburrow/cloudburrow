package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestConsoleBuildTriggersAndWorkerPools (#1422): the Worker pools page
// creates and deletes a pool, and the Triggers page creates a trigger on
// it, offers Run on it (which reaches RunBuildTrigger: the instance has no
// host steps, so the run is refused naming the flag), and deletes it, each
// through the official Cloud Build client on the Run port.
func TestConsoleBuildTriggersAndWorkerPools(t *testing.T) {
	ctx := context.Background()
	s := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	g := grpc.NewServer()
	s.Builds(stubSource{}, blockingBuilder{}).CloudBuild().Register(g)
	runadapter.NewOperationsServer(s).Register(g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	b := runBuildsProvider{defaultProject: "console-trig", runAddr: func() string { return ln.Addr().String() }}
	pools, trigs := buildWorkerPoolsProvider{b: b}, buildTriggersProvider{b: b}

	if _, err := pools.Create(ctx, "", map[string]string{"name": "p1", "region": "us-central1", "machineType": "e2-standard-2"}); err != nil {
		t.Fatalf("pool Create: %v", err)
	}
	l, _ := pools.List(ctx, "")
	if len(l.Items) != 1 || l.Items[0].Name != "p1" || l.Items[0].Fields["Machine type"] != "e2-standard-2" || l.Items[0].Status != "Running" {
		t.Fatalf("pools = %+v", l.Items)
	}
	pool := "projects/console-trig/locations/us-central1/workerPools/p1"
	if _, err := trigs.Create(ctx, "", map[string]string{"name": "t1", "region": "us-central1", "image": "alpine:3.20",
		"command": "echo $TRIGGER_NAME", "pool": pool}); err != nil {
		t.Fatalf("trigger Create: %v", err)
	}
	tl, _ := trigs.List(ctx, "")
	if len(tl.Items) != 1 || tl.Items[0].Name != "t1" || tl.Items[0].Fields["Pool"] != pool {
		t.Fatalf("triggers = %+v", tl.Items)
	}
	acts := trigs.Actions(tl.Items[0])
	if len(acts) != 1 || acts[0].ID != "run" {
		t.Fatalf("actions = %+v", acts)
	}
	if err := trigs.Act(ctx, "", "t1", "run"); err == nil || !strings.Contains(err.Error(), "--cloudbuild-host-steps") {
		t.Errorf("Run = %v; want the host-steps refusal from RunBuildTrigger", err)
	}
	if err := trigs.Delete(ctx, "", "t1"); err != nil {
		t.Fatalf("trigger Delete: %v", err)
	}
	if err := pools.Delete(ctx, "", "p1"); err != nil {
		t.Fatalf("pool Delete: %v", err)
	}
	if l, _ := pools.List(ctx, ""); len(l.Items) != 0 {
		t.Errorf("pools after delete = %+v", l.Items)
	}
	if tl, _ := trigs.List(ctx, ""); len(tl.Items) != 0 {
		t.Errorf("triggers after delete = %+v", tl.Items)
	}
	var _ console.Provider = trigs
}
