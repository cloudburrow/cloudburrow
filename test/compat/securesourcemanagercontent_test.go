//go:build compat

package compat

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// ssmPushed makes a repository whose main holds README.md and src/a.go,
// tagged v1, and whose feature branch adds src/b.go; it returns the
// repository, a clone of it, and git run in that clone.
func ssmPushed(t *testing.T, h *Harness, suffix string) (*ssmpb.Repository, func(args ...string) (string, error)) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	c := ssmClient(t, h)
	_, repo := ssmInstanceAndRepo(t, h, c, suffix)
	work := t.TempDir()
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", work, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	must := func(args ...string) {
		t.Helper()
		if out, err := git(args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, body string) {
		t.Helper()
		_ = os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755)
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("clone", "--quiet", repo.GetUris().GetGitHttps(), ".")
	must("checkout", "--quiet", "-b", "main")
	write("README.md", "hello\n")
	write("src/a.go", "package a\n")
	must("add", ".")
	must("commit", "--quiet", "-m", "first")
	must("tag", "-a", "v1", "-m", "v1")
	must("checkout", "--quiet", "-b", "feature")
	write("src/b.go", "package a\n")
	must("add", ".")
	must("commit", "--quiet", "-m", "b")
	must("push", "--quiet", "origin", "main", "feature", "v1")
	must("checkout", "--quiet", "main")
	return repo, git
}

// covers: google.cloud.securesourcemanager.v1.SecureSourceManager/FetchTree, google.cloud.securesourcemanager.v1.SecureSourceManager/FetchBlob, google.cloud.securesourcemanager.v1.SecureSourceManager/FetchRefs
//
// TestSecureSourceManagerFetch (#1856), with the official client over
// gRPC on the Run port: a pushed repository's tree is listed at the
// default branch and, recursively and paged, at a tag; a blob's content
// is read base64-encoded, and a tree is not a blob; its branches and tags
// are listed, a tag's target being its commit.
func TestSecureSourceManagerFetch(t *testing.T) {
	h := New(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	repo, git := ssmPushed(t, h, suffix)
	c := ssmClient(t, h)
	ctx := h.Context()

	var top []*ssmpb.TreeEntry
	for it := c.FetchTree(ctx, &ssmpb.FetchTreeRequest{Repository: repo.GetName()}); ; {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("FetchTree: %v", err)
		}
		top = append(top, e)
	}
	if len(top) != 2 || top[0].GetPath() != "README.md" || top[0].GetType() != ssmpb.TreeEntry_BLOB || top[0].GetSize() != 6 ||
		top[0].GetMode() != "100644" || top[1].GetType() != ssmpb.TreeEntry_TREE {
		t.Fatalf("FetchTree = %v", top)
	}
	var paths []string
	for it := c.FetchTree(ctx, &ssmpb.FetchTreeRequest{Repository: repo.GetName(), Ref: "v1", Recursive: true, PageSize: 1}); ; {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("FetchTree recursive: %v", err)
		}
		paths = append(paths, e.GetPath())
	}
	if strings.Join(paths, ",") != "README.md,src,src/a.go" {
		t.Errorf("FetchTree recursive at v1 = %v", paths)
	}
	if _, err := c.FetchTree(ctx, &ssmpb.FetchTreeRequest{Repository: repo.GetName(), Ref: "nope"}).Next(); status.Code(err) != codes.NotFound {
		t.Errorf("FetchTree of an unknown ref: %v", err)
	}

	blob, err := c.FetchBlob(ctx, &ssmpb.FetchBlobRequest{Repository: repo.GetName(), Sha: top[0].GetSha()})
	if b, _ := base64.StdEncoding.DecodeString(blob.GetContent()); err != nil || string(b) != "hello\n" || blob.GetSha() != top[0].GetSha() {
		t.Fatalf("FetchBlob = %v, %v", blob, err)
	}
	if _, err := c.FetchBlob(ctx, &ssmpb.FetchBlobRequest{Repository: repo.GetName(), Sha: top[1].GetSha()}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("FetchBlob of a tree: %v", err)
	}

	first, _ := git("rev-parse", "main")
	var refs []*ssmpb.Ref
	for it := c.FetchRefs(ctx, &ssmpb.FetchRefsRequest{Repository: repo.GetName()}); ; {
		r, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("FetchRefs: %v", err)
		}
		refs = append(refs, r)
	}
	if len(refs) != 3 || refs[1].GetName() != "refs/heads/main" || refs[1].GetTarget() != first ||
		refs[2].GetName() != "refs/tags/v1" || refs[2].GetType() != ssmpb.Ref_REF_TYPE_TAG || refs[2].GetTarget() != first {
		t.Fatalf("FetchRefs = %v", refs)
	}
	tags, err := c.FetchRefs(ctx, &ssmpb.FetchRefsRequest{Repository: repo.GetName(), Type: ssmpb.Ref_REF_TYPE_TAG}).Next()
	if err != nil || tags.GetName() != "refs/tags/v1" {
		t.Errorf("FetchRefs of tags = %v, %v", tags, err)
	}
}

