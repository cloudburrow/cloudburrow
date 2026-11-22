//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestGKEServedBlueGreenThroughTheBrowser (#2100), in the served shard: a
// Standard zonal cluster whose control plane runs the newer served version
// and whose one-node default-pool runs the older (made through the API,
// with a "no upgrades" exclusion so no automatic upgrade races). Through
// the pages alone: the node pool's Edit, in its Upgrade strategy section,
// sets Blue-green upgrade with a batch of one node, no batch soak and an
// hour's node pool soak, and its Nodes section a Kubernetes label and a
// taint (the API reads the pool's upgrade_settings and config back);
// the pool's Upgrade starts the blue-green upgrade; while the pool soaks,
// its Complete upgrade ends the soak, after which the API reads the pool
// on the newer version, Running, and Complete upgrade is disabled with
// its reason. The Edit and Complete upgrade dialogs are audited for WCAG.
func TestGKEServedBlueGreenThroughTheBrowser(t *testing.T) {
	needService(t, "gkeclusters")
	gkeAddr := endpoint(t, envGKE)
	project := uniqueProject(t)
	name := fmt.Sprintf("gkebg-%d", time.Now().UnixNano()%1e6)
	const zone = "us-central1-a"
	loc := "http://" + gkeAddr + "/v1/projects/" + project + "/locations/" + zone
	base := loc + "/clusters/" + name
	code, body := do(t, http.MethodGet, loc+"/serverConfig", "", "")
	if code != http.StatusOK {
		t.Fatalf("serverConfig: %d %s", code, body)
	}
	var cfg struct{ ValidMasterVersions []string }
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.ValidMasterVersions) < 2 {
		t.Skipf("one version is served (%v): nothing to upgrade to", cfg.ValidMasterVersions)
	}
	older, newer := cfg.ValidMasterVersions[1], cfg.ValidMasterVersions[0]
	t.Cleanup(func() {
		deadline := time.Now().Add(4 * time.Minute)
		for time.Now().Before(deadline) {
			if code, _ := do(t, http.MethodGet, base, "", ""); code == http.StatusNotFound {
				return
			}
			do(t, http.MethodDelete, base, "", "")
			time.Sleep(3 * time.Second)
		}
		t.Errorf("cluster %s survived its cleanup", name)
	})
	now := time.Now().UTC()
	create := fmt.Sprintf(`{"cluster": {"name": %q, "initialClusterVersion": %q,
		"nodePools": [{"name": "default-pool", "initialNodeCount": 1, "version": %q}],
		"maintenancePolicy": {"window": {"maintenanceExclusions": {"freeze": {"startTime": %q, "endTime": %q,
			"maintenanceExclusionOptions": {"scope": "NO_UPGRADES"}}}}}}}`,
		name, newer, older, now.Add(-time.Hour).Format(time.RFC3339), now.Add(29*24*time.Hour).Format(time.RFC3339))
	if code, body := do(t, http.MethodPost, loc+"/clusters", create, ""); code != http.StatusOK {
		t.Fatalf("create the cluster: %d %s", code, body)
	}
	gkeIdle(t, project, name, 10*time.Minute)
	type pool struct {
		Version         string
		Status          string
		UpgradeSettings struct {
			Strategy          string
			BlueGreenSettings struct {
				NodePoolSoakDuration  string
				StandardRolloutPolicy struct{ BatchNodeCount int }
			}
		}
		UpdateInfo struct{ BlueGreenInfo struct{ Phase string } }
		Config     struct {
			Labels map[string]string
			Taints []struct{ Key, Value, Effect string }
		}
	}
	getPool := func() pool {
		t.Helper()
		code, body := do(t, http.MethodGet, base+"/nodePools/default-pool", "", "")
		var np pool
		if code != http.StatusOK || json.Unmarshal([]byte(body), &np) != nil {
			t.Fatalf("get default-pool: %d %s", code, body)
		}
		return np
	}

	poolsTab := "/kubernetes/clusters/" + name + "?project=" + project + "&tab=nodepools"
	poolRow := `[...document.querySelectorAll("#view tbody tr")].find((r) => [...r.querySelectorAll("td, th")].some((c) => c.textContent.trim() === "default-pool"))`
	openItem := func(label string) string {
		return poolRow + `.querySelector(".overflow-trigger").click(); [...document.querySelectorAll(".overflow-item")].find((b) => b.textContent.trim() === "` + label + `").click()`
	}
	auditScreens(t, []a11yPage{{"Edit node pool dialog (Upgrade strategy)", poolsTab, poolRow + ` !== undefined`, openItem("Edit")}})

	p := open(t)
	p.navigate(poolsTab)
	p.waitFor(poolRow + ` !== undefined`)
	p.rowAction("Node pools", "default-pool", "Edit", map[string]string{"strategy": "Blue-green upgrade", "batchSize": "1", "batchSoak": "0", "poolSoak": "3600",
		"kubeLabels": "team=console", "taints": "dedicated=console:PREFER_NO_SCHEDULE"}, "")
	gkeIdle(t, project, name, 5*time.Minute)
	np := getPool()
	if np.UpgradeSettings.Strategy != "BLUE_GREEN" || np.UpgradeSettings.BlueGreenSettings.NodePoolSoakDuration != "3600s" ||
		np.UpgradeSettings.BlueGreenSettings.StandardRolloutPolicy.BatchNodeCount != 1 || np.Version != older {
		t.Fatalf("default-pool after Edit: %+v", np)
	}
	if np.Config.Labels["team"] != "console" || len(np.Config.Taints) != 1 || np.Config.Taints[0].Effect != "PREFER_NO_SCHEDULE" {
		t.Errorf("default-pool's Kubernetes labels and taints after Edit: %+v", np.Config)
	}

	p = open(t)
	p.navigate(poolsTab)
	p.waitFor(poolRow + ` !== undefined`)
	p.rowAction("Node pools", "default-pool", "Upgrade", map[string]string{"version": newer}, "")
	deadline := time.Now().Add(10 * time.Minute)
	for getPool().UpdateInfo.BlueGreenInfo.Phase != "NODE_POOL_SOAKING" {
		if time.Now().After(deadline) {
			t.Fatalf("default-pool never soaked: %+v", getPool())
		}
		time.Sleep(2 * time.Second)
	}
	auditScreens(t, []a11yPage{{"Complete node pool upgrade dialog", poolsTab, poolRow + ` !== undefined`, openItem("Complete upgrade")}})
	p = open(t)
	p.navigate(poolsTab)
	p.waitFor(poolRow + ` !== undefined`)
	p.rowAction("Node pools", "default-pool", "Complete upgrade", map[string]string{"note": "complete"}, "")
	gkeIdle(t, project, name, 10*time.Minute)
	if np := getPool(); np.Version != newer || np.Status != "RUNNING" || np.UpdateInfo.BlueGreenInfo.Phase != "" {
		t.Errorf("default-pool after Complete upgrade: %+v", np)
	}
	p.navigate(poolsTab)
	p.waitFor(poolRow + ` !== undefined`)
	p.eval(poolRow+`.querySelector(".overflow-trigger").click()`, nil)
	var off bool
	p.eval(`[...document.querySelectorAll(".overflow-item")].some((b) => b.textContent.trim() === "Complete upgrade" && (b.disabled || b.getAttribute("aria-disabled") === "true") &&
		((b.title || "") + (b.getAttribute("aria-description") || "") + (b.closest("[title]") ? b.closest("[title]").title : "")).includes("not soaking"))`, &off)
	if !off {
		var items string
		p.eval(`[...document.querySelectorAll(".overflow-item")].map((b) => b.outerHTML).join("\n")`, &items)
		t.Errorf("Complete upgrade after the upgrade is not disabled with its reason:\n%s", strings.TrimSpace(items))
	}
}
