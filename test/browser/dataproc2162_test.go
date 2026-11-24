//go:build browser

package browser

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// dpWait polls a Dataproc resource through the console API until its
// detail holds want.
func dpWait(t *testing.T, provider, project, name, want string, limit time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		got := dpDetail(t, provider, project, name)
		if strings.Contains(got, want) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not come to hold %q: %s", name, want, got)
		}
		time.Sleep(2 * time.Second)
	}
}

// TestDataprocClustersThroughThePage (#2162), in the spark shard, whose
// instance serves Dataproc. Reached from Dataproc's menu, above
// Serverless: Autoscaling policies' Create and Edit; Create cluster on its
// own page (Standard, image 2.2, 2 workers, the component gateway, a
// label) makes a cluster of Spark containers, whose page has Jobs, VM
// instances, Configuration and Web interfaces (the Spark History Server);
// its Submit job opens Submit a job for it, and Google's SparkPi runs
// there, its Output the driver's; Edit resizes the cluster to 3 workers;
// Workflows' Create workflow template with a cluster selector, Run, and
// its run on the Workflows tab; Stop, Start and Delete. The job type is
// chosen by keyboard, and every screen meets WCAG 2.1 AA.
func TestDataprocClustersThroughThePage(t *testing.T) {
	needService(t, "dataproc-clusters")
	if os.Getenv("CLOUDBURROW_TEST_SPARK") == "" {
		t.Skip("CLOUDBURROW_TEST_SPARK is not set: a cluster runs in the pinned apache/spark images")
	}
	project := uniqueProject(t)
	p := openWithin(t, 30*time.Minute)
	p.navigateByDrawer("Databases", "/dataproc/clusters", "Dataproc")
	for _, link := range []string{"/dataproc/jobs", "/dataproc/workflows/templates", "/dataproc/autoscalingPolicies", "/dataproc/batches"} {
		p.waitFor(fmt.Sprintf(`document.querySelector('a[href^=%q]') !== null`, link))
	}

	// An autoscaling policy.
	p.navigate("/dataproc/autoscalingPolicies?project=" + project)
	p.clickText("#view button", "Create")
	p.submitForm(map[string]string{"policyId": "pol-browser", "maxWorkers": "4"}, "policyId")
	pol := "projects/" + project + "/regions/us-central1/autoscalingPolicies/pol-browser"
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr, #view h1")].some((r) => r.textContent.includes(%q))`, "pol-browser"))
	dpWait(t, "dataproc-autoscaling", project, pol, "2 to 4", time.Minute)
	p.navigate("/dataproc/autoscalingPolicies?project=" + project)
	p.waitFor(listedRow("", "pol-browser"))
	p.follow("pol-browser")
	var polPath string
	p.eval(`location.pathname + location.search`, &polPath)
	p.pageAction("Edit", map[string]string{"maxWorkers": "5"}, "")
	dpWait(t, "dataproc-autoscaling", project, pol, "2 to 5", time.Minute)

	// A cluster.
	p.navigate("/dataproc/clusters?project=" + project)
	p.clickText("#view button", "Create")
	p.waitFor(`["f-clusterName", "f-region", "f-imageVersion", "f-workers", "f-componentGateway"].every((id) => document.getElementById(id) !== null)`)
	p.submitForm(map[string]string{"clusterName": "browser-c", "imageVersion": "2.2-debian12", "workers": "2", "labels": "suite=browser"}, "clusterName")
	cluster := "projects/" + project + "/regions/us-central1/clusters/browser-c"
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr, #view h1")].some((r) => r.textContent.includes(%q))`, "browser-c"))
	t.Cleanup(func() {
		consoleDo(t, "POST", "/api/act/dataproc-clusters?project="+project+"&name="+cluster+"&action=delete", "{}")
	})
	dpWait(t, "dataproc-clusters", project, cluster, `"Running"`, 10*time.Minute)
	p.navigate("/dataproc/clusters?project=" + project)
	p.waitFor(listedRow("", "browser-c"))
	p.follow("browser-c")
	var clusterPath string
	p.eval(`location.pathname + location.search`, &clusterPath)
	p.clickText(`#view [role="tab"]`, "VM instances")
	p.waitFor(`document.querySelector("#view").textContent.includes("browser-c-m") && document.querySelector("#view").textContent.includes("browser-c-w-1")`)
	p.clickText(`#view [role="tab"]`, "Configuration")
	p.waitFor(`document.querySelector("#view").textContent.includes("2.2-debian12")`)
	p.clickText(`#view [role="tab"]`, "Web interfaces")
	p.waitFor(`[...document.querySelectorAll("#view a")].some((a) => a.textContent.includes("Spark History Server") && a.href.includes("/component-gateway/"))`)

	// Submit job, from the cluster's page.
	p.clickText(p.findAction("Submit job"), "Submit job")
	p.waitFor(`location.pathname === "/dataproc/jobs/create" && document.getElementById("f-jobType") !== null`)
	p.waitFor(`document.getElementById("f-cluster").value === "browser-c"`)
	p.eval(`(() => { const s = document.getElementById("f-jobType"); s.value = "SPARK";
		s.dispatchEvent(new Event("input", { bubbles: true })); s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.waitFor(`!document.getElementById("f-mainClass").closest("[hidden]")`)
	p.submitForm(map[string]string{"jobId": "pi-browser", "mainClass": "org.apache.spark.examples.SparkPi",
		"jarFiles": "file:///usr/lib/spark/examples/jars/spark-examples.jar", "args": "100", "labels": "suite=browser"}, "jobId")
	job := "projects/" + project + "/regions/us-central1/jobs/pi-browser"
	dpWait(t, "dataproc-jobs", project, job, `"Done"`, 10*time.Minute)
	p.navigate("/dataproc/jobs?project=" + project)
	p.waitFor(listedRow("", "pi-browser"))
	p.follow("pi-browser")
	var jobPath string
	p.eval(`location.pathname + location.search`, &jobPath)
	p.waitFor(`document.querySelector("#view").textContent.includes("Pi is roughly")`)
	p.clickText(`#view [role="tab"]`, "Configuration")
	p.waitFor(`[...document.querySelectorAll("#view a")].some((a) => a.textContent.includes("Spark History Server") && a.href.includes("/sparkhistory/history/"))`)

	// Edit resizes it.
	p.navigate(clusterPath)
	p.waitFor(`document.querySelector("#view").textContent.includes("browser-c")`)
	p.pageAction("Edit", map[string]string{"workers": "3"}, "")
	dpWait(t, "dataproc-clusters", project, cluster, "browser-c-w-2", 5*time.Minute)
	dpWait(t, "dataproc-clusters", project, cluster, `"Running"`, 5*time.Minute)

	// A workflow template on the cluster, by its label.
	p.navigate("/dataproc/workflows/templates?project=" + project)
	p.clickText("#view button", "Create")
	p.waitFor(`["f-templateId", "f-placement", "f-jobs"].every((id) => document.getElementById(id) !== null)`)
	p.eval(`(() => { document.querySelector('input[type="radio"][name="f-placement-choice"][value="SELECTOR"]').click(); return true; })()`, nil)
	p.waitFor(`!document.getElementById("f-selector").closest("[hidden]")`)
	p.submitForm(map[string]string{"templateId": "wf-browser", "selector": "suite=browser",
		"jobs": "pi-step spark org.apache.spark.examples.SparkPi 10 jars=file:///usr/lib/spark/examples/jars/spark-examples.jar"}, "templateId")
	tmpl := "projects/" + project + "/regions/us-central1/workflowTemplates/wf-browser"
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr, #view h1")].some((r) => r.textContent.includes(%q))`, "wf-browser"))
	p.navigate("/dataproc/workflows/templates?project=" + project)
	p.waitFor(listedRow("", "wf-browser"))
	p.follow("wf-browser")
	var tmplPath string
	p.eval(`location.pathname + location.search`, &tmplPath)
	p.clickAction("Run", "")
	dpWait(t, "dataproc-workflows", project, tmpl, "1 of 1 completed", 10*time.Minute)

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.run(reducedMotion())
		k.navigate("/dataproc/jobs/create?project=" + project)
		k.waitFor(`document.getElementById("f-jobType") !== null`)
		k.tabTo(`document.activeElement === document.getElementById("f-jobType")`)
		// Tab reaches the job type, then the fields it shows, and the
		// form's Submit button.
		k.tabTo(`document.activeElement === document.getElementById("f-mainPythonFile")`)
		k.tabTo(`document.activeElement && document.activeElement.textContent.trim() === "Submit" && document.activeElement.type === "submit"`)
		for _, fl := range k.keyboardFailures() {
			t.Errorf("submit a job: %s", fl)
		}
	})

	auditScreens(t, []a11yPage{
		screen("Dataproc clusters", "/dataproc/clusters", project),
		screen("create cluster", "/dataproc/clusters/create", project),
		{"cluster", clusterPath, settled, ""},
		screen("Dataproc jobs", "/dataproc/jobs", project),
		screen("submit a job", "/dataproc/jobs/create", project),
		{"job", jobPath, settled, ""},
		screen("Dataproc workflow templates", "/dataproc/workflows/templates", project),
		screen("create workflow template", "/dataproc/workflows/templates/create", project),
		{"workflow template", tmplPath, settled, ""},
		screen("Dataproc autoscaling policies", "/dataproc/autoscalingPolicies", project),
		screen("create autoscaling policy", "/dataproc/autoscalingPolicies/create", project),
		{"autoscaling policy", polPath, settled, ""},
	})

	// Stop, Start and Delete.
	p.navigate(clusterPath)
	p.waitFor(`document.querySelector("#view").textContent.includes("browser-c")`)
	p.dpAct("Stop")
	dpWait(t, "dataproc-clusters", project, cluster, `"Stopped"`, 5*time.Minute)
	p.navigate(clusterPath)
	p.waitFor(`document.querySelector("#view").textContent.includes("Stopped")`)
	p.clickAction("Start", "")
	dpWait(t, "dataproc-clusters", project, cluster, `"Running"`, 5*time.Minute)
	p.navigate(tmplPath)
	p.waitFor(`document.querySelector("#view").textContent.includes("wf-browser")`)
	p.dpAct("Delete")
	p.navigate(clusterPath)
	p.waitFor(`document.querySelector("#view").textContent.includes("browser-c")`)
	p.dpAct("Delete")
	p.navigate(polPath)
	p.waitFor(`document.querySelector("#view").textContent.includes("pol-browser")`)
	p.dpAct("Delete")
}

