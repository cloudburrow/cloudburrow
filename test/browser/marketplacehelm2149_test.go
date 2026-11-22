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

// Marketplace's Kubernetes apps (#2149): Helm charts from Artifact Hub
// (its recorded answers, bitnami/nginx's archive replaced by a small
// chart) and a Helm repository's index.yaml, configured from the chart's
// values.schema.json and values.yaml, installed with the Helm SDK,
// upgraded and uninstalled from the Application's page. With
// CLOUDBURROW_MARKETPLACE_KUBECONFIG set, the release is installed on that
// cluster for real.
func TestMarketplaceDeploysHelmChart(t *testing.T) {
	charts := marketplacetest.ChartServer(t)
	cluster := marketplacetest.NewCluster()
	var inv k8s.Invoker = cluster
	kube := marketplace.Kube{Invoker: inv, Helm: marketplacetest.NewHelm(cluster).Config()}
	kubeconfig := "/fake/kubeconfig"
	real := os.Getenv("CLOUDBURROW_MARKETPLACE_KUBECONFIG")
	if real != "" {
		kube, kubeconfig = marketplace.Kube{Invoker: k8s.Subprocess{}}, real
	}
	repos := &marketplace.UserRepos{File: t.TempDir() + "/repos.json"}
	s := console.New("", func(context.Context) console.Status { return console.Status{} })
	s.SetMarketplace(&console.Marketplace{
		Catalog: marketplace.NewCatalog(charts.ArtifactHub(), charts.Repo("rancher", "Rancher")),
		Kube:    kube, Repos: repos,
		Clusters: oneCluster{console.MarketCluster{Name: "cloudburrow-test", Location: "local", Status: "Running", Own: true, Kubeconfig: kubeconfig}}})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	p := openAt(t, strings.TrimPrefix(srv.URL, "http://"))
	t.Cleanup(p.forgive)

	// Browse: each card names its source; the Source filter narrows them.
	p.navigate("/marketplace/browse?q=nginx")
	p.waitFor(`document.querySelector('#marketplace-results .mp-card[data-product="bitnami/nginx"]') !== null`)
	var card struct{ Source, Badge, Type string }
	p.eval(`(() => { const c = document.querySelector('#marketplace-results .mp-card[data-product="bitnami/nginx"]');
		return { Source: c.querySelector(".mp-source").textContent, Badge: (c.querySelector("[data-badge]") || {}).textContent || "", Type: c.querySelector(".mp-type").textContent }; })()`, &card)
	if card.Source != "Artifact Hub" || card.Badge != "Verified Publisher" || card.Type != "Kubernetes apps" {
		t.Errorf("Artifact Hub card %+v", card)
	}
	p.navigate("/marketplace/browse?q=web&filter=source:rancher")
	p.waitFor(`document.querySelector('#marketplace-results .mp-card[data-product="fixture/hello"]') !== null`)
	var only int
	p.eval(`document.querySelectorAll('#marketplace-results .mp-card:not([data-source="rancher"])').length`, &only)
	if only != 0 {
		t.Errorf("Source: Rancher shows %d cards of other sources", only)
	}

	// The product page and Configure: the schema's fields and the values editor.
	p.navigate("/marketplace/product/artifacthub/bitnami/nginx")
	p.waitFor(`document.querySelector("#marketplace-configure") !== null`)
	var tabs string
	p.eval(`[...document.querySelectorAll('[role="tab"]')].map((n) => n.textContent).join(",")`, &tabs)
	if tabs != "Overview,Pricing,Documentation,Support,Versions" {
		t.Errorf("chart tabs %q", tabs)
	}
	p.clickText("#marketplace-configure", "Configure")
	p.waitFor(`location.pathname === "/marketplace/kubernetes/config/artifacthub/bitnami/nginx"`)
	p.waitFor(`document.querySelector('#marketplace-namespace option[value="default"]') !== null && document.getElementById("mp-param-v.replicaCount") !== null && document.getElementById("mp-param-values") !== null`)
	var form struct{ Replicas, ServiceType, Values, Version string }
	p.eval(`({ Replicas: document.getElementById("mp-param-v.replicaCount").value,
		ServiceType: [...document.getElementById("mp-param-v.service.type").options].map((o) => o.value).join(","),
		Values: document.getElementById("mp-param-values").value, Version: document.getElementById("mp-param-chartVersion").value })`, &form)
	if form.Replicas != "1" || form.ServiceType != "ClusterIP,NodePort,LoadBalancer" || !strings.Contains(form.Values, "replicaCount: 1") || form.Version != "25.2.16" {
		t.Errorf("chart Configure %+v", form)
	}
	p.setField("#marketplace-namespace", "__new__")
	p.setField("#marketplace-new-namespace", "charts")
	p.setField("#marketplace-app-name", "hello-1")
	p.eval(`document.getElementById("mp-param-v.replicaCount").value = "2", true`, nil)
	p.clickText("#marketplace-deploy", "Deploy")
	p.waitForWithin(`location.pathname === "/kubernetes/applications/cloudburrow-test/charts/hello-1"`, 3*time.Minute)
	p.waitForWithin(`document.querySelector('#application-info [data-status="OK"]') !== null`, 4*time.Minute)
	var app struct{ Components, Info string }
	p.eval(`({ Components: [...document.querySelectorAll("#application-components-table tbody tr")].map((r) => r.dataset.kind + ":" + r.dataset.name).sort().join(","),
		Info: document.querySelector("#application-info").textContent })`, &app)
	if app.Components != "Deployment:hello-1,Service:hello-1" || !strings.Contains(app.Info, "Helm release") || !strings.Contains(app.Info, "revision 1") ||
		!strings.Contains(app.Info, "Artifact Hub") {
		t.Errorf("chart application %+v", app)
	}

	// Upgrade, as helm upgrade: the release's values, edited.
	p.clickText("#application-upgrade", "Upgrade")
	p.waitFor(`document.querySelector("#application-upgrade-values") !== null`)
	var values string
	p.eval(`document.querySelector("#application-upgrade-values").value`, &values)
	if !strings.Contains(values, "replicaCount: 2") {
		t.Errorf("the release's values %q", values)
	}
	p.eval(`(() => { const v = document.querySelector("#application-upgrade-values"); v.value = v.value.replace("replicaCount: 2", "replicaCount: 3"); return true; })()`, nil)
	p.clickText("#application-upgrade-submit", "Upgrade")
	p.waitForWithin(`document.querySelector("#application-info") !== null && document.querySelector("#application-info").textContent.includes("revision 2")`, 3*time.Minute)

	// Delete uninstalls the release.
	p.clickText("#application-delete", "Delete")
	p.pressConfirm("Delete")
	p.waitForWithin(`location.pathname === "/kubernetes/applications" && document.querySelector("#applications-empty") !== null`, 2*time.Minute)

	// Helm repositories: add one, see it, remove it.
	p.navigate("/marketplace/repositories")
	p.waitFor(`document.querySelector("#marketplace-repos-empty") !== null && document.querySelectorAll("#marketplace-rancher-table tbody tr").length === 3`)
	p.setField("#marketplace-repo-name", "bad name")
	p.setField("#marketplace-repo-url", "https://charts.example.com")
	p.clickText("#marketplace-repo-add", "Add repository")
	p.waitFor(`!document.querySelector("#marketplace-repo-error").hidden`)
	p.setField("#marketplace-repo-name", "example")
	p.clickText("#marketplace-repo-add", "Add repository")
	p.waitFor(`document.querySelector('#marketplace-repos-table tr[data-repo="example"]') !== null`)
	p.clickExpr(`document.querySelector('[data-remove="example"]')`)
	p.pressConfirm("Remove")
	p.waitFor(`document.querySelector("#marketplace-repos-empty") !== null`)
}
