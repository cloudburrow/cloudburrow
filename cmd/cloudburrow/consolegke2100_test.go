package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/cloudburrow/cloudburrow/internal/service/gke"
)

// TestGKEPoolUpgradeStrategyOnTheConsole (#2100): a node pool's Edit has
// Google's Upgrade strategy section. Saving Blue-green upgrade with a
// batch size, batch soak and node pool soak sets the pool's
// upgrade_settings through UpdateNodePool, and the dialog reads them back;
// a percentage batch is a batch_percentage; the Nodes section sets the
// pool's Kubernetes labels, taints and zones (a bad effect refused);
// Surge upgrade sets max surge and max unavailable again; a malformed
// size is refused before the API.
func TestGKEPoolUpgradeStrategyOnTheConsole(t *testing.T) {
	ctx := context.Background()
	api, err := gke.NewServer(&consoleGKEBackend{}, gke.Options{Instance: "cloudburrow-t", Versions: []gke.Version{{GKE: "1.36.4-gke.0", Image: "kindest/node:v1.36.4"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	p := gkeClustersProvider{name: "cloudburrow-t", kubeconfig: "/nonexistent", svc: &gkeService{api: api}, gkeDir: t.TempDir()}
	if _, err := p.Create(ctx, "pc", map[string]string{"mode": "Standard", "name": "us-1", "locationType": "Zonal", "zone": "us-central1-a", "nodes": "1"}); err != nil {
		t.Fatal(err)
	}
	pool := "projects/pc/locations/us-central1-a/clusters/us-1/nodePools/default-pool"
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, _ := api.GetCluster(ctx, &containerpb.GetClusterRequest{Name: "projects/pc/locations/us-central1-a/clusters/us-1"}); c.GetStatus() == containerpb.Cluster_RUNNING {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	at := []string{"us-1", "nodePools", "default-pool"}
	edit := func() map[string]string {
		t.Helper()
		for _, a := range p.DetailActions(ctx, "pc", at) {
			if a.ID == "edit" {
				out := map[string]string{}
				for _, f := range a.Fields {
					out[f.Name] = f.Default
					if f.Name == "strategy" && f.Section != "Upgrade strategy" {
						t.Errorf("strategy field section = %q", f.Section)
					}
				}
				return out
			}
		}
		t.Fatal("no Edit on the node pool")
		return nil
	}
	v := edit()
	if v["strategy"] != "Surge upgrade" || v["maxSurge"] != "1" || v["maxUnavailable"] != "0" || v["poolSoak"] != "3600" {
		t.Errorf("a new pool's Edit = %v", v)
	}
	v["strategy"], v["batchSize"], v["batchSoak"], v["poolSoak"] = "Blue-green upgrade", "2", "10", "600"
	if err := p.ActAt(ctx, "pc", at, "edit", v); err != nil {
		t.Fatalf("Edit to blue-green: %v", err)
	}
	np, _ := api.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	us := np.GetUpgradeSettings()
	rp := us.GetBlueGreenSettings().GetStandardRolloutPolicy()
	if us.GetStrategy() != containerpb.NodePoolUpdateStrategy_BLUE_GREEN || rp.GetBatchNodeCount() != 2 || rp.GetBatchSoakDuration().AsDuration() != 10*time.Second ||
		us.GetBlueGreenSettings().GetNodePoolSoakDuration().AsDuration() != 10*time.Minute {
		t.Errorf("upgrade settings after Edit = %v", us)
	}
	if got := edit(); got["strategy"] != "Blue-green upgrade" || got["batchSize"] != "2" || got["batchSoak"] != "10" || got["poolSoak"] != "600" {
		t.Errorf("Edit read back = %v", got)
	}
	v["batchSize"] = "50%"
	if err := p.ActAt(ctx, "pc", at, "edit", v); err != nil {
		t.Fatalf("Edit with a percentage: %v", err)
	}
	np, _ = api.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	if f := np.GetUpgradeSettings().GetBlueGreenSettings().GetStandardRolloutPolicy().GetBatchPercentage(); f != 0.5 {
		t.Errorf("batch percentage = %v", f)
	}
	if got := edit(); got["batchSize"] != "50%" {
		t.Errorf("percentage read back = %q", got["batchSize"])
	}
	v["batchSize"] = "half"
	if err := p.ActAt(ctx, "pc", at, "edit", v); err == nil || !strings.Contains(err.Error(), "Batch size") {
		t.Errorf("a malformed batch size = %v", err)
	}
	// The Nodes section: Kubernetes labels, taints and zones.
	v["batchSize"] = "1"
	v["kubeLabels"], v["taints"], v["zones"] = "team=a\nenv=dev", "dedicated=gpu:NO_SCHEDULE", "us-central1-a,us-central1-b"
	if err := p.ActAt(ctx, "pc", at, "edit", v); err != nil {
		t.Fatalf("Edit the Nodes section: %v", err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if np, _ = api.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool}); np.GetStatus() == containerpb.NodePool_RUNNING {
			break
		}
	}
	if l := np.GetConfig().GetLabels(); l["team"] != "a" || l["env"] != "dev" || len(np.GetConfig().GetTaints()) != 1 ||
		np.GetConfig().GetTaints()[0].GetEffect() != containerpb.NodeTaint_NO_SCHEDULE || strings.Join(np.GetLocations(), ",") != "us-central1-a,us-central1-b" {
		t.Errorf("after the Nodes edit: labels %v taints %v zones %v", l, np.GetConfig().GetTaints(), np.GetLocations())
	}
	if got := edit(); got["kubeLabels"] != "env=dev\nteam=a" || got["taints"] != "dedicated=gpu:NO_SCHEDULE" || got["zones"] != "us-central1-a,us-central1-b" {
		t.Errorf("Nodes section read back = %q %q %q", got["kubeLabels"], got["taints"], got["zones"])
	}
	v["taints"] = "dedicated=gpu:SOMETIMES"
	if err := p.ActAt(ctx, "pc", at, "edit", v); err == nil || !strings.Contains(err.Error(), "effect") {
		t.Errorf("a bad taint effect = %v", err)
	}
	v["taints"] = "dedicated=gpu:NO_SCHEDULE"
	v["strategy"], v["maxSurge"], v["maxUnavailable"] = "Surge upgrade", "2", "1"
	if err := p.ActAt(ctx, "pc", at, "edit", v); err != nil {
		t.Fatalf("Edit back to surge: %v", err)
	}
	np, _ = api.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	if us := np.GetUpgradeSettings(); us.GetStrategy() != containerpb.NodePoolUpdateStrategy_SURGE || us.GetMaxSurge() != 2 || us.GetMaxUnavailable() != 1 {
		t.Errorf("upgrade settings after surge Edit = %v", us)
	}
}

// TestGKEPoolKubeletConfigText (#2100): the Edit node pool's Kubelet
// configuration reads back as the YAML it is written in, a node system
// configuration file's kubeletConfig wrapper is taken, and an unchanged
// text sends no kubelet_config while a cleared one resets it.
func TestGKEPoolKubeletConfigText(t *testing.T) {
	kc, err := gkeParseKubelet("kubeletConfig:\n  cpuManagerPolicy: static\n  podPidsLimit: 4096\n  evictionSoft:\n    memoryAvailable: 200Mi\n")
	if err != nil {
		t.Fatal(err)
	}
	if kc.GetCpuManagerPolicy() != "static" || kc.GetPodPidsLimit() != 4096 || kc.GetEvictionSoft().GetMemoryAvailable() != "200Mi" {
		t.Fatalf("parsed %v", kc)
	}
	np := &containerpb.NodePool{Config: &containerpb.NodeConfig{KubeletConfig: kc}}
	text := gkeKubeletText(kc)
	req := &containerpb.UpdateNodePoolRequest{}
	if err := gkeParsePoolNodeFields(np, map[string]string{"kubeletConfig": text}, req); err != nil || req.KubeletConfig != nil {
		t.Errorf("the unchanged text %q sent %v (%v)", text, req.KubeletConfig, err)
	}
	if err := gkeParsePoolNodeFields(np, map[string]string{"kubeletConfig": ""}, req); err != nil || req.KubeletConfig == nil || req.KubeletConfig.GetPodPidsLimit() != 0 {
		t.Errorf("a cleared text sent %v (%v)", req.KubeletConfig, err)
	}
	if _, err := gkeParseKubelet("podPidsLimit: [1]"); err == nil {
		t.Error("a malformed kubelet configuration was taken")
	}
}
