package main

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
)

// TestConsoleBuildWorkerPoolPageAndDefaultServiceAccount (#1491): a pool's
// page shows what GetWorkerPool reads, Edit worker pool changes its display
// name, machine type and disk through UpdateWorkerPool with the etag the
// form was drawn from, and the Triggers page names the default build
// service account GetDefaultServiceAccount answers.
func TestConsoleBuildWorkerPoolPageAndDefaultServiceAccount(t *testing.T) {
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
	b := runBuildsProvider{defaultProject: "console-pool", runAddr: func() string { return ln.Addr().String() }}
	pools, trigs := buildWorkerPoolsProvider{b: b}, buildTriggersProvider{b: b}
	if _, err := pools.Create(ctx, "", map[string]string{"name": "p1", "region": "us-central1", "machineType": "e2-standard-2"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pools.Delete(context.Background(), "", "p1") })
	if l, _ := pools.List(ctx, ""); !l.RowsOpenable {
		t.Error("the Worker pools list's rows do not open")
	}
	props := func() map[string]string {
		t.Helper()
		d, err := pools.Detail(ctx, "", []string{"p1"})
		if err != nil || d.Unavailable != "" {
			t.Fatalf("Detail = %+v, %v", d, err)
		}
		out := map[string]string{}
		for _, p := range d.Summary {
			out[p.Label] = p.Value
		}
		return out
	}
	if got := props(); got["Machine type"] != "e2-standard-2" || got["State"] != "RUNNING" {
		t.Errorf("the pool's page = %v", got)
	}
	acts := pools.DetailActions(ctx, "", []string{"p1"})
	if len(acts) != 1 || acts[0].ID != "edit-pool" {
		t.Fatalf("the pool's actions = %+v", acts)
	}
	etag := ""
	for _, f := range acts[0].Fields {
		if f.Name == "etag" {
			etag = f.Default
		}
	}
	if err := pools.ActAt(ctx, "", []string{"p1"}, "edit-pool", map[string]string{"displayName": "Builders", "machineType": "e2-highcpu-8",
		"diskSizeGb": "200", "etag": etag}); err != nil {
		t.Fatal(err)
	}
	if got := props(); got["Display name"] != "Builders" || got["Machine type"] != "e2-highcpu-8" || got["Disk size (GB)"] != "200" {
		t.Errorf("the pool's page after Edit worker pool = %v", got)
	}
	if err := pools.ActAt(ctx, "", []string{"p1"}, "edit-pool", map[string]string{"machineType": "e2-standard-4", "etag": etag}); err == nil {
		t.Error("Edit worker pool with the etag of a pool since changed was accepted")
	}

	l, err := trigs.List(ctx, "")
	if err != nil || len(l.Summary) != 1 || len(l.Summary[0].Properties) != 1 ||
		l.Summary[0].Properties[0].Label != "Default build service account" || l.Summary[0].Properties[0].Value == "" {
		t.Errorf("the Triggers page's summary = %+v, %v; want the default build service account", l.Summary, err)
	}
}
