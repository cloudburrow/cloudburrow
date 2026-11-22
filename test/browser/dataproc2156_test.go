//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// dpDetail reads a Dataproc resource back through the console API.
func dpDetail(t *testing.T, provider, project, name string) string {
	t.Helper()
	q := url.Values{"project": {project}, "name": {name}}
	code, body := consoleDo(t, http.MethodGet, "/api/detail/"+provider+"?"+q.Encode(), "")
	if code != http.StatusOK {
		t.Fatalf("read %s through the console API = %d: %s", name, code, body)
	}
	return body
}

// dpAct performs a resource page's action that has no form, confirming
// its dialog by the word it asks for or by its button.
func (p *tab) dpAct(label string) {
	p.t.Helper()
	p.waitFor(`document.querySelector(".modal") === null`)
	where := p.findAction(label)
	if where == "" {
		p.t.Fatalf("the page offers no %s", label)
	}
	p.clickText(where, label)
	p.waitFor(`document.querySelector(".modal.is-open") !== null`)
	var typed bool
	p.eval(`document.querySelector(".modal.is-open #confirm-input") !== null`, &typed)
	if typed {
		p.typeConfirmWord()
	} else {
		p.eval(`document.querySelector(".modal.is-open .modal-actions button.danger").click(); true`, nil)
	}
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
}

// TestDataprocBatchesThroughThePage (#2156), in the spark shard, whose
// instance serves Serverless for Apache Spark. Reached from Dataproc's
// menu: Create batch on its own page submits Google's quickstart batch
// (Spark, org.apache.spark.examples.SparkPi from the runtime's examples
// jar, with an argument and a label); the list shows it; its page has
// Summary, State history, Output and Job logs; Cancel ends it Cancelled
// and Delete removes it. Session templates' Create session template and
// Edit, read back through the console API, and Delete. The batch type
// is chosen by keyboard, which shows that type's fields, and every screen
// meets WCAG 2.1 AA.
func TestDataprocBatchesThroughThePage(t *testing.T) {
	needService(t, "dataproc-batches")
	if os.Getenv("CLOUDBURROW_TEST_SPARK") == "" {
		t.Skip("CLOUDBURROW_TEST_SPARK is not set: a batch runs in the pinned apache/spark images")
	}
	project := uniqueProject(t)
	p := openWithin(t, 20*time.Minute)
	p.navigateByDrawer("Databases", "/dataproc/batches", "Dataproc")
	p.waitFor(`[...document.querySelectorAll('a[href^="/dataproc/interactive"]')].some((a) => a.textContent.includes("Interactive sessions"))`)

	p.navigate("/dataproc/batches?project=" + project)
	p.clickText("#view button", "Create")
	p.waitFor(`["f-batchId", "f-region", "f-batchType", "f-runtimeVersion"].every((id) => document.getElementById(id) !== null)`)
	p.eval(`(() => { const s = document.getElementById("f-batchType"); s.value = "SPARK";
		s.dispatchEvent(new Event("input", { bubbles: true })); s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.waitFor(`!document.getElementById("f-mainClass").closest("[hidden]")`)
	p.submitForm(map[string]string{"batchId": "sparkpi-browser", "mainClass": "org.apache.spark.examples.SparkPi",
		"jarFiles": "file:///usr/lib/spark/examples/jars/spark-examples.jar", "args": "100000", "labels": "suite=browser"}, "batchId")
	batch := "projects/" + project + "/locations/us-central1/batches/sparkpi-browser"
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr, #view .detail-title, #view h1")].some((r) => r.textContent.includes(%q))`, "sparkpi-browser"))
	if got := dpDetail(t, "dataproc-batches", project, batch); !strings.Contains(got, "SparkPi") || !strings.Contains(got, "suite: browser") {
		t.Errorf("the batch reads back as %s", got)
	}
	p.navigate("/dataproc/batches?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("sparkpi-browser") && r.textContent.includes("Spark"))`)
	p.follow("sparkpi-browser")
	var batchPath string
	p.eval(`location.pathname + location.search`, &batchPath)
	for _, tab := range []string{"State history", "Output", "Job logs", "Summary"} {
		p.clickText(`#view [role="tab"]`, tab)
	}
	p.waitFor(`document.querySelector("#view").textContent.includes("org.apache.spark.examples.SparkPi")`)
	// Running, then cancelled from the page.
	deadline := time.Now().Add(10 * time.Minute)
	for !strings.Contains(dpDetail(t, "dataproc-batches", project, batch), `"Running"`) {
		if time.Now().After(deadline) {
			t.Fatalf("the batch did not run: %s", dpDetail(t, "dataproc-batches", project, batch))
		}
		time.Sleep(2 * time.Second)
	}
	p.navigate(batchPath)
	p.waitFor(`document.querySelector("#view").textContent.includes("sparkpi-browser")`)
	p.dpAct("Cancel")
	deadline = time.Now().Add(3 * time.Minute)
	for !strings.Contains(dpDetail(t, "dataproc-batches", project, batch), `"Cancelled"`) {
		if time.Now().After(deadline) {
			t.Fatalf("the batch was not cancelled: %s", dpDetail(t, "dataproc-batches", project, batch))
		}
		time.Sleep(time.Second)
	}

	// Session templates.
	p.navigate("/dataproc/session-templates?project=" + project)
	p.clickText("#view button", "Create")
	p.waitFor(`["f-templateId", "f-region", "f-runtimeVersion", "f-idleTtl"].every((id) => document.getElementById(id) !== null)`)
	p.submitForm(map[string]string{"templateId": "connect-browser", "runtimeVersion": "3.0", "idleTtl": "30", "description": "From the page"}, "templateId")
	tmpl := "projects/" + project + "/locations/us-central1/sessionTemplates/connect-browser"
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr, #view h1")].some((r) => r.textContent.includes(%q))`, "connect-browser"))
	if got := dpDetail(t, "dataproc-templates", project, tmpl); !strings.Contains(got, "From the page") || !strings.Contains(got, "30 min") {
		t.Errorf("the template reads back as %s", got)
	}
	p.navigate("/dataproc/session-templates?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("connect-browser"))`)
	p.follow("connect-browser")
	var tmplPath string
	p.eval(`location.pathname + location.search`, &tmplPath)
	p.pageAction("Edit", map[string]string{"description": "Edited on the page"}, "")
	if got := dpDetail(t, "dataproc-templates", project, tmpl); !strings.Contains(got, "Edited on the page") {
		t.Errorf("the edited template reads back as %s", got)
	}

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.run(reducedMotion())
		k.navigate("/dataproc/batches/create?project=" + project)
		k.waitFor(`document.getElementById("f-batchType") !== null`)
		k.tabTo(`document.activeElement === document.getElementById("f-batchType")`)
		// Tab reaches the batch type, then the fields it shows, and the
		// form's Create button.
		k.tabTo(`document.activeElement === document.getElementById("f-mainPythonFile")`)
		k.tabTo(`document.activeElement && document.activeElement.textContent.trim() === "Create" && document.activeElement.type === "submit"`)
		for _, fl := range k.keyboardFailures() {
			t.Errorf("create batch: %s", fl)
		}
	})

	auditScreens(t, []a11yPage{
		screen("Dataproc batches", "/dataproc/batches", project),
		screen("create batch", "/dataproc/batches/create", project),
		{"batch", batchPath, settled, ""},
		screen("Dataproc interactive sessions", "/dataproc/interactive", project),
		screen("create interactive session", "/dataproc/interactive/create", project),
		screen("Dataproc session templates", "/dataproc/session-templates", project),
		screen("create session template", "/dataproc/session-templates/create", project),
		{"session template", tmplPath, settled, ""},
	})

	p.navigate(batchPath)
	p.waitFor(`document.querySelector("#view").textContent.includes("sparkpi-browser")`)
	p.dpAct("Delete")
	p.navigate(tmplPath)
	p.waitFor(`document.querySelector("#view").textContent.includes("connect-browser")`)
	p.dpAct("Delete")
}

