//go:build browser

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// nasTrial2214 is a NAS trial: it reads --nas_params_str and reports its
// reward (the filters count over 64) as cloudml-hypertune writes it, under
// CLOUD_ML_HP_METRIC_TAG.
const nasTrial2214 = `import json, os, sys, time; ` +
	`p = json.loads(next(a[len("--nas_params_str="):] for a in sys.argv[1:] if a.startswith("--nas_params_str="))); ` +
	`r = int(p["filters"].split("(")[1].rstrip(")")) / 64; ` +
	`os.makedirs("/tmp/hypertune", exist_ok=True); ` +
	`open("/tmp/hypertune/output.metrics", "w").write(json.dumps({"timestamp": time.time(), os.environ["CLOUD_ML_HP_METRIC_TAG"]: str(r), "global_step": "1"}) + "\n")`

// TestVertexAITrainingNASThroughThePages (#2214): Agent Platform > Model
// development > Training's Neural Architecture Search tab. A grid search
// NAS job made through the official JobClient over a 3-way OneOf runs its
// 3 search trials on Docker and Succeeds; the tab lists it with 3 of 3
// search trials, its page shows the algorithm, reward metric and search
// space, a Rewards chart and, on its Trials tab, each trial's parameters
// and reward. A job kept running is Cancelled from its row and Deleted,
// its ID typed back. The keyboard subtest moves to the tab and opens the
// job; the WCAG subtest audits each screen.
func TestVertexAITrainingNASThroughThePages(t *testing.T) {
	for _, s := range []string{"vertex-nas-jobs", "storage"} {
		needService(t, s)
	}
	img := os.Getenv("CLOUDBURROW_TEST_PIPELINE_IMAGE")
	if img == "" {
		img = "docker.io/library/python:3-alpine"
	}
	if err := exec.Command("docker", "image", "inspect", img).Run(); err != nil {
		t.Skipf("the trial image %s is not on this machine's Docker", img)
	}
	project := servedProject(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	jobs, err := aiplatform.NewJobClient(ctx, localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jobs.Close() })
	parent := "projects/" + project + "/locations/us-central1"
	q := "?project=" + url.QueryEscape(project) + "&region=us-central1"
	stamp := time.Now().UnixNano() % 1e6
	bucket := fmt.Sprintf("nas-%d", stamp)
	fixture(t, "storage", project, `{"name":"`+bucket+`"}`, bucket)

	nas := func(name string, cmd []string, trials int32, space string) *aiplatformpb.NasJob {
		return &aiplatformpb.NasJob{DisplayName: name, NasJobSpec: &aiplatformpb.NasJobSpec{SearchSpaceSpec: space,
			NasAlgorithmSpec: &aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_{MultiTrialAlgorithmSpec: &aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec{
				MultiTrialAlgorithm: aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_GRID_SEARCH,
				Metric:              &aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_MetricSpec{MetricId: "top_1_accuracy", Goal: aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_MetricSpec_MAXIMIZE},
				SearchTrialSpec: &aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_SearchTrialSpec{MaxTrialCount: trials, MaxParallelTrialCount: trials,
					SearchTrialJobSpec: &aiplatformpb.CustomJobSpec{BaseOutputDirectory: &aiplatformpb.GcsDestination{OutputUriPrefix: "gs://" + bucket + "/nas"},
						WorkerPoolSpecs: []*aiplatformpb.WorkerPoolSpec{{MachineSpec: &aiplatformpb.MachineSpec{MachineType: "n1-standard-4"}, ReplicaCount: 1,
							Task: &aiplatformpb.WorkerPoolSpec_ContainerSpec{ContainerSpec: &aiplatformpb.ContainerSpec{ImageUri: img, Command: cmd}}}}}}}}}}
	}
	space := `{"filters": {"_type": "hyper.OneOf", "candidates": [16, 32, 64]}}`
	done, err := jobs.CreateNasJob(ctx, &aiplatformpb.CreateNasJobRequest{Parent: parent, NasJob: nas(fmt.Sprintf("browser nas %d", stamp), []string{"python3", "-c", nasTrial2214}, 3, space)})
	if err != nil {
		t.Fatal(err)
	}
	doneID := done.GetName()[strings.LastIndex(done.GetName(), "/")+1:]
	running, err := jobs.CreateNasJob(ctx, &aiplatformpb.CreateNasJobRequest{Parent: parent, NasJob: nas(fmt.Sprintf("browser nas sleeper %d", stamp), []string{"sh", "-c", "sleep 900"}, 1, space)})
	if err != nil {
		t.Fatal(err)
	}
	runningID := running.GetName()[strings.LastIndex(running.GetName(), "/")+1:]
	t.Cleanup(func() {
		bg := context.Background()
		for _, n := range []string{done.GetName(), running.GetName()} {
			_ = jobs.CancelNasJob(bg, &aiplatformpb.CancelNasJobRequest{Name: n})
			for i := 0; i < 60; i++ {
				if op, err := jobs.DeleteNasJob(bg, &aiplatformpb.DeleteNasJobRequest{Name: n}); err == nil {
					_ = op.Wait(bg)
					break
				}
				time.Sleep(time.Second)
			}
		}
	})

	type item struct {
		Name, Status string
		Fields       map[string]string
	}
	await := func(id string, want string) item {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Minute); ; time.Sleep(2 * time.Second) {
			code, body := consoleDo(t, http.MethodGet, "/api/resources/vertex-nas-jobs"+q, "")
			if code != http.StatusOK {
				t.Fatalf("list NAS jobs = %d: %s", code, body)
			}
			var l struct{ Items []item }
			_ = json.Unmarshal([]byte(body), &l)
			var it item
			for _, x := range l.Items {
				if x.Name == id {
					it = x
				}
			}
			if it.Status == want {
				return it
			}
			if it.Status == "Failed" || time.Now().After(deadline) {
				_, detail := consoleDo(t, http.MethodGet, "/api/detail/vertex-nas-jobs"+q+"&name="+url.QueryEscape(id), "")
				if len(detail) > 4000 {
					detail = detail[:4000]
				}
				t.Fatalf("NAS job %s = %+v, want %s; its page: %s", id, it, want, detail)
			}
		}
	}
	viewHas := func(texts ...string) string {
		var parts []string
		for _, s := range texts {
			parts = append(parts, fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q)`, s))
		}
		return strings.Join(parts, " && ")
	}

	it := await(doneID, "Succeeded")
	if it.Fields["Search trials"] != "3 of 3" || it.Fields["Algorithm"] != "Grid search" {
		t.Errorf("the job's row = %+v", it)
	}
	await(runningID, "Running")

	p := open(t)
	trainingPath := "/agent-platform/training" + q
	nasPath := "/agent-platform/training/nas-jobs" + q
	jobPath := "/agent-platform/training/nas-jobs/" + url.PathEscape(doneID) + q
	p.navigate(trainingPath)
	p.waitFor(settled)
	p.clickText("#view .tab-strip a, #view [role=tab], #view .page-tabs a", "Neural Architecture Search")
	p.waitFor(`location.pathname === "/agent-platform/training/nas-jobs" && ` + rowReady(doneID))
	p.follow(doneID)
	p.waitFor(viewHas("Grid search", "top_1_accuracy (Maximize)", "hyper.OneOf", "Search trial Worker pool 0"))
	p.clickText("#view [role=tab]", "Trials")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].length === 3`)
	var rows []string
	p.eval(`[...document.querySelectorAll("#view tbody tr")].map((r) => r.textContent)`, &rows)
	for _, want := range []string{"0.25", "0.5", "1"} {
		found := false
		for _, r := range rows {
			found = found || (strings.Contains(r, want) && strings.Contains(r, "Succeeded") && strings.Contains(r, "filters"))
		}
		if !found {
			t.Errorf("no Succeeded trial with reward %s: %q", want, rows)
		}
	}
	trialsPath := jobPath + "&tab=trials"

	// The running job: Cancel from its row.
	p.navigate(nasPath)
	p.waitFor(rowReady(runningID))
	p.colabRowItem(runningID, "Cancel")
	p.confirmByName(runningID)
	await(runningID, "Cancelled")

	t.Run("keyboard", func(t *testing.T) {
		parent := p.t
		p.t = t
		defer func() { p.t = parent }()
		p.navigate(trainingPath)
		p.waitFor(settled)
		p.tabTo(`a.closest(".page-tabs") && a.textContent.trim() === "Neural Architecture Search"`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`location.pathname === "/agent-platform/training/nas-jobs" && ` + rowReady(doneID))
		p.tabTo(fmt.Sprintf(`a.textContent.trim() === %q`, doneID))
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`location.pathname === "/agent-platform/training/nas-jobs/` + doneID + `"`)
	})

	t.Run("WCAG", func(t *testing.T) {
		auditScreens(t, []a11yPage{
			{"NAS jobs", nasPath, rowReady(doneID), ""},
			{"NAS job", jobPath, settled, ""},
			{"NAS trials", trialsPath, `[...document.querySelectorAll("#view tbody tr")].length === 3`, ""},
		})
	})

	// Delete the cancelled job, its ID typed back.
	p = open(t)
	p.navigate(nasPath)
	p.waitFor(rowReady(runningID))
	p.openRowDelete(runningID, "Delete")
	p.confirmByName(runningID)
	p.waitFor(`document.querySelector(".modal.is-open") === null && ![...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(` + fmt.Sprintf("%q", runningID) + `))`)
	if _, err := jobs.GetNasJob(ctx, &aiplatformpb.GetNasJobRequest{Name: running.GetName()}); err == nil {
		t.Errorf("the job %s is still there after Delete", running.GetName())
	}
}
