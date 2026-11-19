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

// TestLogsExplorerStreamLogsAndStopStream (#1215, #1863): as Google's Logs
// Explorer, a query's results are drawn when it runs and later entries only
// while Stream logs is on; Stop stream holds what arrives, and Stream logs
// shows it, so nothing written in between is skipped.
func TestLogsExplorerStreamLogsAndStopStream(t *testing.T) {
	needService(t, "logging-buckets")
	logAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envLogging)), "http://")
	if logAddr == "" {
		t.Skipf("%s is not set: the entries are written through the Logging API", envLogging)
	}
	project := uniqueProject(t)
	write := func(text string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"logName": "projects/" + project + "/logs/pause-app", "resource": map[string]string{"type": "global"},
			"entries": []map[string]string{{"severity": "INFO", "textPayload": text}}})
		if code, out := do(t, http.MethodPost, "http://"+logAddr+"/v2/entries:write", string(b), ""); code != http.StatusOK {
			t.Fatalf("entries:write = %d: %s", code, out)
		}
	}
	shown := func(text string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#view tbody td")].some((td) => td.textContent === %q)`, text)
	}
	button := func(label string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#view button")].find((b) => b.textContent === %q)`, label)
	}
	status := `((document.querySelector("#view .status") || {}).textContent || "")`

	write("before-the-query")
	p := open(t)
	p.navigate("/logs?scope=project&project=" + project)
	p.waitFor(shown("before-the-query"))
	p.waitFor(status + ` === "query results"`)
	p.waitFor(button("Stream logs") + ` !== undefined`)

	// Not streaming: a new entry is held, not drawn.
	write("held-one")
	write("held-two")
	p.waitFor(status + ` === "stream stopped — 2 new"`)
	var drawn bool
	p.eval(shown("held-one")+` || `+shown("held-two"), &drawn)
	if drawn {
		t.Fatal("an entry written after the query ran was drawn before Stream logs")
	}

	p.eval(button("Stream logs")+`.click(), true`, nil)
	p.waitFor(shown("held-one") + ` && ` + shown("held-two") + ` && ` + shown("before-the-query"))
	p.waitFor(status + ` === "streaming"`)
	write("while-streaming")
	p.waitFor(shown("while-streaming"))

	p.eval(button("Stop stream")+`.click(), true`, nil)
	p.waitFor(button("Stream logs") + ` !== undefined`)
	write("after-stop")
	p.waitFor(status + ` === "stream stopped — 1 new"`)
	p.eval(button("Stream logs")+`.click(), true`, nil)
	p.waitFor(shown("after-stop"))
}
