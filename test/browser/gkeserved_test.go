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

// envGKE is the Kubernetes Engine API (#1913), exported only by the served
// shard's browser step, whose instance serves it.
const envGKE = "CLOUDBURROW_TEST_GKE"

// gkeServedCluster is a served cluster's row on Kubernetes clusters, read
// through the console API.
func gkeServedCluster(t *testing.T, project, name string) (status string, found bool) {
	t.Helper()
	code, body := consoleDo(t, http.MethodGet, "/api/resources/gkeclusters?project="+project, "")
	if code != http.StatusOK {
		t.Fatalf("list Kubernetes clusters = %d: %s", code, body)
	}
	var l struct {
		Items []struct {
			Name, Status string
			Fields       map[string]string
		}
	}
	if err := json.Unmarshal([]byte(body), &l); err != nil {
		t.Fatalf("decode Kubernetes clusters: %v: %s", err, body)
	}
	for _, it := range l.Items {
		if it.Name == name {
			return it.Status, true
		}
	}
	return "", false
}

// gkeServedDetail is a served cluster's page, through the console API.
type gkeServedDetail struct {
	Sections []struct {
		ID     string
		Groups []struct {
			Properties []struct{ Label, Value string }
		}
		Listing struct {
			Items []struct {
				Name, Status string
				Fields       map[string]string
			}
		}
	}
}

func gkeDetail(t *testing.T, project, name string) gkeServedDetail {
	t.Helper()
	code, body := consoleDo(t, http.MethodGet, "/api/detail/gkeclusters?project="+project+"&name="+name, "")
	if code != http.StatusOK {
		t.Fatalf("the cluster's page = %d: %s", code, body)
	}
	var d gkeServedDetail
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("decode the cluster's page: %v: %s", err, body)
	}
	return d
}

