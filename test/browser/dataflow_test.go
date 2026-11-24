//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestDataflowThroughThePage (#2215): Dataflow's pages as Google's. A
// three-step Beam pipeline (testdata/dataflowpy/graph.pb, staged as the
// Python SDK stages one) behind a classic template in the instance's Cloud
// Storage is run through Create job from template; the Jobs list shows the
// job, which runs on the local runner to Succeeded; its page draws the Job
// graph, whose steps are reached and chosen from the keyboard to show their
// Step info, and has its Execution details, Job metrics and Job logs; and
// each screen passes the accessibility audit.
func TestDataflowThroughThePage(t *testing.T) {
	needService(t, "dataflow")
	sc, _ := storageSDK(t)
	project := uniqueProject(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	bucket := fmt.Sprintf("dataflow-browser-%d", time.Now().UnixNano()%1e9)
	if err := sc.Bucket(bucket).Create(ctx, project, nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		it := sc.Bucket(bucket).Objects(context.Background(), nil)
		for {
			a, err := it.Next()
			if err != nil {
				break
			}
			_ = sc.Bucket(bucket).Object(a.Name).Delete(context.Background())
		}
		_ = sc.Bucket(bucket).Delete(context.Background())
	})
	pb, err := os.ReadFile(filepath.Join("..", "..", "testdata", "dataflowpy", "graph.pb"))
	if err != nil {
		t.Fatal(err)
	}
	put := func(name string, b []byte) {
		t.Helper()
		w := sc.Bucket(bucket).Object(name).NewWriter(ctx)
		if _, err := w.Write(b); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("write gs://%s/%s: %v", bucket, name, err)
		}
	}
	put("staging/graph.pb", pb)
	put("templates/words", []byte(fmt.Sprintf(`{"type": "JOB_TYPE_BATCH", "environment": {
  "sdkPipelineOptions": {"options": {"pipelineUrl": "gs://%s/staging/graph.pb"}},
  "userAgent": {"name": "Apache Beam Python 3.12 SDK", "version": "2.78.0"}}}`, bucket)))

	p := open(t)
	p.navigateByDrawer("Databases", "/dataflow/jobs", "Dataflow")
	p.navigate("/dataflow/jobs?project=" + project)
	p.clickText("#view button, #view a", "Create job from template")
	p.waitFor(`["f-jobName", "f-region", "f-template", "f-gcsPath", "f-tempLocation"].every((id) => document.getElementById(id) !== null)`)
	p.submitForm(map[string]string{"jobName": "browser-words", "gcsPath": "gs://" + bucket + "/templates/words",
		"tempLocation": "gs://" + bucket + "/tmp"}, "jobName")
	p.waitForWithin(`location.pathname === "/dataflow/jobs" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("browser-words"))`, time.Minute)

	find := `[...document.querySelectorAll("#view tbody a")].find((a) => a.textContent.includes("browser-words") && a.getClientRects().length)`
	p.waitFor(find + ` !== undefined`)
	var href string
	p.eval(find+`.getAttribute("href")`, &href)
	name, err := url.QueryUnescape(strings.TrimPrefix(strings.SplitN(href, "?", 2)[0], "/dataflow/jobs/"))
	if err != nil {
		t.Fatal(err)
	}
	// The job runs to the end on the local runner.
	for deadline := time.Now().Add(6 * time.Minute); ; {
		q := url.Values{"project": {project}, "name": {name}}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/dataflow?"+q.Encode(), "")
		if code == http.StatusOK && strings.Contains(body, `"Done"`) {
			break
		}
		if code == http.StatusOK && strings.Contains(body, `"Failed"`) {
			t.Fatalf("the job failed: %s", body)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job did not finish: %d %s", code, body)
		}
		time.Sleep(3 * time.Second)
	}

	p.navigate(href)
	p.waitFor(`document.querySelectorAll("#view .df-step").length === 3`)
	var steps string
	p.eval(`[...document.querySelectorAll("#view .df-step .df-step-name")].map((s) => s.textContent).join(",")`, &steps)
	if steps != "Create words,Upper,Length" {
		t.Errorf("the Job graph's steps are %q", steps)
	}
	var edges int
	p.waitFor(`document.querySelectorAll("#view .df-edge").length === 2`)
	p.eval(`document.querySelectorAll("#view .df-edge").length`, &edges)
	if edges != 2 {
		t.Errorf("the Job graph has %d lines, want 2", edges)
	}

	// From the keyboard: Tab reaches a step, with a focus indicator, and
	// Enter shows its Step info.
	p.eval(`document.querySelector("#view h1").setAttribute("tabindex", "-1"), document.querySelector("#view h1").focus(), true`, nil)
	reached := false
	for i := 0; i < 80 && !reached; i++ {
		p.run(chromedp.KeyEvent(kb.Tab))
		p.eval(`document.activeElement.matches("#view .df-step") && document.activeElement.textContent.includes("Upper")`, &reached)
	}
	if !reached {
		t.Fatal("Tab never reaches the Upper step")
	}
	var ring bool
	p.eval(`(() => { const s = getComputedStyle(document.activeElement); return (s.outlineStyle !== "none" && parseFloat(s.outlineWidth) >= 1) || s.boxShadow !== "none"; })()`, &ring)
	if !ring {
		t.Error("the focused step draws no focus indicator")
	}
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector("#df-step-info h3") !== null && document.querySelector("#df-step-info h3").textContent === "Upper" &&
		document.querySelector("#df-step-info").textContent.includes("Elements added") &&
		document.activeElement.getAttribute("aria-pressed") === "true"`)

	for _, tab := range []string{"Execution details", "Job metrics", "Job info"} {
		p.clickText("#view [role=tab], #view button", tab)
	}
	p.clickText("#view [role=tab], #view button", "Logs")
	p.waitFor(`document.querySelector("#view").textContent.includes("Worker pool stopped.")`)
	var jobPath string
	p.eval(`location.pathname + location.search`, &jobPath)

	auditScreens(t, []a11yPage{
		screen("Dataflow jobs", "/dataflow/jobs", project),
		screen("create a Dataflow job from a template", "/dataflow/jobs/create", project),
		{"Dataflow job", strings.SplitN(href, "?", 2)[0] + "?project=" + project, settled, ""},
		{"Dataflow job logs", jobPath, settled, ""},
	})
}

// TestDataflowSnapshotsPage (#2215): Dataflow's Snapshots page, offered
// beside Jobs in Dataflow's own navigation as Google's, lists a project's snapshots
// under Google's columns (Snapshot ID, Description, Source job, Status,
// Region, Creation time, Expiration time), its controls are reached from
// the keyboard, and it passes the accessibility audit. (A snapshot's
// rows, Delete and a job's Create snapshot are driven by
// TestConsoleDataflowSnapshots and TestDataflowSnapshots.)
func TestDataflowSnapshotsPage(t *testing.T) {
	needService(t, "dataflow")
	project := uniqueProject(t)
	p := open(t)
	p.navigateByDrawer("Databases", "/dataflow/jobs", "Dataflow")
	// The product's own navigation offers Snapshots beside Jobs.
	p.waitFor(`[...document.querySelectorAll('a[href^="/dataflow/snapshots"]')].some((a) => a.textContent.includes("Snapshots"))`)
	p.navigate("/dataflow/snapshots?project=" + project)
	p.waitFor(`document.querySelector("#view h1") !== null && document.querySelector("#view h1").textContent.includes("Snapshots")`)
	var heads string
	p.waitFor(`document.querySelectorAll("#view thead th").length > 0`)
	p.eval(`[...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim()).join(",")`, &heads)
	for _, c := range []string{"Snapshot ID", "Description", "Source job", "Status", "Region", "Creation time", "Expiration time"} {
		if !strings.Contains(heads, c) {
			t.Errorf("the Snapshots page's columns are %q; want %q", heads, c)
		}
	}
	// From the keyboard: Tab leaves the page title and reaches a control
	// of the page, with a focus indicator.
	p.eval(`document.querySelector("#view h1").setAttribute("tabindex", "-1"), document.querySelector("#view h1").focus(), true`, nil)
	reached := false
	for i := 0; i < 40 && !reached; i++ {
		p.run(chromedp.KeyEvent(kb.Tab))
		p.eval(`document.querySelector("#view").contains(document.activeElement) && document.activeElement.matches("a, button, input, select, [tabindex]")`, &reached)
	}
	if !reached {
		t.Fatal("Tab never reaches a control of the Snapshots page")
	}
	auditScreens(t, []a11yPage{screen("Dataflow snapshots", "/dataflow/snapshots", project)})
}
