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

// TestCloudRunManageTrafficThroughTheBrowser (#1224): a service's page has
// Manage traffic, whose form is prefilled with the traffic as it is (all to
// the latest revision); a sum other than 100 is refused on the form with
// UpdateService's own message; tagging the latest revision is sent as one
// action, and the page's Traffic section then shows the tag and the URL
// Knative gave it.
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

	p.navigate("/run/" + id + "?project=" + project)
	openForm := func() {
		p.waitFor(`document.querySelector(".modal") === null`)
		p.clickText("#view .page-actions button", "Manage traffic")
		p.waitFor(`document.querySelector(".modal.is-open #f-traffic") !== null`)
	}
	openForm()
	var prefilled string
	p.eval(`document.querySelector(".modal.is-open #f-traffic").value`, &prefilled)
	if prefilled != "LATEST 100" {
		t.Errorf("Manage traffic is prefilled with %q, want LATEST 100", prefilled)
	}

	setTraffic := func(v string) {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #f-traffic"); f.value = %q;
			f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, v), nil)
		p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	}
	setTraffic("LATEST 60")
	p.waitFor(`(() => { const e = document.querySelector(".modal.is-open .form-error"); return e && !e.hidden && e.textContent.includes("sum to 100"); })()`)
	if sent := p.sent(http.MethodPost, actions); len(sent) != 1 {
		t.Errorf("the refused split sent %d actions, want one: %v", len(sent), sent)
	}
	// The refusal is the API's 400, provoked on purpose.
	p.forgive()

	setTraffic("LATEST 100 browser")
	p.waitFor(`document.querySelector(".modal") === null`)
	if sent := p.sent(http.MethodPost, actions); len(sent) != 2 {
		t.Errorf("the refusal and the tag sent %d actions, want two: %v", len(sent), sent)
	}
	// Knative publishes the tag's URL once the route is reconciled.
	tagHost := "browser-" + id
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		if _, d := consoleDo(t, http.MethodGet, "/api/detail/run?project="+project+"&name="+id, ""); strings.Contains(d, tagHost) {
			break
		}
	}
	p.navigate("/run/" + id + "?project=" + project + "&tab=traffic")
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, tagHost))
	code, detail := consoleDo(t, http.MethodGet, "/api/detail/run?project="+project+"&name="+id, "")
	if code != http.StatusOK || !strings.Contains(detail, tagHost) {
		t.Errorf("the service's detail through the console API = %d, without the tag URL %s…: %.400s", code, tagHost, detail)
	}
	openForm()
	p.eval(`document.querySelector(".modal.is-open #f-traffic").value`, &prefilled)
	if prefilled != "LATEST 100 browser" {
		t.Errorf("after tagging, Manage traffic is prefilled with %q, want LATEST 100 browser", prefilled)
	}
}
