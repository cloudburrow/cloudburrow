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

// hpTrial2210 is a trial's training code: it reads --lr from its
// arguments and reports loss = (lr-0.3)^2 as cloudml-hypertune writes a
// report (JSON lines in /tmp/hypertune/output.metrics; the image has no
// pip, so the line is written as the library writes it).
const hpTrial2210 = `import json, os, sys, time
lr = 0.5
for a in sys.argv[1:]:
    if a.startswith("--lr="):
        lr = float(a[len("--lr="):])
os.makedirs("/tmp/hypertune", exist_ok=True)
with open("/tmp/hypertune/output.metrics", "w") as f:
    f.write(json.dumps({"timestamp": time.time(), "trial": os.environ.get("CLOUD_ML_TRIAL_ID", "0"), "loss": str((lr - 0.3) ** 2), "global_step": "1", "checkpoint_path": ""}, sort_keys=True) + "\n")
`

// TestVertexAITrainingHyperparameterTuningThroughThePages (#2210): Agent
// Platform > Model development > Training's Hyperparameter tuning jobs
// tab, and Train new model's Hyperparameter tuning step, as Google's
// "Create a hyperparameter tuning job" (Console tab) walks them. Train new
// model with Enable hyperparameter tuning, a DISCRETE lr over 0.1, 0.3
// and 0.5, the metric loss to Minimize, grid search over 3 trials, makes a
// training pipeline of the hyperparameter tuning task that Succeeds; its
// page links its hyperparameter tuning job, listed on the tab with 3 of 3
// trials, whose page shows its study and, on its Trials tab, each trial's
// lr and loss and its custom job. A job made through the official
// JobClient and kept running is listed Running and its row's Cancel (its
// ID typed back) makes it Cancelled, then Delete removes it. The keyboard
// subtest moves to the tab and opens a job from the keyboard; the WCAG
// subtest audits every screen.
func TestVertexAITrainingHyperparameterTuningThroughThePages(t *testing.T) {
	for _, s := range []string{"vertex-training-pipelines", "vertex-hyperparameter-tuning-jobs", "storage"} {
		needService(t, s)
	}
	img := os.Getenv("CLOUDBURROW_TEST_PIPELINE_IMAGE")
	if img == "" {
		img = "docker.io/library/python:3-alpine"
	}
	if err := exec.Command("docker", "image", "inspect", img).Run(); err != nil {
		t.Skipf("the task image %s is not on this machine's Docker", img)
	}
	project := servedProject(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	jobs, err := aiplatform.NewJobClient(ctx, localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jobs.Close() })
	parent := "projects/" + project + "/locations/us-central1"
	q := "?project=" + url.QueryEscape(project) + "&region=us-central1"
	stamp := time.Now().UnixNano() % 1e6
	bucket := fmt.Sprintf("hptuning-%d", stamp)
	fixture(t, "storage", project, `{"name":"`+bucket+`"}`, bucket)

	type item struct {
		Name, Status string
		Fields       map[string]string
	}
	find := func(service, column, value string) item {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/resources/"+service+q, "")
		if code != http.StatusOK {
			t.Fatalf("list %s = %d: %s", service, code, body)
		}
		var l struct{ Items []item }
		_ = json.Unmarshal([]byte(body), &l)
		for _, it := range l.Items {
			if it.Fields[column] == value || it.Name == value {
				return it
			}
		}
		return item{}
	}
	await := func(service, column, value string, want ...string) item {
		t.Helper()
		var it item
		for deadline := time.Now().Add(20 * time.Minute); ; time.Sleep(2 * time.Second) {
			it = find(service, column, value)
			for _, w := range want {
				if it.Status == w {
					return it
				}
			}
			if it.Status == "Failed" || time.Now().After(deadline) {
				t.Fatalf("%s %q = %+v, want %v", service, value, it, want)
			}
		}
	}
	inTable := func(text string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, text)
	}
	viewHas := func(texts ...string) string {
		var parts []string
		for _, s := range texts {
			parts = append(parts, fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q)`, s))
		}
		return strings.Join(parts, " && ")
	}

	// A job kept running, made through the official client.
	running, err := jobs.CreateHyperparameterTuningJob(ctx, &aiplatformpb.CreateHyperparameterTuningJobRequest{Parent: parent,
		HyperparameterTuningJob: &aiplatformpb.HyperparameterTuningJob{DisplayName: fmt.Sprintf("browser sleeper %d", stamp), MaxTrialCount: 1, ParallelTrialCount: 1,
			StudySpec: &aiplatformpb.StudySpec{Metrics: []*aiplatformpb.StudySpec_MetricSpec{{MetricId: "loss", Goal: aiplatformpb.StudySpec_MetricSpec_MINIMIZE}},
				Parameters: []*aiplatformpb.StudySpec_ParameterSpec{{ParameterId: "lr", ParameterValueSpec: &aiplatformpb.StudySpec_ParameterSpec_DoubleValueSpec_{
					DoubleValueSpec: &aiplatformpb.StudySpec_ParameterSpec_DoubleValueSpec{MinValue: 0.01, MaxValue: 1}}}}},
			TrialJobSpec: &aiplatformpb.CustomJobSpec{WorkerPoolSpecs: []*aiplatformpb.WorkerPoolSpec{{
				MachineSpec: &aiplatformpb.MachineSpec{MachineType: "n1-standard-4"}, ReplicaCount: 1,
				Task: &aiplatformpb.WorkerPoolSpec_ContainerSpec{ContainerSpec: &aiplatformpb.ContainerSpec{ImageUri: img, Command: []string{"sleep", "900"}}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	runningID := running.GetName()[strings.LastIndex(running.GetName(), "/")+1:]
	t.Cleanup(func() {
		bg := context.Background()
		_ = jobs.CancelHyperparameterTuningJob(bg, &aiplatformpb.CancelHyperparameterTuningJobRequest{Name: running.GetName()})
		for i := 0; i < 60; i++ {
			if op, err := jobs.DeleteHyperparameterTuningJob(bg, &aiplatformpb.DeleteHyperparameterTuningJobRequest{Name: running.GetName()}); err == nil {
				_ = op.Wait(bg)
				return
			}
			time.Sleep(time.Second)
		}
	})

	p := open(t)
	trainingPath := "/agent-platform/training" + q
	hpPath := "/agent-platform/training/hyperparameter-tuning-jobs" + q

	// Train new model with hyperparameter tuning.
	p.navigate(trainingPath)
	p.waitFor(settled)
	p.clickText("#view a, #view button", "Create")
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent.includes("Train new model")`)
	var createPath string
	p.eval(`location.pathname + location.search`, &createPath)
	p.setField("#f-hpEnable", "true")
	p.waitFor(`(() => { const f = document.querySelector("#f-hpParams"); return !!f && f.getClientRects().length > 0; })()`)
	tpName := fmt.Sprintf("browser tuned %d", stamp)
	p.submitForm(map[string]string{"displayName": tpName, "image": img, "outputDir": "gs://" + bucket + "/out",
		"args": "python3\n-c\n" + hpTrial2210, "hpParams": "lr DISCRETE 0.1,0.3,0.5", "hpMetric": "loss", "hpGoal": "MINIMIZE",
		"hpMaxTrials": "3", "hpParallel": "3", "hpAlgorithm": "GRID_SEARCH"}, "displayName")
	tp := await("vertex-training-pipelines", "Name", tpName, "Succeeded")
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/vertex-training-pipelines"+q+"&name="+url.QueryEscape(tp.Name), "")
	})
	if tp.Fields["Training type"] != "Hyperparameter tuning" {
		t.Errorf("the pipeline's Training type = %q", tp.Fields["Training type"])
	}
	tpPath := "/agent-platform/training/" + url.PathEscape(tp.Name) + q
	p.navigate(tpPath)
	p.waitFor(viewHas("Hyperparameter tuning job", "Best trial's custom job", "Worker pool 0", img))

	// Its job, on the Hyperparameter tuning jobs tab.
	hp := await("vertex-hyperparameter-tuning-jobs", "Name", tpName+"-hyperparameter-tuning-job", "Succeeded")
	if hp.Fields["Trials"] != "3 of 3" {
		t.Errorf("the job's row = %+v", hp)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/vertex-hyperparameter-tuning-jobs"+q+"&name="+url.QueryEscape(hp.Name), "")
	})
	p.navigate(trainingPath)
	p.waitFor(rowReady(tp.Name))
	p.clickText("#view .tab-strip a, #view [role=tab]", "Hyperparameter tuning jobs")
	p.waitFor(`location.pathname === "/agent-platform/training/hyperparameter-tuning-jobs" && ` + rowReady(hp.Name))
	p.follow(hp.Name)
	jobPath := "/agent-platform/training/hyperparameter-tuning-jobs/" + url.PathEscape(hp.Name) + q
	p.waitFor(viewHas("Metric to optimize", "loss (Minimize)", "Grid search", "Discrete: 0.1, 0.3, 0.5", "Best trial"))
	p.clickText("#view [role=tab]", "Trials")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].length === 3`)
	var rows []string
	p.eval(`[...document.querySelectorAll("#view tbody tr")].map((r) => r.textContent)`, &rows)
	for _, want := range []string{"0.1", "0.3", "0.5"} {
		found := false
		for _, r := range rows {
			found = found || (strings.Contains(r, want) && strings.Contains(r, "Succeeded"))
		}
		if !found {
			t.Errorf("no Succeeded trial row with lr %s: %q", want, rows)
		}
	}
	trialsPath := jobPath + "&tab=trials"

	// The running job: Running, then Cancel from its row.
	if it := await("vertex-hyperparameter-tuning-jobs", "ID", runningID, "Running"); it.Fields["Name"] != running.GetDisplayName() {
		t.Errorf("the running job's row = %+v", it)
	}
	p.navigate(hpPath)
	p.waitFor(rowReady(runningID))
	p.colabRowItem(runningID, "Cancel")
	p.confirmByName(runningID)
	await("vertex-hyperparameter-tuning-jobs", "ID", runningID, "Cancelled")

	t.Run("keyboard", func(t *testing.T) {
		parent := p.t
		p.t = t
		defer func() { p.t = parent }()
		p.navigate(trainingPath)
		p.waitFor(rowReady(tp.Name))
		p.tabTo(`a.closest(".page-tabs") && a.textContent.trim() === "Hyperparameter tuning jobs"`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`location.pathname === "/agent-platform/training/hyperparameter-tuning-jobs" && ` + rowReady(hp.Name))
		p.tabTo(fmt.Sprintf(`a.textContent.trim() === %q`, hp.Name))
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`location.pathname === "/agent-platform/training/hyperparameter-tuning-jobs/` + hp.Name + `"`)
	})

	t.Run("WCAG", func(t *testing.T) {
		auditScreens(t, []a11yPage{
			{"hyperparameter tuning jobs", hpPath, rowReady(hp.Name), ""},
			{"hyperparameter tuning job", jobPath, settled, ""},
			{"trials", trialsPath, `[...document.querySelectorAll("#view tbody tr")].length === 3`, ""},
			{"train new model", createPath, `document.querySelector("#view #f-hpEnable") !== null`, ""},
			{"tuning pipeline", tpPath, settled, ""},
		})
	})

	// Delete the cancelled job, its ID typed back.
	p = open(t)
	p.navigate(hpPath)
	p.waitFor(rowReady(runningID))
	p.openRowDelete(runningID, "Delete")
	p.confirmByName(runningID)
	p.waitFor(`document.querySelector(".modal.is-open") === null && !` + inTable(runningID))
	if _, err := jobs.GetHyperparameterTuningJob(ctx, &aiplatformpb.GetHyperparameterTuningJobRequest{Name: running.GetName()}); err == nil {
		t.Errorf("the job %s is still there after Delete", running.GetName())
	}
}
