//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestGKEServedSettingsThroughTheBrowser (#2100), in the served shard: a
// Standard zonal cluster's settings changed through the pages, as Google's
// console has them: Details' Edit legacy authorization and Edit network
// policy, and the node pool's Autoscaling and Management. The API reads
// each back. The dialogs are audited for WCAG.
func TestGKEServedSettingsThroughTheBrowser(t *testing.T) {
	needService(t, "gkeclusters")
	gkeAddr := endpoint(t, envGKE)
	project := uniqueProject(t)
	name := fmt.Sprintf("gkeset-%d", time.Now().UnixNano()%1e6)
	const zone = "us-central1-a"
	loc := "http://" + gkeAddr + "/v1/projects/" + project + "/locations/" + zone
	base := loc + "/clusters/" + name
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
	create := fmt.Sprintf(`{"cluster": {"name": %q, "nodePools": [{"name": "default-pool", "initialNodeCount": 1}]}}`, name)
	if code, body := do(t, http.MethodPost, loc+"/clusters", create, ""); code != http.StatusOK {
		t.Fatalf("create the cluster: %d %s", code, body)
	}
	gkeIdle(t, project, name, 8*time.Minute)
	read := func() map[string]any {
		t.Helper()
		code, body := do(t, http.MethodGet, base, "", "")
		if code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", base, code, body)
		}
		var c map[string]any
		if err := json.Unmarshal([]byte(body), &c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	page := "/kubernetes/clusters/" + name + "?project=" + project
	detailReady := `document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "Cluster details" &&
		[...document.querySelectorAll("#view button")].some((b) => b.textContent.trim() === "Edit legacy authorization")`
	click := func(label string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#view button")].find((b) => b.textContent.trim() === %q).click()`, label)
	}
	auditScreens(t, []a11yPage{
		{"Legacy authorization dialog", page, detailReady, click("Edit legacy authorization")},
		{"Edit network policy dialog", page, detailReady, click("Edit network policy")},
	})

	p := open(t)
	p.navigate(page)
	p.waitFor(detailReady)
	p.pageActionWithin("Edit legacy authorization", map[string]string{"enabled": "true"}, "", 3*time.Minute)
	gkeIdle(t, project, name, 4*time.Minute)
	if abac, _ := read()["legacyAbac"].(map[string]any); abac["enabled"] != true {
		t.Errorf("legacyAbac after the dialog = %v", abac)
	}
	p = open(t)
	p.navigate(page)
	p.waitFor(detailReady)
	p.pageActionWithin("Edit network policy", map[string]string{"enabled": "true"}, "", 3*time.Minute)
	gkeIdle(t, project, name, 4*time.Minute)
	if np, _ := read()["networkPolicy"].(map[string]any); np["enabled"] != true {
		t.Errorf("networkPolicy after the dialog = %v", np)
	}

	p = open(t)
	p.navigate(page + "&tab=nodepools")
	poolRow := `[...document.querySelectorAll("#view tbody tr")].find((r) => [...r.querySelectorAll("td, th")].some((c) => c.textContent.trim() === "default-pool"))`
	p.waitFor(poolRow + ` !== undefined`)
	p.rowAction("Node pools", "default-pool", "Autoscaling", map[string]string{"enabled": "true", "min": "1", "max": "2"}, "")
	p = open(t)
	p.navigate(page + "&tab=nodepools")
	p.waitFor(poolRow + ` !== undefined`)
	p.rowAction("Node pools", "default-pool", "Management", map[string]string{"autoUpgrade": "true", "autoRepair": "false"}, "")
	gkeIdle(t, project, name, 4*time.Minute)
	pools, _ := read()["nodePools"].([]any)
	if len(pools) != 1 {
		t.Fatalf("node pools %v", pools)
	}
	np, _ := pools[0].(map[string]any)
	as, _ := np["autoscaling"].(map[string]any)
	m, _ := np["management"].(map[string]any)
	if as["enabled"] != true || as["maxNodeCount"] != float64(2) || m["autoRepair"] == true || m["autoUpgrade"] != true {
		t.Errorf("the pool after the dialogs: autoscaling %v, management %v", as, m)
	}
}
