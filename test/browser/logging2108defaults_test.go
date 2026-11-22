//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestLoggingDefaultSinkAndDefaultSettingsThroughTheBrowser (#2108): the
// Log Router lists the _Default sink with Google's filter, its Edit sink
// and Disable sink offered and its Delete sink greyed out; Disable sink,
// chosen by keyboard from the row's menu, disables it in the Logging API;
// Settings' Edit settings takes the default sink configuration (mode,
// filter, exclusion filters), which the JSON API's GetSettings reads back;
// and these pages pass the WCAG 2.1 AA audit.
func TestLoggingDefaultSinkAndDefaultSettingsThroughTheBrowser(t *testing.T) {
	needService(t, "logging-sinks")
	needService(t, "logging-settings")
	logAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envLogging)), "http://")
	if logAddr == "" {
		t.Skipf("%s is not set: the sink and the settings are read through the Logging port", envLogging)
	}
	api := "http://" + logAddr
	project := uniqueProject(t)
	sink := "projects/" + project + "/sinks/_Default"
	row := fmt.Sprintf(`tr[aria-label=%q]`, "Inspect "+sink)

	p := open(t)
	p.navigate("/logs/sinks?project=" + project)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, row))
	var text string
	p.eval(fmt.Sprintf(`document.querySelector(%q).textContent`, row), &text)
	if !strings.Contains(text, `NOT LOG_ID("cloudaudit.googleapis.com/activity")`) || !strings.Contains(text, "/locations/global/buckets/_Default") {
		t.Errorf("the _Default row reads %q", text)
	}
	if got := p.menuItems(sink); !strings.Contains(got, "Delete sink (disabled)") || strings.Contains(got, "Edit sink (disabled)") ||
		strings.Contains(got, "Disable sink (disabled)") {
		t.Errorf("the _Default sink's menu = %q; want Delete sink alone greyed out", got)
	}

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/logs/sinks?project=" + project)
		k.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, row))
		k.eval(fmt.Sprintf(`document.querySelector(%q).focus(), true`, fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+sink)), nil)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`document.querySelector(".overflow-menu:not([hidden])") !== null`)
		reached := false
		for i := 0; i < 12 && !reached; i++ {
			k.run(chromedp.KeyEvent(kb.ArrowDown))
			k.eval(`(document.activeElement && document.activeElement.textContent.trim()) === "Disable sink"`, &reached)
		}
		if !reached {
			t.Fatal("the arrow keys did not reach Disable sink in the row's menu")
		}
		k.run(chromedp.KeyEvent(kb.Enter))
		var disabled bool
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline) && !disabled; time.Sleep(300 * time.Millisecond) {
			_, body := do(t, http.MethodGet, api+"/v2/"+sink, "", "")
			var s struct{ Disabled bool }
			_ = json.Unmarshal([]byte(body), &s)
			disabled = s.Disabled
		}
		if !disabled {
			t.Error("Disable sink, chosen by keyboard, did not disable the _Default sink")
		}
	})

	// Edit settings: the default sink configuration.
	settings := "projects/" + project + "/settings"
	p.navigate("/logs/settings/" + url.PathEscape(settings) + "?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Edit settings")`)
	p.clickText("#view .page-actions button", "Edit settings")
	p.submitForm(map[string]string{"sinkMode": "APPEND", "sinkFilter": "severity >= WARNING", "sinkExclusions": "no-debug: severity = DEBUG"}, "storageLocation")
	var body string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		if _, body = do(t, http.MethodGet, api+"/v2/"+settings, "", ""); strings.Contains(body, "defaultSinkConfig") {
			break
		}
	}
	var st struct {
		DefaultSinkConfig struct {
			Mode, Filter string
			Exclusions   []struct{ Name, Filter string }
		}
	}
	_ = json.Unmarshal([]byte(body), &st)
	if c := st.DefaultSinkConfig; c.Mode != "APPEND" || c.Filter != "severity >= WARNING" || len(c.Exclusions) != 1 || c.Exclusions[0].Name != "no-debug" {
		t.Fatalf("the JSON API reads the settings as %s", body)
	}
	p.navigate("/logs/settings/" + url.PathEscape(settings) + "?project=" + project)
	p.waitFor(settled + ` && document.querySelector("#view").textContent.includes("Default sink configuration") && document.querySelector("#view").textContent.includes("no-debug")`)

	auditScreens(t, []a11yPage{
		{"Log Router with _Default", "/logs/sinks?project=" + project, settled + ` && document.querySelector("#view table") !== null`, ""},
		{"the _Default sink", "/logs/sinks/" + url.PathEscape(sink) + "?project=" + project, settled, ""},
		{"Settings with the default sink configuration", "/logs/settings/" + url.PathEscape(settings) + "?project=" + project, settled, ""},
	})
}
