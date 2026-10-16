//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const envLogging = "CLOUDBURROW_TEST_LOGGING"

// TestLogsExplorerThroughViewAndParentPicker (#1410, #1411). Entries are
// written through the Logging JSON API, and a view on _Default keeping
// ERROR and above is made through the console's API. On Log storage, the
// bucket's view row links to the Logs Explorer reading through the view:
// there the scope is "Through a log view", the pickers name the bucket and
// view, the note gives the bucket's retention and the view's filter, and
// the table shows the error and not the info entry. An error written then
// is tailed onto the table. Back on Log storage, the parent picker offers
// the organization; choosing it puts ?parent= in the address and lists its
// _Default, whose page offers Copy log entries.
func TestLogsExplorerThroughViewAndParentPicker(t *testing.T) {
	needService(t, "logging-buckets")
	logAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envLogging)), "http://")
	if logAddr == "" {
		t.Skipf("%s is not set: the entries are written through the Logging API", envLogging)
	}
	project := uniqueProject(t)
	write := func(sev, text string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"logName": "projects/" + project + "/logs/app", "resource": map[string]string{"type": "global"},
			"entries": []map[string]string{{"severity": sev, "textPayload": text}}})
		if code, out := do(t, http.MethodPost, "http://"+logAddr+"/v2/entries:write", string(b), ""); code != http.StatusOK {
			t.Fatalf("entries:write = %d: %s", code, out)
		}
	}
	write("INFO", "browser-info")
	write("ERROR", "browser-error")
	bucket := "projects/" + project + "/locations/global/buckets/_Default"
	view := bucket + "/views/errors"
	body, _ := json.Marshal(map[string]any{"Path": []string{bucket}, "Action": "create-view",
		"Values": map[string]string{"id": "errors", "filter": "severity>=ERROR"}})
	if code, out := consoleDo(t, http.MethodPost, "/api/actions/logging-buckets?project="+project, string(body)); code != http.StatusOK {
		t.Fatalf("create-view = %d: %s", code, out)
	}

	p := open(t)
	p.navigate("/logs/storage?project=" + project)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === %q)`, bucket))
	p.clickText("#view tbody a", bucket)
	p.waitFor(`document.querySelector("#tab-views") !== null`)
	p.eval(`document.querySelector("#tab-views").click(), true`, nil)
	p.waitFor(`[...document.querySelectorAll("#view a")].some((a) => a.textContent === "errors" && a.href.includes("scope=view"))`)
	p.eval(`[...document.querySelectorAll("#view a")].find((a) => a.textContent === "errors").click(), true`, nil)

	p.waitFor(`location.pathname === "/logs" && document.querySelector("#log-scope") !== null && document.querySelector("#log-scope").value === "view"`)
	p.waitFor(fmt.Sprintf(`document.querySelector("#log-view").value === %q`, view))
	p.waitFor(`(document.querySelector("#log-view-note").textContent || "").includes("30 days")`)
	var note string
	p.eval(`document.querySelector("#log-view-note").textContent`, &note)
	if !strings.Contains(note, "severity>=ERROR") {
		t.Errorf("the view note is %q; want the view's filter", note)
	}
	p.waitFor(`[...document.querySelectorAll("#view tbody td")].some((td) => td.textContent === "browser-error")`)
	var shown bool
	p.eval(`[...document.querySelectorAll("#view tbody td")].some((td) => td.textContent === "browser-info")`, &shown)
	if shown {
		t.Error("the Explorer shows an INFO entry through a severity>=ERROR view")
	}
	time.Sleep(time.Second)
	write("ERROR", "browser-tailed")
	p.waitFor(`[...document.querySelectorAll("#view tbody td")].some((td) => td.textContent === "browser-tailed")`)

	org := "organizations/100000000001"
	p.navigate("/logs/storage?project=" + project)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#scope-parent option")].some((o) => o.value === %q)`, org))
	p.eval(fmt.Sprintf(`(() => { const s = document.querySelector("#scope-parent"); s.value = %q;
		s.dispatchEvent(new Event("change")); return true; })()`, org), nil)
	p.waitFor(fmt.Sprintf(`new URLSearchParams(location.search).get("parent") === %q`, org))
	orgBucket := org + "/locations/global/buckets/_Default"
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === %q)`, orgBucket))
	p.clickText("#view tbody a", orgBucket)
	p.waitFor(fmt.Sprintf(`new URLSearchParams(location.search).get("parent") === %q`, org))
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Copy log entries")`)
}
