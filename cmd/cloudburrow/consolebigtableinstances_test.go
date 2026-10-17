package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/bigtable/bttest"

	"github.com/cloudburrow/cloudburrow/internal/bigtablefront"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// bigtableInstancesConsole is the Bigtable instances screen against the
// Bigtable front (internal/bigtablefront) in front of the official in-memory
// server, served by a console, with the official instance admin client
// reading the same front.
func bigtableInstancesConsole(t *testing.T) (bigtableInstancesProvider, *httptest.Server, *bigtable.InstanceAdminClient) {
	t.Helper()
	emu, err := bttest.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(emu.Close)
	f, err := bigtablefront.New(emu.Addr, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = f.Serve(ctx, l); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	p := bigtableInstancesProvider{endpoint: l.Addr().String()}
	srv := httptest.NewServer(console.New("127.0.0.1:0", nil, p).Handler())
	t.Cleanup(srv.Close)
	iac, err := bigtable.NewInstanceAdminClient(context.Background(), "demo", localOpts(p.endpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iac.Close() })
	return p, srv, iac
}

func bigtableInstancesAct(t *testing.T, srv *httptest.Server, path []string, action string, values map[string]string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"Path": path, "Action": action, "Values": values})
	resp, err := http.Post(srv.URL+"/api/actions/bigtable-instances?project=demo", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func actionFields(actions []console.Action, id string) (map[string]string, bool) {
	for _, a := range actions {
		if a.ID == id {
			out := map[string]string{}
			for _, f := range a.Fields {
				out[f.Name] = f.Default
			}
			return out, true
		}
	}
	return nil, false
}

// TestConsoleBigtableInstancesScreen (#1358): Create instance makes the
// instance and its first cluster the official client reads; the instance's
// page shows them and its default app profile; Edit instance, Add cluster,
// Edit nodes (autoscaled and back to fixed), Create, Edit and Delete app
// profile, Delete cluster and Delete instance each change what the client
// reads, and a refusal is the front's own message.
func TestConsoleBigtableInstancesScreen(t *testing.T) {
	p, srv, iac := bigtableInstancesConsole(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	name, err := p.Create(ctx, "demo", map[string]string{"instance": "orders-inst", "displayName": "Orders",
		"type": "PRODUCTION", "labels": `{"env":"dev"}`, "cluster": "orders-c1", "zone": "us-east1-b",
		"storage": "HDD", "nodes": "2"})
	if err != nil || name != "orders-inst" {
		t.Fatalf("Create = %q, %v", name, err)
	}
	info, err := iac.InstanceInfo(ctx, "orders-inst")
	if err != nil || info.DisplayName != "Orders" || info.Labels["env"] != "dev" || info.InstanceType != bigtable.PRODUCTION {
		t.Fatalf("InstanceInfo = %+v, %v", info, err)
	}
	c1, err := iac.GetCluster(ctx, "orders-inst", "orders-c1")
	if err != nil || c1.Zone != "us-east1-b" || c1.ServeNodes != 2 || c1.StorageType != bigtable.HDD {
		t.Fatalf("GetCluster = %+v, %v", c1, err)
	}
	if _, err := p.Create(ctx, "demo", map[string]string{"instance": "orders-inst", "displayName": "Orders",
		"cluster": "orders-c1", "zone": "us-east1-b", "nodes": "1"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("Create of a taken ID: %v", err)
	}

	list, err := p.List(ctx, "demo")
	if err != nil || len(list.Items) != 1 || list.Items[0].Fields["Clusters"] != "1" || list.Items[0].Status != "Ready" {
		t.Fatalf("List = %+v, %v", list, err)
	}
	d, err := p.Detail(ctx, "demo", []string{"orders-inst"})
	if err != nil || d.Unavailable != "" || len(d.Sections) != 9 {
		t.Fatalf("Detail = %+v, %v", d, err)
	}
	if cl := d.Sections[0].Listing.Items; len(cl) != 1 || cl[0].Fields["Nodes"] != "2" || cl[0].Fields["Storage type"] != "HDD" {
		t.Errorf("the Clusters tab = %+v", cl)
	}
	if ap := d.Sections[1].Listing.Items; len(ap) != 1 || ap[0].Name != "default" || ap[0].Fields["Routing"] != "Single cluster: orders-c1" {
		t.Errorf("the App profiles tab = %+v", ap)
	}

	page := p.DetailActions(ctx, "demo", []string{"orders-inst"})
	edit, ok := actionFields(page, "editinstance")
	if !ok || edit["displayName"] != "Orders" {
		t.Fatalf("the page's actions = %+v", page)
	}
	edit["displayName"], edit["labels"] = "Orders live", `{"env":"prod","team":"data"}`
	if code, out := bigtableInstancesAct(t, srv, []string{"orders-inst"}, "editinstance", edit); code != http.StatusOK {
		t.Fatalf("Edit instance = %d: %s", code, out)
	}
	if info, _ := iac.InstanceInfo(ctx, "orders-inst"); info.DisplayName != "Orders live" || info.Labels["team"] != "data" {
		t.Errorf("after Edit instance, InstanceInfo = %+v", info)
	}
	edit["displayName"] = "no"
	if code, out := bigtableInstancesAct(t, srv, []string{"orders-inst"}, "editinstance", edit); code == http.StatusOK || !strings.Contains(out, "display_name") {
		t.Errorf("Edit instance with a 2-character name = %d: %s", code, out)
	}

	if code, out := bigtableInstancesAct(t, srv, []string{"orders-inst"}, "addcluster", map[string]string{
		"cluster": "orders-c2", "zone": "europe-west1-b", "storage": "SSD", "autoscaling": "true",
		"minNodes": "2", "maxNodes": "5", "cpuTarget": "50"}); code != http.StatusOK {
		t.Fatalf("Add cluster = %d: %s", code, out)
	}
	c2, err := iac.GetCluster(ctx, "orders-inst", "orders-c2")
	if err != nil || c2.AutoscalingConfig == nil || c2.AutoscalingConfig.MaxNodes != 5 || c2.AutoscalingConfig.CPUTargetPercent != 50 {
		t.Fatalf("GetCluster of the added cluster = %+v, %v", c2, err)
	}
	// Edit nodes: c2 back to a fixed size, c1 to a new fixed size, then
	// autoscaled.
	nodes := map[string]string{"nodes": "4", "autoscaling": "false"}
	for _, c := range []string{"orders-c2", "orders-c1"} {
		if code, out := bigtableInstancesAct(t, srv, []string{"orders-inst", "clusters", c}, "editnodes", nodes); code != http.StatusOK {
			t.Fatalf("Edit nodes of %s = %d: %s", c, code, out)
		}
		got, _ := iac.GetCluster(ctx, "orders-inst", c)
		if got.ServeNodes != 4 || got.AutoscalingConfig != nil {
			t.Errorf("after Edit nodes, %s = %+v", c, got)
		}
	}
	if code, out := bigtableInstancesAct(t, srv, []string{"orders-inst", "clusters", "orders-c1"}, "editnodes", map[string]string{
		"autoscaling": "true", "minNodes": "1", "maxNodes": "3", "cpuTarget": "70"}); code != http.StatusOK {
		t.Fatalf("Edit nodes to autoscaling = %d: %s", code, out)
	}
	if got, _ := iac.GetCluster(ctx, "orders-inst", "orders-c1"); got.AutoscalingConfig == nil || got.AutoscalingConfig.CPUTargetPercent != 70 {
		t.Errorf("after Edit nodes to autoscaling, c1 = %+v", got)
	}

	// App profiles.
	if code, out := bigtableInstancesAct(t, srv, []string{"orders-inst"}, "createprofile", map[string]string{
		"profile": "batch", "description": "Batch jobs", "routing": "SINGLE_CLUSTER", "routeTo": "orders-c2", "transactionalWrites": "true"}); code != http.StatusOK {
		t.Fatalf("Create app profile = %d: %s", code, out)
	}
	ap, err := iac.GetAppProfile(ctx, "orders-inst", "batch")
	if err != nil || ap.Description != "Batch jobs" || ap.GetSingleClusterRouting().GetClusterId() != "orders-c2" ||
		!ap.GetSingleClusterRouting().GetAllowTransactionalWrites() {
		t.Fatalf("GetAppProfile = %v, %v", ap, err)
	}
	// c2 is routed to by batch: Delete cluster is refused with the front's
	// message.
	if code, out := bigtableInstancesAct(t, srv, []string{"orders-inst", "clusters", "orders-c2"}, "deletecluster", nil); code == http.StatusOK || !strings.Contains(out, "routes to cluster") {
		t.Errorf("Delete cluster routed to = %d: %s", code, out)
	}
	if code, out := bigtableInstancesAct(t, srv, []string{"orders-inst", "appProfiles", "batch"}, "editprofile", map[string]string{
		"description": "Any cluster", "routing": "MULTI_CLUSTER"}); code != http.StatusOK {
		t.Fatalf("Edit app profile = %d: %s", code, out)
	}
	if ap, _ := iac.GetAppProfile(ctx, "orders-inst", "batch"); ap.Description != "Any cluster" || ap.GetMultiClusterRoutingUseAny() == nil {
		t.Errorf("after Edit app profile, GetAppProfile = %v", ap)
	}
	if code, out := bigtableInstancesAct(t, srv, []string{"orders-inst", "appProfiles", "batch"}, "deleteprofile", nil); code != http.StatusOK {
		t.Fatalf("Delete app profile = %d: %s", code, out)
	}
	if _, err := iac.GetAppProfile(ctx, "orders-inst", "batch"); err == nil {
		t.Error("the app profile is still there after Delete app profile")
	}
	// default routes to c1; c2 can go.
	if code, out := bigtableInstancesAct(t, srv, []string{"orders-inst", "clusters", "orders-c2"}, "deletecluster", nil); code != http.StatusOK {
		t.Fatalf("Delete cluster = %d: %s", code, out)
	}
	if cs, _ := iac.Clusters(ctx, "orders-inst"); len(cs) != 1 {
		t.Errorf("after Delete cluster, Clusters = %v", cs)
	}
	if code, out := bigtableInstancesAct(t, srv, []string{"orders-inst", "clusters", "orders-c1"}, "deletecluster", nil); code == http.StatusOK || !strings.Contains(out, "last cluster") {
		t.Errorf("Delete of the last cluster = %d: %s", code, out)
	}

	// Permissions: a binding set on the tab reads back through the client.
	pol, err := p.GetPolicy(ctx, "demo", []string{"orders-inst"})
	if err != nil {
		t.Fatal(err)
	}
	pol.Bindings = append(pol.Bindings, console.Binding{Role: "roles/bigtable.user", Members: []string{"user:ada@example.com"}})
	if err := p.SetPolicy(ctx, "demo", []string{"orders-inst"}, pol); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	got, err := iac.InstanceIAM("orders-inst").Policy(ctx)
	if err != nil || !got.HasRole("user:ada@example.com", "roles/bigtable.user") {
		t.Errorf("the client's policy = %v, %v", got, err)
	}
	if err := p.SetPolicy(ctx, "demo", []string{"orders-inst"}, pol); err == nil {
		t.Error("SetPolicy with the etag the change made stale was accepted")
	}
	if p.PolicyOn(ctx, []string{"orders-inst", "clusters", "orders-c1"}) != nil {
		t.Error("a cluster has a Permissions tab")
	}

	if code, out := bigtableInstancesAct(t, srv, []string{"orders-inst"}, "deleteinstance", nil); code != http.StatusOK {
		t.Fatalf("Delete instance = %d: %s", code, out)
	}
	if _, err := iac.InstanceInfo(ctx, "orders-inst"); err == nil {
		t.Error("the instance is still there after Delete instance")
	}
}

// TestBigtableInstanceFormsRefuseWhatTheyCannotSend: the form values the
// provider reads are checked before anything is sent.
func TestBigtableInstanceFormsRefuseWhatTheyCannotSend(t *testing.T) {
	if _, err := formCluster("i", map[string]string{"storage": "TAPE"}); err == nil {
		t.Error("storage type TAPE was accepted")
	}
	if _, err := formCluster("i", map[string]string{"nodes": "two"}); err == nil {
		t.Error("nodes \"two\" was accepted")
	}
	if _, err := formRouting(map[string]string{"routing": "SINGLE_CLUSTER"}); err == nil {
		t.Error("single-cluster routing with no cluster was accepted")
	}
	cc, err := formCluster("i", map[string]string{"cluster": "c-one1", "zone": "z", "autoscaling": "true", "minNodes": "1", "maxNodes": "2", "cpuTarget": "40"})
	if err != nil || cc.AutoscalingConfig == nil || cc.NumNodes != 0 {
		t.Errorf("formCluster autoscaled = %+v, %v", cc, err)
	}
}
