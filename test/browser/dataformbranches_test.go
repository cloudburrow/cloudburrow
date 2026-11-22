//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestDataformBranchesAndTriggersThroughThePage (#2105): Dataform's
// v1beta1 parts on its pages. Create development workspace with Enable
// branch management; a file committed and pushed; Switch branch creating
// feature, a commit pushed to it by Push; the Branches tab listing the
// workspace's and the remote's branches and Commit history its commits;
// Switch branch back, Delete branch forcing it, Fetch; then a workflow
// configuration created with a table update trigger (All tables updated),
// its Trigger summary and Trigger evaluations tab, and Edit clearing it.
// The console API reads each result back; every screen meets WCAG 2.1 AA
// and is walked by keyboard.
func TestDataformBranchesAndTriggersThroughThePage(t *testing.T) {
	needService(t, "dataform")
	project := servedProject(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	repoID := "branches-" + suffix
	repo := "projects/" + project + "/locations/us-central1/repositories/" + repoID
	type detailDoc struct {
		Summary  []struct{ Label, Value string }
		Sections []struct {
			ID      string
			Listing struct {
				Items []struct{ Name, Status string }
			}
		}
	}
	detail := func(path ...string) (string, detailDoc) {
		t.Helper()
		q := url.Values{"project": {project}, "name": path}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/dataform?"+q.Encode(), "")
		if code != http.StatusOK {
			t.Fatalf("read %v through the console API = %d: %s", path, code, body)
		}
		var d detailDoc
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			t.Fatal(err)
		}
		return body, d
	}
	summary := func(d detailDoc, label string) string {
		for _, s := range d.Summary {
			if s.Label == label {
				return s.Value
			}
		}
		return ""
	}
	items := func(d detailDoc, id string) []string {
		var out []string
		for _, s := range d.Sections {
			if s.ID == id {
				for _, it := range s.Listing.Items {
					out = append(out, it.Name)
				}
			}
		}
		return out
	}
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{repo}, "Action": "delete", "Values": map[string]string{}})
		consoleDo(t, http.MethodPost, "/api/actions/dataform?project="+url.QueryEscape(project), string(b))
	})

	p := open(t)
	// pushed waits for a Push to have reached the remote, before the page
	// is left.
	pushed := func() {
		t.Helper()
		p.waitForWithin(`document.querySelector("#view").textContent.includes("0 ahead, 0 behind")`, 30*time.Second)
	}
	p.navigate("/bigquery-dataform?project=" + url.QueryEscape(project))
	p.clickText("#view button", "Create repository")
	p.submitForm(map[string]string{"repositoryId": repoID, "region": "us-central1"}, "repositoryId")
	p.waitFor(fmt.Sprintf(`location.pathname === "/bigquery-dataform" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, repo))
	p.follow(repo)
	var repoPath string
	p.eval(`location.pathname + location.search`, &repoPath)

	p.pageAction("Create development workspace", map[string]string{"workspaceId": "dev", "branchManagement": "true"}, "")
	p.follow("dev")
	var wsPath string
	p.eval(`location.pathname + location.search`, &wsPath)
	ws := []string{repo, "workspaces", "dev"}
	if _, d := detail(ws...); summary(d, "Branch") != "main" {
		t.Fatalf("the new workspace's branch = %q", summary(d, "Branch"))
	}
	p.pageAction("Create file", map[string]string{"path": "definitions/a.sqlx", "contents": "select 1 as a\n"}, "")
	p.pageAction("Commit", map[string]string{"message": "First"}, "")
	p.clickText(p.findAction("Push to default branch"), "Push to default branch")
	p.waitFor(`document.querySelector(".modal") === null`)
	pushed()

	p.navigate(wsPath)
	p.pageAction("Switch branch", map[string]string{"branch": "feature", "create": "true"}, "")
	if _, d := detail(ws...); summary(d, "Branch") != "feature" {
		t.Fatalf("after Switch branch the workspace's branch = %q", summary(d, "Branch"))
	}
	p.navigate(wsPath)
	p.pageAction("Create file", map[string]string{"path": "definitions/b.sqlx", "contents": "select 2 as b\n"}, "")
	p.pageAction("Commit", map[string]string{"message": "Second"}, "")
	p.clickText(p.findAction("Push to default branch"), "Push to default branch")
	p.waitFor(`document.querySelector(".modal") === null`)
	pushed()
	_, d := detail(ws...)
	if got := strings.Join(items(d, "branches"), ","); got != "feature,main,origin/feature,origin/main" {
		t.Errorf("Branches = %q", got)
	}
	if got := strings.Join(items(d, "history"), ","); got != "Second,First" {
		t.Errorf("Commit history = %q", got)
	}
	p.navigate(wsPath)
	p.clickText(`#view [role="tab"]`, "Branches")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("origin/feature"))`)
	p.clickText(`#view [role="tab"]`, "Commit history")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Second"))`)

	p.navigate(wsPath)
	p.pageAction("Switch branch", map[string]string{"branch": "main"}, "")
	p.navigate(wsPath)
	p.pageAction("Delete branch", map[string]string{"branch": "feature", "force": "true"}, "")
	p.navigate(wsPath)
	p.pageAction("Fetch", nil, "")
	if _, d := detail(ws...); strings.Join(items(d, "branches"), ",") != "main,origin/feature,origin/main" || summary(d, "Branch") != "main" {
		t.Errorf("after Delete branch and Fetch: branch %q, branches %v", summary(d, "Branch"), items(d, "branches"))
	}

	// A workflow configuration with a table update trigger.
	p.navigate(repoPath)
	p.pageAction("Create release configuration", map[string]string{"releaseId": "prod", "gitCommitish": "main"}, "")
	p.navigate(repoPath)
	p.pageAction("Create workflow configuration", map[string]string{"workflowId": "on-update", "releaseConfig": "prod",
		"triggerTables": "sales.orders\n" + project + ".sales.customers", "triggerCondition": "All tables updated", "maxWait": "600"}, "")
	wf := []string{repo, "workflows", "on-update"}
	if _, d := detail(wf...); !strings.Contains(summary(d, "Trigger"), "all of sales.orders, "+project+".sales.customers") {
		t.Fatalf("the workflow configuration's trigger = %q", summary(d, "Trigger"))
	}
	p.navigate(repoPath)
	p.clickText(`#view [role="tab"]`, "Releases & Scheduling")
	p.clickText(`#view [role="tab"]`, "Workflow configurations")
	p.follow("on-update")
	var workflowPath string
	p.eval(`location.pathname + location.search`, &workflowPath)
	p.waitFor(`document.querySelector("#view").textContent.includes("sales.orders")`)
	p.clickText(`#view [role="tab"]`, "Trigger evaluations")
	p.pageAction("Edit", map[string]string{"triggerTables": ""}, "")
	if _, d := detail(wf...); summary(d, "Trigger") != "None" {
		t.Errorf("after Edit cleared the tables the trigger = %q", summary(d, "Trigger"))
	}

	// Switch branch by keyboard alone: Tab to it, Enter opens its form.
	p.navigate(wsPath)
	p.waitFor(settled)
	p.tabTo(`a.textContent.trim() === "Switch branch"`)
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal.is-open #f-branch") !== null`)

	auditScreens(t, []a11yPage{
		{"branch-managed workspace", wsPath, settled, ""},
		{"workflow configuration with a trigger", workflowPath, settled, ""},
	})
}
