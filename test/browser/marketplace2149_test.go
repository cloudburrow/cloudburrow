//go:build browser

package browser

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/marketplace"
	"github.com/cloudburrow/cloudburrow/internal/marketplace/marketplacetest"
)

// Marketplace (#2149): Google Cloud Marketplace's home, browse with
// search and filters, a product's page, Configure and Deploy for a
// Kubernetes app, then the app on Kubernetes Engine > Applications with
// its components and Delete
// (https://docs.cloud.google.com/kubernetes-engine/docs/how-to/deploying-marketplace-app).
//
// The catalogue is Docker Hub's recorded answers (marketplacetest). The
// cluster is an in-memory kubectl by default; with
// CLOUDBURROW_MARKETPLACE_KUBECONFIG set to a kind cluster's kubeconfig,
// the deploy runs for real there and waits for nginx to be ready.

type oneCluster struct{ c console.MarketCluster }

func (o oneCluster) Clusters(context.Context, string) ([]console.MarketCluster, error) {
	return []console.MarketCluster{o.c}, nil
}
func (oneCluster) CreateCluster(context.Context, string, string, string) error { return nil }
func (oneCluster) CanCreate() string {
	return "Not available locally: no Kubernetes Engine API in this test."
}

func marketConsole(t *testing.T, hub *marketplace.DockerHub) (*tab, *marketplacetest.Cluster) {
	t.Helper()
	cluster := marketplacetest.NewCluster()
	var inv k8s.Invoker = cluster
	kubeconfig := "/fake/kubeconfig"
	if kc := os.Getenv("CLOUDBURROW_MARKETPLACE_KUBECONFIG"); kc != "" {
		inv, kubeconfig = k8s.Subprocess{}, kc
	}
	s := console.New("", func(context.Context) console.Status { return console.Status{} })
	s.SetMarketplace(&console.Marketplace{Catalog: marketplace.NewCatalog(hub), Kube: marketplace.Kube{Invoker: inv},
		Clusters: oneCluster{console.MarketCluster{Name: "cloudburrow-test", Location: "local", Status: "Running", Own: true, Kubeconfig: kubeconfig}}})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	p := openAt(t, strings.TrimPrefix(srv.URL, "http://"))
	t.Cleanup(p.forgive)
	return p, cluster
}

func TestMarketplaceBrowseSearchAndProduct(t *testing.T) {
	hub, rec := marketplacetest.DockerHub(t)
	p, _ := marketConsole(t, hub)

	p.navigate("/marketplace")
	p.waitFor(`document.querySelector('#marketplace-popular .mp-card[data-product="library/nginx"]') !== null`)
	p.waitFor(`document.querySelectorAll('#marketplace-categories .mp-category').length > 5`)
	var placeholder string
	p.eval(`document.querySelector("#marketplace-search").placeholder`, &placeholder)
	if placeholder != "Search Marketplace" {
		t.Errorf("the search reads %q, want Google's Search Marketplace", placeholder)
	}

	// Search.
	p.setField("#marketplace-search", "nginx")
	p.eval(`document.querySelector("#marketplace-search").form.requestSubmit(), true`, nil)
	p.waitFor(`location.pathname === "/marketplace/browse" && new URLSearchParams(location.search).get("q") === "nginx"`)
	p.waitFor(`document.querySelector('#marketplace-results .mp-card[data-product="library/nginx"] [data-badge="official"]') !== null`)
	var card struct{ Source, Verified, Count string }
	p.eval(`({ Source: document.querySelector('#marketplace-results .mp-card[data-product="library/nginx"] .mp-source').textContent,
		Verified: (document.querySelector('#marketplace-results [data-badge="verified_publisher"]') || {}).textContent || "",
		Count: document.querySelector("#marketplace-count").textContent })`, &card)
	if card.Source != "Docker Hub" || card.Verified != "Verified Publisher" || !strings.Contains(card.Count, "results") {
		t.Errorf("results: %+v", card)
	}

	// Filters: Google's Filter panel, here Publisher > Docker Official Image.
	p.waitFor(`document.querySelector('#marketplace-filters [data-group="category"]') !== null && document.querySelector('#marketplace-filters [data-group="type"]') !== null && document.querySelector('#marketplace-filters [data-group="source"]') !== null`)
	p.clickExpr(`document.querySelector('#marketplace-filters input[data-filter="badge:official"]')`)
	p.waitFor(`new URLSearchParams(location.search).getAll("filter").includes("badge:official")`)
	p.waitFor(`document.querySelector('#marketplace-active-filters [data-filter="badge:official"]') !== null && document.querySelector('#marketplace-count') !== null`)
	found := false
	for _, u := range rec.URLs {
		if strings.Contains(u, "badges=official") {
			found = true
		}
	}
	if !found {
		t.Errorf("the Publisher filter did not reach Docker Hub's search: %v", rec.URLs)
	}
	// Type: Kubernetes apps has no Docker Hub image.
	p.navigate("/marketplace/browse?filter=type:Kubernetes%20apps")
	p.waitFor(`document.querySelector("#marketplace-count") !== null`)
	var none string
	p.eval(`document.querySelector("#marketplace-count").textContent`, &none)
	if !strings.HasPrefix(none, "0 results") {
		t.Errorf("Type: Kubernetes apps reads %q; Docker Hub serves container images", none)
	}

	// A product's page: Overview (the README, as markup), the tabs, Tags.
	p.navigate("/marketplace/browse?q=nginx")
	p.clickExpr(`document.querySelector('#marketplace-results .mp-card[data-product="library/nginx"]')`)
	p.waitFor(`location.pathname === "/marketplace/product/dockerhub/library/nginx"`)
	p.waitFor(`document.querySelector("#marketplace-overview h2 + .md h2") !== null`)
	var page struct {
		Tabs, Overview, Configure, Facts string
		Scripts                          int
	}
	p.eval(`({ Tabs: [...document.querySelectorAll('[role="tab"]')].map((n) => n.textContent).join(","),
		Overview: document.querySelector("#marketplace-overview").textContent.slice(0, 400),
		Configure: document.querySelector("#marketplace-configure").textContent,
		Facts: document.querySelector("#marketplace-additional-details").textContent,
		Scripts: document.querySelectorAll("#marketplace-overview script, #marketplace-overview img, #marketplace-overview iframe").length })`, &page)
	if page.Tabs != "Overview,Pricing,Documentation,Support,Tags" || !strings.Contains(page.Overview, "Quick reference") ||
		page.Configure != "Configure" || !strings.Contains(page.Facts, "Container images") || page.Scripts != 0 {
		t.Errorf("product page: %+v", page)
	}
	p.clickText(`[role="tab"]`, "Tags")
	p.waitFor(`document.querySelectorAll("#marketplace-tags tbody tr").length === 3`)
	p.clickText(`[role="tab"]`, "Pricing")
	p.waitFor(`document.querySelector("#marketplace-pricing") !== null`)
}

