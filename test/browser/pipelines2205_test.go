//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// controlFlowSpec2205 is a compiled KFP pipeline (IR) with a nested DAG
// holding an If (a condition DAG) and a ParallelFor of two iterations,
// whose steps write an artifact each.
func controlFlowSpec2205(image string) string {
	return strings.ReplaceAll(`{"pipelineInfo": {"name": "browser-control-flow"}, "schemaVersion": "2.1.0", "sdkVersion": "kfp-2.18.2",
 "root": {"inputDefinitions": {"parameters": {"go": {"parameterType": "STRING", "defaultValue": "yes"}}},
  "dag": {"tasks": {
   "check": {"taskInfo": {"name": "check"}, "componentRef": {"name": "comp-say"}, "inputs": {"parameters": {"msg": {"componentInputParameter": "go"}}}},
   "condition-1": {"taskInfo": {"name": "condition-1"}, "componentRef": {"name": "comp-condition-1"}, "dependentTasks": ["check"],
     "triggerPolicy": {"condition": "inputs.parameter_values['pipelinechannel--check-Output'] == 'yes'"},
     "inputs": {"parameters": {"pipelinechannel--check-Output": {"taskOutputParameter": {"producerTask": "check", "outputParameterKey": "Output"}}}}}
  }}},
 "components": {
  "comp-condition-1": {"inputDefinitions": {"parameters": {"pipelinechannel--check-Output": {"parameterType": "STRING"}}},
   "dag": {"tasks": {
    "for-loop-2": {"taskInfo": {"name": "for-loop-2"}, "componentRef": {"name": "comp-for-loop-2"},
      "parameterIterator": {"items": {"raw": "[\"a\", \"b\"]"}, "itemInput": "pipelinechannel--loop-item-param-1"}, "iteratorPolicy": {"parallelismLimit": 1}}}}},
  "comp-for-loop-2": {"inputDefinitions": {"parameters": {"pipelinechannel--loop-item-param-1": {"parameterType": "STRING"}}},
   "dag": {"tasks": {"write": {"taskInfo": {"name": "write"}, "componentRef": {"name": "comp-write"},
     "inputs": {"parameters": {"text": {"componentInputParameter": "pipelinechannel--loop-item-param-1"}}}}}}},
  "comp-say": {"executorLabel": "exec-say", "inputDefinitions": {"parameters": {"msg": {"parameterType": "STRING"}}},
   "outputDefinitions": {"parameters": {"Output": {"parameterType": "STRING"}}}},
  "comp-write": {"executorLabel": "exec-write", "inputDefinitions": {"parameters": {"text": {"parameterType": "STRING"}}},
   "outputDefinitions": {"artifacts": {"out": {"artifactType": {"schemaTitle": "system.Dataset", "schemaVersion": "0.0.1"}}}}}},
 "deploymentSpec": {"executors": {
  "exec-say": {"container": {"image": "IMAGE", "command": ["sh", "-c", "printf '%s' \"$0\" > \"$1\""],
    "args": ["{{$.inputs.parameters['msg']}}", "{{$.outputs.parameters['Output'].output_file}}"]}},
  "exec-write": {"container": {"image": "IMAGE", "command": ["sh", "-c", "mkdir -p \"$(dirname \"$1\")\" && printf '%s' \"$0\" > \"$1\""],
    "args": ["{{$.inputs.parameters['text']}}", "{{$.outputs.artifacts['out'].path}}"]}}}}}`, "IMAGE", image)
}

