//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	securesourcemanager "cloud.google.com/go/securesourcemanager/apiv1"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ssmClient is the official Secure Source Manager client on the Run port,
// or a skip when the shard does not serve it.
func ssmClient(t *testing.T) *securesourcemanager.Client {
	t.Helper()
	needService(t, "secure-source-manager")
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed: the repository's pages need a pushed commit")
	}
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through Secure Source Manager itself", envRun)
	}
	c, err := securesourcemanager.NewClient(context.Background(), option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// ssmGit runs git in dir as a fixed author.
func ssmGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=browser", "-c", "user.email=browser@example.com"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// ssmPush clones the repository, commits files on branch (made from main
// when it is not main) and pushes it.
func ssmPush(t *testing.T, clone, branch string, files map[string]string) {
	t.Helper()
	work := t.TempDir()
	ssmGit(t, work, "clone", "--quiet", clone, ".")
	if branch == "main" {
		ssmGit(t, work, "checkout", "--quiet", "-B", "main")
	} else {
		ssmGit(t, work, "checkout", "--quiet", "-b", branch)
	}
	for name, body := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ssmGit(t, work, "add", ".")
	ssmGit(t, work, "commit", "--quiet", "-m", "add "+branch)
	ssmGit(t, work, "push", "--quiet", "origin", branch)
}

// TestCloudRunSourceManagerRepositoryPagesThroughThePage (#1856), in the
// run shard, whose Run port serves Secure Source Manager:
//   - Create instance is a page of Instance ID, a Region list, Labels,
//     Private instance drawn disabled with its reason and Encryption, whose
//     customer-managed choice reveals the Cloud KMS key; Create makes the
//     instance GetInstance reads with its labels;
//   - Create repository on the instance's page takes a Service account and
//     draws Initialize repository disabled; GetRepository reads it;
//   - after a push, the Code tab lists the pushed files and a file's page
//     its content; Branches lists main;
//   - New issue, Comment and Close issue change what GetIssue and
//     ListIssueComments read;
//   - New pull request from a pushed branch into main, its Files changed,
//     and Merge pull request, after which GetPullRequest reads MERGED;
//   - Settings' Branch rules tab's Add branch rule makes the rule
//     GetBranchRule reads;
//   - a pull request's Comment, Add line comment, Resolve conversation and
//     Review, and Settings' Webhooks tab's Add webhook and a webhook's
//     Test delivery (#1927).
func TestCloudRunSourceManagerRepositoryPagesThroughThePage(t *testing.T) {
	c := ssmClient(t)
	ctx := context.Background()
	project := uniqueProject(t)
	inst := fmt.Sprintf("browser-code-%d", time.Now().UnixNano()%1e6)
	loc := "projects/" + project + "/locations/us-east1"
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &ssmpb.DeleteInstanceRequest{Name: loc + "/instances/" + inst, Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})

	p := open(t)
	p.navigate("/secure-source-manager?project=" + project)
	p.clickText("#view button", "Create instance")
	p.waitFor(`location.pathname === "/secure-source-manager/create" && document.querySelector("#view #f-name") !== null`)
	hidden := func(id string) bool {
		var h bool
		p.eval(fmt.Sprintf(`document.querySelector("#view #%s").closest(".form-row").hidden`, id), &h)
		return h
	}
	var form struct {
		Region  string
		Private bool
	}
	p.eval(`(() => ({ Region: document.querySelector("#view #f-region").tagName, Private: document.querySelector("#view #f-private").disabled }))()`, &form)
	if form.Region != "SELECT" || !form.Private {
		t.Errorf("Create instance draws %+v; want a Region list and Private instance disabled", form)
	}
	if !hidden("f-kmsKey") {
		t.Error("Create instance shows the Cloud KMS key before Customer-managed encryption key is chosen")
	}
	clickRadio(p, "encryption", 1)
	if hidden("f-kmsKey") {
		t.Error("Customer-managed encryption key does not reveal the Cloud KMS key")
	}
	clickRadio(p, "encryption", 0)
	setField(p, "#view", "f-name", inst)
	setField(p, "#view", "f-region", "us-east1")
	setField(p, "#view", "f-labels", "team=web")
	p.run(chromedp.Click(chromedp.CSS(`#view button[type="submit"]`)))
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, inst))
	in, err := c.GetInstance(ctx, &ssmpb.GetInstanceRequest{Name: loc + "/instances/" + inst})
	if err != nil || in.GetLabels()["team"] != "web" || in.GetKmsKey() != "" {
		t.Fatalf("GetInstance after Create instance = %v, %v", in, err)
	}
	var heads string
	p.eval(`[...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim()).filter(Boolean).join(",")`, &heads)
	if !strings.Contains(heads, "Instance ID,Region,Instance URL,Encryption,Created") {
		t.Errorf("the Instances columns are %q", heads)
	}

	p.navigate("/secure-source-manager/" + inst + "?project=" + project)
	p.clickText("#view .section-actions button, #view .page-actions button", "Create repository")
	p.waitFor(`document.querySelector(".modal.is-open #f-initialize") !== null`)
	var initOff bool
	p.eval(`document.querySelector(".modal.is-open #f-initialize").disabled`, &initOff)
	if !initOff {
		t.Error("Create repository's Initialize repository is choosable")
	}
	p.clickText(".modal.is-open .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)
	sa := "builder@" + project + ".iam.gserviceaccount.com"
	p.pageAction("Create repository", map[string]string{"id": "app", "description": "code", "serviceAccount": sa}, "")
	repo := loc + "/repositories/app"
	r, err := c.GetRepository(ctx, &ssmpb.GetRepositoryRequest{Name: repo})
	if err != nil || r.GetServiceAccount() != sa {
		t.Fatalf("GetRepository after Create repository = %v, %v", r, err)
	}
	ssmPush(t, r.GetUris().GetGitHttps(), "main", map[string]string{"README.md": "# app\n", "src/main.go": "package main\n"})

	repoPage := func(tab string) {
		p.navigate("/secure-source-manager/" + inst + "/app?project=" + project + "&tab=" + tab)
		p.waitFor(fmt.Sprintf(`new URLSearchParams(location.search).get("tab") === %q && document.querySelector("#view [role=tabpanel]") !== null`, tab))
	}
	repoPage("code")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].map((r) => r.textContent).join("|").includes("README.md")`)
	p.clickText("#view tbody a", "README.md")
	p.waitFor(`location.pathname.endsWith("/app/tree/main/README.md") && document.querySelector("#view").textContent.includes("# app")`)
	repoPage("branches")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("main") && r.textContent.includes("add main"))`)

	repoPage("issues")
	p.pageAction("New issue", map[string]string{"title": "It breaks", "body": "on Mondays"}, "")
	issues := c.ListIssues(ctx, &ssmpb.ListIssuesRequest{Parent: repo})
	is, err := issues.Next()
	if err != nil || is.GetTitle() != "It breaks" || is.GetBody() != "on Mondays" {
		t.Fatalf("ListIssues after New issue = %v, %v", is, err)
	}
	repoPage("issues")
	p.clickText("#view tbody a", lastPart(is.GetName()))
	p.waitFor(`location.pathname.includes("/app/issues/") && document.querySelector("#view").textContent.includes("on Mondays")`)
	p.pageAction("Comment", map[string]string{"body": "seen it"}, "")
	cm, err := c.ListIssueComments(ctx, &ssmpb.ListIssueCommentsRequest{Parent: is.GetName()}).Next()
	if err != nil || cm.GetBody() != "seen it" {
		t.Errorf("ListIssueComments after Comment = %v, %v", cm, err)
	}
	p.clickText("#view .page-actions button", "Close issue")
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent.trim() === "Reopen issue")`)
	if got, err := c.GetIssue(ctx, &ssmpb.GetIssueRequest{Name: is.GetName()}); err != nil || got.GetState() != ssmpb.Issue_CLOSED {
		t.Errorf("GetIssue after Close issue = %v, %v", got, err)
	}

	ssmPush(t, r.GetUris().GetGitHttps(), "feature", map[string]string{"NOTES.md": "notes\n"})
	repoPage("pulls")
	p.pageAction("New pull request", map[string]string{"base": "main", "head": "feature", "title": "Add notes"}, "")
	pr, err := c.ListPullRequests(ctx, &ssmpb.ListPullRequestsRequest{Parent: repo}).Next()
	if err != nil || pr.GetTitle() != "Add notes" || pr.GetHead().GetRef() != "feature" || pr.GetBase().GetRef() != "main" {
		t.Fatalf("ListPullRequests after New pull request = %v, %v", pr, err)
	}
	repoPage("pulls")
	p.clickText("#view tbody a", lastPart(pr.GetName()))
	p.waitFor(`location.pathname.includes("/app/pulls/")`)
	p.clickText(`#view [role="tab"]`, "Files changed")
	p.waitFor(`document.querySelector("#view [role=tabpanel]").textContent.includes("NOTES.md")`)
	ssmReviewThroughThePage(t, p, c, pr.GetName())
	p.clickText("#view .page-actions button", "Merge pull request")
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].every((b) => b.textContent.trim() !== "Merge pull request")`)
	if got, err := c.GetPullRequest(ctx, &ssmpb.GetPullRequestRequest{Name: pr.GetName()}); err != nil || got.GetState() != ssmpb.PullRequest_MERGED {
		t.Errorf("GetPullRequest after Merge pull request = %v, %v", got, err)
	}

	repoPage("settings")
	p.pageAction("Add branch rule", map[string]string{"id": "protect-main", "filter": "main", "requirePullRequest": "true"}, "")
	br, err := c.GetBranchRule(ctx, &ssmpb.GetBranchRuleRequest{Name: repo + "/branchRules/protect-main"})
	if err != nil || br.GetIncludePattern() != "main" || !br.GetRequirePullRequest() {
		t.Errorf("GetBranchRule after Add branch rule = %v, %v", br, err)
	}
	ssmWebhookThroughThePage(t, p, c, repo, func() { repoPage("settings") }, "/secure-source-manager/"+inst+"/app/hooks/ci?project="+project)
}

// ssmReviewThroughThePage (#1927), on a pull request's page: Comment on
// the Conversation tab, Add line comment on Files changed, Resolve
// conversation on the line comment's row, and Review with Comment (the
// page's caller opened the pull request, so Approve and Request changes
// are refused, as Google refuses them on your own pull request); each
// read back through ListPullRequestComments.
func ssmReviewThroughThePage(t *testing.T, p *tab, c *securesourcemanager.Client, pr string) {
	t.Helper()
	ctx := context.Background()
	comments := func() []*ssmpb.PullRequestComment {
		var out []*ssmpb.PullRequestComment
		it := c.ListPullRequestComments(ctx, &ssmpb.ListPullRequestCommentsRequest{Parent: pr})
		for {
			cm, err := it.Next()
			if err == iterator.Done {
				return out
			}
			if err != nil {
				t.Fatalf("ListPullRequestComments: %v", err)
			}
			out = append(out, cm)
		}
	}
	p.pageAction("Add line comment", map[string]string{"path": "NOTES.md", "line": "1", "lineBody": "why notes?"}, "")
	cms := comments()
	if len(cms) != 1 || cms[0].GetCode().GetBody() != "why notes?" || cms[0].GetCode().GetPosition().GetPath() != "NOTES.md" ||
		cms[0].GetCode().GetPosition().GetLine() != 1 {
		t.Fatalf("ListPullRequestComments after Add line comment = %v", cms)
	}
	code := cms[0].GetName()
	p.clickText(`#view [role="tab"]`, "Conversation")
	p.pageAction("Comment", map[string]string{"body": "looks fine"}, "")
	p.rowAction("Conversation", lastPart(code), "Resolve conversation", nil, "")
	// A row action with no form opens no dialog to wait for: the page
	// shows the conversation resolved once it is.
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("why notes?") && r.textContent.includes("Resolved"))`)
	if got, err := c.GetPullRequestComment(ctx, &ssmpb.GetPullRequestCommentRequest{Name: code}); err != nil || !got.GetCode().GetResolved() {
		t.Errorf("GetPullRequestComment after Resolve conversation = %v, %v", got, err)
	}
	p.pageAction("Review", map[string]string{"body": "one note"}, "")
	var review, general *ssmpb.PullRequestComment
	for _, cm := range comments() {
		switch {
		case cm.GetReview() != nil:
			review = cm
		case cm.GetComment() != nil:
			general = cm
		}
	}
	if general.GetComment().GetBody() != "looks fine" {
		t.Errorf("Comment made %v", general)
	}
	if review.GetReview().GetActionType() != ssmpb.PullRequestComment_Review_COMMENT || review.GetReview().GetBody() != "one note" {
		t.Errorf("Review made %v", review)
	}
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Commented") && r.textContent.includes("one note"))`)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("NOTES.md:1") && r.textContent.includes("Resolved"))`)
}

// ssmWebhookThroughThePage (#1927): Settings › Webhooks' Add webhook makes
// the hook GetHook reads; its page's Test delivery reaches the target URL
// with Google's headers, and Recent deliveries lists it.
func ssmWebhookThroughThePage(t *testing.T, p *tab, c *securesourcemanager.Client, repo string, settings func(), hookPage string) {
	t.Helper()
	got := make(chan http.Header, 4)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			got <- r.Header.Clone()
		}
	}))
	t.Cleanup(target.Close)
	settings()
	p.clickText(`#view [role="tab"]`, "Webhooks")
	p.waitFor(`new URLSearchParams(location.search).get("subtab") === "webhooks"`)
	p.pageAction("Add webhook", map[string]string{"id": "ci", "targetUri": target.URL + "/hook", "sensitiveQueryString": "key=k&secret=s",
		"branchFilter": "main"}, "")
	h, err := c.GetHook(context.Background(), &ssmpb.GetHookRequest{Name: repo + "/hooks/ci"})
	if err != nil || h.GetTargetUri() != target.URL+"/hook" || h.GetDisabled() || len(h.GetEvents()) != 1 || h.GetEvents()[0] != ssmpb.Hook_PUSH ||
		h.GetPushOption().GetBranchFilter() != "main" {
		t.Fatalf("GetHook after Add webhook = %v, %v", h, err)
	}
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("ci") && r.textContent.includes("Active"))`)
	p.navigate(hookPage)
	p.clickText("#view .page-actions button", "Test delivery")
	select {
	case hd := <-got:
		if hd.Get("X-SecureSourceManager-Event") != "push" || hd.Get("X-SecureSourceManager-Delivery") == "" {
			t.Errorf("Test delivery's headers = %v", hd)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Test delivery reached no target")
	}
	p.navigate(hookPage)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("push") && r.textContent.includes("Succeeded"))`)
}

