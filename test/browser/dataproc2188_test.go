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

// TestDataprocHDFSHighAvailabilityThroughThePage (#2188): Create cluster
// with the High Availability type and one local SSD on each worker makes
// a running cluster whose VM instances tab lists its three masters
// (-m-0, -m-1, -m-2), whose Configuration tab shows the workers' local
// SSDs, and whose Web interfaces tab links the HDFS NameNode, -m-0's,
// which reports itself in the nameservice with its live DataNodes; the
// form's local SSD fields are reached by keyboard; every screen meets
// WCAG 2.1 AA.
func TestDataprocHDFSHighAvailabilityThroughThePage(t *testing.T) {
	needService(t, "dataproc-clusters")
	if os.Getenv("CLOUDBURROW_TEST_SPARK") == "" {
		t.Skip("CLOUDBURROW_TEST_SPARK is not set: a cluster runs in the pinned apache/spark images")
	}
	project := uniqueProject(t)
	p := openWithin(t, 35*time.Minute)
	p.navigate("/dataproc/clusters?project=" + project)
	p.clickText("#view button", "Create")
	p.waitFor(`document.getElementById("f-clusterName") !== null && document.getElementById("f-workerLocalSsds") !== null`)
	var createPath string
	p.eval(`location.pathname + location.search`, &createPath)
	p.submitForm(map[string]string{"clusterName": "hdfs-ha", "clusterType": "HA", "workerLocalSsds": "1", "workerLocalSsdInterface": "nvme"}, "clusterName")
	cluster := "projects/" + project + "/regions/us-central1/clusters/hdfs-ha"
	t.Cleanup(func() {
		dpDeleteCluster(t, project, cluster)
	})
	dpWait(t, "dataproc-clusters", project, cluster, `"Running"`, 30*time.Minute)
	p.navigate("/dataproc/clusters?project=" + project)
	p.waitFor(listedRow("", "hdfs-ha"))
	p.follow("hdfs-ha")
	var clusterPath string
	p.eval(`location.pathname + location.search`, &clusterPath)

	p.clickText(`#view [role="tab"]`, "VM instances")
	p.waitFor(`["hdfs-ha-m-0", "hdfs-ha-m-1", "hdfs-ha-m-2", "hdfs-ha-w-0", "hdfs-ha-w-1"].every((n) => document.querySelector("#view").textContent.includes(n))`)

	p.clickText(`#view [role="tab"]`, "Configuration")
	p.waitFor(`document.querySelector("#view").textContent.includes("Number of local SSDs") && document.querySelector("#view").textContent.includes("NVMe")`)

	p.clickText(`#view [role="tab"]`, "Web interfaces")
	p.waitFor(`[...document.querySelectorAll("#view a")].some((a) => a.textContent.trim() === "HDFS NameNode" && a.href.includes("/component-gateway/"))`)
	var nn string
	p.eval(`[...document.querySelectorAll("#view a")].find((a) => a.textContent.trim() === "HDFS NameNode").href`, &nn)
	if !strings.HasSuffix(nn, "/hdfs/dfshealth.html") {
		t.Errorf("the HDFS NameNode link %q", nn)
	}

	// -m-0's NameNode through the gateway: in the nameservice, its state
	// (active or standby), and the two workers' DataNodes live.
	r := open(t)
	r.expected = append(r.expected, "/favicon.ico")
	r.run(chromedp.Navigate(nn))
	r.waitFor(`document.body.textContent.includes("hdfs-ha-m-0:8020") && /\((active|standby)\)/.test(document.body.textContent) && document.body.textContent.includes("Live Nodes")`)

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.run(reducedMotion())
		k.navigate(createPath)
		k.waitFor(`document.getElementById("f-clusterName") !== null`)
		// keyboardFailures walks the form by Tab: the local SSD fields
		// are reached.
		for _, fl := range k.keyboardFailures() {
			t.Errorf("Create cluster: %s", fl)
		}
		k.navigate(clusterPath)
		k.waitFor(`document.querySelectorAll("#view [role=tab]").length > 1`)
		k.run(chromedp.Focus(chromedp.CSS(`#view [role=tab]`)))
		for i := 0; i < 8; i++ {
			var label string
			k.eval(`document.activeElement ? document.activeElement.textContent.trim() : ""`, &label)
			if label == "Configuration" {
				break
			}
			k.run(chromedp.KeyEvent(kb.ArrowRight))
		}
		k.waitFor(`document.activeElement && document.activeElement.textContent.trim() === "Configuration"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`document.querySelector("#view").textContent.includes("Number of local SSDs")`)
	})

	auditScreens(t, []a11yPage{
		screen("Dataproc clusters with an HA cluster", "/dataproc/clusters", project),
		{"Create cluster with local SSDs", createPath, settled, ""},
		{"HA cluster", clusterPath, settled, ""},
	})
}