// covers: google.cloud.securesourcemanager.v1.SecureSourceManager/CreateIssue, google.cloud.securesourcemanager.v1.SecureSourceManager/GetIssue, google.cloud.securesourcemanager.v1.SecureSourceManager/ListIssues, google.cloud.securesourcemanager.v1.SecureSourceManager/UpdateIssue, google.cloud.securesourcemanager.v1.SecureSourceManager/DeleteIssue, google.cloud.securesourcemanager.v1.SecureSourceManager/OpenIssue, google.cloud.securesourcemanager.v1.SecureSourceManager/CloseIssue, google.cloud.securesourcemanager.v1.SecureSourceManager/CreateIssueComment, google.cloud.securesourcemanager.v1.SecureSourceManager/GetIssueComment, google.cloud.securesourcemanager.v1.SecureSourceManager/ListIssueComments, google.cloud.securesourcemanager.v1.SecureSourceManager/UpdateIssueComment, google.cloud.securesourcemanager.v1.SecureSourceManager/DeleteIssueComment
//
// TestSecureSourceManagerIssues (#1856): an issue is numbered from 1,
// OPEN, closed (close_time set) and reopened (cleared); a stale etag is
// ABORTED; its title changes under an update mask; it is listed by state;
// a comment on it is made, read, listed, edited and deleted; deleting the
// issue leaves it NOT_FOUND.
func TestSecureSourceManagerIssues(t *testing.T) {
	h := New(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	c := ssmClient(t, h)
	ctx := h.Context()
	_, repo := ssmInstanceAndRepo(t, h, c, strings.ReplaceAll(uuid.NewString(), "-", "")[:8])

	op, err := c.CreateIssue(ctx, &ssmpb.CreateIssueRequest{Parent: repo.GetName(), Issue: &ssmpb.Issue{Title: "bug", Body: "it breaks"}})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	is, err := op.Wait(ctx)
	if err != nil || is.GetName() != repo.GetName()+"/issues/1" || is.GetState() != ssmpb.Issue_OPEN || is.GetEtag() == "" {
		t.Fatalf("the issue = %v, %v", is, err)
	}
	if _, err := c.CreateIssue(ctx, &ssmpb.CreateIssueRequest{Parent: repo.GetName(), Issue: &ssmpb.Issue{}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an issue with no title: %v", err)
	}
	cop, err := c.CloseIssue(ctx, &ssmpb.CloseIssueRequest{Name: is.GetName(), Etag: is.GetEtag()})
	if err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}
	closed, err := cop.Wait(ctx)
	if err != nil || closed.GetState() != ssmpb.Issue_CLOSED || closed.GetCloseTime() == nil {
		t.Fatalf("the closed issue = %v, %v", closed, err)
	}
	if _, err := c.OpenIssue(ctx, &ssmpb.OpenIssueRequest{Name: is.GetName(), Etag: is.GetEtag()}); status.Code(err) != codes.Aborted {
		t.Errorf("OpenIssue with a stale etag: %v", err)
	}
	if _, err := c.ListIssues(ctx, &ssmpb.ListIssuesRequest{Parent: repo.GetName(), Filter: "state=OPEN"}).Next(); err != iterator.Done {
		t.Errorf("ListIssues state=OPEN lists the closed issue: %v", err)
	}
	oop, err := c.OpenIssue(ctx, &ssmpb.OpenIssueRequest{Name: is.GetName()})
	if err != nil {
		t.Fatalf("OpenIssue: %v", err)
	}
	if o, err := oop.Wait(ctx); err != nil || o.GetState() != ssmpb.Issue_OPEN || o.GetCloseTime() != nil {
		t.Errorf("the reopened issue = %v, %v", o, err)
	}
	uop, err := c.UpdateIssue(ctx, &ssmpb.UpdateIssueRequest{Issue: &ssmpb.Issue{Name: is.GetName(), Title: "bug 2"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"title"}}})
	if err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	if u, err := uop.Wait(ctx); err != nil || u.GetTitle() != "bug 2" || u.GetBody() != "it breaks" {
		t.Errorf("the updated issue = %v, %v", u, err)
	}
	if got, err := c.GetIssue(ctx, &ssmpb.GetIssueRequest{Name: is.GetName()}); err != nil || got.GetTitle() != "bug 2" {
		t.Errorf("GetIssue = %v, %v", got, err)
	}
	if got, err := c.ListIssues(ctx, &ssmpb.ListIssuesRequest{Parent: repo.GetName()}).Next(); err != nil || got.GetName() != is.GetName() {
		t.Errorf("ListIssues = %v, %v", got, err)
	}

	ccop, err := c.CreateIssueComment(ctx, &ssmpb.CreateIssueCommentRequest{Parent: is.GetName(), IssueComment: &ssmpb.IssueComment{Body: "me too"}})
	if err != nil {
		t.Fatalf("CreateIssueComment: %v", err)
	}
	cm, err := ccop.Wait(ctx)
	if err != nil || cm.GetName() != is.GetName()+"/issueComments/1" {
		t.Fatalf("the comment = %v, %v", cm, err)
	}
	if got, err := c.GetIssueComment(ctx, &ssmpb.GetIssueCommentRequest{Name: cm.GetName()}); err != nil || got.GetBody() != "me too" {
		t.Errorf("GetIssueComment = %v, %v", got, err)
	}
	ucop, err := c.UpdateIssueComment(ctx, &ssmpb.UpdateIssueCommentRequest{IssueComment: &ssmpb.IssueComment{Name: cm.GetName(), Body: "me three"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"body"}}})
	if err != nil {
		t.Fatalf("UpdateIssueComment: %v", err)
	}
	if _, err := ucop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := c.ListIssueComments(ctx, &ssmpb.ListIssueCommentsRequest{Parent: is.GetName()}).Next(); err != nil || got.GetBody() != "me three" {
		t.Errorf("ListIssueComments = %v, %v", got, err)
	}
	dcop, err := c.DeleteIssueComment(ctx, &ssmpb.DeleteIssueCommentRequest{Name: cm.GetName()})
	if err != nil || dcop.Wait(ctx) != nil {
		t.Fatalf("DeleteIssueComment: %v", err)
	}
	dop, err := c.DeleteIssue(ctx, &ssmpb.DeleteIssueRequest{Name: is.GetName()})
	if err != nil || dop.Wait(ctx) != nil {
		t.Fatalf("DeleteIssue: %v", err)
	}
	if _, err := c.GetIssue(ctx, &ssmpb.GetIssueRequest{Name: is.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetIssue after its delete: %v", err)
	}
}

