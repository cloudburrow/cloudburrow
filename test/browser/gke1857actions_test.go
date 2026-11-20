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

// gkeKubectl runs kubectl against the instance's own cluster.
func gkeKubectl(t *testing.T, kubeconfig string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", kubeconfig}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// gkeEventually waits up to a minute for kubectl's answer to be want.
func gkeEventually(t *testing.T, kubeconfig, want string, args ...string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	got := ""
	for time.Now().Before(deadline) {
		got = gkeKubectl(t, kubeconfig, args...)
		if got == want {
			return
		}
		time.Sleep(time.Second)
	}
	t.Errorf("kubectl %s = %q, want %q", strings.Join(args, " "), got, want)
}

// TestKubernetesEnginePagesWorkloadActionsThroughTheBrowser (#1857), in the
// storage and served shards: a Deployment's Actions as Google's console
// has them, through the pages alone. Edit replicas scales it, Autoscale
// makes its HorizontalPodAutoscaler, Expose its Service, Rolling update
// changes its image and update strategy, the YAML tab's Edit saves a
// label, and Delete from its row on Workloads removes it. Then the
// workload's page, its Actions' dialogs and the Workloads list are
// audited for WCAG: names, contrast in both themes, the keyboard, reduced
// motion and reflow.
func TestKubernetesEnginePagesWorkloadActionsThroughTheBrowser(t *testing.T) {
	needService(t, "workloads")
	kubeconfig := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_KUBECONFIG"))
	if kubeconfig == "" {
		t.Skip("CLOUDBURROW_TEST_KUBECONFIG is not set: the actions are read back from the cluster")
	}
	name := fmt.Sprintf("gkeact-%d", time.Now().UnixNano()%1e6)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, kind := range []string{"deployment", "service", "hpa"} {
			obj := name
			if kind == "service" {
				obj = name + "-service"
			}
			_ = exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "delete", kind, obj, "-n", "default", "--ignore-not-found").Run()
		}
	})
	// An image that never needs pulling for the object to exist: the
	// actions change the object, whatever its pods do.
	gkeKubectl(t, kubeconfig, "create", "deployment", name, "--image=example.invalid/web:1", "--replicas=1", "-n", "default")

	p := open(t)
	page := "/kubernetes/workloads/" + name
	ready := `document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "Workload details" &&
		[...document.querySelectorAll("#view button")].some((b) => b.textContent.trim() === "Edit replicas")`
	p.navigate(page)
	p.waitFor(ready)
	for _, want := range []string{"Edit replicas", "Autoscale", "Expose", "Rolling update", "Delete"} {
		if p.findAction(want) == "" {
			t.Errorf("the workload's page offers no %s", want)
		}
	}

	p.pageAction("Edit replicas", map[string]string{"replicas": "2"}, "")
	gkeEventually(t, kubeconfig, "2", "get", "deployment", name, "-n", "default", "-o", "jsonpath={.spec.replicas}")

	p.navigate(page)
	p.waitFor(ready)
	p.pageAction("Autoscale", map[string]string{"min": "1", "max": "3", "cpu": "60"}, "")
	gkeEventually(t, kubeconfig, "1 3", "get", "hpa", name, "-n", "default", "-o", "jsonpath={.spec.minReplicas} {.spec.maxReplicas}")

	p.navigate(page)
	p.waitFor(ready)
	p.pageAction("Expose", map[string]string{"port": "80", "targetPort": "8080", "type": "Cluster IP"}, "")
	gkeEventually(t, kubeconfig, "ClusterIP 80 8080", "get", "service", name+"-service", "-n", "default",
		"-o", "jsonpath={.spec.type} {.spec.ports[0].port} {.spec.ports[0].targetPort}")

	p.navigate(page)
	p.waitFor(ready)
	p.pageAction("Rolling update", map[string]string{"image-web": "example.invalid/web:2", "maxSurge": "1", "maxUnavailable": "0"}, "")
	gkeEventually(t, kubeconfig, "example.invalid/web:2 1 0", "get", "deployment", name, "-n", "default",
		"-o", "jsonpath={.spec.template.spec.containers[0].image} {.spec.strategy.rollingUpdate.maxSurge} {.spec.strategy.rollingUpdate.maxUnavailable}")

	// The YAML tab's Edit.
	p.navigate(page + "?tab=object")
	p.waitFor(`document.querySelector("#section-actions-object") !== null`)
	p.clickText("#section-actions-object button", "Edit")
	p.waitFor(`document.querySelector(".modal.is-open #f-yaml") !== null`)
	p.eval(`(() => { const f = document.querySelector(".modal.is-open #f-yaml");
		f.value = f.value.replace(/\n  labels:\n/, "\n  labels:\n    edited: by-console\n");
		f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	p.clickText(".modal.is-open button[type=submit]", "Save")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	gkeEventually(t, kubeconfig, "by-console", "get", "deployment", name, "-n", "default", "-o", "jsonpath={.metadata.labels.edited}")

	// WCAG, before the Delete takes the page away.
	openDialog := func(label string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#view button")].find((b) => b.textContent.trim() === %q).click()`, label)
	}
	auditScreens(t, []a11yPage{
		{"workload details", page, ready, ""},
		{"Edit replicas dialog", page, ready, openDialog("Edit replicas")},
		{"Autoscale dialog", page, ready, openDialog("Autoscale")},
		{"Expose dialog", page, ready, openDialog("Expose")},
		{"Rolling update dialog", page, ready, openDialog("Rolling update")},
		{"Workloads", "/kubernetes/workloads", rowReady(name), ""},
	})

	// Delete, from the workload's row on Workloads.
	p = open(t)
	p.navigate("/kubernetes/workloads")
	p.waitFor(rowReady(name))
	find := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q))`, name)
	p.eval(find+`.querySelector(".overflow-trigger").click()`, nil)
	var items string
	p.eval(`[...document.querySelectorAll(".overflow-item")].map((b) => b.textContent.trim()).join(",")`, &items)
	if !strings.Contains(items, "Delete") || !strings.Contains(items, "Edit replicas") {
		t.Errorf("the row's menu is %q", items)
	}
	// "When prompted to confirm, click Delete." (#2070)
	p.clickText(".overflow-item", "Delete")
	p.pressConfirm("Delete")
	gkeEventually(t, kubeconfig, "", "get", "deployment", name, "-n", "default", "--ignore-not-found", "-o", "name")
}