func lastPart(name string) string { return name[strings.LastIndex(name, "/")+1:] }

// TestCloudRunSourceManagerScreensMeetWCAG (#1856), in the run shard: the
// Instances page, Create instance, an instance's page, a repository's
// Code, Branches, Issues, Pull requests and Settings tabs, a file's page,
// an issue's and a pull request's (its Conversation with a review and a
// line comment, and its Files changed), Settings' Webhooks tab and a
// webhook's page (#1927), audited as every other screen is.
func TestCloudRunSourceManagerScreensMeetWCAG(t *testing.T) {
	c := ssmClient(t)
	ctx := context.Background()
	project := uniqueProject(t)
	inst := fmt.Sprintf("a11y-ssm-%d", time.Now().UnixNano()%1e6)
	parent := "projects/" + project + "/locations/us-central1"
	op, err := c.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: inst, Instance: &ssmpb.Instance{}})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &ssmpb.DeleteInstanceRequest{Name: parent + "/instances/" + inst, Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	rop, err := c.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "app",
		Repository: &ssmpb.Repository{Instance: parent + "/instances/" + inst}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := rop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ssmPush(t, r.GetUris().GetGitHttps(), "main", map[string]string{"README.md": "# app\n"})
	ssmPush(t, r.GetUris().GetGitHttps(), "feature", map[string]string{"NOTES.md": "notes\n"})
	iop, err := c.CreateIssue(ctx, &ssmpb.CreateIssueRequest{Parent: r.GetName(), Issue: &ssmpb.Issue{Title: "An issue", Body: "body"}})
	if err != nil {
		t.Fatal(err)
	}
	is, err := iop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pop, err := c.CreatePullRequest(ctx, &ssmpb.CreatePullRequestRequest{Parent: r.GetName(), PullRequest: &ssmpb.PullRequest{Title: "A change",
		Base: &ssmpb.PullRequest_Branch{Ref: "main"}, Head: &ssmpb.PullRequest_Branch{Ref: "feature"}}})
	if err != nil {
		t.Fatal(err)
	}
	pr, err := pop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListBranchRules(ctx, &ssmpb.ListBranchRulesRequest{Parent: r.GetName()}).Next(); err != nil && err != iterator.Done {
		t.Fatal(err)
	}
	// A comment, a line comment and a review on the pull request, and a
	// webhook, so their tabs and the webhook's page are audited with rows.
	cop, err := c.BatchCreatePullRequestComments(ctx, &ssmpb.BatchCreatePullRequestCommentsRequest{Parent: pr.GetName(),
		Requests: []*ssmpb.CreatePullRequestCommentRequest{
			{Parent: pr.GetName(), PullRequestComment: &ssmpb.PullRequestComment{CommentDetail: &ssmpb.PullRequestComment_Review_{
				Review: &ssmpb.PullRequestComment_Review{ActionType: ssmpb.PullRequestComment_Review_COMMENT, Body: "a review"}}}},
			{Parent: pr.GetName(), PullRequestComment: &ssmpb.PullRequestComment{CommentDetail: &ssmpb.PullRequestComment_Code_{
				Code: &ssmpb.PullRequestComment_Code{Body: "a line comment", Position: &ssmpb.PullRequestComment_Position{Path: "NOTES.md", Line: 1}}}}}}})
	if err == nil {
		_, err = cop.Wait(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	hop, err := c.CreateHook(ctx, &ssmpb.CreateHookRequest{Parent: r.GetName(), HookId: "ci",
		Hook: &ssmpb.Hook{TargetUri: "http://127.0.0.1:9/hook", Events: []ssmpb.Hook_HookEventType{ssmpb.Hook_PUSH}}})
	if err == nil {
		_, err = hop.Wait(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	base := "/secure-source-manager/" + inst
	pages := listAndCreate("Secure Source Manager instances", "/secure-source-manager", project)
	pages = append(pages, screen("an instance", base, project))
	for _, tab := range []string{"code", "branches", "issues", "pulls", "settings"} {
		pages = append(pages, screen("a repository's "+tab, base+"/app?tab="+tab, project))
	}
	pages = append(pages, screen("a file", base+"/app/tree/main/README.md", project),
		screen("an issue", base+"/app/issues/"+lastPart(is.GetName()), project),
		screen("a pull request", base+"/app/pulls/"+lastPart(pr.GetName()), project),
		screen("a pull request's Files changed", base+"/app/pulls/"+lastPart(pr.GetName())+"?tab=files", project),
		screen("a repository's webhooks", base+"/app?tab=settings&subtab=webhooks", project),
		screen("a webhook", base+"/app/hooks/ci", project))
	auditScreens(t, pages)
}
