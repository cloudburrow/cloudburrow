package run

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

type hookCall struct {
	query  string
	header http.Header
	body   map[string]any
}

// TestSourceManagerHooksReviewsAndRules (#1927): hooks get push,
// pull_request and pull_request_comment events with Google's headers, the
// sensitive query string and the branch filter; comments, code comments,
// replies and resolution behave as documented; the author cannot approve
// their own pull request; and a branch rule counts real approvals of the
// head commit, unresolved conversations and change requests.
func TestSourceManagerHooksReviewsAndRules(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if err := UseSourceManagerRoot(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	SetSourceManagerCaller(func(b string) (string, bool) { return b + "@example.com", true }, "owner@example.com")
	defer SetSourceManagerCaller(nil, "")
	as := func(who string) context.Context {
		return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+who))
	}
	ctx := context.Background() // the instance's own account

	var mu sync.Mutex
	var calls []hookCall
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var v map[string]any
		_ = json.Unmarshal(b, &v)
		mu.Lock()
		calls = append(calls, hookCall{query: r.URL.RawQuery, header: r.Header.Clone(), body: v})
		mu.Unlock()
	}))
	defer sink.Close()
	waitFor := func(event string, n int) []hookCall {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			mu.Lock()
			var got []hookCall
			for _, c := range calls {
				if c.header.Get("X-SecureSourceManager-Event") == event {
					got = append(got, c)
				}
			}
			mu.Unlock()
			if len(got) >= n || time.Now().After(deadline) {
				if len(got) < n {
					t.Fatalf("%d %s deliveries, want %d", len(got), event, n)
				}
				return got
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	s := NewServer(&Knative{}, "t", time.Minute)
	var srv *httptest.Server
	m := s.SecureSourceManager(func() string { return srv.URL })
	srv = httptest.NewServer(SourceManagerHandlerOr(m, nil, func(g grpc.ServiceRegistrar) { m.Register(g) }, nil))
	defer srv.Close()
	parent := "projects/ssm-reviews/locations/us-central1"
	repo := parent + "/repositories/r"
	if _, err := m.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "i"}); err != nil {
		t.Fatal(err)
	}
	op, err := m.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "r",
		Repository: &ssmpb.Repository{Instance: parent + "/instances/i"}})
	r := opResult(t, op, err, &ssmpb.Repository{})

	// Hooks: validation, the hidden query string, and an update.
	if _, err := m.CreateHook(ctx, &ssmpb.CreateHookRequest{Parent: repo, HookId: "Bad", Hook: &ssmpb.Hook{TargetUri: sink.URL}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateHook with a bad ID: %v", err)
	}
	if _, err := m.CreateHook(ctx, &ssmpb.CreateHookRequest{Parent: repo, HookId: "h", Hook: &ssmpb.Hook{TargetUri: "ftp://x"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateHook with an ftp target: %v", err)
	}
	op, err = m.CreateHook(ctx, &ssmpb.CreateHookRequest{Parent: repo, HookId: "all", Hook: &ssmpb.Hook{TargetUri: sink.URL + "/hook",
		SensitiveQueryString: "key=k&secret=s", PushOption: &ssmpb.Hook_PushOption{BranchFilter: "{main,release*}"},
		Events: []ssmpb.Hook_HookEventType{ssmpb.Hook_PUSH, ssmpb.Hook_PULL_REQUEST, ssmpb.Hook_PULL_REQUEST_COMMENT}}})
	h := opResult(t, op, err, &ssmpb.Hook{})
	if h.GetName() != repo+"/hooks/all" || h.GetUid() == "" || h.GetSensitiveQueryString() != ssmQueryPlaceholder {
		t.Fatalf("CreateHook = %v", h)
	}
	op, err = m.UpdateHook(ctx, &ssmpb.UpdateHookRequest{Hook: &ssmpb.Hook{Name: h.GetName(), SensitiveQueryString: ssmQueryPlaceholder, Disabled: false},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"sensitive_query_string", "disabled"}}})
	opResult(t, op, err, &ssmpb.Hook{})
	list, err := m.ListHooks(ctx, &ssmpb.ListHooksRequest{Parent: repo})
	if err != nil || len(list.GetHooks()) != 1 {
		t.Fatalf("ListHooks = %v, %v", list, err)
	}

	// A push to main is delivered; one to a branch the filter leaves out
	// is not.
	work := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", work, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		_ = os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755)
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("clone", "--quiet", r.GetUris().GetGitHttps(), ".")
	git("checkout", "--quiet", "-b", "main")
	write("README.md", "hello\n")
	git("add", ".")
	git("commit", "--quiet", "-m", "first")
	git("push", "--quiet", "origin", "main")
	push := waitFor("push", 1)[0]
	if push.query != "key=k&secret=s" || push.header.Get("Content-Type") != "application/json" || push.header.Get("X-SecureSourceManager-Delivery") == "" ||
		push.body["ref"] != "refs/heads/main" || len(push.body["commits"].([]any)) != 1 {
		t.Fatalf("push delivery = %+v", push)
	}
	if _, ok := push.header["X-Securesourcemanager-Signature"]; !ok {
		t.Errorf("push delivery has no X-SecureSourceManager-Signature header")
	}
	if c := push.body["commits"].([]any)[0].(map[string]any); c["message"] != "first\n" || len(c["added"].([]any)) != 1 {
		t.Errorf("push commit = %v", c)
	}
	git("checkout", "--quiet", "-b", "feature")
	write("README.md", "hello\nworld\n")
	git("commit", "--quiet", "-am", "second")
	git("push", "--quiet", "origin", "feature")

	// A pull request by alice: opened.
	op, err = m.CreatePullRequest(as("alice"), &ssmpb.CreatePullRequestRequest{Parent: repo, PullRequest: &ssmpb.PullRequest{Title: "t",
		Base: &ssmpb.PullRequest_Branch{Ref: "main"}, Head: &ssmpb.PullRequest_Branch{Ref: "feature"}}})
	pr := opResult(t, op, err, &ssmpb.PullRequest{})
	opened := waitFor("pull_request", 1)[0]
	if opened.body["action"] != "opened" || opened.body["number"] != float64(1) ||
		opened.body["pull_request"].(map[string]any)["user"].(map[string]any)["email"] != "alice@example.com" {
		t.Fatalf("pull_request delivery = %v", opened.body)
	}
	if n := len(waitFor("push", 1)); n != 1 {
		t.Errorf("a push to feature, which the branch filter leaves out, was delivered (%d pushes)", n)
	}

	// Comments.
	op, err = m.CreatePullRequestComment(as("bob"), &ssmpb.CreatePullRequestCommentRequest{Parent: pr.GetName(),
		PullRequestComment: &ssmpb.PullRequestComment{CommentDetail: &ssmpb.PullRequestComment_Comment_{Comment: &ssmpb.PullRequestComment_Comment{Body: "looks good"}}}})
	general := opResult(t, op, err, &ssmpb.PullRequestComment{})
	if general.GetName() != pr.GetName()+"/pullRequestComments/1" {
		t.Fatalf("CreatePullRequestComment = %v", general)
	}
	if c := waitFor("pull_request_comment", 1)[0]; c.body["action"] != "created" || c.body["is_pull"] != true ||
		c.body["comment"].(map[string]any)["body"] != "looks good" {
		t.Fatalf("pull_request_comment delivery = %v", c.body)
	}
	if _, err := m.UpdatePullRequestComment(as("carol"), &ssmpb.UpdatePullRequestCommentRequest{PullRequestComment: &ssmpb.PullRequestComment{Name: general.GetName(),
		CommentDetail: &ssmpb.PullRequestComment_Comment_{Comment: &ssmpb.PullRequestComment_Comment{Body: "x"}}}}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("UpdatePullRequestComment by another account: %v", err)
	}
	code := func(path string, line int64) *ssmpb.PullRequestComment {
		return &ssmpb.PullRequestComment{CommentDetail: &ssmpb.PullRequestComment_Code_{Code: &ssmpb.PullRequestComment_Code{Body: "why?",
			Position: &ssmpb.PullRequestComment_Position{Path: path, Line: line}}}}
	}
	if _, err := m.CreatePullRequestComment(as("bob"), &ssmpb.CreatePullRequestCommentRequest{Parent: pr.GetName(), PullRequestComment: code("other.txt", 1)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a code comment on a file the pull request does not change: %v", err)
	}
	review := func(a ssmpb.PullRequestComment_Review_ActionType) *ssmpb.PullRequestComment {
		return &ssmpb.PullRequestComment{CommentDetail: &ssmpb.PullRequestComment_Review_{Review: &ssmpb.PullRequestComment_Review{ActionType: a}}}
	}
	op, err = m.BatchCreatePullRequestComments(as("bob"), &ssmpb.BatchCreatePullRequestCommentsRequest{Parent: pr.GetName(), Requests: []*ssmpb.CreatePullRequestCommentRequest{
		{PullRequestComment: review(ssmpb.PullRequestComment_Review_CHANGE_REQUESTED)}, {PullRequestComment: code("README.md", 2)}}})
	batch := opResult(t, op, err, &ssmpb.BatchCreatePullRequestCommentsResponse{})
	if len(batch.GetPullRequestComments()) != 2 || batch.GetPullRequestComments()[0].GetCode() == nil ||
		batch.GetPullRequestComments()[1].GetReview().GetEffectiveCommitSha() == "" {
		t.Fatalf("BatchCreatePullRequestComments = %v", batch)
	}
	root := batch.GetPullRequestComments()[0]
	if _, err := m.BatchCreatePullRequestComments(as("bob"), &ssmpb.BatchCreatePullRequestCommentsRequest{Parent: pr.GetName(),
		Requests: []*ssmpb.CreatePullRequestCommentRequest{{PullRequestComment: code("README.md", 1)}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a batch with no review: %v", err)
	}
	op, err = m.CreatePullRequestComment(as("alice"), &ssmpb.CreatePullRequestCommentRequest{Parent: pr.GetName(),
		PullRequestComment: &ssmpb.PullRequestComment{CommentDetail: &ssmpb.PullRequestComment_Code_{Code: &ssmpb.PullRequestComment_Code{Body: "fixed", Reply: root.GetName()}}}})
	reply := opResult(t, op, err, &ssmpb.PullRequestComment{})
	if reply.GetCode().GetEffectiveRootComment() != root.GetName() || reply.GetCode().GetPosition().GetPath() != "README.md" {
		t.Fatalf("reply = %v", reply)
	}

	// The author cannot approve; a rule wants one approver, resolved
	// conversations, and no change requests.
	if _, err := m.CreatePullRequestComment(as("alice"), &ssmpb.CreatePullRequestCommentRequest{Parent: pr.GetName(),
		PullRequestComment: review(ssmpb.PullRequestComment_Review_APPROVED)}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("the author approving their own pull request: %v", err)
	}
	op, err = m.CreateBranchRule(ctx, &ssmpb.CreateBranchRuleRequest{Parent: repo, BranchRuleId: "main", BranchRule: &ssmpb.BranchRule{
		IncludePattern: "main", MinimumApprovalsCount: 1, MinimumReviewsCount: 1, RequireCommentsResolved: true}})
	opResult(t, op, err, &ssmpb.BranchRule{})
	merge := func() error {
		_, err := m.MergePullRequest(ctx, &ssmpb.MergePullRequestRequest{Name: pr.GetName()})
		return err
	}
	if err := merge(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "approving reviewers") {
		t.Fatalf("merge with no approvals: %v", err)
	}
	// bob approves, but holds no role on the repository yet.
	op, err = m.CreatePullRequestComment(as("bob"), &ssmpb.CreatePullRequestCommentRequest{Parent: pr.GetName(), PullRequestComment: review(ssmpb.PullRequestComment_Review_APPROVED)})
	opResult(t, op, err, &ssmpb.PullRequestComment{})
	if err := merge(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("merge approved by an account with no role: %v", err)
	}
	ssmState.mu.Lock()
	ssmState.policies[repo] = iampolicy.Stored{Bindings: []iampolicy.Binding{{Role: "roles/securesourcemanager.repoAdmin", Members: []string{"user:bob@example.com"}}}}
	ssmState.mu.Unlock()
	if err := merge(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("merge with an unresolved conversation: %v", err)
	}
	if _, err := m.ResolvePullRequestComments(ctx, &ssmpb.ResolvePullRequestCommentsRequest{Parent: pr.GetName(), Names: []string{root.GetName()}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("resolving part of a conversation without auto_fill: %v", err)
	}
	op, err = m.ResolvePullRequestComments(ctx, &ssmpb.ResolvePullRequestCommentsRequest{Parent: pr.GetName(), Names: []string{reply.GetName()}, AutoFill: true})
	resolved := opResult(t, op, err, &ssmpb.ResolvePullRequestCommentsResponse{})
	if len(resolved.GetPullRequestComments()) != 2 || !resolved.GetPullRequestComments()[0].GetCode().GetResolved() {
		t.Fatalf("ResolvePullRequestComments = %v", resolved)
	}

	// A new commit makes the approval stale.
	write("README.md", "hello\nworld\nagain\n")
	git("commit", "--quiet", "-am", "third")
	git("push", "--quiet", "origin", "feature")
	if err := merge(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "approving reviewers") {
		t.Fatalf("merge with a stale approval: %v", err)
	}
	op, err = m.CreatePullRequestComment(as("bob"), &ssmpb.CreatePullRequestCommentRequest{Parent: pr.GetName(), PullRequestComment: review(ssmpb.PullRequestComment_Review_APPROVED)})
	opResult(t, op, err, &ssmpb.PullRequestComment{})
	if err := merge(); err != nil {
		t.Fatalf("merge approved by an approver: %v", err)
	}
	// The merge is a push to main, then the pull request closes merged.
	if p := waitFor("push", 2)[1]; p.body["ref"] != "refs/heads/main" {
		t.Errorf("the merge's push delivery = %v", p.body["ref"])
	}
	var closed hookCall
	for _, c := range waitFor("pull_request", 2) {
		if c.body["action"] == "closed" {
			closed = c
		}
	}
	if closed.body == nil || closed.body["pull_request"].(map[string]any)["merged"] != true {
		t.Fatalf("no closed, merged pull_request delivery")
	}
	if ds := SourceManagerHookDeliveries(h.GetName()); len(ds) < 5 || ds[0].Status != 200 {
		t.Errorf("deliveries = %d, first %+v", len(ds), ds)
	}
	if err := SourceManagerTestDelivery(ctx, h.GetName()); err != nil {
		t.Errorf("SourceManagerTestDelivery: %v", err)
	}

	// The comments survive a restart; deleting the repository drops them.
	if err := UseSourceManagerRoot(ssmState.root); err != nil {
		t.Fatal(err)
	}
	all, err := m.ListPullRequestComments(ctx, &ssmpb.ListPullRequestCommentsRequest{Parent: pr.GetName()})
	if err != nil || len(all.GetPullRequestComments()) != 6 {
		t.Fatalf("ListPullRequestComments = %d, %v", len(all.GetPullRequestComments()), err)
	}
	if _, err := m.DeleteRepository(ctx, &ssmpb.DeleteRepositoryRequest{Name: repo}); err != nil {
		t.Fatal(err)
	}
	ssmState.mu.Lock()
	left := len(ssmState.hooks) + len(ssmState.prComments)
	ssmState.mu.Unlock()
	if left != 0 {
		t.Errorf("%d hooks and comments left after the repository was deleted", left)
	}
}

func TestSourceManagerGlob(t *testing.T) {
	for _, c := range []struct {
		p, s string
		ok   bool
	}{{"main", "main", true}, {"main", "mainx", false}, {"{main,release*}", "release-1", true}, {"{main,dev}", "feature", false},
		{"dev-*", "dev-a", true}, {"[a-c]x", "bx", true}, {"[!a-c]x", "bx", false}, {"?", "ab", false}} {
		re, err := ssmGlobRE(c.p)
		if err != nil || re.MatchString(c.s) != c.ok {
			t.Errorf("glob %q on %q = %v, %v; want %v", c.p, c.s, err == nil && re.MatchString(c.s), err, c.ok)
		}
	}
	if _, err := ssmGlobRE("{a,b"); err == nil {
		t.Error("an unclosed { compiled")
	}
}