// TestVertexAIPipelineRunGraphThroughThePages (#2205): a run of a pipeline
// with a condition and a loop, started from Create pipeline run with
// Caching configuration set, Succeeds; its page opens on the Runtime graph
// with the Pipeline run analysis pane (Run details, Run parameters); the
// condition's DAG expands in place to its loop, the loop to its two
// iterations and each iteration to its step; Expand artifacts draws the
// steps' artifacts; choosing a step shows its Node info with its Input
// parameters, an artifact its Type and URI. The keyboard subtest does it
// all from the keyboard; the WCAG subtest audits the page collapsed,
// expanded and with a node chosen.
func TestVertexAIPipelineRunGraphThroughThePages(t *testing.T) {
	for _, s := range []string{"vertex-pipeline-runs", "storage"} {
		needService(t, s)
	}
	img := os.Getenv("CLOUDBURROW_TEST_PIPELINE_IMAGE")
	if img == "" {
		img = "python:3-alpine"
	}
	if err := exec.Command("docker", "image", "inspect", img).Run(); err != nil {
		t.Skipf("the pipeline task image %s is not on this machine's Docker", img)
	}
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	r := "&region=us-central1"
	stamp := time.Now().UnixNano() % 1e6
	bucket := fmt.Sprintf("pl-graph-%d", stamp)
	fixture(t, "storage", project, `{"name":"`+bucket+`"}`, bucket)
	type item struct {
		Name, Status string
		Fields       map[string]string
	}
	find := func(value string) item {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/resources/vertex-pipeline-runs"+q+r, "")
		if code != http.StatusOK {
			t.Fatalf("list runs = %d: %s", code, body)
		}
		var l struct{ Items []item }
		_ = json.Unmarshal([]byte(body), &l)
		for _, it := range l.Items {
			if it.Fields["Run name"] == value {
				return it
			}
		}
		return item{}
	}
	p := open(t)
	runsPath := "/agent-platform/pipelines" + q
	p.navigate(runsPath)
	p.waitFor(settled)
	p.clickText("#view a, #view button", "Create run")
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent.includes("Create pipeline run")`)
	runName := fmt.Sprintf("graph run %d", stamp)
	p.submitForm(map[string]string{"spec": controlFlowSpec2205(img), "displayName": runName, "output": "gs://" + bucket + "/root",
		"caching": "disabled"}, "displayName")
	var run item
	for deadline := time.Now().Add(5 * time.Minute); ; {
		run = find(runName)
		if run.Status == "Succeeded" || run.Status == "Failed" || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if run.Status != "Succeeded" {
		t.Fatalf("the run = %+v", run)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/vertex-pipeline-runs"+q+r+"&name="+url.QueryEscape(run.Name), "")
	})
	p.navigate(runsPath)
	p.waitFor(rowReady(runName))
	p.follow(run.Name)
	var runPath string
	p.waitFor(`document.querySelector(".pl-pane") !== null`)
	p.eval(`location.pathname + location.search`, &runPath)
	node := func(name string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#view button.pl-node")].find((b) => b.querySelector(".pl-node-name").textContent === %q)`, name)
	}
	expand := func(name string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#view button.pl-expand")].find((b) => b.getAttribute("aria-label") === %q)`, "Expand "+name)
	}
	graphReady := node("condition-1") + ` !== undefined && document.querySelector(".pl-pane h2").textContent === "Pipeline run analysis"`

	p.waitFor(graphReady)
	var pane string
	p.eval(`document.querySelector(".pl-pane").textContent`, &pane)
	for _, want := range []string{"Run details", "Run parameters", "go", "yes", "browser-control-flow"} {
		if !strings.Contains(pane, want) {
			t.Errorf("the Pipeline run analysis pane lacks %q: %s", want, pane)
		}
	}
	var states string
	p.eval(node("check")+`.getAttribute("aria-label") + "|" + `+node("condition-1")+`.getAttribute("aria-label")`, &states)
	if states != "check, Step, Succeeded|condition-1, DAG, Succeeded" {
		t.Errorf("the root steps = %q", states)
	}
	// The condition expands to its loop, the loop to its iterations, an
	// iteration to its step.
	p.clickExpr(expand("condition-1"))
	p.waitFor(node("for-loop-2") + ` !== undefined`)
	p.clickExpr(expand("for-loop-2"))
	p.waitFor(node("for-loop-2-iteration-1") + ` !== undefined`)
	p.clickExpr(expand("for-loop-2-iteration-0"))
	p.waitFor(node("write") + ` !== undefined`)
	p.clickExpr(node("write"))
	p.waitFor(`document.querySelector(".pl-pane h2").textContent === "Node info" && document.querySelector(".pl-pane").textContent.includes("Input parameters")`)
	p.eval(`document.querySelector(".pl-pane").textContent`, &pane)
	if !strings.Contains(pane, "text") || !strings.Contains(pane, "a") || !strings.Contains(pane, "system.ContainerExecution") {
		t.Errorf("write's Node info = %s", pane)
	}
	p.clickExpr(`document.querySelector("#pl-expand-artifacts")`)
	p.waitFor(node("out") + ` !== undefined`)
	p.clickExpr(node("out"))
	p.waitFor(`document.querySelector(".pl-pane").textContent.includes("system.Dataset") && document.querySelector(".pl-pane").textContent.includes("gs://` + bucket + `/root/")`)

	t.Run("keyboard", func(t *testing.T) {
		parent := p.t
		p.t = t
		defer func() { p.t = parent }()
		p.navigate(runPath)
		p.waitFor(graphReady)
		// Tab reaches the graph's one tab stop, a step; the arrow keys move
		// along the steps; Enter opens a step's Node info.
		p.tabTo(`a.classList.contains("pl-node")`)
		var first, second string
		p.eval(`document.activeElement.dataset.node`, &first)
		p.run(chromedp.KeyEvent(kb.ArrowDown))
		p.eval(`document.activeElement.dataset.node`, &second)
		if first == second || second == "" {
			t.Errorf("ArrowDown moved focus from %q to %q", first, second)
		}
		p.run(chromedp.KeyEvent(kb.Home))
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`document.querySelector(".pl-pane h2").textContent === "Node info"`)
		// Tab moves on to the condition's Expand, which Enter opens.
		p.tabTo(`a.classList.contains("pl-expand") && a.getAttribute("aria-label") === "Expand condition-1"`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(node("for-loop-2") + ` !== undefined && document.activeElement.getAttribute("aria-expanded") === "true"`)
		// Back to run summary returns focus to the chosen step.
		p.tabTo(`a.classList.contains("pl-pane-back")`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`document.querySelector(".pl-pane h2").textContent === "Pipeline run analysis" && document.activeElement.classList.contains("pl-node")`)
	})

	t.Run("WCAG", func(t *testing.T) {
		auditScreens(t, []a11yPage{
			{"pipeline run graph", runPath, graphReady, ""},
			{"pipeline run graph expanded with artifacts", runPath, graphReady,
				`(() => { const go = (sel, then) => { const b = sel(); if (b) { b.click(); setTimeout(then, 200); } else setTimeout(() => go(sel, then), 100); };
				const exp = (n) => () => [...document.querySelectorAll("#view button.pl-expand")].find((b) => b.getAttribute("aria-label") === "Expand " + n);
				go(exp("condition-1"), () => go(exp("for-loop-2"), () => go(exp("for-loop-2-iteration-0"), () => {
					document.querySelector("#pl-expand-artifacts").click();
					setTimeout(() => [...document.querySelectorAll("#view button.pl-node")].find((b) => b.textContent.includes("write")).click(), 200); }))); })()`},
		})
	})
}
