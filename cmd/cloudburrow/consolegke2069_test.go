package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/gke"
)

// TestGKEMaintenanceAndCompleteUpgradeOnTheConsole (#2069): a served
// cluster's Edit maintenance sets its window and exclusions with
// SetMaintenancePolicy (read back into the dialog), refusing a malformed
// line; a node pool's Complete upgrade is drawn disabled with the reason
// outside a blue-green soak, and the API's refusal is what it answers.
func TestGKEMaintenanceAndCompleteUpgradeOnTheConsole(t *testing.T) {
	ctx := context.Background()
	api, err := gke.NewServer(&consoleGKEBackend{}, gke.Options{Instance: "cloudburrow-t", Versions: []gke.Version{{GKE: "1.36.4-gke.0", Image: "kindest/node:v1.36.4"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	p := gkeClustersProvider{name: "cloudburrow-t", kubeconfig: "/nonexistent", svc: &gkeService{api: api}, gkeDir: t.TempDir()}
	if _, err := p.Create(ctx, "pc", map[string]string{"mode": "Standard", "name": "mt-1", "locationType": "Zonal", "zone": "us-central1-a", "nodes": "1"}); err != nil {
		t.Fatal(err)
	}
	name := "projects/pc/locations/us-central1-a/clusters/mt-1"
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, _ := api.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name}); c.GetStatus() == containerpb.Cluster_RUNNING {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	ex := "freeze 2026-12-20T00:00:00Z 2027-01-02T00:00:00Z no_minor_upgrades"
	if err := p.ActAt(ctx, "pc", []string{"mt-1"}, "maintenance", map[string]string{"window": "daily 03:00", "exclusions": ex}); err != nil {
		t.Fatalf("Edit maintenance: %v", err)
	}
	c, _ := api.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	w := c.GetMaintenancePolicy().GetWindow()
	if w.GetDailyMaintenanceWindow().GetStartTime() != "03:00" || w.GetMaintenanceExclusions()["freeze"].GetMaintenanceExclusionOptions().GetScope() != containerpb.MaintenanceExclusionOptions_NO_MINOR_UPGRADES {
		t.Errorf("maintenance policy = %v", c.GetMaintenancePolicy())
	}
	var a console.Action
	for _, x := range p.DetailActions(ctx, "pc", []string{"mt-1"}) {
		if x.ID == "maintenance" {
			a = x
		}
	}
	if len(a.Fields) != 2 || a.Fields[0].Default != "daily 03:00" || a.Fields[1].Default != ex {
		t.Errorf("Edit maintenance = %+v", a)
	}
	// Saved again from what it shows: the resource version is current.
	if err := p.ActAt(ctx, "pc", []string{"mt-1"}, "maintenance", map[string]string{"window": "", "exclusions": ex}); err != nil {
		t.Errorf("Edit maintenance again: %v", err)
	}
	if err := p.ActAt(ctx, "pc", []string{"mt-1"}, "maintenance", map[string]string{"exclusions": "freeze tomorrow"}); err == nil || !strings.Contains(err.Error(), "NAME START END SCOPE") {
		t.Errorf("a malformed exclusion = %v", err)
	}
	np := &containerpb.NodePool{Name: "default-pool"}
	if got := gkePoolCompleteUpgrade(np); !strings.Contains(got.Unavailable, "not soaking") {
		t.Errorf("Complete upgrade outside a soak = %+v", got)
	}
	np.UpdateInfo = &containerpb.NodePool_UpdateInfo{BlueGreenInfo: &containerpb.NodePool_UpdateInfo_BlueGreenInfo{
		Phase: containerpb.NodePool_UpdateInfo_BlueGreenInfo_NODE_POOL_SOAKING, GreenPoolVersion: "1.36.4-gke.0"}}
	if got := gkePoolCompleteUpgrade(np); got.Unavailable != "" || !strings.Contains(got.Fields[0].Default, "1.36.4-gke.0") {
		t.Errorf("Complete upgrade while soaking = %+v", got)
	}
	if err := p.ActAt(ctx, "pc", []string{"mt-1", "nodePools", "default-pool"}, "complete", nil); err == nil || !strings.Contains(err.Error(), "not soaking") {
		t.Errorf("Complete upgrade through the API outside a soak = %v", err)
	}
}
