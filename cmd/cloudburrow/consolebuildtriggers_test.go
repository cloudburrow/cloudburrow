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

// A trigger's page (the console parity audit): it shows what
// GetBuildTrigger reads, Edit trigger changes the description, the step and
// the pool through UpdateBuildTrigger, and Disable and Enable flip
// disabled, which takes Run off the page and puts it back.
func TestConsoleBuildTriggerPageEditAndDisable(t *testing.T) {
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
	trigs := buildTriggersProvider{b: runBuildsProvider{defaultProject: "console-trig-edit", runAddr: func() string { return ln.Addr().String() }}}
	if _, err := trigs.Create(ctx, "", map[string]string{"name": "t2", "region": "global", "image": "alpine:3.20",
		"command": "echo one", "description": "first"}); err != nil {
		t.Fatalf("trigger Create: %v", err)
	}
	t.Cleanup(func() { _ = trigs.Delete(ctx, "", "t2") })
	ids := func() string {
		var out []string
		for _, a := range trigs.DetailActions(ctx, "", []string{"t2"}) {
			out = append(out, a.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids(); got != "edit-trigger,disable-trigger,run" {
		t.Errorf("an enabled manual trigger's actions: %s", got)
	}
	if err := trigs.ActAt(ctx, "", []string{"t2"}, "edit-trigger", map[string]string{"description": "second", "image": "busybox:1.36",
		"command": "echo two", "pool": ""}); err != nil {
		t.Fatalf("Edit trigger: %v", err)
	}
	d, err := trigs.Detail(ctx, "", []string{"t2"})
	if err != nil || d.Unavailable != "" {
		t.Fatalf("Detail = %+v, %v", d, err)
	}
	got := map[string]string{}
	for _, pr := range d.Summary {
		got[pr.Label] = pr.Value
	}
	if got["Description"] != "second" || got["Step image"] != "busybox:1.36" || got["Command"] != "echo two" || got["Status"] != "Enabled" {
		t.Errorf("the trigger's page after Edit trigger: %v", got)
	}
	if err := trigs.ActAt(ctx, "", []string{"t2"}, "disable-trigger", nil); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if got := ids(); got != "edit-trigger,enable-trigger" {
		t.Errorf("a disabled trigger's actions: %s", got)
	}
	if tl, _ := trigs.List(ctx, ""); len(tl.Items) != 1 || tl.Items[0].Status != "Disabled" {
		t.Errorf("the list after Disable: %+v", tl.Items)
	}
	if err := trigs.ActAt(ctx, "", []string{"t2"}, "enable-trigger", nil); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if d, _ := trigs.Detail(ctx, "", []string{"t2"}); !strings.Contains(consoleDetailText(d), "Enabled") {
		t.Errorf("the trigger's page after Enable: %+v", d.Summary)
	}
	if err := trigs.ActAt(ctx, "", []string{"t2"}, "edit-trigger", map[string]string{"image": "", "command": "x"}); err == nil {
		t.Error("Edit trigger took an empty step image")
	}
}

// nopSubs is a Pub/Sub that takes every subscription.
type nopSubs struct{}

func (nopSubs) CreateTopic(context.Context, string, map[string]string) error { return nil }
func (nopSubs) DeleteTopic(context.Context, string) error                    { return nil }
func (nopSubs) CreatePushSubscription(context.Context, runadapter.PushSubscription) error {
	return nil
}
func (nopSubs) DeleteSubscription(context.Context, string) error { return nil }
func (nopSubs) CreateNotification(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (nopSubs) DeleteNotification(context.Context, string, string) error { return nil }

// The Triggers page makes a Pub/Sub trigger from its topic field and lists
// its event as the topic's messages (#1506).
func TestConsoleBuildPubsubTrigger(t *testing.T) {
	ctx := context.Background()
	s := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	g := grpc.NewServer()
	s.Builds(stubSource{}, blockingBuilder{}).WithTriggerEvents(nopSubs{}, func() string { return "http://h:1" }).CloudBuild().Register(g)
	runadapter.NewOperationsServer(s).Register(g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	trigs := buildTriggersProvider{b: runBuildsProvider{defaultProject: "console-ps", runAddr: func() string { return ln.Addr().String() }}}
	if _, err := trigs.Create(ctx, "", map[string]string{"name": "ps1", "image": "alpine:3.20", "command": "true",
		"topic": "projects/console-ps/topics/builds"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = trigs.Delete(context.Background(), "", "ps1") })
	tl, _ := trigs.List(ctx, "")
	if len(tl.Items) != 1 || tl.Items[0].Fields["Event"] != "Pub/Sub message: projects/console-ps/topics/builds" {
		t.Fatalf("triggers = %+v", tl.Items)
	}
	if acts := trigs.Actions(tl.Items[0]); len(acts) != 0 {
		t.Errorf("a Pub/Sub trigger offers %+v; it runs from its topic", acts)
	}
}
