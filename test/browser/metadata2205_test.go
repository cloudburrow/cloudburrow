//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"google.golang.org/protobuf/types/known/structpb"
)

// TestVertexAIMetadataThroughThePages (#2205): an artifact, an execution
// that produced it and a context holding both, recorded through the
// official MetadataClient in the default store, show on the Metadata page's
// Artifacts, Executions and Contexts tabs; the artifact's page has its
// properties, its Lineage and its producing execution, each opening its
// page, and its metadata as JSON; the execution's page its output
// artifact; the context's its artifacts and executions. Delete removes the
// artifact once its ID is typed back. The keyboard subtest moves through
// the page's tabs and a page's section tabs and opens a row from the
// keyboard; the WCAG subtest audits every screen.
func TestVertexAIMetadataThroughThePages(t *testing.T) {
	for _, s := range []string{"vertex-metadata-artifacts", "vertex-metadata-executions", "vertex-metadata-contexts"} {
		needService(t, s)
	}
	project := servedProject(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c, err := aiplatform.NewMetadataClient(ctx, localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	store := "projects/" + project + "/locations/us-central1/metadataStores/default"
	stamp := time.Now().UnixNano() % 1e9
	artID, execID, ctxID := fmt.Sprintf("browser-art-%d", stamp), fmt.Sprintf("browser-exe-%d", stamp), fmt.Sprintf("browser-run-%d", stamp)
	md, _ := structpb.NewStruct(map[string]any{"accuracy": 0.875})
	art, err := c.CreateArtifact(ctx, &aiplatformpb.CreateArtifactRequest{Parent: store, ArtifactId: artID, Artifact: &aiplatformpb.Artifact{
		DisplayName: "browser model", SchemaTitle: "system.Model", Uri: "gs://browser-bucket/model", State: aiplatformpb.Artifact_LIVE, Metadata: md}})
	if err != nil {
		t.Fatal(err)
	}
	exe, err := c.CreateExecution(ctx, &aiplatformpb.CreateExecutionRequest{Parent: store, ExecutionId: execID, Execution: &aiplatformpb.Execution{
		DisplayName: "browser train", SchemaTitle: "system.ContainerExecution", State: aiplatformpb.Execution_COMPLETE}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := c.CreateContext(ctx, &aiplatformpb.CreateContextRequest{Parent: store, ContextId: ctxID, Context: &aiplatformpb.Context{
		DisplayName: "browser run", SchemaTitle: "system.PipelineRun"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		if op, err := c.DeleteArtifact(bg, &aiplatformpb.DeleteArtifactRequest{Name: art.GetName()}); err == nil {
			_ = op.Wait(bg)
		}
		if op, err := c.DeleteExecution(bg, &aiplatformpb.DeleteExecutionRequest{Name: exe.GetName()}); err == nil {
			_ = op.Wait(bg)
		}
		if op, err := c.DeleteContext(bg, &aiplatformpb.DeleteContextRequest{Name: run.GetName()}); err == nil {
			_ = op.Wait(bg)
		}
	})
	if _, err := c.AddExecutionEvents(ctx, &aiplatformpb.AddExecutionEventsRequest{Execution: exe.GetName(),
		Events: []*aiplatformpb.Event{{Artifact: art.GetName(), Type: aiplatformpb.Event_OUTPUT}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddContextArtifactsAndExecutions(ctx, &aiplatformpb.AddContextArtifactsAndExecutionsRequest{Context: run.GetName(),
		Artifacts: []string{art.GetName()}, Executions: []string{exe.GetName()}}); err != nil {
		t.Fatal(err)
	}

	q := "?project=" + url.QueryEscape(project) + "&region=us-central1"
	artifactsPath := "/agent-platform/metadata/artifacts" + q
	executionsPath := "/agent-platform/metadata/executions" + q
	contextsPath := "/agent-platform/metadata/contexts" + q
	artPath := "/agent-platform/metadata/artifacts/" + artID + q
	exePath := "/agent-platform/metadata/executions/" + execID + q
	ctxPath := "/agent-platform/metadata/contexts/" + ctxID + q
	inTable := func(text string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, text)
	}
	p := open(t)
	p.navigate(artifactsPath)
	p.waitFor(rowReady(artID) + " && " + inTable("browser model") + " && " + inTable("system.Model"))
	p.clickText("#view .tab-strip a, #view [role=tab]", "Executions")
	p.waitFor(`location.pathname === "/agent-platform/metadata/executions" && ` + rowReady(execID) + " && " + inTable("Complete"))
	p.clickText("#view .tab-strip a, #view [role=tab]", "Contexts")
	p.waitFor(`location.pathname === "/agent-platform/metadata/contexts" && ` + rowReady(ctxID) + " && " + inTable("system.PipelineRun"))
	p.clickText("#view .tab-strip a, #view [role=tab]", "Artifacts")
	p.waitFor(`location.pathname === "/agent-platform/metadata/artifacts" && ` + rowReady(artID))

	// The artifact's page: properties, Lineage, Executions and Metadata.
	p.follow(artID)
	p.waitFor(`location.pathname === "/agent-platform/metadata/artifacts/` + artID + `" && document.querySelector("#view").textContent.includes("gs://browser-bucket/model")`)
	p.waitFor(inTable("browser train"))
	p.clickText(`#view [role="tab"]`, "Executions")
	p.waitFor(inTable("browser train") + " && " + inTable("Output"))
	p.clickText(`#view [role="tab"]`, "Metadata")
	p.waitFor(`document.querySelector("#view").textContent.includes("0.875")`)
	// The producing execution opens its page, whose Output artifacts
	// list the artifact.
	p.clickText(`#view [role="tab"]`, "Executions")
	p.follow(execID)
	p.waitFor(`location.pathname === "/agent-platform/metadata/executions/` + execID + `"`)
	p.clickText(`#view [role="tab"]`, "Output artifacts")
	p.waitFor(inTable("browser model"))
	// The context's page lists both.
	p.navigate(ctxPath)
	p.waitFor(inTable("browser model"))
	p.clickText(`#view [role="tab"]`, "Executions")
	p.waitFor(inTable("browser train"))

	t.Run("keyboard", func(t *testing.T) {
		parent := p.t
		p.t = t
		defer func() { p.t = parent }()
		// Tab reaches the page's Executions tab, Enter opens it.
		p.navigate(artifactsPath)
		p.waitFor(rowReady(artID))
		p.tabTo(`a.closest(".page-tabs") && a.textContent.trim() === "Executions"`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`location.pathname === "/agent-platform/metadata/executions" && ` + rowReady(execID))
		// Tab reaches the execution's row, Enter opens its page.
		p.tabTo(fmt.Sprintf(`a.textContent.trim() === %q`, execID))
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`location.pathname === "/agent-platform/metadata/executions/` + execID + `"`)
		// The arrow keys move along the page's section tabs.
		p.tabTo(`a.getAttribute("role") === "tab" && a.getAttribute("aria-selected") === "true"`)
		var first, next string
		p.eval(`document.activeElement.textContent.trim()`, &first)
		p.run(chromedp.KeyEvent(kb.ArrowRight))
		p.waitFor(`[...document.querySelectorAll("#view [role=tab][aria-selected=true]")].length === 1`)
		p.eval(`document.querySelector("#view [role=tab][aria-selected=true]").textContent.trim()`, &next)
		if first == next {
			t.Errorf("ArrowRight left the selected tab on %q", first)
		}
		p.run(chromedp.KeyEvent(kb.Home))
		p.waitFor(fmt.Sprintf(`document.querySelector("#view [role=tab][aria-selected=true]").textContent.trim() === %q`, first))
	})

	t.Run("WCAG", func(t *testing.T) {
		auditScreens(t, []a11yPage{
			{"metadata artifacts", artifactsPath, rowReady(artID), ""},
			{"metadata executions", executionsPath, rowReady(execID), ""},
			{"metadata contexts", contextsPath, rowReady(ctxID), ""},
			{"metadata artifact", artPath, inTable("browser train"), ""},
			{"metadata execution", exePath, settled, ""},
			{"metadata context", ctxPath, inTable("browser model"), ""},
		})
	})

	// Delete, its ID typed back (Google's docs describe no console delete).
	p = open(t)
	p.navigate(artifactsPath)
	p.waitFor(rowReady(artID))
	p.openRowDelete(artID, "Delete")
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal #confirm-input`), artID), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal.is-open") === null && !` + inTable(artID))
	if _, err := c.GetArtifact(ctx, &aiplatformpb.GetArtifactRequest{Name: art.GetName()}); err == nil {
		t.Fatalf("the artifact %s is still there after Delete", art.GetName())
	}
}
