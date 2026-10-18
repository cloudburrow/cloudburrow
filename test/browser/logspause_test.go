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

// TestLogsExplorerPauseBuffersAndResumeShowsWhatHappened (#1576), in the
// storage shard. The Logs Explorer streams a project's entries; Pause
// stops drawing new ones and counts them as buffered, and Resume draws
// what arrived while paused rather than skipping it, beside the lines
// that were there before.
func TestLogsExplorerPauseBuffersAndResumeShowsWhatHappened(t *testing.T) {
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

	write("before-pause")
	p := open(t)
	p.navigate("/logs?scope=project&project=" + project)
	p.waitFor(shown("before-pause"))
	p.waitFor(status + ` === "streaming"`)

	p.waitFor(button("Pause") + ` !== undefined`)
	p.eval(button("Pause")+`.click(), true`, nil)
	p.waitFor(button("Resume") + ` !== undefined`)
	write("while-paused-one")
	write("while-paused-two")
	p.waitFor(status + ` === "paused — 2 buffered"`)
	var drawn bool
	p.eval(shown("while-paused-one")+` || `+shown("while-paused-two"), &drawn)
	if drawn {
		t.Fatal("a line that arrived while paused was drawn before Resume")
	}

	p.eval(button("Resume")+`.click(), true`, nil)
	p.waitFor(shown("while-paused-one") + ` && ` + shown("while-paused-two") + ` && ` + shown("before-pause"))
	p.waitFor(button("Pause") + ` !== undefined`)
	write("after-resume")
	p.waitFor(shown("after-resume"))
}
