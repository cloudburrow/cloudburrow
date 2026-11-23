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

// TestDataprocHDFSThroughThePage (#2186): Create cluster makes a running
// single node cluster, whose Web interfaces tab links the HDFS NameNode
// through the component gateway, among the Hadoop interfaces in the
// console's order (YARN ResourceManager, MapReduce Job History, Spark
// History Server, HDFS NameNode); the NameNode's own page there reports
// the cluster's HDFS (its live DataNode, the master's); the tab and the
// link are reached by keyboard; every screen meets WCAG 2.1 AA.
func TestDataprocHDFSThroughThePage(t *testing.T) {
	needService(t, "dataproc-clusters")
	if os.Getenv("CLOUDBURROW_TEST_SPARK") == "" {
		t.Skip("CLOUDBURROW_TEST_SPARK is not set: a cluster runs in the pinned apache/spark images")
	}
	project := uniqueProject(t)
	p := openWithin(t, 30*time.Minute)
	p.navigate("/dataproc/clusters?project=" + project)
	p.clickText("#view button", "Create")
	p.waitFor(`document.getElementById("f-clusterName") !== null`)
	p.submitForm(map[string]string{"clusterName": "hdfs-b", "clusterType": "SINGLE"}, "clusterName")
	cluster := "projects/" + project + "/regions/us-central1/clusters/hdfs-b"
	t.Cleanup(func() {
		consoleDo(t, "POST", "/api/act/dataproc-clusters?project="+project+"&name="+cluster+"&action=delete", "{}")
	})
	dpWait(t, "dataproc-clusters", project, cluster, `"Running"`, 25*time.Minute)
	p.navigate("/dataproc/clusters?project=" + project)
	p.waitFor(listedRow("", "hdfs-b"))
	p.follow("hdfs-b")
	var clusterPath string
	p.eval(`location.pathname + location.search`, &clusterPath)
	p.clickText(`#view [role="tab"]`, "Web interfaces")
	p.waitFor(`[...document.querySelectorAll("#view a")].some((a) => a.textContent.trim() === "HDFS NameNode" && a.href.includes("/component-gateway/"))`)
	var order []string
	p.eval(`[...document.querySelectorAll("#view a")].map((a) => a.textContent.trim()).filter((s) => ["YARN ResourceManager", "MapReduce Job History", "Spark History Server", "HDFS NameNode"].includes(s))`, &order)
	if strings.Join(order, ",") != "YARN ResourceManager,MapReduce Job History,Spark History Server,HDFS NameNode" {
		t.Errorf("the Web interfaces' order: %v", order)
	}
	var nn string
	p.eval(`[...document.querySelectorAll("#view a")].find((a) => a.textContent.trim() === "HDFS NameNode").href`, &nn)
	if !strings.HasSuffix(nn, "/hdfs/dfshealth.html") {
		t.Errorf("the HDFS NameNode link %q", nn)
	}

	// The NameNode's own page, through the gateway: its scripts read its
	// JMX there, and it reports the master's DataNode live.
	r := open(t)
	r.run(chromedp.Navigate(nn))
	r.waitFor(`document.body.textContent.includes("Live Nodes") && document.body.textContent.includes("hdfs-b-m:8020")`)

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
		k.waitFor(`[...document.querySelectorAll("#view a")].some((a) => a.textContent.trim() === "HDFS NameNode")`)
		// Tab reaches the link.
		reached := false
		for i := 0; i < 40 && !reached; i++ {
			k.run(chromedp.KeyEvent(kb.Tab))
			var label string
			k.eval(`document.activeElement ? document.activeElement.textContent.trim() : ""`, &label)
			reached = label == "HDFS NameNode"
		}
		if !reached {
			t.Error("Tab does not reach the HDFS NameNode link")
		}
		for _, fl := range k.keyboardFailures() {
			t.Errorf("HDFS cluster: %s", fl)
		}
	})

	auditScreens(t, []a11yPage{
		screen("Dataproc clusters with an HDFS cluster", "/dataproc/clusters", project),
		{"HDFS cluster", clusterPath, settled, ""},
	})
}
