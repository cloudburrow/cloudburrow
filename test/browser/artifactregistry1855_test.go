//go:build browser

package browser

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/chromedp/chromedp"
	"github.com/google/go-containerregistry/pkg/crane"
)

// TestCloudRunArtifactRegistryEditPageTagsAndDigestTabsThroughThePage
// (#1855), in the run shard.
//   - Edit Repository is Google's page, not a dialog: the repository's
//     page with ?edit=1, headed Edit repository, ending in Update; the
//     description saved there reads back through the official client's
//     GetRepository, and the row menu's Edit Repository opens the same page.
//   - An image's versions list has the Vulnerabilities column; a version
//     row's More actions > Edit tags sets its tags with Save (a new tag
//     pulls); Delete over the checked versions, confirmed by its button
//     alone, deletes the checked version.
//   - A version's page is Google's image digest page: Overview (format,
//     location, repository, virtual size, tags), Vulnerabilities,
//     Dependencies and Attachments (#2111), then Layers and Manifest.
//
// The edit page, the image page and the digest page's tabs pass the WCAG
// 2.1 AA audit.
func TestCloudRunArtifactRegistryEditPageTagsAndDigestTabsThroughThePage(t *testing.T) {
	c := arClient(t)
	ctx := context.Background()
	project := uniqueProject(t)
	id := fmt.Sprintf("browser-edit-%d", time.Now().UnixNano()%1e6)
	name := "projects/" + project + "/locations/us-central1/repositories/" + id
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/artifact-registry?project="+project,
		`{"id":"`+id+`","format":"DOCKER","location":"us-central1","description":"first"}`); code != http.StatusOK {
		t.Fatalf("create repository %s through the console API = %d: %s", id, code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/artifact-registry?project="+project+"&name="+id, "")
	})
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	_, port, _ := net.SplitHostPort(addr)
	tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	t.Cleanup(tr.CloseIdleConnections)
	ref := "us-central1-docker.pkg.localhost:" + port + "/" + project + "/" + id + "/web"
	push := func(tag string, content string) string {
		img, err := crane.Image(map[string][]byte{"app.txt": []byte(content)})
		if err != nil {
			t.Fatal(err)
		}
		if err := crane.Push(img, ref+":"+tag, crane.Insecure, crane.WithTransport(tr)); err != nil {
			t.Fatal(err)
		}
		d, _ := img.Digest()
		return d.String()
	}
	tagged := push("v1", "one")
	other := push("v2", "two")

	p := open(t)
	repoPage := "/artifact-registry/" + id + "?project=" + project

	// Edit Repository, a page ending in Update.
	p.navigate(repoPage)
	p.clickText("#view .page-actions a", "Edit Repository")
	p.waitFor(`new URLSearchParams(location.search).get("edit") === "1" && document.querySelector("#edit-page-form #f-description") !== null`)
	var form struct{ Heading, Submit, Cancel, Description string }
	p.eval(`(() => ({ Heading: document.querySelector("#view h1").textContent,
		Submit: document.querySelector("#edit-page-form button[type=submit]").textContent,
		Cancel: [...document.querySelectorAll("#edit-page-form a.button")].map((a) => a.textContent).join(","),
		Description: document.querySelector("#edit-page-form #f-description").value }))()`, &form)
	if form.Heading != "Edit repository" || form.Submit != "Update" || form.Cancel != "Cancel" || form.Description != "first" {
		t.Errorf("the edit page is %+v; want Google's Edit repository, prefilled, ending in Update", form)
	}
	var dialogs int
	p.eval(`document.querySelectorAll(".modal.is-open").length`, &dialogs)
	if dialogs != 0 {
		t.Errorf("Edit Repository opened %d dialogs; want its page", dialogs)
	}
	p.eval(`(() => { const f = document.querySelector("#edit-page-form #f-description"); f.value = "edited on the page";
		f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`#edit-page-form button[type="submit"]`)))
	p.waitFor(`!new URLSearchParams(location.search).has("edit") && document.querySelector("#edit-page-form") === null &&
		document.querySelector("#view [role=tabpanel]") !== null`)
	if r, err := c.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: name}); err != nil || r.GetDescription() != "edited on the page" {
		t.Errorf("GetRepository after Edit Repository's Update = %v, %v", r, err)
	}
	// The row menu's Edit Repository opens the same page.
	p.navigate("/artifact-registry?project=" + project)
	row := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q))`, id)
	p.waitFor(row + ` !== undefined`)
	p.eval(row+`.querySelector(".overflow-trigger").click()`, nil)
	p.clickText(".overflow-item", "Edit Repository")
	p.waitFor(fmt.Sprintf(`location.pathname === "/artifact-registry/%s" && new URLSearchParams(location.search).get("edit") === "1" &&
		document.querySelector("#edit-page-form") !== null`, id))

	// The image's versions: Vulnerabilities column, Edit tags, Delete.
	imagePage := "/artifact-registry/" + id + "/web?project=" + project
	p.navigate(imagePage)
	p.waitFor(`document.querySelector("#view tbody tr") !== null`)
	var heads string
	p.eval(`[...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim()).filter(Boolean).join(",")`, &heads)
	if !strings.Contains(heads, "Vulnerabilities") {
		t.Errorf("the versions columns are %q; want Google's Vulnerabilities column", heads)
	}
	vrow := func(digest string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q))`, digest[:19])
	}
	p.eval(vrow(tagged)+`.querySelector(".overflow-trigger").click()`, nil)
	p.clickText(".overflow-item", "Edit tags")
	p.waitFor(`document.querySelector(".modal.is-open #f-tags") !== null`)
	var save string
	p.eval(`document.querySelector(".modal.is-open button[type=submit]").textContent`, &save)
	if save != "Save" {
		t.Errorf("Edit tags submits with %q; want Save", save)
	}
	p.eval(`(() => { const f = document.querySelector(".modal.is-open #f-tags"); f.value = "v1, prod";
		f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	if got, err := crane.Digest(ref+":prod", crane.Insecure, crane.WithTransport(tr)); err != nil || got != tagged {
		t.Errorf("prod after Edit tags pulls %q, %v; want %s", got, err, tagged)
	}
	p.navigate(imagePage)
	p.waitFor(vrow(other) + ` !== undefined`)
	p.eval(vrow(other)+`.querySelector("input[type=checkbox]").click()`, nil)
	p.eval(`[...document.querySelectorAll("#view button")].find((b) => b.textContent === "Delete" && !b.closest(".page-actions") && !b.disabled).click()`, nil)
	p.confirmOnlyDialog()
	p.clickText(".modal.is-open .modal-actions button", "Delete")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	p.waitFor(vrow(other) + ` === undefined`)
	if _, err := crane.Digest(ref+"@"+other, crane.Insecure, crane.WithTransport(tr)); err == nil {
		t.Error("the checked version still pulls after Delete")
	}

	// The image digest page's tabs.
	digestPage := "/artifact-registry/" + id + "/web/" + tagged + "?project=" + project
	p.navigate(digestPage)
	p.waitFor(`document.querySelector("#view [role=tabpanel] dl") !== null`)
	var tabs, overview string
	p.eval(`[...document.querySelectorAll("#view [role=tab]")].map((x) => x.textContent.trim()).join(",")`, &tabs)
	p.eval(`document.querySelector("#view [role=tabpanel]").textContent`, &overview)
	if !strings.HasPrefix(tabs, "Overview,Vulnerabilities,Dependencies,Attachments,Layers,Manifest") {
		t.Errorf("the digest page's tabs are %q; want Google's Overview, Vulnerabilities, Dependencies, Attachments first", tabs)
	}
	for _, want := range []string{"Format", "Docker", "Location", "us-central1", "Repository", id, "Virtual size", "Tags", "prod, v1"} {
		if !strings.Contains(overview, want) {
			t.Errorf("the Overview tab %q lacks %q", overview, want)
		}
	}

	auditScreens(t, []a11yPage{
		{"Edit repository page", "/artifact-registry/" + id + "?project=" + project + "&edit=1", settled, ""},
		{"image versions", imagePage, settled, ""},
		{"image digest overview", digestPage, settled, ""},
		{"image digest vulnerabilities", digestPage + "&tab=vulnerabilities", settled, ""},
		{"image digest dependencies", digestPage + "&tab=dependencies", settled, ""},
	})
}