// covers: google.cloud.securesourcemanager.v1.SecureSourceManager/CreatePullRequest, google.cloud.securesourcemanager.v1.SecureSourceManager/GetPullRequest, google.cloud.securesourcemanager.v1.SecureSourceManager/ListPullRequests, google.cloud.securesourcemanager.v1.SecureSourceManager/UpdatePullRequest, google.cloud.securesourcemanager.v1.SecureSourceManager/MergePullRequest, google.cloud.securesourcemanager.v1.SecureSourceManager/OpenPullRequest, google.cloud.securesourcemanager.v1.SecureSourceManager/ClosePullRequest, google.cloud.securesourcemanager.v1.SecureSourceManager/ListPullRequestFileDiffs, google.cloud.securesourcemanager.v1.SecureSourceManager/CreateBranchRule, google.cloud.securesourcemanager.v1.SecureSourceManager/GetBranchRule, google.cloud.securesourcemanager.v1.SecureSourceManager/ListBranchRules, google.cloud.securesourcemanager.v1.SecureSourceManager/UpdateBranchRule, google.cloud.securesourcemanager.v1.SecureSourceManager/DeleteBranchRule
//
// TestSecureSourceManagerPullRequestsAndBranchRules (#1856): a pull
// request of feature into main is opened with its branches' tips, its
// file diffs list the added file with its patch, it is retitled, closed
// and reopened; a branch rule on main requiring a pull request refuses a
// direct git push, and requiring an approval refuses the merge; with the
// approval dropped the merge makes a merge commit on main and the pull
// request is MERGED; a rule requiring a pull request on .* is refused.
func TestSecureSourceManagerPullRequestsAndBranchRules(t *testing.T) {
	h := New(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	repo, git := ssmPushed(t, h, suffix)
	c := ssmClient(t, h)
	ctx := h.Context()
	first, _ := git("rev-parse", "main")

	op, err := c.CreatePullRequest(ctx, &ssmpb.CreatePullRequestRequest{Parent: repo.GetName(), PullRequest: &ssmpb.PullRequest{
		Title: "add b", Base: &ssmpb.PullRequest_Branch{Ref: "main"}, Head: &ssmpb.PullRequest_Branch{Ref: "feature"}}})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	pr, err := op.Wait(ctx)
	if err != nil || pr.GetName() != repo.GetName()+"/pullRequests/1" || pr.GetState() != ssmpb.PullRequest_OPEN || pr.GetBase().GetSha() != first {
		t.Fatalf("the pull request = %v, %v", pr, err)
	}
	if _, err := c.CreatePullRequest(ctx, &ssmpb.CreatePullRequestRequest{Parent: repo.GetName(), PullRequest: &ssmpb.PullRequest{
		Title: "x", Base: &ssmpb.PullRequest_Branch{Ref: "main"}, Head: &ssmpb.PullRequest_Branch{Ref: "nope"}}}); status.Code(err) != codes.NotFound {
		t.Errorf("a pull request from no branch: %v", err)
	}
	d, err := c.ListPullRequestFileDiffs(ctx, &ssmpb.ListPullRequestFileDiffsRequest{Name: pr.GetName()}).Next()
	if err != nil || d.GetName() != "src/b.go" || d.GetAction() != ssmpb.FileDiff_ADDED || !strings.Contains(d.GetPatch(), "+package a") {
		t.Fatalf("ListPullRequestFileDiffs = %v, %v", d, err)
	}
	uop, err := c.UpdatePullRequest(ctx, &ssmpb.UpdatePullRequestRequest{PullRequest: &ssmpb.PullRequest{Name: pr.GetName(), Title: "add b.go"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"title"}}})
	if err != nil {
		t.Fatalf("UpdatePullRequest: %v", err)
	}
	if u, err := uop.Wait(ctx); err != nil || u.GetTitle() != "add b.go" {
		t.Errorf("the retitled pull request = %v, %v", u, err)
	}
	cop, err := c.ClosePullRequest(ctx, &ssmpb.ClosePullRequestRequest{Name: pr.GetName()})
	if err != nil {
		t.Fatalf("ClosePullRequest: %v", err)
	}
	if cl, err := cop.Wait(ctx); err != nil || cl.GetState() != ssmpb.PullRequest_CLOSED || cl.GetCloseTime() == nil {
		t.Errorf("the closed pull request = %v, %v", cl, err)
	}
	if _, err := c.MergePullRequest(ctx, &ssmpb.MergePullRequestRequest{Name: pr.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("merging a closed pull request: %v", err)
	}
	oop, err := c.OpenPullRequest(ctx, &ssmpb.OpenPullRequestRequest{Name: pr.GetName()})
	if err != nil {
		t.Fatalf("OpenPullRequest: %v", err)
	}
	if _, err := oop.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := c.CreateBranchRule(ctx, &ssmpb.CreateBranchRuleRequest{Parent: repo.GetName(), BranchRuleId: "all",
		BranchRule: &ssmpb.BranchRule{IncludePattern: ".*", RequirePullRequest: true}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("require_pull_request with .*: %v", err)
	}
	bop, err := c.CreateBranchRule(ctx, &ssmpb.CreateBranchRuleRequest{Parent: repo.GetName(), BranchRuleId: "protect-main",
		BranchRule: &ssmpb.BranchRule{IncludePattern: "main", RequirePullRequest: true, MinimumApprovalsCount: 1}})
	if err != nil {
		t.Fatalf("CreateBranchRule: %v", err)
	}
	rule, err := bop.Wait(ctx)
	if err != nil || rule.GetUid() == "" {
		t.Fatalf("the branch rule = %v, %v", rule, err)
	}
	if got, err := c.GetBranchRule(ctx, &ssmpb.GetBranchRuleRequest{Name: rule.GetName()}); err != nil || got.GetIncludePattern() != "main" {
		t.Errorf("GetBranchRule = %v, %v", got, err)
	}
	if got, err := c.ListBranchRules(ctx, &ssmpb.ListBranchRulesRequest{Parent: repo.GetName()}).Next(); err != nil || got.GetName() != rule.GetName() {
		t.Errorf("ListBranchRules = %v, %v", got, err)
	}
	if out, err := git("commit", "--quiet", "--allow-empty", "-m", "direct"); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	if out, err := git("push", "origin", "main"); err == nil || !strings.Contains(out, "requires a pull request") {
		t.Errorf("a direct push to the protected branch: %v\n%s", err, out)
	}
	if _, err := c.MergePullRequest(ctx, &ssmpb.MergePullRequestRequest{Name: pr.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("merging under a rule needing an approval: %v", err)
	}
	urop, err := c.UpdateBranchRule(ctx, &ssmpb.UpdateBranchRuleRequest{BranchRule: &ssmpb.BranchRule{Name: rule.GetName()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"minimum_approvals_count"}}})
	if err != nil {
		t.Fatalf("UpdateBranchRule: %v", err)
	}
	if _, err := urop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	mop, err := c.MergePullRequest(ctx, &ssmpb.MergePullRequestRequest{Name: pr.GetName()})
	if err != nil {
		t.Fatalf("MergePullRequest: %v", err)
	}
	merged, err := mop.Wait(ctx)
	if err != nil || merged.GetState() != ssmpb.PullRequest_MERGED {
		t.Fatalf("the merged pull request = %v, %v", merged, err)
	}
	if _, err := git("fetch", "--quiet", "origin"); err != nil {
		t.Fatal(err)
	}
	if parents, _ := git("log", "-1", "--format=%P", "origin/main"); len(strings.Fields(parents)) != 2 {
		t.Errorf("main after the merge has parents %q; want a merge commit", parents)
	}
	if got, err := c.ListPullRequests(ctx, &ssmpb.ListPullRequestsRequest{Parent: repo.GetName()}).Next(); err != nil || got.GetState() != ssmpb.PullRequest_MERGED {
		t.Errorf("ListPullRequests = %v, %v", got, err)
	}
	if got, err := c.GetPullRequest(ctx, &ssmpb.GetPullRequestRequest{Name: pr.GetName()}); err != nil || got.GetCloseTime() == nil {
		t.Errorf("GetPullRequest = %v, %v", got, err)
	}
	drop, err := c.DeleteBranchRule(ctx, &ssmpb.DeleteBranchRuleRequest{Name: rule.GetName()})
	if err != nil || drop.Wait(ctx) != nil {
		t.Fatalf("DeleteBranchRule: %v", err)
	}
	if out, err := git("push", "origin", "HEAD:refs/heads/after-rule"); err != nil {
		t.Errorf("a push once the rule is deleted: %v\n%s", err, out)
	}
}
