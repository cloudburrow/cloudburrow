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
)

// TestDataformThroughThePage (#2105), in a shard whose instance serves
// Dataform beside BigQuery. Reached from BigQuery's menu: Create repository
// (Repository ID, Region); the repository's tabs; Create development
// workspace; in the workspace's editor, Create file (workflow_settings.yaml
// and a SQLX file that refers to a missing table), Compile, its error on
// Compilation errors; the file's page, Edit and Save fixing it; Compile
// again, its table on Compiled graph; Start execution, the execution on
// Workflow Execution Logs and its action Succeeded on its page, the table
// in BigQuery; Commit, Push and Pull; Create release configuration and
// Create workflow configuration with their schedules, and New compilation.
// The console API reads each result back. Every screen meets WCAG 2.1 AA.
func TestDataformThroughThePage(t *testing.T) {
	needService(t, "dataform")
	needService(t, "bigquery")
	project := servedProject(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	repoID := "browser-" + suffix
	dataset := "df_browser_" + suffix
	repo := "projects/" + project + "/locations/us-central1/repositories/" + repoID
	detail := func(path ...string) string {
		t.Helper()
		q := url.Values{"project": {project}, "name": path}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/dataform?"+q.Encode(), "")
		if code != http.StatusOK {
			t.Fatalf("read %v through the console API = %d: %s", path, code, body)
		}
		return body
	}
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{repo}, "Action": "delete", "Values": map[string]string{}})
		consoleDo(t, http.MethodPost, "/api/actions/dataform?project="+url.QueryEscape(project), string(b))
	})

	p := open(t)
	// BigQuery's menu has Dataform in Google's place.
	p.navigate("/bigquery-studio?project=" + url.QueryEscape(project))
	p.waitFor(`[...document.querySelectorAll('a[href^="/bigquery-dataform"]')].some((a) => a.textContent.includes("Dataform"))`)
	p.navigate("/bigquery-dataform?project=" + url.QueryEscape(project))
	p.clickText("#view button", "Create repository")
	p.submitForm(map[string]string{"repositoryId": repoID, "region": "us-central1"}, "repositoryId")
	p.waitFor(fmt.Sprintf(`location.pathname === "/bigquery-dataform" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, repo))
	p.follow(repo)
	var repoPath string
	p.eval(`location.pathname + location.search`, &repoPath)
	p.waitFor(`["Development workspaces", "Releases & Scheduling", "Workflow Execution Logs", "Settings", "Git connection"].every((l) =>
		[...document.querySelectorAll("#view [role=tab]")].some((t) => t.textContent.trim() === l))`)

	p.pageAction("Create development workspace", map[string]string{"workspaceId": "dev"}, "")
	p.follow("dev")
	var wsPath string
	p.eval(`location.pathname + location.search`, &wsPath)
	ws := []string{repo, "workspaces", "dev"}
	settings := fmt.Sprintf("defaultProject: %s\ndefaultLocation: US\ndefaultDataset: %s\ndefaultAssertionDataset: %s_assertions\ndataformCoreVersion: 3.1.6\n",
		project, dataset, dataset)
	p.pageAction("Create file", map[string]string{"path": "workflow_settings.yaml", "contents": settings}, "")
	p.pageAction("Create file", map[string]string{"path": "definitions/first.sqlx",
		"contents": "config { type: \"table\" }\nselect * from ${ref(\"missing_table\")}\n"}, "")
	if got := detail(ws...); !strings.Contains(got, "definitions/first.sqlx") || !strings.Contains(got, `"Added"`) {
		t.Fatalf("the workspace reads back as %s", got)
	}

	compile := func() {
		t.Helper()
		p.clickText(p.findAction("Compile"), "Compile")
		p.waitForWithin(`document.querySelector(".modal") === null && document.querySelector("#view").textContent.includes("Last compiled") &&
			!document.querySelector("#view").textContent.includes("Not compiled")`, 3*time.Minute)
	}
	compile()
	p.navigate(wsPath)
	p.clickText(`#view [role="tab"]`, "Compilation errors")
	p.waitFor(`document.querySelector("#view").textContent.includes("missing_table")`)

	p.clickText(`#view [role="tab"]`, "Files")
	p.follow("definitions/first.sqlx")
	var filePath string
	p.eval(`location.pathname + location.search`, &filePath)
	p.waitFor(`document.querySelector("#view .code-view, #view pre") !== null`)
	p.pageAction("Edit", map[string]string{"contents": "config { type: \"table\" }\nselect 1 as a, 'one' as b\n"}, "")
	file := append(append([]string{}, ws...), "files", "definitions/first.sqlx")
	if got := detail(file...); !strings.Contains(got, "select 1 as a") {
		t.Fatalf("Save did not write the file: %s", got)
	}
	p.navigate(wsPath)
	compile()
	p.navigate(wsPath)
	p.clickText(`#view [role="tab"]`, "Compiled graph")
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q)`, dataset+".first"))

	// Start execution with All actions.
	p.pageAction("Start execution", nil, "")
	var inv string
	deadline := time.Now().Add(3 * time.Minute)
	for {
		var d struct {
			Sections []struct {
				ID      string
				Listing struct {
					Items []struct {
						Name, Status string
					}
				}
			}
		}
		if err := json.Unmarshal([]byte(detail(repo)), &d); err != nil {
			t.Fatal(err)
		}
		state := ""
		for _, s := range d.Sections {
			if s.ID == "logs" && len(s.Listing.Items) > 0 {
				inv, state = s.Listing.Items[0].Name, s.Listing.Items[0].Status
			}
		}
		if state == "Succeeded" {
			break
		}
		if state == "Failed" || time.Now().After(deadline) {
			t.Fatalf("the execution is %q: %s", state, detail(repo, "invocations", inv))
		}
		time.Sleep(time.Second)
	}
	p.navigate(repoPath)
	p.clickText(`#view [role="tab"]`, "Workflow Execution Logs")
	p.follow(inv)
	var invPath string
	p.eval(`location.pathname + location.search`, &invPath)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q) && r.textContent.includes("Succeeded"))`,
		dataset+".first"))
	p.clickText(`#view [role="tab"]`, "Execution log")
	p.waitFor(`document.querySelector("#view").textContent.toLowerCase().includes("create or replace table")`)

	// Commit, Push and Pull.
	p.navigate(wsPath)
	p.pageAction("Commit", map[string]string{"message": "First table"}, "")
	p.clickText(p.findAction("Push to default branch"), "Push to default branch")
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitForWithin(`(() => { const s = [...document.querySelectorAll("#view .summary, #view dl, #view")].map((n) => n.textContent).join(" ");
		return s.includes("0 ahead, 0 behind"); })()`, 30*time.Second)
	p.clickText(p.findAction("Pull from default branch"), "Pull from default branch")
	p.waitFor(`document.querySelector(".modal") === null`)
	if got := detail(ws...); strings.Contains(got, `"Added"`) || !strings.Contains(got, "0 ahead, 0 behind") {
		t.Errorf("after Commit, Push and Pull the workspace reads %s", got)
	}

	// Releases & Scheduling.
	p.navigate(repoPath)
	p.pageAction("Create release configuration", map[string]string{"releaseId": "prod", "gitCommitish": "main",
		"cronSchedule": "0 * * * *", "timeZone": "Europe/Paris"}, "")
	p.navigate(repoPath)
	p.pageAction("Create workflow configuration", map[string]string{"workflowId": "hourly", "releaseConfig": "prod",
		"cronSchedule": "30 * * * *", "timeZone": "Europe/Paris"}, "")
	if got := detail(repo); !strings.Contains(got, "Europe/Paris") || !strings.Contains(got, "30 * * * *") || !strings.Contains(got, "0 * * * *") {
		t.Errorf("Releases & Scheduling reads %s", got)
	}
	p.navigate(repoPath)
	p.clickText(`#view [role="tab"]`, "Releases & Scheduling")
	p.follow("prod")
	var releasePath string
	p.eval(`location.pathname + location.search`, &releasePath)
	p.clickText(p.findAction("New compilation"), "New compilation")
	p.waitForWithin(`document.querySelector(".modal") === null`, 3*time.Minute)
	var rel struct {
		Sections []struct {
			ID      string
			Listing struct{ Items []struct{ Name string } }
		}
	}
	if err := json.Unmarshal([]byte(detail(repo, "releases", "prod")), &rel); err != nil {
		t.Fatal(err)
	}
	manual := 0
	for _, s := range rel.Sections {
		if s.ID == "manual" {
			manual = len(s.Listing.Items)
		}
	}
	if manual != 1 {
		t.Errorf("New compilation made %d manual compilation results, want 1", manual)
	}
	p.navigate(repoPath)
	p.clickText(`#view [role="tab"]`, "Releases & Scheduling")
	p.follow("hourly")
	var workflowPath string
	p.eval(`location.pathname + location.search`, &workflowPath)

	auditScreens(t, []a11yPage{
		screen("Dataform", "/bigquery-dataform", project),
		screen("create repository", "/bigquery-dataform/create", project),
		{"repository", repoPath, settled, ""},
		{"workspace", wsPath, settled, ""},
		{"workspace file", filePath, settled, ""},
		{"workflow execution", invPath, settled, ""},
		{"release configuration", releasePath, settled, ""},
		{"workflow configuration", workflowPath, settled, ""},
	})
}
