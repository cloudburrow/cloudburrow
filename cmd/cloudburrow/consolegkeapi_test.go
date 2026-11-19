package main

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/cloudburrow/cloudburrow/internal/service/gke"
)

type consoleGKEBackend struct{ deleted []string }

func (consoleGKEBackend) Create(context.Context, string, string, bool) (gke.Access, error) {
	return gke.Access{Endpoint: "127.0.0.1:1", CACert: "Q0E="}, nil
}
func (consoleGKEBackend) Resize(context.Context, string, string, string, int) error { return nil }
func (b *consoleGKEBackend) Delete(_ context.Context, kind string) error {
	b.deleted = append(b.deleted, kind)
	return nil
}
func (consoleGKEBackend) FreeBytes() (uint64, error) { return 100 << 30, nil }

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
	for len(be.deleted) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(be.deleted) != 1 {
		t.Errorf("deleted kind clusters = %v", be.deleted)
	}
}
