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
// page shows what GetWorkerPool reads, Edit private pool changes its
// machine type, disk, release and egress through UpdateWorkerPool with the
// etag the page was drawn from, and the Triggers page names the default build
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
	// Edit private pool (#1854): the pool's name opens it, a page of
	// Create private pool's settings prefilled, ending in Save, and each
	// row's Delete is a trash icon.
	l, _ := pools.List(ctx, "")
	if len(l.Items) != 1 || l.Items[0].Link != "/cloud-build/worker-pools/p1?edit=1&project=console-pool" {
		t.Errorf("the pool's row link = %+v", l.Items)
	}
	if !pools.RowDeleteIcon() {
		t.Error("the rows' Delete is not a trash icon")
	}
	d, _ := pools.Detail(ctx, "", []string{"p1"})
	if d.Edit == nil || !d.Edit.Page || d.Edit.Title != "Edit private pool" || d.Edit.Submit != "Save" {
		t.Fatalf("the pool's edit form = %+v", d.Edit)
	}
	values := map[string]string{}
	for _, f := range d.Edit.Fields {
		if f.Name == "name" || f.Name == "region" || f.Name == "network" {
			t.Errorf("Edit private pool offers %s, which is fixed", f.Name)
		}
		values[f.Name] = f.Default
	}
	if values["series"] != "e2" || values["machineType-e2"] != "e2-standard-2" || values["externalIPs"] != "true" || values["etag"] == "" {
		t.Errorf("Edit private pool is not prefilled: %v", values)
	}
	etag := values["etag"]
	values["machineType-e2"], values["diskSizeGb"], values["channel"] = "e2-highcpu-8", "200", "stable"
	delete(values, "externalIPs") // unchecked
	if err := pools.Edit(ctx, "", []string{"p1"}, values); err != nil {
		t.Fatal(err)
	}
	if got := props(); got["Machine type"] != "e2-highcpu-8" || got["Disk size (GB)"] != "200" {
		t.Errorf("the pool's page after Save = %v", got)
	}
	d, _ = pools.Detail(ctx, "", []string{"p1"})
	again := map[string]string{}
	for _, f := range d.Edit.Fields {
		again[f.Name] = f.Default
	}
	if again["channel"] != "stable" || again["externalIPs"] != "false" || again["diskSizeGb"] != "200" {
		t.Errorf("Edit private pool after Save = %v", again)
	}
	values["etag"] = etag
	if err := pools.Edit(ctx, "", []string{"p1"}, values); err == nil {
		t.Error("Save with the etag of a pool since changed was accepted")
	}
	values["diskSizeGb"], values["etag"] = "50", again["etag"]
	if err := pools.Edit(ctx, "", []string{"p1"}, values); err == nil {
		t.Error("Save with a 50 GB disk was accepted")
	}

	// The default build service account is Create trigger's Service
	// account choice, and Settings' (#1854).
	sa := trigs.defaultServiceAccount(ctx, "")
	if len(sa) != 1 || len(sa[0].Properties) != 1 || sa[0].Properties[0].Value == "" {
		t.Errorf("the default build service account = %+v", sa)
	}
}
