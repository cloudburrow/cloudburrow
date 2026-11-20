package run

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func opResult[T proto.Message](t *testing.T, op *longrunningpb.Operation, err error, into T) T {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if e := op.GetError(); e != nil {
		t.Fatalf("operation failed: %v", e)
	}
	if r := op.GetResponse(); r != nil {
		if err := r.UnmarshalTo(into); err != nil {
			t.Fatal(err)
		}
	}
	return into
}

// TestSourceManagerContentIssuesPullsAndRules (#1856): a pushed
// repository's tree, blobs and refs are read; issues and their comments
// are numbered, closed and reopened; a pull request is merged as a merge
// commit, a conflicting one is refused; a branch rule requiring a pull
// request refuses a direct push, and one requiring approvals refuses a
// merge; deleting the repository deletes them all, and they survive a
// restart.
func TestSourceManagerContentIssuesPullsAndRules(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	if err := UseSourceManagerRoot(root); err != nil {
		t.Fatal(err)
	}
	s := NewServer(&Knative{}, "t", time.Minute)
	var srv *httptest.Server
	m := s.SecureSourceManager(func() string { return srv.URL })
	srv = httptest.NewServer(SourceManagerHandlerOr(m, nil, func(g grpc.ServiceRegistrar) { m.Register(g) }, nil))
	defer srv.Close()
	ctx := context.Background()
	parent := "projects/ssm-content/locations/us-central1"
	repo := parent + "/repositories/r"
	if _, err := m.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "i"}); err != nil {
		t.Fatal(err)
	}
	op, err := m.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "r",
		Repository: &ssmpb.Repository{Instance: parent + "/instances/i"}})
	got := opResult(t, op, err, &ssmpb.Repository{})

	if _, err := m.FetchTree(ctx, &ssmpb.FetchTreeRequest{Repository: repo}); status.Code(err) != codes.NotFound {
		t.Fatalf("FetchTree of an empty repository: %v", err)
	}

	work := t.TempDir()
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", work, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	must := func(args ...string) string {
		t.Helper()
		out, err := git(args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return out
	}
	write := func(name, body string) {
		t.Helper()
		_ = os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755)
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("clone", "--quiet", got.GetUris().GetGitHttps(), ".")
	must("checkout", "--quiet", "-b", "main")
	write("README.md", "hello\n")
	write("src/a.go", "package a\n")
	must("add", ".")
	must("commit", "--quiet", "-m", "first")
	must("tag", "-a", "v1", "-m", "v1")
	must("push", "--quiet", "origin", "main", "v1")
	first := must("rev-parse", "HEAD")

	tree, err := m.FetchTree(ctx, &ssmpb.FetchTreeRequest{Repository: repo})
	if err != nil || len(tree.GetTreeEntries()) != 2 || tree.GetTreeEntries()[0].GetPath() != "README.md" ||
		tree.GetTreeEntries()[0].GetSize() != 6 || tree.GetTreeEntries()[1].GetType() != ssmpb.TreeEntry_TREE {
		t.Fatalf("FetchTree = %v, %v", tree, err)
	}
	rec, err := m.FetchTree(ctx, &ssmpb.FetchTreeRequest{Repository: repo, Ref: "v1", Recursive: true, PageSize: 2})
	if err != nil || len(rec.GetTreeEntries()) != 2 || rec.GetNextPageToken() == "" {
		t.Fatalf("FetchTree recursive page = %v, %v", rec, err)
	}
	rest, err := m.FetchTree(ctx, &ssmpb.FetchTreeRequest{Repository: repo, Ref: "v1", Recursive: true, PageToken: rec.GetNextPageToken()})
	if err != nil || len(rest.GetTreeEntries()) != 1 || rest.GetTreeEntries()[0].GetPath() != "src/a.go" {
		t.Fatalf("FetchTree next page = %v, %v", rest, err)
	}
	blob, err := m.FetchBlob(ctx, &ssmpb.FetchBlobRequest{Repository: repo, Sha: tree.GetTreeEntries()[0].GetSha()})
	if b, _ := base64.StdEncoding.DecodeString(blob.GetContent()); err != nil || string(b) != "hello\n" {
		t.Fatalf("FetchBlob = %v, %v", blob, err)
	}
	if _, err := m.FetchBlob(ctx, &ssmpb.FetchBlobRequest{Repository: repo, Sha: tree.GetTreeEntries()[1].GetSha()}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("FetchBlob of a tree: %v", err)
	}
	refs, err := m.FetchRefs(ctx, &ssmpb.FetchRefsRequest{Repository: repo})
	if err != nil || len(refs.GetRefs()) != 2 || refs.GetRefs()[1].GetName() != "refs/tags/v1" || refs.GetRefs()[1].GetTarget() != first {
		t.Fatalf("FetchRefs = %v, %v", refs, err)
	}

	// Issues.
	op, err = m.CreateIssue(ctx, &ssmpb.CreateIssueRequest{Parent: repo, Issue: &ssmpb.Issue{Title: "bug", Body: "it breaks"}})
	is := opResult(t, op, err, &ssmpb.Issue{})
	if is.GetName() != repo+"/issues/1" || is.GetState() != ssmpb.Issue_OPEN {
		t.Fatalf("CreateIssue = %v", is)
	}
	op, err = m.CloseIssue(ctx, &ssmpb.CloseIssueRequest{Name: is.GetName(), Etag: is.GetEtag()})
	closed := opResult(t, op, err, &ssmpb.Issue{})
	if closed.GetState() != ssmpb.Issue_CLOSED || closed.GetCloseTime() == nil {
		t.Fatalf("CloseIssue = %v", closed)
	}
	if _, err := m.OpenIssue(ctx, &ssmpb.OpenIssueRequest{Name: is.GetName(), Etag: is.GetEtag()}); status.Code(err) != codes.Aborted {
		t.Errorf("OpenIssue with a stale etag: %v", err)
	}
	if l, err := m.ListIssues(ctx, &ssmpb.ListIssuesRequest{Parent: repo, Filter: "state=OPEN"}); err != nil || len(l.GetIssues()) != 0 {
		t.Errorf("ListIssues state=OPEN = %v, %v", l, err)
	}
	op, err = m.UpdateIssue(ctx, &ssmpb.UpdateIssueRequest{Issue: &ssmpb.Issue{Name: is.GetName(), Title: "bug 2"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"title"}}})
	if u := opResult(t, op, err, &ssmpb.Issue{}); u.GetTitle() != "bug 2" || u.GetBody() != "it breaks" {
		t.Errorf("UpdateIssue = %v", u)
	}
	op, err = m.CreateIssueComment(ctx, &ssmpb.CreateIssueCommentRequest{Parent: is.GetName(), IssueComment: &ssmpb.IssueComment{Body: "me too"}})
	if c := opResult(t, op, err, &ssmpb.IssueComment{}); c.GetName() != is.GetName()+"/issueComments/1" {
		t.Errorf("CreateIssueComment = %v", c)
	}

	// Pull requests.
	must("checkout", "--quiet", "-b", "feature")
	write("src/b.go", "package a\n")
	must("add", ".")
	must("commit", "--quiet", "-m", "b")
	must("push", "--quiet", "origin", "feature")
	op, err = m.CreatePullRequest(ctx, &ssmpb.CreatePullRequestRequest{Parent: repo, PullRequest: &ssmpb.PullRequest{Title: "add b",
		Base: &ssmpb.PullRequest_Branch{Ref: "main"}, Head: &ssmpb.PullRequest_Branch{Ref: "feature"}}})
	pr := opResult(t, op, err, &ssmpb.PullRequest{})
	if pr.GetName() != repo+"/pullRequests/1" || pr.GetState() != ssmpb.PullRequest_OPEN || pr.GetBase().GetSha() != first {
		t.Fatalf("CreatePullRequest = %v", pr)
	}
	diffs, err := m.ListPullRequestFileDiffs(ctx, &ssmpb.ListPullRequestFileDiffsRequest{Name: pr.GetName()})
	if err != nil || len(diffs.GetFileDiffs()) != 1 || diffs.GetFileDiffs()[0].GetName() != "src/b.go" ||
		diffs.GetFileDiffs()[0].GetAction() != ssmpb.FileDiff_ADDED || !strings.Contains(diffs.GetFileDiffs()[0].GetPatch(), "+package a") {
		t.Fatalf("ListPullRequestFileDiffs = %v, %v", diffs, err)
	}

	// A rule requiring approvals refuses the merge; requiring a pull
	// request refuses a direct push.
	if _, err := m.CreateBranchRule(ctx, &ssmpb.CreateBranchRuleRequest{Parent: repo, BranchRuleId: "all",
		BranchRule: &ssmpb.BranchRule{IncludePattern: ".*", RequirePullRequest: true}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("require_pull_request with .*: %v", err)
	}
	op, err = m.CreateBranchRule(ctx, &ssmpb.CreateBranchRuleRequest{Parent: repo, BranchRuleId: "protect-main",
		BranchRule: &ssmpb.BranchRule{IncludePattern: "main", RequirePullRequest: true, MinimumApprovalsCount: 1}})
	rule := opResult(t, op, err, &ssmpb.BranchRule{})
	if _, err := m.MergePullRequest(ctx, &ssmpb.MergePullRequestRequest{Name: pr.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("merging under a rule needing approvals: %v", err)
	}
	must("checkout", "--quiet", "main")
	write("direct.txt", "x")
	must("add", ".")
	must("commit", "--quiet", "-m", "direct")
	if out, err := git("push", "origin", "main"); err == nil || !strings.Contains(out, "requires a pull request") {
		t.Errorf("a direct push to a protected branch: %v\n%s", err, out)
	}
	must("reset", "--quiet", "--hard", first)
	op, err = m.UpdateBranchRule(ctx, &ssmpb.UpdateBranchRuleRequest{BranchRule: &ssmpb.BranchRule{Name: rule.GetName(), MinimumApprovalsCount: 0},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"minimum_approvals_count"}}})
	opResult(t, op, err, &ssmpb.BranchRule{})

	op, err = m.MergePullRequest(ctx, &ssmpb.MergePullRequestRequest{Name: pr.GetName()})
	merged := opResult(t, op, err, &ssmpb.PullRequest{})
	if merged.GetState() != ssmpb.PullRequest_MERGED || merged.GetCloseTime() == nil {
		t.Fatalf("MergePullRequest = %v", merged)
	}
	must("fetch", "--quiet", "origin")
	if parents := must("log", "-1", "--format=%P", "origin/main"); len(strings.Fields(parents)) != 2 {
		t.Errorf("the merge is not a merge commit: parents %q", parents)
	}
	if _, err := m.MergePullRequest(ctx, &ssmpb.MergePullRequestRequest{Name: pr.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("merging twice: %v", err)
	}

	// Two branches changing one line conflict.
	must("checkout", "--quiet", "-B", "c1", "origin/main")
	write("README.md", "one\n")
	must("commit", "--quiet", "-am", "c1")
	must("checkout", "--quiet", "-B", "c2", "origin/main")
	write("README.md", "two\n")
	must("commit", "--quiet", "-am", "c2")
	must("push", "--quiet", "origin", "c1", "c2")
	op, err = m.CreatePullRequest(ctx, &ssmpb.CreatePullRequestRequest{Parent: repo, PullRequest: &ssmpb.PullRequest{Title: "c1",
		Base: &ssmpb.PullRequest_Branch{Ref: "c2"}, Head: &ssmpb.PullRequest_Branch{Ref: "c1"}}})
	cpr := opResult(t, op, err, &ssmpb.PullRequest{})
	if _, err := m.MergePullRequest(ctx, &ssmpb.MergePullRequestRequest{Name: cpr.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("merging a conflict: %v", err)
	}
	op, err = m.ClosePullRequest(ctx, &ssmpb.ClosePullRequestRequest{Name: cpr.GetName()})
	if c := opResult(t, op, err, &ssmpb.PullRequest{}); c.GetState() != ssmpb.PullRequest_CLOSED {
		t.Errorf("ClosePullRequest = %v", c)
	}

	bs, err := SourceManagerBranches(ctx, repo)
	if err != nil || len(bs) != 4 {
		t.Errorf("SourceManagerBranches = %v, %v", bs, err)
	}

	// A restart keeps them.
	ssmState.issues, ssmState.pulls, ssmState.rules = map[string]*ssmpb.Issue{}, map[string]*ssmpb.PullRequest{}, map[string]*ssmpb.BranchRule{}
	if err := UseSourceManagerRoot(root); err != nil {
		t.Fatal(err)
	}
	if l, err := m.ListPullRequests(ctx, &ssmpb.ListPullRequestsRequest{Parent: repo}); err != nil || len(l.GetPullRequests()) != 2 {
		t.Errorf("ListPullRequests after a restart = %v, %v", l, err)
	}
	if _, err := m.GetBranchRule(ctx, &ssmpb.GetBranchRuleRequest{Name: rule.GetName()}); err != nil {
		t.Errorf("GetBranchRule after a restart: %v", err)
	}

	if _, err := m.DeleteRepository(ctx, &ssmpb.DeleteRepositoryRequest{Name: repo}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetIssue(ctx, &ssmpb.GetIssueRequest{Name: is.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("an issue outlived its repository: %v", err)
	}
	ssmState.mu.Lock()
	n := len(ssmState.pulls) + len(ssmState.rules) + len(ssmState.issueComments)
	ssmState.mu.Unlock()
	if n != 0 {
		t.Errorf("%d children outlived their repository", n)
	}
}
