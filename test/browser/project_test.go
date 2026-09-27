//go:build browser

package browser

import (
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestPickingAProjectRescopesTheNavigation: the navigation is drawn once,
// with each link's ?project= taken from the address at that moment, so after
// the project picker changed project every nav link still named the old one
// (or none), and each screen a user then opened showed the old project or
// "Choose a project". Found on a live instance on 2026-09-27. Picking a
// project must carry it to the screen a nav link opens.
func TestPickingAProjectRescopesTheNavigation(t *testing.T) {
	p := open(t)
	project := uniqueProject(t)
	p.navigate("/")
	p.run(chromedp.Click("#project-button", chromedp.ByQuery))
	p.waitFor(`[...document.querySelectorAll("#project-list button")].some((b) => b.textContent.includes("` + project + `"))`)
	p.eval(`[...document.querySelectorAll("#project-list button")].find((b) => b.textContent.includes("`+project+`")).click()`, nil)
	p.waitFor(`document.getElementById("project-current").textContent === "` + project + `"`)

	var hrefs []string
	p.eval(`[...document.querySelectorAll("nav a[href^='/']")].map((a) => a.getAttribute("href"))`, &hrefs)
	if len(hrefs) == 0 {
		t.Fatal("the navigation has no links")
	}
	for _, h := range hrefs {
		if !strings.Contains(h, "project="+project) {
			t.Errorf("after picking %s, the nav link %q does not carry it", project, h)
		}
	}

	p.eval(`[...document.querySelectorAll("nav a")].find((a) => a.textContent.includes("Cloud Storage")).click()`, nil)
	p.waitFor(`location.pathname.startsWith("/storage")`)
	var got struct{ Search, Subtitle string }
	p.eval(`({ Search: location.search, Subtitle: (document.querySelector("#view .subtitle") || {}).textContent || "" })`, &got)
	if !strings.Contains(got.Search, "project="+project) || got.Subtitle != "Project "+project {
		t.Errorf("Cloud Storage opened with %q, subtitle %q; want project %s", got.Search, got.Subtitle, project)
	}
}
