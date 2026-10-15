//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// TestCloudRunExecutionTasksInTheBrowser (#1223): an execution's page has a
// Tasks section listing one row per task, read through the Tasks API, each
// with its state and exit code; a task's row opens the task's page, with
// its index, retries, exit code and its own pod's log.
func TestCloudRunExecutionTasksInTheBrowser(t *testing.T) {
	needService(t, "run-jobs")
	p := open(t)
	project := uniqueProject(t)
	id := fmt.Sprintf("browser-tasks-%d", time.Now().UnixNano()%1e6)
	q := "?project=" + project
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/run-jobs"+q+"&name="+id, "") })
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/run-jobs"+q, fmt.Sprintf(
		`{"name":%q,"image":"docker.io/library/busybox:1.36","command":"sh -c","args":"'echo task $CLOUD_RUN_TASK_INDEX'","taskCount":"2","maxRetries":"0"}`, id)); code != http.StatusOK {
		t.Fatalf("create job through the console API = %d: %s", code, body)
	}
	b, _ := json.Marshal(map[string]any{"Path": []string{id}, "Action": "execute"})
	if code, body := consoleDo(t, http.MethodPost, "/api/actions/run-jobs"+q, string(b)); code != http.StatusOK {
		t.Fatalf("execute through the console API = %d: %s", code, body)
	}
	var exec string
	for deadline := time.Now().Add(time.Minute); exec == "" && time.Now().Before(deadline); time.Sleep(time.Second) {
		_, body := consoleDo(t, http.MethodGet, "/api/detail/run-jobs?"+url.Values{"project": {project}, "name": {id}}.Encode(), "")
		var job struct {
			Sections []struct {
				ID      string
				Listing struct{ Items []struct{ Name string } }
			}
		}
		_ = json.Unmarshal([]byte(body), &job)
		for _, s := range job.Sections {
			if s.ID == "executions" && len(s.Listing.Items) > 0 {
				exec = s.Listing.Items[0].Name
			}
		}
	}
	if exec == "" {
		t.Fatal("the job has no execution a minute after Execute")
	}

	task := exec + "-task1"
	p.navigate("/run/jobs/" + id + "/" + exec + q + "&tab=tasks")
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].filter((r) => r.textContent.includes("Succeeded")).length === 2 &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, task))
	p.clickText("#view tbody a", task)
	p.waitFor(fmt.Sprintf(`location.pathname.endsWith(%q)`, "/"+task))
	p.waitFor(`document.querySelector("#view").textContent.includes("Exit code") &&
		document.querySelector("#view").textContent.includes("Succeeded")`)
	var props map[string]string
	p.eval(`Object.fromEntries([...document.querySelectorAll("#view dt")].map((d) => [d.textContent.trim(), d.nextElementSibling ? d.nextElementSibling.textContent.trim() : ""]))`, &props)
	if props["Index"] != "1" || props["Exit code"] != "0" || props["Execution"] != exec {
		t.Errorf("the task's page shows %v; want index 1, exit code 0, execution %s", props, exec)
	}
	p.navigate("/run/jobs/" + id + "/" + exec + "/" + task + q + "&tab=logs")
	p.waitFor(`document.querySelector("#view").textContent.includes("task 1")`)
}
