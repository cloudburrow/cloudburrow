//go:build browser

package browser

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestKubernetesEnginePagesThroughTheBrowser (#1857), in the storage
// shard (every instance has its cluster): Kubernetes Engine as Google's
// console has it, over the instance's own kind cluster.
//   - The menu is Clusters, Workloads, Applications, Secrets & ConfigMaps,
//     Gateways, Services & Ingress, Storage, Object Browser, Backup for GKE,
//     Config, Policy, Security; the pages CloudBurrow does not serve say
//     "Not available locally".
//   - Kubernetes clusters lists the one cluster with Google's columns; its
//     page is "Cluster details" with Details, Nodes, Storage, Observability
//     and Logs, Connect giving the kubectl commands, and Edit, Upgrade and
//     Delete disabled with the reason. Create cluster offers Autopilot and
//     Standard, both disabled with the reason.
//   - Workloads hides Kubernetes system objects until they are Shown, and
//     Deploy makes a Deployment, whose page has Google's tabs.
//   - Gateways, Services & Ingress and Storage are one page each with
//     their tabs; Secrets & ConfigMaps and Object Browser list the cluster.
func TestKubernetesEnginePagesThroughTheBrowser(t *testing.T) {
	needService(t, "gkeclusters")
	cluster := strings.TrimSpace(os.Getenv(envCluster))
	kubeconfig := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_KUBECONFIG"))
	if cluster == "" {
		t.Skipf("%s is not set: the cluster's name is its row on Kubernetes clusters", envCluster)
	}
	p := open(t)

	// The menu, in Google's order.
	p.navigate("/kubernetes/clusters")
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === %q)`, cluster))
	var menu string
	p.eval(`[...document.querySelectorAll("#product-nav-pages a")].map((a) => a.textContent).join(",")`, &menu)
	if want := "Clusters,Workloads,Applications,Secrets & ConfigMaps,Gateways, Services & Ingress,Storage,Object Browser,Backup for GKE,Config,Policy,Security"; menu != want {
		t.Errorf("the menu is %q, want %q", menu, want)
	}
	var head string
	p.eval(`[...document.querySelectorAll("#view thead th")].map((th) => th.textContent.trim()).filter(Boolean).join(",")`, &head)
	for _, col := range []string{"Name", "Location", "Mode", "Number of nodes", "Total vCPUs", "Total memory"} {
		if !strings.Contains(","+head+",", ","+col+",") {
			t.Errorf("the clusters list's columns are %q, want %s among them", head, col)
		}
	}

	// The cluster's page.
	p.clickText("#view tbody a", cluster)
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "Cluster details"`)
	var tabs string
	p.eval(`[...document.querySelectorAll("#view [role=tab]")].map((b) => b.textContent).join(",")`, &tabs)
	if tabs != "Details,Nodes,Storage,Observability,Logs" {
		t.Errorf("the cluster's tabs are %q", tabs)
	}
	var disabled string
	p.eval(`[...document.querySelectorAll("#view button")].filter((b) => ["Edit", "Upgrade", "Delete"].includes(b.textContent) &&
		b.disabled && (b.title || "").includes("Not available locally")).map((b) => b.textContent).join(",")`, &disabled)
	if disabled != "Edit,Upgrade,Delete" {
		t.Errorf("disabled with the reason: %q, want Edit, Upgrade and Delete", disabled)
	}
	p.clickText("#view button", "Connect")
	p.waitFor(`[...document.querySelectorAll("dialog[open] textarea")].some((a) => a.value.includes("export KUBECONFIG=") && a.value.includes("kubectl config use-context kind-"))`)
	p.eval(`(() => { document.querySelector("dialog[open]").close(); return true; })()`, nil)
	p.clickText("#view [role=tab]", "Nodes")
	p.waitFor(`[...document.querySelectorAll("#detail-panel a")].some((a) => new URL(a.href).pathname.startsWith("/kubernetes/nodes/"))`)

	// Create cluster: Autopilot or Standard, refused with the reason.
	p.navigate("/kubernetes/clusters/create")
	p.waitFor(`document.querySelector("#view form") !== null && [...document.querySelectorAll("#view input[type=radio][name=mode]")].length === 2`)
	var refused bool
	p.eval(`[...document.querySelectorAll("#view input[type=radio][name=mode]")].every((r) => r.disabled) &&
		document.querySelector("#view form").textContent.includes("#1913")`, &refused)
	if !refused {
		t.Error("Create cluster's Autopilot and Standard are not drawn disabled with the reason")
	}

	// Workloads: system objects hidden, then shown.
	p.navigate("/kubernetes/workloads")
	p.waitFor(`document.querySelector("#scope-system") !== null && document.querySelector("#view table") !== null`)
	var coredns bool
	p.eval(`[...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === "coredns")`, &coredns)
	if coredns {
		t.Error("Workloads lists kube-system's coredns while Kubernetes system objects are hidden")
	}
	p.setField("#scope-system", "shown")
	p.waitFor(`[...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === "coredns")`)

	// Deploy.
	name := fmt.Sprintf("gke1857-%d", time.Now().UnixNano()%1e6)
	t.Cleanup(func() {
		// The instance's cluster goes with the instance; with its kubeconfig
		// the deployment goes now.
		if kubeconfig == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "delete", "deployment", name, "-n", "default", "--ignore-not-found").Run()
	})
	p.navigate("/kubernetes/workloads")
	p.waitFor(`[...document.querySelectorAll("#view button")].some((b) => b.textContent.trim() === "Deploy")`)
	p.clickText("#view button", "Deploy")
	p.waitFor(`document.querySelector("#view form.create-page #f-image") !== null`)
	p.setField("#f-image", "example.invalid/web:1")
	p.setField("#f-name", name)
	p.setField("#f-replicas", "1")
	p.eval(`(() => { [...document.querySelectorAll("#view form.create-page button")].find((b) => b.textContent.trim() === "Deploy").click(); return true; })()`, nil)
	p.waitFor(`location.pathname !== "/kubernetes/workloads/create"`)
	p.navigate("/kubernetes/workloads/" + name)
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "Workload details" &&
		document.querySelectorAll("#view [role=tab]").length >= 7`)
	p.eval(`[...document.querySelectorAll("#view [role=tablist]")][0] && [...[...document.querySelectorAll("#view [role=tablist]")][0].querySelectorAll("[role=tab]")].map((b) => b.textContent).join(",")`, &tabs)
	if tabs != "Overview,Details,Observability,Revision history,Events,Logs,YAML" {
		t.Errorf("the workload's tabs are %q", tabs)
	}

	// Gateways, Services & Ingress: one page with three tabs.
	p.navigate("/kubernetes/services")
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "Gateways, Services & Ingress" &&
		document.querySelector("#view nav.page-tabs") !== null`)
	var strip string
	p.eval(`[...document.querySelectorAll("#view nav.page-tabs a")].map((a) => a.textContent + (a.getAttribute("aria-current") ? "*" : "")).join(",")`, &strip)
	if strip != "Gateways,Services*,Ingress" {
		t.Errorf("the page's tabs are %q", strip)
	}
	p.clickText("#view nav.page-tabs a", "Ingress")
	p.waitFor(`location.pathname === "/kubernetes/ingress" && document.querySelector("#product-nav-pages a[aria-current=page]") &&
		document.querySelector("#product-nav-pages a[aria-current=page]").textContent === "Gateways, Services & Ingress"`)

	// Storage: Persistent volume claims and Storage classes.
	p.navigate("/kubernetes/storage-classes")
	p.waitFor(`[...document.querySelectorAll("#view nav.page-tabs a")].map((a) => a.textContent).join(",") === "Persistent volume claims,Storage classes" &&
		[...document.querySelectorAll("#view tbody tr")].length > 0`)

	// Secrets & ConfigMaps and Object Browser.
	p.navigate("/kubernetes/config")
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "Secrets & ConfigMaps" && document.querySelector("#view table") !== null`)
	p.navigate("/kubernetes/object-browser")
	p.waitFor(`[...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === "deployments.apps")`)
	p.clickText("#view tbody a", "deployments.apps")
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#detail-panel a, #view a")].some((a) => a.textContent === "default/%s")`, name))

	// A page CloudBurrow does not serve.
	p.navigate("/kubernetes/backup")
	p.waitFor(`document.querySelector("#view h2") && document.querySelector("#view h2").textContent === "Not available locally"`)
}
