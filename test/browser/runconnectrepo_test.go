//go:build browser

package browser

import (
	"net/http"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestCloudRunConnectRepoAndInlineEditorThroughTheBrowser (#1852): the
// Services list has Google's Connect repo: Source repository (Repository
// Provider, Repository), Build Configuration (Branch ^main$, Build Type
// Dockerfile or buildpacks with their fields), then the service, ending in
// Create; with no repository chosen it is refused on the form and nothing
// is sent. Where Cloud Functions runs, Write a function is Google's inline
// editor: the default runtime's sample files, the next runtime's sample on
// a change, and the files kept once one is edited.
func TestCloudRunConnectRepoAndInlineEditorThroughTheBrowser(t *testing.T) {
	needService(t, "run")
	project := uniqueProject(t)
	p := open(t)
	p.navigate("/run?project=" + project)
	p.clickText("#view button", "Connect repo")
	p.waitFor(`document.querySelector(".modal.is-open #f-repository") !== null`)
	var form struct {
		Provider, Branch, BuildType, Submit, Dockerfile string
		Sections, Choices                               []string
	}
	p.eval(`(() => { const m = document.querySelector(".modal.is-open"); return {
		Provider: m.querySelector("#f-provider").value, Branch: m.querySelector("#f-branch").value,
		BuildType: m.querySelector("#f-buildType").value, Dockerfile: m.querySelector("#f-dockerfile").value,
		Submit: m.querySelector('button[type="submit"]').textContent,
		Sections: [...m.querySelectorAll(".form-section-title")].map((n) => n.textContent),
		Choices: [...m.querySelectorAll(".is-radio label")].map((n) => n.textContent) }; })()`, &form)
	if form.Provider != "Secure Source Manager (Developer Connect)" || form.Branch != "^main$" || form.BuildType != "Dockerfile" ||
		form.Dockerfile != "/Dockerfile" || form.Submit != "Create" ||
		strings.Join(form.Sections, "|") != "Source repository|Build Configuration|Configure" ||
		len(form.Choices) != 2 || !strings.Contains(form.Choices[1], "buildpacks") {
		t.Errorf("Connect repo reads %+v", form)
	}
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal.is-open #f-name`), "from-repo"))
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`(() => { const e = document.querySelector(".modal.is-open #e-repository"); return e && !e.hidden; })()`)
	if sent := p.sent(http.MethodPost, "/api/actions/run"); len(sent) != 0 {
		t.Errorf("Connect repo with no repository sent %v", sent)
	}
	p.clickText(".modal.is-open button", "Cancel")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)

	code, body := consoleDo(t, http.MethodGet, "/api/services", "")
	if code != http.StatusOK || !strings.Contains(body, `"functions"`) {
		t.Log("Cloud Functions is not served: Write a function is not offered")
		return
	}
	p.navigate("/run?project=" + project)
	p.clickText("#view button", "Write a function")
	p.waitFor(`document.querySelector(".modal.is-open .sourcefiles-editor") !== null`)
	files := func() string {
		var s string
		p.eval(`[...document.querySelectorAll(".modal.is-open .sourcefiles-file")].map((n) => n.textContent).join("|")`, &s)
		return s
	}
	if got := files(); got != "index.js|package.json" {
		t.Errorf("the editor starts with %q; want Node.js's index.js and package.json", got)
	}
	setRuntime := func(v string) {
		p.eval(`(() => { const s = document.querySelector(".modal.is-open #f-runtime"); s.value = "`+v+`";
			s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	}
	setRuntime("python312")
	if got := files(); got != "main.py|requirements.txt" {
		t.Errorf("after choosing python312 the editor holds %q; want main.py and requirements.txt", got)
	}
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal.is-open .sourcefiles-editor`), "# edited\n"))
	setRuntime("nodejs22")
	if got := files(); got != "main.py|requirements.txt" {
		t.Errorf("after an edit, choosing nodejs22 replaced the files with %q", got)
	}
	var source string
	p.eval(`document.querySelector(".modal.is-open #f-source").value`, &source)
	if !strings.Contains(source, `# edited`) || !strings.Contains(source, `"requirements.txt"`) {
		t.Errorf("the submitted source is %q", source)
	}
}
