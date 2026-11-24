//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestDataprocOnGKEThroughThePage (#2162), on an instance serving
// Dataproc and Kubernetes Engine: a Dataproc on GKE cluster, made through
// the API as Google's console does not make one, on a GKE cluster of the
// instance; Clusters lists it with its staging bucket; its page has Jobs,
// Node pools (default-pool, DEFAULT) and Configuration (type Dataproc on
// GKE, the GKE cluster, namespace and Spark version); its Submit job runs
// SparkPi as driver and executor pods on the GKE cluster, its Output the
// driver's; the cluster's tabs are reached by keyboard; every screen meets
// WCAG 2.1 AA; Delete removes it.
func TestDataprocOnGKEThroughThePage(t *testing.T) {
	needService(t, "dataproc-clusters")
	needService(t, "gkeclusters")
	gkeAddr := endpoint(t, envGKE)
	dpAddr := endpoint(t, "CLOUDBURROW_TEST_DATAPROC")
	if os.Getenv("CLOUDBURROW_TEST_SPARK") == "" {
		t.Skip("CLOUDBURROW_TEST_SPARK is not set: the driver runs the pinned apache/spark image")
	}
	project := uniqueProject(t)
	const zone = "us-central1-a"
	gke := fmt.Sprintf("dpg-%d", time.Now().UnixNano()%1e6)
	target := "projects/" + project + "/locations/" + zone + "/clusters/" + gke
	gkeURL := "http://" + gkeAddr + "/v1/" + target
	t.Cleanup(func() {
		deadline := time.Now().Add(4 * time.Minute)
		for time.Now().Before(deadline) {
			if code, _ := do(t, http.MethodGet, gkeURL, "", ""); code == http.StatusNotFound {
				return
			}
			do(t, http.MethodDelete, gkeURL, "", "")
			time.Sleep(3 * time.Second)
		}
	})
	if code, body := do(t, http.MethodPost, "http://"+gkeAddr+"/v1/projects/"+project+"/locations/"+zone+"/clusters",
		`{"cluster":{"name":"`+gke+`","initialNodeCount":1}}`, ""); code != http.StatusOK {
		t.Fatalf("create the GKE cluster = %d: %s", code, body)
	}
	gkeIdle(t, project, gke, 8*time.Minute)

	name := "vgke-browser"
	cluster := "projects/" + project + "/regions/us-central1/clusters/" + name
	body := `{"clusterName":"` + name + `","virtualClusterConfig":{"kubernetesClusterConfig":{
		"gkeClusterConfig":{"gkeClusterTarget":"` + target + `","nodePoolTarget":[{"nodePool":"` + target + `/nodePools/default-pool","roles":["DEFAULT"]}]},
		"kubernetesSoftwareConfig":{"componentVersion":{"SPARK":"3.5-dataproc-28"}}}}}`
	if code, resp := do(t, http.MethodPost, "http://"+dpAddr+"/v1/projects/"+project+"/regions/us-central1/clusters", body, ""); code != http.StatusOK {
		t.Fatalf("create the virtual cluster = %d: %s", code, resp)
	}
	t.Cleanup(func() {
		consoleDo(t, "POST", "/api/act/dataproc-clusters?project="+project+"&name="+cluster+"&action=delete", "{}")
	})
	dpWait(t, "dataproc-clusters", project, cluster, `"Running"`, 10*time.Minute)

	p := openWithin(t, 30*time.Minute)
	p.navigate("/dataproc/clusters?project=" + project)
	p.waitFor(listedRow("", name))
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q) && r.textContent.includes("dataproc-staging-us-central1-"))`, name))
	p.follow(name)
	var clusterPath string
	p.eval(`location.pathname + location.search`, &clusterPath)
	p.waitFor(`[...document.querySelectorAll("#view [role=tab]")].map((b) => b.textContent.trim()).join(",") === "Jobs,Node pools,Configuration"`)
	p.clickText(`#view [role="tab"]`, "Node pools")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("default-pool") && r.textContent.includes("DEFAULT"))`)
	p.clickText(`#view [role="tab"]`, "Configuration")
	p.waitFor(fmt.Sprintf(`["Dataproc on GKE", %q, "3.5-dataproc-28", "Namespace"].every((s) => document.querySelector("#view").textContent.includes(s))`, target))

	// Submit job, from the cluster's page: SparkPi on GKE.
	p.navigate(clusterPath)
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q)`, name))
	p.clickText(p.findAction("Submit job"), "Submit job")
	p.waitFor(`location.pathname === "/dataproc/jobs/create" && document.getElementById("f-jobType") !== null`)
	p.waitFor(fmt.Sprintf(`document.getElementById("f-cluster").value === %q`, name))
	p.eval(`(() => { const s = document.getElementById("f-jobType"); s.value = "SPARK";
		s.dispatchEvent(new Event("input", { bubbles: true })); s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.waitFor(`!document.getElementById("f-mainClass").closest("[hidden]")`)
	p.submitForm(map[string]string{"jobId": "gke-pi-browser", "mainClass": "org.apache.spark.examples.SparkPi",
		"jarFiles": "file:///usr/lib/spark/examples/jars/spark-examples.jar", "args": "10"}, "jobId")
	job := "projects/" + project + "/regions/us-central1/jobs/gke-pi-browser"
	dpWait(t, "dataproc-jobs", project, job, `"Done"`, 25*time.Minute)
	p.navigate("/dataproc/jobs?project=" + project)
	p.waitFor(listedRow("", "gke-pi-browser"))
	p.follow("gke-pi-browser")
	var jobPath string
	p.eval(`location.pathname + location.search`, &jobPath)
	p.waitFor(`document.querySelector("#view").textContent.includes("Pi is roughly")`)

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.run(reducedMotion())
		k.navigate(clusterPath)
		k.waitFor(`document.querySelectorAll("#view [role=tab]").length === 3`)
		k.run(chromedp.Focus(chromedp.CSS(`#view [role=tab]`)), chromedp.KeyEvent(kb.ArrowRight))
		k.waitFor(`document.activeElement && document.activeElement.getAttribute("role") === "tab" && document.activeElement.textContent.trim() === "Node pools"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("default-pool"))`)
		for _, fl := range k.keyboardFailures() {
			t.Errorf("Dataproc on GKE cluster: %s", fl)
		}
	})

	auditScreens(t, []a11yPage{
		screen("Dataproc clusters with a GKE cluster", "/dataproc/clusters", project),
		{"Dataproc on GKE cluster", clusterPath, settled, ""},
		{"job on a Dataproc on GKE cluster", jobPath, settled, ""},
	})

	p.navigate(clusterPath)
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q)`, name))
	p.dpAct("Delete")
	deadline := time.Now().Add(3 * time.Minute)
	for {
		_, list := consoleDo(t, http.MethodGet, "/api/resources/dataproc-clusters?project="+project, "")
		if !strings.Contains(list, `"`+name+`"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cluster was not deleted: %s", list)
		}
		time.Sleep(2 * time.Second)
	}
}
