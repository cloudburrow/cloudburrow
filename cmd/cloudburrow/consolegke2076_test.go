package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/cloudburrow/cloudburrow/internal/service/gke"
)

// TestGKERollbackAndClockOnTheConsole (#2076): a node pool's Roll back is
// drawn disabled with the reason when no upgrade is incomplete (and in a
// blue-green upgrade's DELETING_BLUE_POOL phase), offered with Respect pod
// disruption budgets after a cancelled or paused one and during a
// blue-green soak, and sends RollbackNodePoolUpgrade, whose refusal is
// what it answers; the clusters list's Advance Kubernetes Engine clock
// moves the API's clock, refusing a malformed duration.
func TestGKERollbackAndClockOnTheConsole(t *testing.T) {
	ctx := context.Background()
	api, err := gke.NewServer(&consoleGKEBackend{}, gke.Options{Instance: "cloudburrow-t", Versions: []gke.Version{{GKE: "1.36.4-gke.0", Image: "kindest/node:v1.36.4"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	p := gkeClustersProvider{name: "cloudburrow-t", kubeconfig: "/nonexistent", svc: &gkeService{api: api}, gkeDir: t.TempDir()}

	np := &containerpb.NodePool{Name: "default-pool", Version: "1.35.8-gke.0", Status: containerpb.NodePool_RUNNING}
	if got := gkePoolRollback(np); !strings.Contains(got.Unavailable, "no incomplete upgrade") {
		t.Errorf("Roll back with no upgrade = %+v", got)
	}
	np.StatusMessage = "The upgrade to 1.36.4-gke.0 was cancelled: resume it by upgrading again, or roll it back."
	got := gkePoolRollback(np)
	if got.Unavailable != "" || len(got.Fields) != 2 || got.Fields[1].Name != "respectPdb" || got.Fields[1].Type != "checkbox" ||
		!strings.Contains(got.Fields[0].Default, "1.35.8-gke.0") {
		t.Errorf("Roll back after a cancel = %+v", got)
	}
	np.StatusMessage = ""
	np.UpdateInfo = &containerpb.NodePool_UpdateInfo{BlueGreenInfo: &containerpb.NodePool_UpdateInfo_BlueGreenInfo{Phase: containerpb.NodePool_UpdateInfo_BlueGreenInfo_NODE_POOL_SOAKING}}
	if got := gkePoolRollback(np); got.Unavailable != "" {
		t.Errorf("Roll back while soaking = %+v", got)
	}
	np.UpdateInfo.BlueGreenInfo.Phase = containerpb.NodePool_UpdateInfo_BlueGreenInfo_DELETING_BLUE_POOL
	if got := gkePoolRollback(np); !strings.Contains(got.Unavailable, "deleting its blue nodes") {
		t.Errorf("Roll back while deleting the blue pool = %+v", got)
	}

	if _, err := p.Create(ctx, "pc", map[string]string{"mode": "Standard", "name": "rb-1", "locationType": "Zonal", "zone": "us-central1-a", "nodes": "1"}); err != nil {
		t.Fatal(err)
	}
	name := "projects/pc/locations/us-central1-a/clusters/rb-1"
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, _ := api.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name}); c.GetStatus() == containerpb.Cluster_RUNNING {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// This backend upgrades nothing: the API's refusal is the answer.
	if err := p.ActAt(ctx, "pc", []string{"rb-1", "nodePools", "default-pool"}, "rollback", map[string]string{"respectPdb": "true"}); err == nil ||
		!strings.Contains(err.Error(), "not available with this backend") {
		t.Errorf("Roll back through the API = %v", err)
	}

	acts := p.ListActions(ctx, "pc")
	if len(acts) != 1 || acts[0].ID != actGKEClock || acts[0].Unavailable != "" || !strings.Contains(acts[0].Label, "CloudBurrow extension") {
		t.Fatalf("list actions = %+v", acts)
	}
	before := api.Now()
	out, err := p.ActAtResult(ctx, "pc", nil, actGKEClock, map[string]string{"duration": "1d2h"})
	if err != nil {
		t.Fatal(err)
	}
	if d := api.Now().Sub(before); d < 26*time.Hour || d > 26*time.Hour+time.Minute {
		t.Errorf("the clock moved %s, want 26h", d)
	}
	if out == nil || len(out.Items) != 1 || !strings.Contains(out.Note, "Kubernetes Engine clock is now") {
		t.Errorf("clock result = %+v", out)
	}
	if _, err := p.ActAtResult(ctx, "pc", nil, actGKEClock, map[string]string{"duration": "soon"}); err == nil {
		t.Error("a malformed duration was accepted")
	}
}
