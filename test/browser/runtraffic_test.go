//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestCloudRunManageTrafficThroughTheBrowser (#1224, #1852): a service's
// Revisions tab has Google's Manage traffic dialog, one percent per revision
// and the latest healthy revision, ending in Save, prefilled with the traffic
// as it is (all to the latest revision); a sum other than 100 is refused on
// the form. A revision's page has Manage revision tags: a tag is sent as one
// action, the Revisions tab then shows the tag and the URL Knative gave it,
// and the dialog opens again prefilled with it.
func TestCloudRunManageTrafficThroughTheBrowser(t *testing.T) {
	needService(t, "run")
	project := uniqueProject(t)
	id := fmt.Sprintf("browser-traffic-%d", time.Now().UnixNano()%1e6)
	const image = "ghcr.io/knative/helloworld-go:latest"
	body, _ := json.Marshal(map[string]string{"name": id, "image": image, "env": `{"TARGET":"traffic"}`})
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/run?project="+project+"&name="+id, "")
	})
	code, resp := consoleDeploy(t, http.MethodPost, "/api/resources/run?project="+project, string(body))
	switch {
	case code == http.StatusOK:
	case code == http.StatusBadRequest && strings.Contains(resp, "did not answer in time") && strings.Contains(resp, `"operation"`):
		waitForRunService(t, project, id, image, 5*time.Minute)
	default:
		t.Fatalf("deploy a service through the console API = %d: %s", code, resp)
	}
	p := open(t)
	const actions = "/api/actions/run"

	// Manage traffic is on the Revisions tab, as Google's is (#1852).
	p.navigate("/run/" + id + "?project=" + project + "&tab=revisions")
	p.waitFor(`document.querySelector(".modal") === null`)
	p.clickText("#view .section-actions button", "Manage traffic")
	p.waitFor(`document.querySelector(".modal.is-open #f-traffic-latest") !== null`)
	var dialog struct {
		Latest, Submit string
		Revisions      []string
	}
	p.eval(`(() => ({ Latest: document.querySelector(".modal.is-open #f-traffic-latest").value,
		Submit: document.querySelector('.modal.is-open button[type="submit"]').textContent,
		Revisions: [...document.querySelectorAll('.modal.is-open input[id^="f-traffic-revision-"]')].map((f) => f.id.slice("f-traffic-revision-".length)) }))()`, &dialog)
	if dialog.Latest != "100" || dialog.Submit != "Save" || len(dialog.Revisions) != 1 {
		t.Fatalf("Manage traffic = %+v, want the latest at 100, one revision and Save", dialog)
	}
	revision := dialog.Revisions[0]

	setLatest := func(v string) {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #f-traffic-latest"); f.value = %q;
			f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, v), nil)
		p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	}
	setLatest("60")
	p.waitFor(`(() => { const e = document.querySelector(".modal.is-open .form-error"); return e && !e.hidden && e.textContent.includes("sum to 100"); })()`)
	if sent := p.sent(http.MethodPost, actions); len(sent) != 1 {
		t.Errorf("the refused split sent %d actions, want one: %v", len(sent), sent)
	}
	// The refusal is a 400, provoked on purpose.
	p.forgive()
	setLatest("100")
	p.waitFor(`document.querySelector(".modal") === null`)

	// Manage revision tags, on the revision's page.
	p.navigate("/run/" + id + "/" + revision + "?project=" + project)
	p.pageAction("Manage revision tags", map[string]string{"tags": "browser"}, "")
	if sent := p.sent(http.MethodPost, actions); len(sent) != 3 {
		t.Errorf("the refusal, the save and the tag sent %d actions, want three: %v", len(sent), sent)
	}
	// Knative publishes the tag's URL once the route is reconciled.
	tagHost := "browser-" + id
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		if _, d := consoleDo(t, http.MethodGet, "/api/detail/run?project="+project+"&name="+id, ""); strings.Contains(d, tagHost) {
			break
		}
	}
	p.navigate("/run/" + id + "?project=" + project + "&tab=revisions")
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, tagHost))
	code, detail := consoleDo(t, http.MethodGet, "/api/detail/run?project="+project+"&name="+id, "")
	if code != http.StatusOK || !strings.Contains(detail, tagHost) {
		t.Errorf("the service's detail through the console API = %d, without the tag URL %s…: %.400s", code, tagHost, detail)
	}
	p.navigate("/run/" + id + "/" + revision + "?project=" + project)
	p.waitFor(`document.querySelector(".modal") === null`)
	p.clickText("#view .page-actions button", "Manage revision tags")
	p.waitFor(`document.querySelector(".modal.is-open #f-tags") !== null`)
	var prefilled string
	p.eval(`document.querySelector(".modal.is-open #f-tags").value`, &prefilled)
	if prefilled != "browser" {
		t.Errorf("after tagging, Manage revision tags is prefilled with %q, want browser", prefilled)
	}
}