// gkeIdle waits, up to limit, for the cluster to be Running with no
// operation still running, as a kind cluster or node takes a minute or
// more to make.
func gkeIdle(t *testing.T, project, name string, limit time.Duration) gkeServedDetail {
	t.Helper()
	deadline := time.Now().Add(limit)
	last := ""
	for time.Now().Before(deadline) {
		st, ok := gkeServedCluster(t, project, name)
		last = st
		if ok && st == "Running" {
			d := gkeDetail(t, project, name)
			busy := false
			for _, s := range d.Sections {
				if s.ID == "operations" {
					for _, o := range s.Listing.Items {
						busy = busy || o.Status == "Running" || o.Status == "Pending"
						if o.Status == "Error" {
							t.Fatalf("operation %s (%s) failed: %s", o.Name, o.Fields["Type"], o.Fields["Message"])
						}
					}
				}
			}
			if !busy {
				return d
			}
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("cluster %s did not become idle in %s (last status %q)", name, limit, last)
	return gkeServedDetail{}
}

// TestGKEServedClusterThroughTheBrowser (#1913, #1925), in the served
// shard, whose instance serves the Kubernetes Engine API: through the
// pages alone, Create cluster makes a Standard zonal cluster (a real kind
// cluster), which Kubernetes clusters lists; its page's Edit sets labels;
// its Node pools tab's Add node pool adds a pool, whose Resize, Edit and
// Delete work from the pool's row; its Operations tab lists what was done;
// and Delete removes it.
func TestGKEServedClusterThroughTheBrowser(t *testing.T) {
	needService(t, "gkeclusters")
	gkeAddr := endpoint(t, envGKE)
	project := uniqueProject(t)
	name := fmt.Sprintf("gkeb-%d", time.Now().UnixNano()%1e6)
	const zone = "us-central1-a"
	t.Cleanup(func() {
		// Through the API, by name, whatever the test reached: the kind
		// cluster goes with it.
		u := "http://" + gkeAddr + "/v1/projects/" + project + "/locations/" + zone + "/clusters/" + name
		deadline := time.Now().Add(4 * time.Minute)
		for time.Now().Before(deadline) {
			if code, _ := do(t, http.MethodGet, u, "", ""); code == http.StatusNotFound {
				return
			}
			do(t, http.MethodDelete, u, "", "")
			time.Sleep(3 * time.Second)
		}
		t.Errorf("cluster %s survived its cleanup", name)
	})
	p := open(t)

	// Create cluster: Standard, zonal, one node.
	p.navigate("/kubernetes/clusters/create?project=" + project)
	p.waitFor(`document.querySelector("#view form.create-page") !== null &&
		[...document.querySelectorAll("#view input[type=radio][name=f-mode-choice]")].length === 2`)
	var enabled bool
	p.eval(`[...document.querySelectorAll("#view input[type=radio][name=f-mode-choice]")].every((r) => !r.disabled)`, &enabled)
	if !enabled {
		t.Fatal("Create cluster's Autopilot and Standard are disabled on an instance that serves the Kubernetes Engine API")
	}
	p.eval(`(() => { document.querySelector("#view input[type=radio][name=f-mode-choice][value=Standard]").click(); return true; })()`, nil)
	p.waitFor(`document.querySelector("#f-nodes") !== null && document.querySelector("#f-nodes").getClientRects().length > 0`)
	p.eval(`(() => { document.querySelector("#view input[type=radio][name=f-locationType-choice][value=Zonal]").click(); return true; })()`, nil)
	p.waitFor(`document.querySelector("#f-zone") !== null && document.querySelector("#f-zone").getClientRects().length > 0`)
	p.setField("#f-name", name)
	p.setField("#f-zone", zone)
	p.setField("#f-nodes", "1")
	p.eval(`(() => { [...document.querySelectorAll("#view form.create-page button")].find((b) => b.textContent.trim() === "Create").click(); return true; })()`, nil)
	p.waitFor(`location.pathname !== "/kubernetes/clusters/create"`)
	gkeIdle(t, project, name, 6*time.Minute)

	// Listed, Standard, in its zone.
	p.navigate("/kubernetes/clusters?project=" + project)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q) && r.textContent.includes("Standard") && r.textContent.includes(%q))`, name, zone))

	// Edit: labels.
	p.navigate("/kubernetes/clusters/" + name + "?project=" + project)
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "Cluster details"`)
	p.pageAction("Edit", map[string]string{"labels": "env=browser"}, "")
	labelled := false
	for _, s := range gkeIdle(t, project, name, time.Minute).Sections {
		for _, g := range s.Groups {
			for _, pr := range g.Properties {
				labelled = labelled || (pr.Label == "Labels" && pr.Value == "env: browser")
			}
		}
	}
	if !labelled {
		t.Error("the cluster's labels after Edit are not env: browser")
	}

	// Node pools (#1925): Add node pool, then Resize, Edit and Delete from
	// its row.
	p.navigate("/kubernetes/clusters/" + name + "?project=" + project + "&tab=nodepools")
	p.pageAction("Add node pool", map[string]string{"name": "pool-b", "nodes": "1"}, "")
	pool := func(d gkeServedDetail) (nodes, labels string, ok bool) {
		for _, s := range d.Sections {
			if s.ID == "nodepools" {
				for _, it := range s.Listing.Items {
					if it.Name == "pool-b" {
						return it.Fields["Number of nodes"], it.Fields["Labels"], true
					}
				}
			}
		}
		return "", "", false
	}
	if n, _, ok := pool(gkeIdle(t, project, name, 4*time.Minute)); !ok || n != "1" {
		t.Fatalf("pool-b after Add node pool: %q nodes, found %v", n, ok)
	}
	p.navigate("/kubernetes/clusters/" + name + "?project=" + project + "&tab=nodepools")
	p.rowAction("Node pools", "pool-b", "Resize", map[string]string{"nodes": "0"}, "")
	if n, _, _ := pool(gkeIdle(t, project, name, 3*time.Minute)); n != "0" {
		t.Errorf("pool-b after Resize to 0 has %q nodes", n)
	}
	p.navigate("/kubernetes/clusters/" + name + "?project=" + project + "&tab=nodepools")
	p.rowAction("Node pools", "pool-b", "Edit", map[string]string{"labels": "team=b"}, "")
	if _, l, _ := pool(gkeIdle(t, project, name, time.Minute)); l != "team: b" {
		t.Errorf("pool-b's labels after Edit are %q", l)
	}
	p.navigate("/kubernetes/clusters/" + name + "?project=" + project + "&tab=nodepools")
	p.rowAction("Node pools", "pool-b", "Delete", nil, "pool-b")
	if _, _, ok := pool(gkeIdle(t, project, name, 3*time.Minute)); ok {
		t.Error("pool-b is still listed after Delete")
	}

	// Operations: what was done, newest first.
	p.navigate("/kubernetes/clusters/" + name + "?project=" + project + "&tab=operations")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Delete node pool")) &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Create cluster"))`)
	var types string
	p.eval(`[...document.querySelectorAll("#view tbody tr")].map((r) => r.textContent).join("|")`, &types)
	for _, want := range []string{"Create cluster", "Set labels", "Create node pool", "Set node pool size", "Upgrade nodes", "Delete node pool"} {
		if !strings.Contains(types, want) {
			t.Errorf("the Operations tab lists no %s: %s", want, types)
		}
	}

	// Delete.
	p.navigate("/kubernetes/clusters/" + name + "?project=" + project)
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "Cluster details"`)
	p.clickAction("Delete", name)
	deadline := time.Now().Add(4 * time.Minute)
	for {
		if _, ok := gkeServedCluster(t, project, name); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cluster %s is still listed 4 minutes after Delete", name)
		}
		time.Sleep(3 * time.Second)
	}
	p.navigate("/kubernetes/clusters?project=" + project)
	p.waitFor(`document.querySelector("#view tbody tr") !== null`)
	var listed bool
	p.eval(fmt.Sprintf(`[...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === %q)`, name), &listed)
	if listed {
		t.Errorf("Kubernetes clusters still lists %s after Delete", name)
	}
}