func TestMarketplaceDeploysToApplications(t *testing.T) {
	hub, _ := marketplacetest.DockerHub(t)
	p, cluster := marketConsole(t, hub)
	real := os.Getenv("CLOUDBURROW_MARKETPLACE_KUBECONFIG") != ""

	p.navigate("/marketplace/product/dockerhub/library/nginx")
	p.clickText("#marketplace-configure", "Configure")
	p.waitFor(`location.pathname === "/marketplace/kubernetes/config/dockerhub/library/nginx"`)
	p.waitFor(`document.querySelector('#marketplace-namespace option[value="default"]') !== null && document.querySelector('#mp-param-ports') !== null`)
	var form struct{ Tabs, Ports, Clusters, NewCluster string }
	p.eval(`({ Tabs: [...document.querySelectorAll('[role="tab"]')].map((n) => n.textContent).join(","),
		Ports: document.querySelector("#mp-param-ports").value,
		Clusters: [...document.querySelectorAll("#marketplace-cluster option")].map((o) => o.textContent).join(","),
		NewCluster: document.querySelector("#marketplace-create-unavailable") ? "unavailable" : "" })`, &form)
	if form.Tabs != "Deploy via Google Cloud,Deploy via command line" || form.Ports != "80/tcp" ||
		!strings.Contains(form.Clusters, "cloudburrow-test") || !strings.Contains(form.Clusters, "Create a new cluster") || form.NewCluster != "unavailable" {
		t.Errorf("Configure: %+v", form)
	}
	p.setField("#marketplace-namespace", "__new__")
	p.setField("#marketplace-new-namespace", "shop")
	p.setField("#marketplace-app-name", "web-1")
	p.setField("#mp-param-tag", "alpine")
	p.clickText("#marketplace-deploy", "Deploy")
	p.waitForWithin(`location.pathname === "/kubernetes/applications/cloudburrow-test/shop/web-1"`, 2*time.Minute)
	p.waitForWithin(`document.querySelector('#application-info [data-status="OK"]') !== null`, 4*time.Minute)
	var app struct{ Components, Info string }
	p.eval(`({ Components: [...document.querySelectorAll("#application-components-table tbody tr")].map((r) => r.dataset.kind + ":" + r.dataset.name).join(","),
		Info: document.querySelector("#application-info").textContent })`, &app)
	if app.Components != "Deployment:web-1,Service:web-1" || !strings.Contains(app.Info, "docker.io/library/nginx:alpine") || !strings.Contains(app.Info, "shop") {
		t.Errorf("application page: %+v", app)
	}
	if !real {
		joined := strings.Join(cluster.Calls, "\n")
		if !strings.Contains(joined, "crd/applications.app.k8s.io") {
			t.Errorf("the Application CRD was not installed:\n%s", joined)
		}
	}

	p.navigate("/kubernetes/applications")
	p.waitFor(`document.querySelector('#applications-table tr[data-app="web-1"]') !== null`)
	var row string
	p.eval(`document.querySelector('#applications-table tr[data-app="web-1"]').textContent`, &row)
	if !strings.Contains(row, "nginx") || !strings.Contains(row, "alpine") || !strings.Contains(row, "cloudburrow-test") {
		t.Errorf("Applications row %q", row)
	}
	p.clickText(`#applications-table a`, "web-1")
	p.waitFor(`document.querySelector("#application-delete") !== null`)
	p.clickText("#application-delete", "Delete")
	p.pressConfirm("Delete")
	p.waitForWithin(`location.pathname === "/kubernetes/applications" && document.querySelector("#applications-empty") !== null`, 2*time.Minute)
}

func TestMarketplaceOffline(t *testing.T) {
	gone := httptest.NewServer(nil)
	gone.Close()
	hub := &marketplace.DockerHub{HubURL: gone.URL, AuthURL: gone.URL, RegistryURL: gone.URL}
	p, _ := marketConsole(t, hub)
	p.navigate("/marketplace")
	p.waitFor(`document.querySelector("#marketplace-error") !== null`)
	p.navigate("/marketplace/browse?q=redis")
	p.waitFor(`document.querySelector("#marketplace-results #marketplace-error button") !== null`)
}