// TestBigQuerySparkNotebookThroughThePage (#2156), in the spark shard,
// with BigQuery, Dataform and Serverless for Apache Spark served: Home's
// Create new's Apache Spark notebook opens Google's starter notebook;
// Connect starts its runtime; the first cell's DataprocSparkSession
// (Google's dataproc-spark-connect) creates a Spark Connect session of
// the instance, listed on Interactive sessions, through which the next
// cells run Spark SQL and read a BigQuery table with the Spark BigQuery
// connector; spark.stop() terminates the session.
func TestBigQuerySparkNotebookThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	needService(t, "dataform")
	needService(t, "dataproc-sessions")
	if os.Getenv("CLOUDBURROW_TEST_NOTEBOOK_RUNTIME") == "" || os.Getenv("CLOUDBURROW_TEST_SPARK") == "" {
		t.Skip("CLOUDBURROW_TEST_NOTEBOOK_RUNTIME and CLOUDBURROW_TEST_SPARK are not both set: the notebook runtime image is built on the first Connect, and the session runs in the pinned apache/spark image")
	}
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	ds := fmt.Sprintf("spark_nb_%d", time.Now().UnixNano()%1e9)
	p := openWithin(t, 40*time.Minute)
	press := func(sel, text string) {
		t.Helper()
		p.waitFor(fmt.Sprintf(`[...document.querySelectorAll(%q)].some((b) => b.textContent.trim() === %q && !b.disabled && !b.hidden)`, sel, text))
		p.eval(fmt.Sprintf(`[...document.querySelectorAll(%q)].find((b) => b.textContent.trim() === %q && !b.disabled && !b.hidden).click(); true`, sel, text), nil)
	}
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })
	stmt, _ := json.Marshal(map[string]any{"Path": []string{ds}, "Mode": "read-write",
		"Statement": "CREATE TABLE " + ds + ".t AS SELECT x FROM UNNEST(GENERATE_ARRAY(1, 6)) AS x"})
	if code, body := consoleDo(t, http.MethodPost, "/api/query/bigquery"+q, string(stmt)); code != http.StatusOK {
		t.Fatalf("CREATE TABLE through the console API = %d: %s", code, body)
	}

	p.navigate("/bigquery-studio" + q)
	p.waitFor(`document.querySelector(".bq-home") !== null && document.querySelector(".bq-tree") !== null`)
	p.clickText(".bq-home-action", "Apache Spark notebook")
	p.waitFor(`document.querySelector(".bq-notebook") !== null && [...document.querySelectorAll(".bq-nb-source")].some((t) => t.value.includes("DataprocSparkSession"))`)
	press(".bq-notebook .bq-toolbar button", "Connect")
	p.waitForWithin(`document.querySelector(".bq-nb-runtime").textContent.startsWith("Connected") || document.querySelector(".bq-nb-runtime").className.includes("form-error")`, 25*time.Minute)
	var status string
	p.eval(`document.querySelector(".bq-nb-runtime").textContent`, &status)
	if !strings.HasPrefix(status, "Connected to a runtime") {
		t.Fatalf("Connect: %q", status)
	}
	cell := func(code string, limit time.Duration) string {
		t.Helper()
		press(".bq-notebook .bq-toolbar button", "+ Code")
		p.setField(".bq-nb-cell:last-child .bq-nb-source", code)
		press(".bq-nb-cell:last-child button", "Run cell")
		p.waitForWithin(`(() => { const c = document.querySelector(".bq-nb-cell:last-child");
			return !c.querySelector(".bq-nb-output .spinner") && (/\[\d+\]/.test(c.querySelector(".bq-nb-count").textContent) || c.querySelector(".bq-nb-output .form-error") !== null); })()`, limit)
		var out string
		p.eval(`document.querySelector(".bq-nb-cell:last-child .bq-nb-output").textContent`, &out)
		return out
	}
	// The starter cell, as Google's.
	out := cell("from google.cloud.dataproc_spark_connect import DataprocSparkSession\nfrom google.cloud.dataproc_v1 import Session\n\n"+
		"session = Session()\nspark = DataprocSparkSession.builder.dataprocSessionConfig(session).getOrCreate()\nprint('session', spark.version)", 15*time.Minute)
	if !strings.Contains(out, "session 4.0") {
		t.Fatalf("DataprocSparkSession: %q", out)
	}
	code, body := consoleDo(t, http.MethodGet, "/api/resources/dataproc-sessions?project="+url.QueryEscape(project), "")
	if code != http.StatusOK || !strings.Contains(body, "Active") || !strings.Contains(body, "Spark Connect") {
		t.Errorf("Interactive sessions lists %d: %s", code, body)
	}
	if out := cell(`print(spark.sql("SELECT sum(id) AS s FROM range(10)").collect()[0].s)`, 5*time.Minute); !strings.Contains(out, "45") {
		t.Errorf("Spark SQL on the session: %q", out)
	}
	if out := cell(fmt.Sprintf(`print("rows", spark.read.format("bigquery").load("%s.%s.t").count())`, project, ds), 5*time.Minute); !strings.Contains(out, "rows 6") {
		t.Errorf("reading BigQuery on the session: %q", out)
	}
	for _, fl := range p.keyboardFailures() {
		t.Errorf("Spark notebook: %s", fl)
	}
	for _, fl := range p.contrastFailures() {
		t.Errorf("Spark notebook: %s", fl)
	}
	for _, u := range p.unnamedControls() {
		t.Errorf("Spark notebook: an unnamed %s", u)
	}
	cell("spark.stop()", 3*time.Minute)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		_, body := consoleDo(t, http.MethodGet, "/api/resources/dataproc-sessions?project="+url.QueryEscape(project), "")
		if !strings.Contains(body, `"Active"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("spark.stop() left the session active: %s", body)
			break
		}
		time.Sleep(2 * time.Second)
	}
	press(".bq-notebook .bq-toolbar button", "Disconnect and delete runtime")
	p.waitFor(`document.querySelector(".bq-nb-runtime").textContent === "Not connected."`)
}
