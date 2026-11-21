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

// gkeClusterVersions reads a cluster's control plane version and its
// node pools' versions through the API.
func gkeClusterVersions(t *testing.T, base string) (master string, pools map[string]string) {
	t.Helper()
	code, body := do(t, http.MethodGet, base, "", "")
	if code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", base, code, body)
	}
	var c struct {
		CurrentMasterVersion string
		NodePools            []struct{ Name, Version string }
	}
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	pools = map[string]string{}
	for _, np := range c.NodePools {
		pools[np.Name] = np.Version
	}
	return c.CurrentMasterVersion, pools
}

// TestGKEServedUpgradeThroughTheBrowser (#2069), in the served shard: a
// Standard zonal cluster made on the older served version (through the
// API, with a "no upgrades" exclusion so no automatic upgrade races) is
// upgraded through the pages alone, as Google's console does it: its
// node pool's Upgrade first says to upgrade the control plane; the
// cluster's Upgrade, now enabled, offers the newer version and upgrades
// the control plane; the node pool's Upgrade then upgrades its nodes. The
// API reads both versions back, and the Operations tab lists the two
// upgrades. The Upgrade dialogs are audited for WCAG.
func TestGKEServedUpgradeThroughTheBrowser(t *testing.T) {
	needService(t, "gkeclusters")
	gkeAddr := endpoint(t, envGKE)
	project := uniqueProject(t)
	name := fmt.Sprintf("gkeup-%d", time.Now().UnixNano()%1e6)
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
		"nodePools": [{"name": "default-pool", "initialNodeCount": 1}],
		"maintenancePolicy": {"window": {"maintenanceExclusions": {"freeze": {"startTime": %q, "endTime": %q,
			"maintenanceExclusionOptions": {"scope": "NO_UPGRADES"}}}}}}}`,
		name, older, now.Add(-time.Hour).Format(time.RFC3339), now.Add(24*time.Hour).Format(time.RFC3339))
	if code, body := do(t, http.MethodPost, loc+"/clusters", create, ""); code != http.StatusOK {
		t.Fatalf("create the cluster: %d %s", code, body)
	}
	gkeIdle(t, project, name, 8*time.Minute)

	p := open(t)
	// The pool's Upgrade says to upgrade the control plane first.
	p.navigate("/kubernetes/clusters/" + name + "?project=" + project + "&tab=nodepools")
	poolRow := `[...document.querySelectorAll("#view tbody tr")].find((r) => [...r.querySelectorAll("td, th")].some((c) => c.textContent.trim() === "default-pool"))`
	p.waitFor(poolRow + ` !== undefined`)
	p.eval(poolRow+`.querySelector(".overflow-trigger").click()`, nil)
	var poolFirst bool
	p.eval(`[...document.querySelectorAll(".overflow-item")].some((b) => b.textContent.trim() === "Upgrade" && (b.disabled || b.getAttribute("aria-disabled") === "true") &&
		((b.title || "") + (b.getAttribute("aria-description") || "") + (b.closest("[title]") ? b.closest("[title]").title : "")).includes("upgrade the control plane first"))`, &poolFirst)
	if !poolFirst {
		var items string
		p.eval(`[...document.querySelectorAll(".overflow-item")].map((b) => b.outerHTML).join("\n")`, &items)
		t.Errorf("default-pool's Upgrade is not disabled with \"upgrade the control plane first\":\n%s", items)
	}

	// The control plane: the cluster's Upgrade is enabled and offers the
	// newer version.
	p.navigate("/kubernetes/clusters/" + name + "?project=" + project)
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "Cluster details" &&
		[...document.querySelectorAll("#view button")].some((b) => b.textContent.trim() === "Upgrade")`)
	var upgradeOn bool
	p.eval(`[...document.querySelectorAll("#view button")].some((b) => b.textContent.trim() === "Upgrade" && !b.disabled)`, &upgradeOn)
	if !upgradeOn {
		t.Fatal("the cluster's Upgrade is not enabled on a cluster at the older served version")
	}
	auditScreens(t, []a11yPage{{"Upgrade control plane dialog", "/kubernetes/clusters/" + name + "?project=" + project,
		`document.querySelector("#view h1") !== null && document.querySelectorAll("#view [role=tab]").length > 0`,
		`[...document.querySelectorAll("#view button")].find((b) => b.textContent.trim() === "Upgrade").click()`}})
	p = open(t)
	p.navigate("/kubernetes/clusters/" + name + "?project=" + project)
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "Cluster details" &&
		[...document.querySelectorAll("#view button")].some((b) => b.textContent.trim() === "Upgrade" && !b.disabled)`)
	p.pageActionWithin("Upgrade", map[string]string{"version": newer}, "", 2*time.Minute)
	gkeIdle(t, project, name, 10*time.Minute)
	if m, pools := gkeClusterVersions(t, base); m != newer || pools["default-pool"] != older {
		t.Fatalf("after the control plane's Upgrade: master %s, default-pool %s; want %s, %s", m, pools["default-pool"], newer, older)
	}

	// The node pool's Upgrade, from its row (a new tab: the upgrade
	// outlasts one tab's deadline).
	p = open(t)
	p.navigate("/kubernetes/clusters/" + name + "?project=" + project + "&tab=nodepools")
	p.waitFor(poolRow + ` !== undefined`)
	p.rowAction("Node pools", "default-pool", "Upgrade", map[string]string{"version": newer}, "")
	gkeIdle(t, project, name, 10*time.Minute)
	if m, pools := gkeClusterVersions(t, base); m != newer || pools["default-pool"] != newer {
		t.Errorf("after the node pool's Upgrade: master %s, default-pool %s; want %s", m, pools["default-pool"], newer)
	}

	// Operations lists both upgrades.
	p = open(t)
	p.navigate("/kubernetes/clusters/" + name + "?project=" + project + "&tab=operations")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Create cluster"))`)
	var types string
	p.eval(`[...document.querySelectorAll("#view tbody tr")].map((r) => r.textContent).join("|")`, &types)
	for _, want := range []string{"Upgrade master", "Upgrade nodes"} {
		if !strings.Contains(types, want) {
			t.Errorf("the Operations tab lists no %s: %s", want, types)
		}
	}
	// Both at the newest version, the Upgrade buttons say why again.
	p.navigate("/kubernetes/clusters/" + name + "?project=" + project)
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "Cluster details" &&
		[...document.querySelectorAll("#view button")].some((b) => b.textContent.trim() === "Upgrade")`)
	var off bool
	p.eval(`[...document.querySelectorAll("#view button")].some((b) => b.textContent.trim() === "Upgrade" && b.disabled)`, &off)
	if !off {
		t.Error("the cluster's Upgrade is enabled at the newest version served")
	}
}
