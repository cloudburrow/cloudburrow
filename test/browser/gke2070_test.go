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

	"github.com/chromedp/chromedp"
)

// gkeCheckRow checks a row's checkbox on the list shown.
func gkeCheckRow(p *tab, name string) {
	p.t.Helper()
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, fmt.Sprintf(`#view input[type=checkbox][aria-label="Select %s"]`, name)))
	p.eval(fmt.Sprintf(`(() => { const b = document.querySelector(%q); if (!b.checked) b.click(); return true; })()`,
		fmt.Sprintf(`#view input[type=checkbox][aria-label="Select %s"]`, name)), nil)
}

// gkeListDelete is the Workloads list's Delete over the checked rows.
const gkeListDelete = `[...document.querySelectorAll("#view .action-bar button")].find((b) => b.textContent.trim() === "Delete")`

// TestKubernetesEnginePagesWorkloadsBulkDeleteThroughTheBrowser (#2070), in the
// storage and served shards: Workloads has row checkboxes and one Delete
// for the checked workloads, confirmed as Google's "When prompted to
// confirm, click Delete" (stateless-apps); a Kubernetes system object
// among the checked is refused with the reason and nothing is deleted. A
// workload's Overview tab charts CPU, Memory and Disk from the kubelet's
// readings, or says why one has none. Then the list with rows checked, the
// Delete dialog and the Overview tab are audited for WCAG: names, contrast
// in both themes, the keyboard, reduced motion and reflow.
func TestKubernetesEnginePagesWorkloadsBulkDeleteThroughTheBrowser(t *testing.T) {
	needService(t, "workloads")
	kubeconfig := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_KUBECONFIG"))
	if kubeconfig == "" {
		t.Skip("CLOUDBURROW_TEST_KUBECONFIG is not set: the deletes are read back from the cluster")
	}
	stamp := time.Now().UnixNano() % 1e6
	names := []string{fmt.Sprintf("gkebulk-a-%d", stamp), fmt.Sprintf("gkebulk-b-%d", stamp), fmt.Sprintf("gkebulk-c-%d", stamp)}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, n := range names {
			_ = exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "delete", "deployment", n, "-n", "default", "--ignore-not-found").Run()
		}
	})
	for _, n := range names {
		gkeKubectl(t, kubeconfig, "create", "deployment", n, "--image=example.invalid/web:1", "--replicas=1", "-n", "default")
	}

	p := open(t)
	p.navigate("/kubernetes/workloads")
	p.waitFor(rowReady(names[2]))
	var disabled bool
	p.eval(gkeListDelete+`.disabled`, &disabled)
	if !disabled {
		t.Error("the list's Delete is enabled with nothing checked")
	}

	// A system object checked with one of ours: refused, nothing deleted
	// (the refusal is the action's 400).
	p.expected = []string{"/api/actions/workloads"}
	p.setField("#scope-system", "shown")
	p.waitFor(rowReady("coredns"))
	gkeCheckRow(p, names[2])
	gkeCheckRow(p, "coredns")
	p.eval(gkeListDelete+`.click()`, nil)
	p.confirmOnlyDialog()
	var title string
	p.eval(`document.querySelector(".modal.is-open #confirm-title").textContent`, &title)
	if title != "Delete 2 workloads?" {
		t.Errorf("the dialog is titled %q, want Delete 2 workloads?", title)
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open .modal-actions button.danger`)))
	if got := p.modalRefusal(); !strings.Contains(got, "Kubernetes system objects") || !strings.Contains(got, "nothing was deleted") {
		t.Errorf("deleting coredns with %s was answered %q", names[2], got)
	}
	p.clickText(".modal.is-open button", "Cancel")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	gkeEventually(t, kubeconfig, "deployment.apps/"+names[2], "get", "deployment", names[2], "-n", "default", "-o", "name")
	gkeEventually(t, kubeconfig, "deployment.apps/coredns", "get", "deployment", "coredns", "-n", "kube-system", "-o", "name")

	// The first two checked, deleted with one Delete.
	p = open(t)
	p.navigate("/kubernetes/workloads")
	p.waitFor(rowReady(names[1]))
	gkeCheckRow(p, names[0])
	gkeCheckRow(p, names[1])
	p.eval(gkeListDelete+`.click()`, nil)
	p.pressConfirm("Delete")
	for _, n := range names[:2] {
		gkeEventually(t, kubeconfig, "", "get", "deployment", n, "-n", "default", "--ignore-not-found", "-o", "name")
	}
	gkeEventually(t, kubeconfig, "deployment.apps/"+names[2], "get", "deployment", names[2], "-n", "default", "-o", "name")

	// The Overview tab's utilization charts: CPU, Memory and Disk, each a
	// chart of readings or the reason it has none.
	page := "/kubernetes/workloads/" + names[2]
	overview := `document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "Workload details" &&
		document.querySelectorAll("#detail-panel .chart-card").length === 3`
	p.navigate(page)
	p.waitFor(overview)
	var tabs, cards string
	p.eval(`[...document.querySelectorAll("#view [role=tablist]")][0] && [...[...document.querySelectorAll("#view [role=tablist]")][0].querySelectorAll("[role=tab]")].filter((b) => !b.hidden).map((b) => b.textContent).join(",")`, &tabs)
	if !strings.HasPrefix(tabs, "Overview,Details,") {
		t.Errorf("the workload's tabs are %q", tabs)
	}
	p.eval(`[...document.querySelectorAll("#detail-panel .chart-card")].map((c) => c.querySelector("h3").textContent + ":" +
		(c.querySelector("svg.chart") ? "chart" : (c.querySelector("p.unavailable") || {}).textContent ? "said" : "empty")).join(",")`, &cards)
	for _, want := range []string{"CPU:", "Memory:", "Disk:"} {
		if !strings.Contains(cards, want) {
			t.Errorf("the Overview's charts are %q, want %s", cards, want)
		}
	}
	if strings.Contains(cards, "empty") {
		t.Errorf("a chart draws neither readings nor a reason: %q", cards)
	}

	listChecked := rowReady(names[2]) + ` && (() => { const b = document.querySelector('#view input[type=checkbox][aria-label="Select ` + names[2] + `"]');
		if (b && !b.checked) b.click(); return true; })()`
	auditScreens(t, []a11yPage{
		{"Workloads with a row checked", "/kubernetes/workloads", listChecked, ""},
		{"Delete workloads dialog", "/kubernetes/workloads", listChecked, gkeListDelete + `.click()`},
		{"workload Overview", page, overview, ""},
	})
}

// TestKubernetesEnginePagesObjectBrowserTreeThroughTheBrowser (#2070), in the
// storage and served shards: Object Browser filters "by specific API
// groups and Resource Kinds" with a tree of checkboxes in place of the
// shared filter bar. Unchecking a group hides its resource types,
// unchecking a kind hides that type, and checking them again shows them;
// then the page, a group open, is audited for WCAG.
func TestKubernetesEnginePagesObjectBrowserTreeThroughTheBrowser(t *testing.T) {
	needService(t, "k8sobjects")
	p := open(t)
	p.navigate("/kubernetes/object-browser")
	ready := rowReady("deployments.apps") + ` && document.querySelector("#view fieldset.check-tree") !== null`
	p.waitFor(ready)
	var bar bool
	p.eval(`document.querySelector("#view input.filter") !== null`, &bar)
	if bar {
		t.Error("Object Browser still draws the shared filter bar beside its tree")
	}
	var legend string
	p.eval(`document.querySelector("#view fieldset.check-tree legend").textContent`, &legend)
	if legend != "API groups" {
		t.Errorf("the tree is %q, want API groups", legend)
	}

	group := func(label string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#view .check-tree-groups > li")].find((li) => li.querySelector(".check-tree-row label").textContent === %q)`, label)
	}
	shown := func(row string) bool {
		var on bool
		p.eval(fmt.Sprintf(`[...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === %q)`, row), &on)
		return on
	}

	// The apps group unchecked: its types go; checked again, they return.
	p.eval(group("apps")+`.querySelector(".check-tree-row input").click()`, nil)
	p.waitFor(`![...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === "deployments.apps")`)
	if !shown("pods") && !shown("configmaps") {
		t.Error("unchecking apps hid core's resource types too")
	}
	p.eval(group("apps")+`.querySelector(".check-tree-row input").click()`, nil)
	p.waitFor(rowReady("deployments.apps"))

	// core opened, its Pod unchecked: pods goes and the group is mixed.
	p.eval(group("core")+`.querySelector(".check-tree-toggle").click()`, nil)
	p.waitFor(group("core") + `.querySelector(".check-tree-kinds").hidden === false`)
	var expanded string
	p.eval(group("core")+`.querySelector(".check-tree-toggle").getAttribute("aria-expanded")`, &expanded)
	if expanded != "true" {
		t.Errorf("core's toggle is aria-expanded=%q once opened", expanded)
	}
	p.eval(`[...`+group("core")+`.querySelectorAll(".check-tree-kinds label")].find((l) => l.textContent === "Pod").querySelector("input").click()`, nil)
	p.waitFor(`![...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === "pods")`)
	var mixed bool
	p.eval(group("core")+`.querySelector(".check-tree-row input").indeterminate`, &mixed)
	if !mixed {
		t.Error("core is not drawn mixed with one of its kinds unchecked")
	}
	if !shown("deployments.apps") {
		t.Error("unchecking Pod hid apps' resource types")
	}

	openCore := group("core") + `.querySelector(".check-tree-toggle").click()`
	auditScreens(t, []a11yPage{
		{"Object Browser", "/kubernetes/object-browser", ready, ""},
		{"Object Browser with core open", "/kubernetes/object-browser", ready, openCore},
	})
}