// TestDataprocHiveOnDriverNodesThroughThePage (#2162), in the spark
// shard: Create cluster with a driver node group of one node and the
// ZooKeeper component; its Configuration lists the node group
// (CLUSTER-np-XXXX) and the component; Submit a job of type Hive, with its
// queries and the driver memory and vCores a cluster with a driver node
// group needs, runs on the cluster's HiveServer2, its Output Beeline's
// table; Resize node group makes it two nodes, and Stop is not offered;
// and the screens meet WCAG 2.1 AA.
func TestDataprocHiveOnDriverNodesThroughThePage(t *testing.T) {
	needService(t, "dataproc-clusters")
	if os.Getenv("CLOUDBURROW_TEST_SPARK") == "" {
		t.Skip("CLOUDBURROW_TEST_SPARK is not set: a cluster runs in the pinned apache/spark images")
	}
	project := uniqueProject(t)
	p := openWithin(t, 30*time.Minute)
	p.navigate("/dataproc/clusters?project=" + project)
	p.clickText("#view button", "Create")
	p.waitFor(`["f-clusterName", "f-driverNodes", "f-comp_ZOOKEEPER", "f-comp_TRINO", "f-comp_JUPYTER"].every((id) => document.getElementById(id) !== null)`)
	p.submitForm(map[string]string{"clusterName": "browser-ng", "imageVersion": "2.2-debian12", "workers": "2", "driverNodes": "1",
		"comp_ZOOKEEPER": "true"}, "clusterName")
	cluster := "projects/" + project + "/regions/us-central1/clusters/browser-ng"
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr, #view h1")].some((r) => r.textContent.includes(%q))`, "browser-ng"))
	t.Cleanup(func() {
		consoleDo(t, "POST", "/api/act/dataproc-clusters?project="+project+"&name="+cluster+"&action=delete", "{}")
	})
	dpWait(t, "dataproc-clusters", project, cluster, `"Running"`, 10*time.Minute)
	p.navigate("/dataproc/clusters?project=" + project)
	p.waitFor(listedRow("", "browser-ng"))
	p.follow("browser-ng")
	var clusterPath string
	p.eval(`location.pathname + location.search`, &clusterPath)
	p.clickText(`#view [role="tab"]`, "Configuration")
	p.waitFor(`document.querySelector("#view").textContent.includes("ZOOKEEPER") && /\(DRIVER\): browser-ng-np-[a-z0-9]{4}/.test(document.querySelector("#view").textContent)`)

	p.clickText(p.findAction("Submit job"), "Submit job")
	p.waitFor(`location.pathname === "/dataproc/jobs/create" && document.getElementById("f-cluster").value === "browser-ng"`)
	p.eval(`(() => { const s = document.getElementById("f-jobType"); s.value = "HIVE";
		s.dispatchEvent(new Event("input", { bubbles: true })); s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.waitFor(`!document.getElementById("f-queries").closest("[hidden]") && document.getElementById("f-mainPythonFile").closest("[hidden]") !== null`)
	p.submitForm(map[string]string{"jobId": "hive-browser", "queries": "SELECT 6 * 7 AS answer;", "driverMemory": "1024", "driverVcores": "1"}, "jobId")
	job := "projects/" + project + "/regions/us-central1/jobs/hive-browser"
	dpWait(t, "dataproc-jobs", project, job, `"Done"`, 10*time.Minute)
	p.navigate("/dataproc/jobs?project=" + project)
	p.waitFor(listedRow("", "hive-browser"))
	p.follow("hive-browser")
	var jobPath string
	p.eval(`location.pathname + location.search`, &jobPath)
	p.waitFor(`document.querySelector("#view").textContent.includes("answer") && document.querySelector("#view").textContent.includes("42")`)

	// Resize node group, from the cluster's page.
	p.navigate(clusterPath)
	p.waitFor(`document.querySelector("#view").textContent.includes("browser-ng")`)
	p.pageAction("Resize node group", map[string]string{"size": "2"}, "")
	dpWait(t, "dataproc-clusters", project, cluster, "DRIVER): browser-ng-np-", time.Minute)
	got := dpWait(t, "dataproc-clusters", project, cluster, ", browser-ng-np-", 3*time.Minute)
	if !strings.Contains(got, "Clusters with node groups cannot be stopped.") {
		t.Errorf("Stop is offered on a cluster with node groups: %s", got)
	}

	auditScreens(t, []a11yPage{
		screen("create cluster", "/dataproc/clusters/create", project),
		{"cluster with a driver node group", clusterPath, settled, ""},
		screen("submit a job", "/dataproc/jobs/create", project),
		{"Hive job", jobPath, settled, ""},
	})
	p.navigate(clusterPath)
	p.waitFor(`document.querySelector("#view").textContent.includes("browser-ng")`)
	p.dpAct("Delete")
}
