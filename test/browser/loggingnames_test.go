//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestLogsExplorerLogNamePicker (#1554), in the storage shard. The Logs
// Explorer's Log name picker lists the log names Cloud Logging's ListLogs
// lists for the project in scope, as Google's Logs Explorer lists them in its
// Log name filter, and picking one shows that log's entries and not the
// other's, with the choice in the URL as the resource filter.
func TestLogsExplorerLogNamePicker(t *testing.T) {
	needService(t, "logging-buckets")
	logAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envLogging)), "http://")
	if logAddr == "" {
		t.Skipf("%s is not set: the entries are written through the Logging API", envLogging)
	}
	project := uniqueProject(t)
	write := func(log, text string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"logName": "projects/" + project + "/logs/" + log, "resource": map[string]string{"type": "global"},
			"entries": []map[string]string{{"severity": "INFO", "textPayload": text}}})
		if code, out := do(t, http.MethodPost, "http://"+logAddr+"/v2/entries:write", string(b), ""); code != http.StatusOK {
			t.Fatalf("entries:write = %d: %s", code, out)
		}
	}
	write("picker-app", "from-the-app")
	write("picker-worker", "from-the-worker")
	app := "projects/" + project + "/logs/picker-app"
	worker := "projects/" + project + "/logs/picker-worker"

	p := open(t)
	p.navigate("/logs?scope=project&project=" + project)
	p.waitFor(fmt.Sprintf(`(() => { const s = document.querySelector("#log-name"); return s !== null && !s.hidden &&
		[...s.options].some((o) => o.value === %q && o.textContent === "picker-app") &&
		[...s.options].some((o) => o.value === %q); })()`, app, worker))
	p.waitFor(`[...document.querySelectorAll("#view tbody td")].some((td) => td.textContent === "from-the-worker")`)
	p.eval(fmt.Sprintf(`(() => { const s = document.querySelector("#log-name"); s.value = %q;
		s.dispatchEvent(new Event("change")); return true; })()`, app), nil)
	p.waitFor(fmt.Sprintf(`new URLSearchParams(location.search).get("resource") === %q`, app))
	p.waitFor(`[...document.querySelectorAll("#view tbody td")].some((td) => td.textContent === "from-the-app")`)
	p.waitFor(`![...document.querySelectorAll("#view tbody td")].some((td) => td.textContent === "from-the-worker")`)

	// Reloaded, the picker reads its choice back from the URL.
	p.navigate(fmt.Sprintf("/logs?scope=project&project=%s&resource=%s", project, app))
	p.waitFor(fmt.Sprintf(`document.querySelector("#log-name") !== null && document.querySelector("#log-name").value === %q`, app))
}
