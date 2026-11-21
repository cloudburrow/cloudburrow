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

// TestGKEServedRollbackThroughTheBrowser (#2076), in the served shard: a
// Standard zonal cluster whose control plane runs the newer served version
// and whose two-node default-pool runs the older (made through the API,
// with a "no upgrades" exclusion so no automatic upgrade races) has its
// pool's upgrade started and cancelled through the API, one node upgraded.
// Through the pages alone, the Operations tab shows the cancelled upgrade
// as Cancelled, and the node pool's Roll back (Respect pod disruption
// budgets checked) rolls it back: the API reads the pool on the older
// version and Running, and the Operations tab lists the rollback's Upgrade
// nodes. Roll back is then disabled with the reason, as nothing is left to
// roll back. On the clusters list, Advance Kubernetes Engine clock
// (CloudBurrow extension), confirmed, moves the API's clock an hour. The
// Roll back and Advance clock dialogs are audited for WCAG.
func TestGKEServedRollbackThroughTheBrowser(t *testing.T) {
	needService(t, "gkeclusters")
	gkeAddr := endpoint(t, envGKE)
	project := uniqueProject(t)
	name := fmt.Sprintf("gkerb-%d", time.Now().UnixNano()%1e6)
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
		t.Skipf("one version is served (%v): nothing to roll back", cfg.ValidMasterVersions)
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
		"nodePools": [{"name": "default-pool", "initialNodeCount": 2, "version": %q}],
		"maintenancePolicy": {"window": {"maintenanceExclusions": {"freeze": {"startTime": %q, "endTime": %q,
			"maintenanceExclusionOptions": {"scope": "NO_UPGRADES"}}}}}}}`,
		name, newer, older, now.Add(-time.Hour).Format(time.RFC3339), now.Add(29*24*time.Hour).Format(time.RFC3339))
	if code, body := do(t, http.MethodPost, loc+"/clusters", create, ""); code != http.StatusOK {
		t.Fatalf("create the cluster: %d %s", code, body)
	}
	gkeIdle(t, project, name, 10*time.Minute)

	// The upgrade, cancelled at once: the node it started completes.
	code, body = do(t, http.MethodPut, base+"/nodePools/default-pool", fmt.Sprintf(`{"nodeVersion": %q, "imageType": "COS_CONTAINERD"}`, newer), "")
	if code != http.StatusOK {
		t.Fatalf("upgrade the node pool: %d %s", code, body)
	}
	var op struct{ Name string }
	if err := json.Unmarshal([]byte(body), &op); err != nil || op.Name == "" {
		t.Fatalf("the upgrade's operation: %v %s", err, body)
	}
	if code, body := do(t, http.MethodPost, loc+"/operations/"+op.Name+":cancel", `{}`, ""); code != http.StatusOK {
		t.Fatalf("cancel the upgrade: %d %s", code, body)
	}
	gkeIdle(t, project, name, 10*time.Minute)
	if _, pools := gkeClusterVersions(t, base); pools["default-pool"] != older {
		t.Fatalf("the cancelled pool's version = %s, want %s", pools["default-pool"], older)
	}

	p := open(t)
	p.navigate("/kubernetes/clusters/" + name + "?project=" + project + "&tab=operations")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Upgrade nodes"))`)
	var cancelled bool
	p.eval(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Upgrade nodes") && r.textContent.includes("Cancelled"))`, &cancelled)
	if !cancelled {
		var rows string
		p.eval(`[...document.querySelectorAll("#view tbody tr")].map((r) => r.textContent).join("|")`, &rows)
		t.Errorf("the Operations tab shows no cancelled Upgrade nodes: %s", rows)
	}

	poolsTab := "/kubernetes/clusters/" + name + "?project=" + project + "&tab=nodepools"
	poolRow := `[...document.querySelectorAll("#view tbody tr")].find((r) => [...r.querySelectorAll("td, th")].some((c) => c.textContent.trim() === "default-pool"))`
	openRollback := poolRow + `.querySelector(".overflow-trigger").click(); [...document.querySelectorAll(".overflow-item")].find((b) => b.textContent.trim() === "Roll back").click()`
	auditScreens(t, []a11yPage{{"Roll back node pool upgrade dialog", poolsTab, poolRow + ` !== undefined`, openRollback}})

	p = open(t)
	p.navigate(poolsTab)
	p.waitFor(poolRow + ` !== undefined`)
	p.rowAction("Node pools", "default-pool", "Roll back", map[string]string{"respectPdb": "true"}, "")
	gkeIdle(t, project, name, 15*time.Minute)
	if _, pools := gkeClusterVersions(t, base); pools["default-pool"] != older {
		t.Errorf("after Roll back: default-pool %s, want %s", pools["default-pool"], older)
	}
	code, body = do(t, http.MethodGet, base+"/nodePools/default-pool", "", "")
	if code != http.StatusOK || !strings.Contains(body, `"RUNNING"`) {
		t.Errorf("default-pool after Roll back: %d %s", code, body)
	}
	p = open(t)
	p.navigate("/kubernetes/clusters/" + name + "?project=" + project + "&tab=operations")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].filter((r) => r.textContent.includes("Upgrade nodes")).length >= 2`)
	// Nothing is left to roll back: Roll back says why.
	p.navigate(poolsTab)
	p.waitFor(poolRow + ` !== undefined`)
	p.eval(poolRow+`.querySelector(".overflow-trigger").click()`, nil)
	var off bool
	p.eval(`[...document.querySelectorAll(".overflow-item")].some((b) => b.textContent.trim() === "Roll back" && (b.disabled || b.getAttribute("aria-disabled") === "true") &&
		((b.title || "") + (b.getAttribute("aria-description") || "") + (b.closest("[title]") ? b.closest("[title]").title : "")).includes("no incomplete upgrade"))`, &off)
	if !off {
		var items string
		p.eval(`[...document.querySelectorAll(".overflow-item")].map((b) => b.outerHTML).join("\n")`, &items)
		t.Errorf("Roll back after the rollback is not disabled with its reason:\n%s", items)
	}

	// The clock, from the clusters list.
	list := "/kubernetes/clusters?project=" + project
	auditScreens(t, []a11yPage{{"Advance Kubernetes Engine clock dialog", list, `document.querySelector("#list-action-advance-gke-clock") !== null`,
		`document.querySelector("#list-action-advance-gke-clock").click()`}})
	clock := func() time.Time {
		code, body := do(t, http.MethodPost, "http://"+gkeAddr+"/cloudburrow/v1/clock:advance", `{"duration": "0s"}`, "")
		var c struct{ Now string }
		if code != http.StatusOK || json.Unmarshal([]byte(body), &c) != nil {
			t.Fatalf("read the clock: %d %s", code, body)
		}
		at, err := time.Parse(time.RFC3339Nano, c.Now)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	before := clock()
	p = open(t)
	p.navigate(list)
	rows := p.listAction("advance-gke-clock", map[string]string{"duration": "1h"})
	if !strings.Contains(rows, "Kubernetes Engine clock") {
		t.Errorf("Advance Kubernetes Engine clock answered %q", rows)
	}
	if d := clock().Sub(before); d < time.Hour || d > time.Hour+5*time.Minute {
		t.Errorf("the clock moved %s, want an hour", d)
	}
}
