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

// trainingServe2210 is a model server for the custom container contract:
// GET is healthy, POST {"instances": [[a, b], ...]} answers each
// instance's sum as its prediction.
const trainingServe2210 = `import http.server, json, os
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(s):
        s.send_response(200); s.end_headers()
    def do_POST(s):
        b = json.loads(s.rfile.read(int(s.headers["Content-Length"])))
        out = json.dumps({"predictions": [sum(i) for i in b["instances"]]}).encode()
        s.send_response(200); s.send_header("Content-Type", "application/json"); s.end_headers(); s.wfile.write(out)
http.server.HTTPServer(("", int(os.environ.get("AIP_HTTP_PORT", "8080"))), H).serve_forever()
`

// TestVertexAITrainingThroughThePages (#2210): Agent Platform > Model
// development > Training and Models > Batch inference, as Google's
// "Create a training pipeline" and "Get batch inferences" walk them.
// Training pipelines' Create opens Train new model (Training method, Model
// details, Training container, Compute and pricing, Prediction container,
// Start training); the pipeline, a custom container printing AIP_MODEL_DIR,
// Succeeds, and its page shows its worker pool and its custom job, listed
// on the Custom jobs tab. A custom job made through the official JobClient
// and kept running is listed Running, its page shows its worker pool, and
// its row's Cancel (its ID typed back) makes it Cancelled. Batch
// inference's Create opens New batch prediction; its model (uploaded
// through the ModelClient, serving the instances' sums) reads a JSON Lines
// file from Cloud Storage, and the job Succeeds with 2 successful
// predictions on its page. Delete removes each. The keyboard subtest moves
// between the Training tabs and opens a row from the keyboard; the WCAG
// subtest audits every screen.
func TestVertexAITrainingThroughThePages(t *testing.T) {
	for _, s := range []string{"vertex-training-pipelines", "vertex-custom-jobs", "vertex-batch-predictions", "storage"} {
		needService(t, s)
	}
	img := os.Getenv("CLOUDBURROW_TEST_PIPELINE_IMAGE")
	if img == "" {
		img = "docker.io/library/python:3-alpine" // fully qualified: the cluster pulls the model's container, a short name being dev.local
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
	models, err := aiplatform.NewModelClient(ctx, localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = models.Close() })
	parent := "projects/" + project + "/locations/us-central1"
	q := "?project=" + url.QueryEscape(project) + "&region=us-central1"
	stamp := time.Now().UnixNano() % 1e6
	bucket := fmt.Sprintf("training-%d", stamp)
	fixture(t, "storage", project, `{"name":"`+bucket+`"}`, bucket)
	uploadObject(t, project, []string{bucket, "in"}, "instances.jsonl", "[1, 2]\n[3, 4]\n")

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

	// A custom job kept running, made through the official client.
	running, err := jobs.CreateCustomJob(ctx, &aiplatformpb.CreateCustomJobRequest{Parent: parent, CustomJob: &aiplatformpb.CustomJob{
		DisplayName: fmt.Sprintf("browser sleeper %d", stamp), JobSpec: &aiplatformpb.CustomJobSpec{WorkerPoolSpecs: []*aiplatformpb.WorkerPoolSpec{{
			MachineSpec: &aiplatformpb.MachineSpec{MachineType: "n1-standard-4"}, ReplicaCount: 1,
			Task: &aiplatformpb.WorkerPoolSpec_ContainerSpec{ContainerSpec: &aiplatformpb.ContainerSpec{ImageUri: img, Command: []string{"sleep"}, Args: []string{"900"}}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	runningID := running.GetName()[strings.LastIndex(running.GetName(), "/")+1:]
	t.Cleanup(func() {
		bg := context.Background()
		_ = jobs.CancelCustomJob(bg, &aiplatformpb.CancelCustomJobRequest{Name: running.GetName()})
		if op, err := jobs.DeleteCustomJob(bg, &aiplatformpb.DeleteCustomJobRequest{Name: running.GetName()}); err == nil {
			_ = op.Wait(bg)
		}
	})

	// A model serving the instances' sums, for the batch prediction.
	up, err := models.UploadModel(ctx, &aiplatformpb.UploadModelRequest{Parent: parent, Model: &aiplatformpb.Model{
		DisplayName: fmt.Sprintf("browser summer %d", stamp), ContainerSpec: &aiplatformpb.ModelContainerSpec{ImageUri: img,
			Command: []string{"python3", "-c", trainingServe2210}, PredictRoute: "/predict", HealthRoute: "/health",
			Ports: []*aiplatformpb.Port{{ContainerPort: 8080}}}}})
	if err != nil {
		t.Fatal(err)
	}
	uploaded, err := up.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	model := uploaded.GetModel()
	t.Cleanup(func() {
		bg := context.Background()
		if op, err := models.DeleteModel(bg, &aiplatformpb.DeleteModelRequest{Name: model}); err == nil {
			_ = op.Wait(bg)
		}
	})

	p := open(t)
	trainingPath := "/agent-platform/training" + q
	customPath := "/agent-platform/training/custom-jobs" + q
	batchPath := "/agent-platform/batch-predictions" + q

	// Train new model, a custom container with no prediction container.
	p.navigate(trainingPath)
	p.waitFor(settled)
	p.clickText("#view a, #view button", "Create")
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent.includes("Train new model")`)
	var createTrainingPath string
	p.eval(`location.pathname + location.search`, &createTrainingPath)
	tpName := fmt.Sprintf("browser model %d", stamp)
	p.submitForm(map[string]string{"displayName": tpName, "image": img, "outputDir": "gs://" + bucket + "/out",
		"args": "python3\n-c\nimport os; print(os.environ['AIP_MODEL_DIR'])"}, "displayName")
	tp := await("vertex-training-pipelines", "Name", tpName, "Succeeded")
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/vertex-training-pipelines"+q+"&name="+url.QueryEscape(tp.Name), "")
	})
	if tp.Fields["Training type"] != "Custom training" {
		t.Errorf("the pipeline's Training type = %q", tp.Fields["Training type"])
	}
	tpPath := "/agent-platform/training/" + url.PathEscape(tp.Name) + q
	p.navigate(trainingPath)
	p.waitFor(rowReady(tp.Name))
	p.follow(tp.Name)
	p.waitFor(viewHas("Worker pool 0", img, "gs://"+bucket+"/out", "Custom job"))
	// Its custom job, on the Custom jobs tab.
	p.navigate(trainingPath)
	p.waitFor(rowReady(tp.Name))
	p.clickText("#view .tab-strip a, #view [role=tab]", "Custom jobs")
	p.waitFor(`location.pathname === "/agent-platform/training/custom-jobs" && ` + rowReady(runningID))

	// The running job's page, then Cancel from its row.
	if it := await("vertex-custom-jobs", "ID", runningID, "Running"); it.Fields["Name"] != running.GetDisplayName() {
		t.Errorf("the running job's row = %+v", it)
	}
	runningPath := "/agent-platform/training/custom-jobs/" + runningID + q
	p.navigate(runningPath)
	p.waitFor(viewHas("Worker pool 0", "sleep", "n1-standard-4", "Running"))
	p.navigate(customPath)
	p.waitFor(rowReady(runningID))
	p.colabRowItem(runningID, "Cancel")
	p.confirmByName(runningID)
	await("vertex-custom-jobs", "ID", runningID, "Cancelled")
	if j, err := jobs.GetCustomJob(ctx, &aiplatformpb.GetCustomJobRequest{Name: running.GetName()}); err != nil || j.GetState() != aiplatformpb.JobState_JOB_STATE_CANCELLED {
		t.Errorf("after Cancel the job is %v, %v", j.GetState(), err)
	}

	// New batch prediction over the JSON Lines file.
	p.navigate(batchPath)
	p.waitFor(settled)
	p.clickText("#view a, #view button", "Create")
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent.includes("New batch prediction")`)
	var createBatchPath string
	p.eval(`location.pathname + location.search`, &createBatchPath)
	bpName := fmt.Sprintf("browser batch %d", stamp)
	p.submitForm(map[string]string{"displayName": bpName, "model": model, "sourcePath": "gs://" + bucket + "/in/instances.jsonl",
		"destination": "gs://" + bucket + "/predictions"}, "displayName")
	bp := await("vertex-batch-predictions", "Name", bpName, "Succeeded")
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/vertex-batch-predictions"+q+"&name="+url.QueryEscape(bp.Name), "")
	})
	bpPath := "/agent-platform/batch-predictions/" + url.PathEscape(bp.Name) + q
	p.navigate(bpPath)
	p.waitFor(viewHas("Successful predictions", "jsonl", "gs://"+bucket+"/in/instances.jsonl", "gs://"+bucket+"/predictions"))
	var stats string
	p.eval(`[...document.querySelectorAll("#view dt")].map((e) => e.textContent.trim() + "=" + ((e.nextElementSibling || {}).textContent || "").trim()).join("|")`, &stats)
	if !strings.Contains(stats, "Successful predictions=2") {
		t.Errorf("the job's page: %s", stats)
	}

	t.Run("keyboard", func(t *testing.T) {
		parent := p.t
		p.t = t
		defer func() { p.t = parent }()
		// Tab reaches the Custom jobs tab, Enter opens it.
		p.navigate(trainingPath)
		p.waitFor(rowReady(tp.Name))
		p.tabTo(`a.closest(".page-tabs") && a.textContent.trim() === "Custom jobs"`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`location.pathname === "/agent-platform/training/custom-jobs" && ` + rowReady(runningID))
		// Tab reaches the job's row, Enter opens its page.
		p.tabTo(fmt.Sprintf(`a.textContent.trim() === %q`, runningID))
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`location.pathname === "/agent-platform/training/custom-jobs/` + runningID + `"`)
		// And back to Training pipelines by its tab.
		p.navigate(customPath)
		p.waitFor(rowReady(runningID))
		p.tabTo(`a.closest(".page-tabs") && a.textContent.trim() === "Training pipelines"`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`location.pathname === "/agent-platform/training" && ` + rowReady(tp.Name))
	})

	t.Run("WCAG", func(t *testing.T) {
		auditScreens(t, []a11yPage{
			{"training pipelines", trainingPath, rowReady(tp.Name), ""},
			{"train new model", createTrainingPath, `document.querySelector("#view #f-displayName") !== null`, ""},
			{"training pipeline", tpPath, settled, ""},
			{"custom jobs", customPath, rowReady(runningID), ""},
			{"custom job", runningPath, settled, ""},
			{"batch predictions", batchPath, rowReady(bp.Name), ""},
			{"new batch prediction", createBatchPath, `document.querySelector("#view #f-displayName") !== null`, ""},
			{"batch prediction", bpPath, settled, ""},
		})
	})

	// Delete each, its ID typed back (Google's docs describe no console
	// delete for these).
	p = open(t)
	for _, d := range []struct{ path, id string }{{customPath, runningID}, {trainingPath, tp.Name}, {batchPath, bp.Name}} {
		p.navigate(d.path)
		p.waitFor(rowReady(d.id))
		p.openRowDelete(d.id, "Delete")
		p.confirmByName(d.id)
		p.waitFor(`document.querySelector(".modal.is-open") === null && !` + inTable(d.id))
	}
	if _, err := jobs.GetCustomJob(ctx, &aiplatformpb.GetCustomJobRequest{Name: running.GetName()}); err == nil {
		t.Errorf("the custom job %s is still there after Delete", running.GetName())
	}
}
