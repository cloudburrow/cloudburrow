package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/gke"
)

// consoleGKEBackend's Delete runs on the API's operation goroutine, so
// deleted is guarded and read through deletedKinds.
type consoleGKEBackend struct {
	mu      sync.Mutex
	deleted []string
}

func (b *consoleGKEBackend) deletedKinds() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.deleted...)
}

func (*consoleGKEBackend) Create(context.Context, string, string, bool) (gke.Access, error) {
	return gke.Access{Endpoint: "127.0.0.1:1", CACert: "Q0E="}, nil
}
func (*consoleGKEBackend) Resize(context.Context, string, string, string, int) error { return nil }
func (b *consoleGKEBackend) Delete(_ context.Context, kind string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deleted = append(b.deleted, kind)
	return nil
}
func (*consoleGKEBackend) FreeBytes() (uint64, error) { return 100 << 30, nil }

// TestGKEServedClustersOnTheConsole (#1913): with the Kubernetes Engine API
// served, Create cluster makes an Autopilot or a Standard cluster through
// it, the list shows them beside the instance's own, a served cluster's
// Edit and Delete are enabled (Upgrade says why it is not), and the
// instance's own cluster still refuses them with its reason.
func TestGKEServedClustersOnTheConsole(t *testing.T) {
	ctx := context.Background()
	be := &consoleGKEBackend{}
	api, err := gke.NewServer(be, gke.Options{Instance: "cloudburrow-t", Versions: []gke.Version{{GKE: "1.36.4-gke.0", Image: "kindest/node:v1.36.4"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	p := gkeClustersProvider{name: "cloudburrow-t", kubeconfig: "/nonexistent", svc: &gkeService{api: api}, gkeDir: t.TempDir()}
	_, fields := p.CreateForm()
	for _, f := range fields {
		if f.Name == "mode" && len(f.UnavailableOptions) != 0 {
			t.Errorf("Create cluster's modes are refused: %v", f.UnavailableOptions)
		}
	}
	if _, err := p.Create(ctx, "pc", map[string]string{"mode": "Autopilot", "name": "ap-1", "locationType": "Regional", "region": "us-central1", "channel": "Regular channel"}); err != nil {
		t.Fatalf("Create Autopilot: %v", err)
	}
	if _, err := p.Create(ctx, "pc", map[string]string{"mode": "Standard", "name": "std-1", "locationType": "Zonal", "zone": "us-central1-a",
		"channel": "Stable channel", "nodes": "1", "machine": "e2-medium"}); err != nil {
		t.Fatalf("Create Standard: %v", err)
	}
	if _, err := p.Create(ctx, "pc", map[string]string{"mode": "Standard", "name": "Bad", "locationType": "Zonal", "zone": "us-central1-a", "nodes": "1"}); err == nil {
		t.Error("a bad name was accepted")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, _ := api.GetCluster(ctx, &containerpb.GetClusterRequest{Name: "projects/pc/locations/us-central1-a/clusters/std-1"})
		if c.GetStatus() == containerpb.Cluster_RUNNING {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	l, err := p.List(ctx, "pc")
	if err != nil || len(l.Items) != 3 {
		t.Fatalf("list = %v, %v", l.Items, err)
	}
	modes := map[string]string{}
	for _, r := range l.Items {
		modes[r.Name] = r.Fields["Mode"]
	}
	if modes["ap-1"] != "Autopilot" || modes["std-1"] != "Standard" {
		t.Errorf("modes = %v", modes)
	}
	acts := map[string]string{}
	for _, a := range p.DetailActions(ctx, "pc", []string{"std-1"}) {
		acts[a.ID] = a.Unavailable
	}
	if acts["edit"] != "" || acts["delete"] != "" || acts["upgrade"] == "" {
		t.Errorf("std-1's actions = %v", acts)
	}
	for _, a := range p.DetailActions(ctx, "pc", []string{"cloudburrow-t"}) {
		if a.ID == "delete" && a.Unavailable != gkeOwnClusterNote {
			t.Errorf("the instance's own cluster's Delete = %q", a.Unavailable)
		}
	}
	if err := p.ActAt(ctx, "pc", []string{"std-1"}, "edit", map[string]string{"channel": "Rapid channel", "labels": "env=dev"}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	c, _ := api.GetCluster(ctx, &containerpb.GetClusterRequest{Name: "projects/pc/locations/us-central1-a/clusters/std-1"})
	if c.GetResourceLabels()["env"] != "dev" || c.GetReleaseChannel().GetChannel() != containerpb.ReleaseChannel_RAPID {
		t.Errorf("after Edit: labels %v, channel %v", c.GetResourceLabels(), c.GetReleaseChannel())
	}
	if err := p.ActAt(ctx, "pc", []string{"std-1"}, "delete", nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for len(be.deletedKinds()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := be.deletedKinds(); len(got) != 1 {
		t.Errorf("deleted kind clusters = %v", got)
	}
}

// TestGKENodePoolsAndOperationsOnTheConsole (#1925): a served Standard
// cluster's Node pools tab has Add node pool, and each pool Resize, Edit
// and Delete, at [cluster, nodePools, pool]; its Operations tab lists what
// was done to the cluster and its pools; an Autopilot cluster's pools are
// refused with the reason.
func TestGKENodePoolsAndOperationsOnTheConsole(t *testing.T) {
	ctx := context.Background()
	api, err := gke.NewServer(&consoleGKEBackend{}, gke.Options{Instance: "cloudburrow-t", Versions: []gke.Version{{GKE: "1.36.4-gke.0", Image: "kindest/node:v1.36.4"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	p := gkeClustersProvider{name: "cloudburrow-t", kubeconfig: "/nonexistent", svc: &gkeService{api: api}, gkeDir: t.TempDir()}
	const name = "projects/pc/locations/us-central1-a/clusters/np-1"
	get := func() *containerpb.Cluster {
		c, err := api.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	idle := func() {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if get().GetStatus() == containerpb.Cluster_RUNNING {
				ops, _ := api.ListOperations(ctx, &containerpb.ListOperationsRequest{Parent: "projects/pc/locations/-"})
				busy := false
				for _, o := range ops.GetOperations() {
					busy = busy || o.GetStatus() != containerpb.Operation_DONE
				}
				if !busy {
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("the cluster stayed busy")
	}
	if _, err := p.Create(ctx, "pc", map[string]string{"mode": "Standard", "name": "np-1", "locationType": "Zonal", "zone": "us-central1-a",
		"channel": "Regular channel", "nodes": "1", "machine": "e2-medium"}); err != nil {
		t.Fatal(err)
	}
	idle()
	d, _ := p.Detail(ctx, "pc", []string{"np-1"})
	var pools, ops *console.Section
	for i := range d.Sections {
		switch d.Sections[i].ID {
		case "nodepools":
			pools = &d.Sections[i]
		case "operations":
			ops = &d.Sections[i]
		}
	}
	if pools == nil || ops == nil {
		t.Fatalf("sections = %v", d.Sections)
	}
	if len(pools.Actions) != 1 || pools.Actions[0].ID != "addpool" || pools.Actions[0].Unavailable != "" {
		t.Errorf("the Node pools tab's actions = %v", pools.Actions)
	}
	if len(pools.Listing.Items) != 1 || strings.Join(pools.Listing.Items[0].Target, "/") != "np-1/nodePools/default-pool" {
		t.Fatalf("pools = %+v", pools.Listing.Items)
	}
	ids := map[string]bool{}
	for _, a := range p.DetailActions(ctx, "pc", []string{"np-1", "nodePools", "default-pool"}) {
		ids[a.ID] = true
	}
	if !ids["resize"] || !ids["edit"] || !ids["delete"] {
		t.Errorf("a pool's actions = %v", ids)
	}
	if len(ops.Listing.Items) != 1 || ops.Listing.Items[0].Fields["Type"] != "Create cluster" {
		t.Errorf("operations after create = %+v", ops.Listing.Items)
	}

	if err := p.ActAt(ctx, "pc", []string{"np-1"}, "addpool", map[string]string{"name": "pool-2", "nodes": "2", "machine": "e2-small", "image": "UBUNTU_CONTAINERD"}); err != nil {
		t.Fatalf("Add node pool: %v", err)
	}
	idle()
	if err := p.ActAt(ctx, "pc", []string{"np-1", "nodePools", "pool-2"}, "resize", map[string]string{"nodes": "1"}); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	idle()
	if err := p.ActAt(ctx, "pc", []string{"np-1", "nodePools", "pool-2"}, "edit", map[string]string{"image": "COS_CONTAINERD", "labels": "team=a"}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	idle()
	np, err := api.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: name + "/nodePools/pool-2"})
	if err != nil || np.GetInitialNodeCount() != 1 || np.GetConfig().GetImageType() != "COS_CONTAINERD" || np.GetConfig().GetResourceLabels()["team"] != "a" {
		t.Fatalf("pool-2 after Resize and Edit = %v, %v", np, err)
	}
	if err := p.ActAt(ctx, "pc", []string{"np-1", "nodePools", "pool-2"}, "delete", nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	idle()
	if len(get().GetNodePools()) != 1 {
		t.Errorf("pools after Delete = %v", get().GetNodePools())
	}
	if err := p.ActAt(ctx, "pc", []string{"np-1", "nodePools", "nope"}, "delete", nil); err == nil {
		t.Error("deleting a pool that does not exist succeeded")
	}
	d, _ = p.Detail(ctx, "pc", []string{"np-1"})
	var types []string
	for _, s := range d.Sections {
		if s.ID == "operations" {
			for _, r := range s.Listing.Items {
				types = append(types, r.Fields["Type"]+" "+r.Fields["Target"])
			}
		}
	}
	want := "Delete node pool np-1/pool-2,Upgrade nodes np-1/pool-2,Set node pool size np-1/pool-2,Create node pool np-1/pool-2,Create cluster np-1"
	if strings.Join(types, ",") != want {
		t.Errorf("operations = %q, want %q", strings.Join(types, ","), want)
	}

	// Autopilot: refused with the reason.
	if _, err := p.Create(ctx, "pc", map[string]string{"mode": "Autopilot", "name": "ap-2", "locationType": "Regional", "region": "us-central1", "channel": "Regular channel"}); err != nil {
		t.Fatal(err)
	}
	for _, a := range p.DetailActions(ctx, "pc", []string{"ap-2"}) {
		if a.ID == "addpool" && a.Unavailable != gkeAutopilotPools {
			t.Errorf("Autopilot's Add node pool = %+v", a)
		}
	}
	if err := p.ActAt(ctx, "pc", []string{"ap-2", "nodePools", "default-pool"}, "resize", map[string]string{"nodes": "3"}); err == nil {
		t.Error("resizing an Autopilot pool succeeded")
	}
}
