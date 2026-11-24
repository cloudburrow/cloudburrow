//go:build browser

package browser

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestDataprocYARNHighAvailabilityThroughThePage (#2189): a High
// Availability cluster made with Create cluster's High Availability type
// links its YARN ResourceManager on the Web interfaces tab; through the
// Component Gateway the link opens the active ResourceManager, whose About
// page reports its HA state active, with its ZooKeeper state store, and
// whose Nodes page lists both workers' NodeManagers. The tab and the link
// are reached by keyboard, and every screen meets WCAG 2.1 AA.
func TestDataprocYARNHighAvailabilityThroughThePage(t *testing.T) {
	needService(t, "dataproc-clusters")
	if os.Getenv("CLOUDBURROW_TEST_SPARK") == "" {
		t.Skip("CLOUDBURROW_TEST_SPARK is not set: a cluster runs in the pinned apache/spark images")
	}
	project := uniqueProject(t)
	p := openWithin(t, 35*time.Minute)
	p.navigate("/dataproc/clusters?project=" + project)
	p.clickText("#view button", "Create")
	p.waitFor(`document.getElementById("f-clusterName") !== null`)
	p.submitForm(map[string]string{"clusterName": "yarn-ha", "clusterType": "HA"}, "clusterName")
	cluster := "projects/" + project + "/regions/us-central1/clusters/yarn-ha"
	t.Cleanup(func() {
		consoleDo(t, "POST", "/api/act/dataproc-clusters?project="+project+"&name="+cluster+"&action=delete", "{}")
	})
	dpWait(t, "dataproc-clusters", project, cluster, `"Running"`, 30*time.Minute)
	p.navigate("/dataproc/clusters?project=" + project)
	p.waitFor(listedRow("", "yarn-ha"))
	p.follow("yarn-ha")
	var clusterPath string
	p.eval(`location.pathname + location.search`, &clusterPath)

	p.clickText(`#view [role="tab"]`, "Web interfaces")
	p.waitFor(`[...document.querySelectorAll("#view a")].some((a) => a.textContent.trim() === "YARN ResourceManager" && a.href.includes("/component-gateway/"))`)
	var rm string
	p.eval(`[...document.querySelectorAll("#view a")].find((a) => a.textContent.trim() === "YARN ResourceManager").href`, &rm)
	if !strings.HasSuffix(rm, "/yarn/") {
		t.Fatalf("the YARN ResourceManager link %q", rm)
	}

	// The active ResourceManager through the gateway: its About page and
	// its NodeManagers.
	r := open(t)
	r.expected = append(r.expected, "/favicon.ico")
	r.run(chromedp.Navigate(rm + "cluster/cluster"))
	r.waitFor(`/ResourceManager HA state:\s*active/.test(document.body.textContent) && document.body.textContent.includes("ZKRMStateStore")`)
	r.run(chromedp.Navigate(rm + "cluster/nodes"))
	r.waitFor(`document.body.textContent.includes("yarn-ha-w-0") && document.body.textContent.includes("yarn-ha-w-1")`)

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.run(reducedMotion())
		k.navigate(clusterPath)
		k.waitFor(`document.querySelectorAll("#view [role=tab]").length > 1`)
		k.run(chromedp.Focus(chromedp.CSS(`#view [role=tab]`)))
		for i := 0; i < 8; i++ {
			var label string
			k.eval(`document.activeElement ? document.activeElement.textContent.trim() : ""`, &label)
			if label == "Web interfaces" {
				break
			}
			k.run(chromedp.KeyEvent(kb.ArrowRight))
		}
		k.waitFor(`document.activeElement && document.activeElement.textContent.trim() === "Web interfaces"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`[...document.querySelectorAll("#view a")].some((a) => a.textContent.trim() === "YARN ResourceManager")`)
		for i := 0; i < 40; i++ {
			var label string
			k.eval(`document.activeElement ? document.activeElement.textContent.trim() : ""`, &label)
			if label == "YARN ResourceManager" {
				break
			}
			k.run(chromedp.KeyEvent(kb.Tab))
		}
		k.waitFor(`document.activeElement && document.activeElement.textContent.trim() === "YARN ResourceManager"`)
	})

	auditScreens(t, []a11yPage{
		screen("Dataproc clusters with an HA cluster", "/dataproc/clusters", project),
		{"HA cluster", clusterPath, settled, ""},
	})
}
