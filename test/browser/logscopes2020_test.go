//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestLoggingLogScopesTabAndExplorer (#2020): Log Scopes is a tab of
// Cloud Logging's Settings, as on Google, the menu marking Settings on
// it; Set as default patches the Observability API's _Default scope; the
// Logs Explorer then opens on that log scope and shows the entries of both
// of its projects, Refine scope reading "Log scope"; and the Settings
// tabs, the Log Scopes tab and the Explorer on a log scope pass the WCAG
// 2.1 AA audit.
func TestLoggingLogScopesTabAndExplorer(t *testing.T) {
	needService(t, "logging-log-scopes")
	logAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envLogging)), "http://")
	if logAddr == "" {
		t.Skipf("%s is not set: the entries and the log scope are written through the Logging API", envLogging)
	}
	pa := uniqueProject(t)
	pb := pa + "-b"
	for _, p := range []string{pa, pb} {
		b, _ := json.Marshal(map[string]any{"logName": "projects/" + p + "/logs/app", "resource": map[string]string{"type": "global"},
			"entries": []map[string]any{{"severity": "INFO", "textPayload": "scope2020 from " + p}}})
		if code, out := do(t, http.MethodPost, "http://"+logAddr+"/v2/entries:write", string(b), ""); code != http.StatusOK {
			t.Fatalf("entries:write = %d: %s", code, out)
		}
	}
	scope := "projects/" + pa + "/locations/global/logScopes/both"
	b, _ := json.Marshal(map[string]any{"resourceNames": []string{"projects/" + pa, "projects/" + pb}, "description": "two projects"})
	if code, out := do(t, http.MethodPost, "http://"+logAddr+"/v2/projects/"+pa+"/locations/global/logScopes?logScopeId=both", string(b), ""); code != http.StatusOK {
		t.Fatalf("logScopes.create = %d: %s", code, out)
	}
	t.Cleanup(func() { do(t, http.MethodDelete, "http://"+logAddr+"/v2/"+scope, "", "") })

	p := open(t)
	p.navigate("/logs/settings?project=" + pa)
	tabs := `[...document.querySelectorAll("#view .page-tabs a")].map((a) => a.textContent.trim() + (a.getAttribute("aria-current") === "page" ? "*" : "")).join("|")`
	p.waitFor(`document.querySelector("#view .page-tabs") !== null`)
	var got string
	p.eval(tabs, &got)
	if got != "Settings*|Log Scopes|Trace Scopes|Observability Settings|Observability Buckets" {
		t.Errorf("Settings' tabs = %q", got)
	}
	p.clickText("#view .page-tabs a", "Log Scopes")
	row := fmt.Sprintf(`tr[aria-label=%q]`, "Inspect "+scope)
	p.waitFor(fmt.Sprintf(`location.pathname === "/logs/log-scopes" && document.querySelector(%q) !== null`, row))
	p.eval(tabs, &got)
	if got != "Settings|Log Scopes*|Trace Scopes|Observability Settings|Observability Buckets" {
		t.Errorf("on Log Scopes, the tabs = %q", got)
	}
	var current string
	p.eval(`(document.querySelector("#product-nav a.is-current") || {}).textContent || ""`, &current)
	if current != "Settings" {
		t.Errorf("the menu marks %q on Log Scopes; want Settings", current)
	}
	var menu string
	p.eval(`[...document.querySelectorAll("#product-nav a")].map((a) => a.textContent.trim()).join("|")`, &menu)
	if strings.Contains(menu, "Log scopes") || strings.Contains(menu, "Log Scopes") {
		t.Errorf("the menu still lists Log Scopes as a page: %q", menu)
	}

	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+scope))))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Set as default")
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null && [...document.querySelector(%q).cells].some((c) => c.textContent.trim() === "Default")`, row, row))
	code, out := do(t, http.MethodGet, "http://"+logAddr+"/v1/projects/"+pa+"/locations/global/scopes/_Default", "", "")
	var sc struct{ LogScope string }
	_ = json.Unmarshal([]byte(out), &sc)
	if code != http.StatusOK || sc.LogScope != "//logging.googleapis.com/"+scope {
		t.Fatalf("the Observability _Default scope after Set as default = %d %s", code, out)
	}

	// The Explorer opens on the default log scope: both projects' entries.
	p.navigate("/logs?project=" + pa)
	both := fmt.Sprintf(`[...document.querySelectorAll("#view tr.log-entry td.log-summary")].some((td) => td.textContent.includes(%q)) && `+
		`[...document.querySelectorAll("#view tr.log-entry td.log-summary")].some((td) => td.textContent.includes(%q))`,
		"scope2020 from "+pa, "scope2020 from "+pb)
	p.waitFor(both)
	var refine, picked string
	p.eval(`document.querySelector("#log-scope").selectedOptions[0].textContent`, &refine)
	p.eval(`document.querySelector("#log-scope-name").value`, &picked)
	if refine != "Log scope" || picked != scope {
		t.Errorf("Refine scope = %q, log scope %q; want Log scope, %s", refine, picked, scope)
	}
	var note string
	p.eval(`document.querySelector("#log-scope-note").textContent`, &note)
	if !strings.Contains(note, "projects/"+pb) {
		t.Errorf("the log scope note = %q", note)
	}
	// Refine scope back to _Default: the project's own entries only.
	p.eval(fmt.Sprintf(`(() => { const s = document.querySelector("#log-scope-name"); s.value = %q; s.dispatchEvent(new Event("change")); return true; })()`,
		"projects/"+pa+"/locations/global/logScopes/_Default"), nil)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tr.log-entry td.log-summary")].some((td) => td.textContent.includes(%q)) && `+
		`![...document.querySelectorAll("#view tr.log-entry td.log-summary")].some((td) => td.textContent.includes(%q))`,
		"scope2020 from "+pa, "scope2020 from "+pb))

	auditScreens(t, []a11yPage{
		{"Logging Settings tabs", "/logs/settings?project=" + pa, settled + ` && document.querySelector("#view .page-tabs") !== null`, ""},
		{"Log Scopes tab", "/logs/log-scopes?project=" + pa, settled + ` && document.querySelector("#view table") !== null`, ""},
		{"Logs Explorer on a log scope", "/logs?scope=logScope&logScope=" + scope + "&project=" + pa,
			settled + ` && document.querySelector("#view tr.log-entry") !== null`, ""},
	})
}
