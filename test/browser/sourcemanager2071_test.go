//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/iterator"
)

// ssmModalForm fills the open dialog's fields and submits it, failing on a
// refusal.
func ssmModalForm(p *tab, values map[string]string) {
	p.t.Helper()
	p.waitFor(`document.querySelector(".modal.is-open form") !== null`)
	for id, v := range values {
		setField(p, ".modal.is-open", "f-"+id, v)
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null || [...document.querySelectorAll(".modal.is-open .form-error")].some((e) => !e.hidden && e.textContent)`)
	var refused string
	p.eval(`[...document.querySelectorAll(".modal.is-open .form-error")].filter((e) => !e.hidden).map((e) => e.textContent).join(" ")`, &refused)
	if refused != "" {
		p.t.Fatalf("the form was refused: %s", refused)
	}
}

// TestCloudRunSourceManagerRound3ThroughThePage (#2071), in the run shard,
// in the order Google's pages give the features:
//   - Create repository's Initialize repository is choosable and reveals
//     .gitignore and license; with go and mit it makes a repository whose
//     first commit FetchTree reads and git clones (.gitignore, LICENSE,
//     README.md);
//   - after a push and a tag, a file's page highlights its syntax, Blame
//     shows each line's commit, the branch and tag selector opens the
//     file at the tag, and History lists the commit;
//   - the Issues tab's Sort orders the rows, and its Labels menu and
//     Assigned to me filter them, after New label, New milestone and the
//     issue page's Edit labels, Edit milestone and Edit assignees;
//   - Files changed: Split view and Ignore whitespace changes redraw it,
//     the plus icon beside a line opens the comment form, Add single
//     comment makes a code comment ListPullRequestComments reads, Start
//     review keeps one Pending across a reload, and Review sends it;
//   - Add webhook with Enable service account authentication on a
//     repository with a service account: Test delivery carries an ID token.
//
// Each page passes the WCAG 2.1 AA audit.
func TestCloudRunSourceManagerRound3ThroughThePage(t *testing.T) {
	c := ssmClient(t)
	ctx := context.Background()
	project := uniqueProject(t)
	inst := fmt.Sprintf("browser-r3-%d", time.Now().UnixNano()%1e6)
	loc := "projects/" + project + "/locations/us-central1"
	iop, err := c.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: loc, InstanceId: inst})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &ssmpb.DeleteInstanceRequest{Name: loc + "/instances/" + inst, Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	base := "/secure-source-manager/" + inst
	p := open(t)

	// Initialize repository.
	p.navigate(base + "?project=" + project + "&action=create-repo")
	p.waitFor(`document.querySelector("#action-page-form #f-initialize") !== null`)
	var init struct {
		Disabled, Hidden, Shown bool
		Licenses                int
	}
	p.eval(`(() => ({ Disabled: document.querySelector("#action-page-form #f-initialize").disabled,
		Hidden: document.querySelector("#action-page-form #f-gitignore").closest(".form-row").hidden }))()`, &init)
	setField(p, "#action-page-form", "f-initialize", "true")
	p.eval(`!document.querySelector("#action-page-form #f-license").closest(".form-row").hidden`, &init.Shown)
	p.eval(`document.querySelector("#action-page-form #f-license").options.length`, &init.Licenses)
	if init.Disabled || !init.Hidden || !init.Shown || init.Licenses != 460 {
		t.Errorf("Initialize repository = %+v; want choosable, revealing .gitignore and the 459 licenses", init)
	}
	p.navigate(base + "?project=" + project)
	p.waitFor(settled)
	ssmCreateRepositoryPage(p, inst, map[string]string{"id": "app", "description": "Made initialized.", "branch": "main",
		"initialize": "true", "gitignore": "go", "license": "mit"})
	repo := loc + "/repositories/app"
	var files []string
	it := c.FetchTree(ctx, &ssmpb.FetchTreeRequest{Repository: repo})
	for {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("FetchTree: %v", err)
		}
		files = append(files, e.GetPath())
	}
	if strings.Join(files, " ") != ".gitignore LICENSE README.md" {
		t.Fatalf("FetchTree after Initialize repository = %v", files)
	}
	r, err := c.GetRepository(ctx, &ssmpb.GetRepositoryRequest{Name: repo})
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	ssmGit(t, work, "clone", "--quiet", r.GetUris().GetGitHttps(), ".")
	if b, err := os.ReadFile(filepath.Join(work, "LICENSE")); err != nil || !strings.Contains(string(b), "MIT License") {
		t.Fatalf("the clone's LICENSE = %.60q, %v", b, err)
	}

	// The code view.
	if err := os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\n\n// main runs.\nfunc main() {\n\tprintln(\"hi\")\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ssmGit(t, work, "add", "main.go")
	ssmGit(t, work, "commit", "--quiet", "-m", "Add main")
	ssmGit(t, work, "tag", "v1")
	ssmGit(t, work, "push", "--quiet", "origin", "main", "v1")
	filePage := base + "/app/tree/main/main.go?project=" + project
	p.navigate(filePage)
	p.waitFor(settled + ` && document.querySelector("#view .code-view") !== null`)
	var code struct {
		Keywords, Strings, Comments, Lines int
	}
	p.eval(`(() => ({ Keywords: document.querySelectorAll("#view .code-view .tok-kw").length,
		Strings: document.querySelectorAll("#view .code-view .tok-str").length,
		Comments: document.querySelectorAll("#view .code-view .tok-comment").length,
		Lines: document.querySelectorAll("#view .code-view tbody tr").length }))()`, &code)
	if code.Keywords < 2 || code.Strings != 1 || code.Comments != 1 || code.Lines != 6 {
		t.Errorf("main.go's highlighting = %+v", code)
	}
	p.run(chromedp.Click(chromedp.CSS(`#code-blame-toggle`)))
	p.waitFor(`document.querySelector("#view .code-blame") !== null`)
	var blame string
	p.eval(`document.querySelector("#view .code-blame").textContent`, &blame)
	if !strings.Contains(blame, "browser") || !strings.Contains(blame, "Add main") {
		t.Errorf("Blame = %q", blame)
	}
	p.eval(`(() => { const s = document.querySelector("#ref-picker"); s.value = "t:v1"; s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.waitFor(`location.pathname.endsWith("/app/tree/v1/main.go")`)
	p.waitFor(settled)
	p.clickText(`#view [role="tab"]`, "History")
	p.waitFor(`document.querySelector("#view [role=tabpanel]").textContent.includes("Add main")`)

	// Issues: labels, milestones, assignees, Sort and the menus.
	for _, title := range []string{"First", "Second"} {
		op, err := c.CreateIssue(ctx, &ssmpb.CreateIssueRequest{Parent: repo, Issue: &ssmpb.Issue{Title: title}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	repoTab := func(tab string) {
		p.navigate(base + "/app?project=" + project + "&tab=" + tab)
		p.waitFor(settled + fmt.Sprintf(` && new URLSearchParams(location.search).get("tab") === %q`, tab))
	}
	repoTab("labels")
	p.pageAction("New label", map[string]string{"name": "bug", "color": "#d93025", "description": "Something is wrong"}, "")
	repoTab("milestones")
	p.pageAction("New milestone", map[string]string{"title": "v1", "due": "2026-12-31"}, "")
	issuePage := base + "/app/issues/1?project=" + project
	p.navigate(issuePage)
	p.waitFor(settled)
	p.clickText("#view a.property-action", "Edit labels")
	ssmModalForm(p, map[string]string{"label-0": "true"})
	p.navigate(issuePage)
	p.waitFor(settled)
	p.clickText("#view a.property-action", "Edit milestone")
	ssmModalForm(p, map[string]string{"milestone": "v1"})
	p.navigate(issuePage)
	p.waitFor(settled)
	var me string
	p.clickText("#view a.property-action", "Edit assignees")
	p.waitFor(`document.querySelector(".modal.is-open form") !== null`)
	p.eval(`document.querySelector(".modal.is-open .form-help").textContent.match(/You are (\S+)\./)[1]`, &me)
	ssmModalForm(p, map[string]string{"assignees": me})
	p.navigate(issuePage)
	p.waitFor(settled + ` && document.querySelector("#view").textContent.includes("bug")`)
	var details string
	p.eval(`document.querySelector("#view").textContent`, &details)
	for _, want := range []string{"bug", "v1", me} {
		if !strings.Contains(details, want) {
			t.Errorf("the issue's page lacks %q after its gears", want)
		}
	}
	repoTab("issues")
	order := func() string {
		var o string
		p.eval(`[...document.querySelectorAll("#view [role=tabpanel] tbody tr")].map((r) => r.querySelector("a") ? r.querySelector("a").textContent.trim() : "").join(",")`, &o)
		return o
	}
	if o := order(); o != "2,1" {
		t.Errorf("the Issues tab opens %q; want newest first", o)
	}
	p.eval(`(() => { const s = document.querySelector("#issues-menu-sort"); s.value = "Oldest"; s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.waitFor(`[...document.querySelectorAll("#view [role=tabpanel] tbody tr a")].map((a) => a.textContent.trim()).join(",") === "1,2"`)
	p.eval(`(() => { const s = document.querySelector("#issues-menu-issues-labels"); s.value = "bug"; s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.waitFor(`document.querySelectorAll("#view [role=tabpanel] tbody tr").length === 1`)
	p.eval(`(() => { const s = document.querySelector("#issues-menu-issues-labels"); s.value = ""; s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.waitFor(`document.querySelectorAll("#view [role=tabpanel] tbody tr").length === 2`)
	p.run(chromedp.Click(chromedp.CSS(`#issues-menu-mine`)))
	p.waitFor(`document.querySelectorAll("#view [role=tabpanel] tbody tr").length === 1 && document.querySelector("#issues-menu-mine").getAttribute("aria-pressed") === "true"`)

	// Files changed: the diff options and the plus icon.
	ssmGit(t, work, "checkout", "--quiet", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\n\n// main runs, twice.\nfunc main() {\n    println(\"hi\")\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ssmGit(t, work, "commit", "--quiet", "-am", "Twice")
	ssmGit(t, work, "push", "--quiet", "origin", "feature")
	pop, err := c.CreatePullRequest(ctx, &ssmpb.CreatePullRequestRequest{Parent: repo, PullRequest: &ssmpb.PullRequest{Title: "Twice",
		Base: &ssmpb.PullRequest_Branch{Ref: "main"}, Head: &ssmpb.PullRequest_Branch{Ref: "feature"}}})
	if err != nil {
		t.Fatal(err)
	}
	pr, err := pop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	filesPage := base + "/app/pulls/" + lastPart(pr.GetName()) + "?project=" + project + "&tab=files"
	p.navigate(filesPage)
	p.waitFor(settled + ` && document.querySelector("#view .diff-table") !== null`)
	rowsChanged := func() int {
		var n int
		p.eval(`document.querySelectorAll("#view .diff-table tr.is-add, #view .diff-table tr.is-del").length`, &n)
		return n
	}
	if n := rowsChanged(); n != 4 {
		t.Errorf("Files changed draws %d changed lines; want 4", n)
	}
	p.eval(`(() => { const s = document.querySelector("#diff-whitespace"); s.value = "ignore"; s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.waitFor(`new URLSearchParams(location.search).get("whitespace") === "ignore"`)
	if n := rowsChanged(); n != 2 {
		t.Errorf("Ignore whitespace changes leaves %d changed lines; want 2", n)
	}
	p.run(chromedp.Click(chromedp.CSS(`#diff-view-toggle`)))
	p.waitFor(`document.querySelector("#view .diff-table.is-split") !== null && document.querySelector("#diff-view-toggle").textContent === "Unified view"`)
	p.run(chromedp.Click(chromedp.CSS(`#diff-view-toggle`)))
	p.waitFor(`document.querySelector("#view .diff-table.is-split") === null`)
	plus := func(label string) {
		p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`#view button.diff-plus[aria-label="Add a comment on %s of main.go"]`, label))))
		p.waitFor(`document.querySelector("#view .diff-comment-form textarea") !== null`)
	}
	plus("line 3")
	p.eval(`(() => { const t = document.querySelector("#view .diff-comment-form textarea"); t.value = "why twice?"; return true; })()`, nil)
	p.clickText("#view .diff-comment-form button", "Add single comment")
	p.waitFor(`document.querySelector("#view .diff-comment") !== null && document.querySelector("#view .diff-comment").textContent.includes("why twice?")`)
	comments := func() []*ssmpb.PullRequestComment {
		var out []*ssmpb.PullRequestComment
		it := c.ListPullRequestComments(ctx, &ssmpb.ListPullRequestCommentsRequest{Parent: pr.GetName()})
		for {
			cm, err := it.Next()
			if err == iterator.Done {
				return out
			}
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, cm)
		}
	}
	if cms := comments(); len(cms) != 1 || cms[0].GetCode().GetPosition().GetLine() != 3 || cms[0].GetCode().GetBody() != "why twice?" {
		t.Fatalf("ListPullRequestComments after Add single comment = %v", cms)
	}
	plus("old line 3")
	p.eval(`(() => { const t = document.querySelector("#view .diff-comment-form textarea"); t.value = "this was fine"; return true; })()`, nil)
	p.clickText("#view .diff-comment-form button", "Start review")
	p.waitFor(`document.querySelector("#view .diff-pending") !== null`)
	p.navigate(filesPage) // kept across a reload
	p.waitFor(settled + ` && document.querySelector("#view .diff-comment.is-pending") !== null`)
	if cms := comments(); len(cms) != 1 {
		t.Fatalf("a pending comment was sent before Review: %v", cms)
	}
	auditScreens(t, []a11yPage{
		screen("Initialize repository", base+"?action=create-repo", project),
		screen("a highlighted file", base+"/app/tree/main/main.go", project),
		screen("a file's blame", base+"/app/tree/main/main.go?blame=1", project),
		screen("a file's history", base+"/app/tree/main/main.go?tab=history", project),
		screen("the Issues tab's menus", base+"/app?tab=issues", project),
		screen("the Labels tab", base+"/app?tab=labels", project),
		screen("the Milestones tab", base+"/app?tab=milestones", project),
		screen("an issue's labels", base+"/app/issues/1", project),
		screen("Files changed with a pending comment", base+"/app/pulls/"+lastPart(pr.GetName())+"?tab=files", project),
		screen("Files changed, split", base+"/app/pulls/"+lastPart(pr.GetName())+"?tab=files&diff-view=split", project),
	})
	p = open(t)
	p.navigate(filesPage)
	p.waitFor(settled + ` && document.querySelector("#view .diff-comment.is-pending") !== null`)
	p.pageAction("Review", map[string]string{"decision": "COMMENT", "body": "two notes"}, "")
	if cms := comments(); len(cms) != 3 {
		t.Fatalf("ListPullRequestComments after Review = %d comments; want the line comment, the review and its pending comment", len(cms))
	}

	// Enable service account authentication.
	var mu sync.Mutex
	var auth []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			auth = append(auth, r.Header.Get("Authorization"))
			mu.Unlock()
		}
	}))
	t.Cleanup(target.Close)
	rop, err := c.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: loc, RepositoryId: "svc",
		Repository: &ssmpb.Repository{Instance: loc + "/instances/" + inst, ServiceAccount: "builder@" + project + ".iam.gserviceaccount.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	p.navigate(base + "/svc?project=" + project + "&tab=settings&subtab=webhooks")
	p.waitFor(settled + ` && new URLSearchParams(location.search).get("subtab") === "webhooks"`)
	p.pageAction("Add webhook", map[string]string{"id": "sa", "targetUri": target.URL + "/hook", "serviceAccountAuth": "true"}, "")
	p.navigate(base + "/svc/hooks/sa?project=" + project)
	p.waitFor(settled + ` && document.querySelector("#view").textContent.includes("Service account authentication")`)
	p.clickText("#view .page-actions button", "Test delivery")
	deadline := time.Now().Add(20 * time.Second)
	for {
		mu.Lock()
		n := len(auth)
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auth) == 0 || !strings.HasPrefix(auth[0], "Bearer ") || len(auth[0]) < 40 {
		t.Errorf("the delivery's Authorization = %q; want the service account's ID token", auth)
	}
}
