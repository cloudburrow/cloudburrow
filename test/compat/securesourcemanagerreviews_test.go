//go:build compat

package compat

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

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	credentialspb "cloud.google.com/go/iam/credentials/apiv1/credentialspb"
	securesourcemanager "cloud.google.com/go/securesourcemanager/apiv1"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// ssmHookSink is a local webhook target that records what it is sent.
type ssmHookSink struct {
	mu    sync.Mutex
	calls []ssmHookCall
	srv   *httptest.Server
}

type ssmHookCall struct {
	query  string
	header http.Header
	body   map[string]any
}

func newSSMHookSink(t *testing.T) *ssmHookSink {
	s := &ssmHookSink{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var v map[string]any
		_ = json.Unmarshal(b, &v)
		s.mu.Lock()
		if r.Method == http.MethodPost {
			s.calls = append(s.calls, ssmHookCall{query: r.URL.RawQuery, header: r.Header.Clone(), body: v})
		}
		s.mu.Unlock()
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// wait returns the deliveries of an event once there are n of them.
func (s *ssmHookSink) wait(t *testing.T, event string, n int) []ssmHookCall {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		s.mu.Lock()
		var got []ssmHookCall
		for _, c := range s.calls {
			if c.header.Get("X-SecureSourceManager-Event") == event {
				got = append(got, c)
			}
		}
		s.mu.Unlock()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d %s deliveries, want %d", len(got), event, n)
		}
	}
}

// ssmClientAs is the official client calling as a service account of the
// test's own: its token is minted through IAM Credentials, as the ADC
// fixture's account granted Token Creator on it.
func ssmClientAs(t *testing.T, h *Harness, prefix string) (*securesourcemanager.Client, string) {
	t.Helper()
	me, source := instanceAccount(t)
	sa := newServiceAccount(t, h, iamProject(t, h, prefix+"-"), prefix) // a project per account, so a test can have several
	grantOn(t, h, sa.GetName(), "roles/iam.serviceAccountTokenCreator", "serviceAccount:"+me)
	tok, err := iamCredentialsAs(t, h, source).GenerateAccessToken(h.Context(), &credentialspb.GenerateAccessTokenRequest{
		Name: "projects/-/serviceAccounts/" + sa.GetEmail(), Scope: []string{"https://www.googleapis.com/auth/cloud-platform"}})
	if err != nil {
		t.Fatalf("GenerateAccessToken: %v", err)
	}
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: tok.GetAccessToken()})
	c, err := securesourcemanager.NewClient(h.Context(), append(runClientOptions(h),
		option.WithGRPCDialOption(grpc.WithPerRPCCredentials(plainTokenCreds{ts})))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, sa.GetEmail()
}

func ssmReview(a ssmpb.PullRequestComment_Review_ActionType, body string) *ssmpb.PullRequestComment {
	return &ssmpb.PullRequestComment{CommentDetail: &ssmpb.PullRequestComment_Review_{Review: &ssmpb.PullRequestComment_Review{ActionType: a, Body: body}}}
}

func ssmCode(path string, line int64, body string) *ssmpb.PullRequestComment {
	return &ssmpb.PullRequestComment{CommentDetail: &ssmpb.PullRequestComment_Code_{Code: &ssmpb.PullRequestComment_Code{Body: body,
		Position: &ssmpb.PullRequestComment_Position{Path: path, Line: line}}}}
}

// covers: google.cloud.securesourcemanager.v1.SecureSourceManager/CreateHook, google.cloud.securesourcemanager.v1.SecureSourceManager/GetHook, google.cloud.securesourcemanager.v1.SecureSourceManager/ListHooks, google.cloud.securesourcemanager.v1.SecureSourceManager/UpdateHook, google.cloud.securesourcemanager.v1.SecureSourceManager/DeleteHook, google.cloud.securesourcemanager.v1.SecureSourceManager/CreatePullRequestComment, google.cloud.securesourcemanager.v1.SecureSourceManager/BatchCreatePullRequestComments, google.cloud.securesourcemanager.v1.SecureSourceManager/GetPullRequestComment, google.cloud.securesourcemanager.v1.SecureSourceManager/ListPullRequestComments, google.cloud.securesourcemanager.v1.SecureSourceManager/UpdatePullRequestComment, google.cloud.securesourcemanager.v1.SecureSourceManager/DeletePullRequestComment, google.cloud.securesourcemanager.v1.SecureSourceManager/ResolvePullRequestComments, google.cloud.securesourcemanager.v1.SecureSourceManager/UnresolvePullRequestComments
//
// TestSecureSourceManagerReviewsAndHooks (#1927), with the official client
// over gRPC on the Run port, as two accounts (the instance's own and a
// service account whose token IAM Credentials mints): a hook on a local
// listener gets push, pull_request and pull_request_comment events with
// Google's headers, the sensitive query string and the branch filter, and
// reads back with the query string hidden; a pull request is reviewed as
// the web interface does (Start review: a batch of one review and code
// comments; a reply; Resolve conversation), the author cannot approve it,
// and a branch rule requiring an approval and resolved conversations
// refuses the merge until the other account approves the head commit and
// the conversation is resolved; the merge is a push to the base branch.
func TestSecureSourceManagerReviewsAndHooks(t *testing.T) {
	h := New(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_RESOURCEMANAGER", EnvResourceManager)
	h.Endpoint(EnvMetadata)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	repo, git := ssmPushed(t, h, suffix)
	c := ssmClient(t, h)
	dev, devEmail := ssmClientAs(t, h, "ssm-dev")
	ctx := h.Context()
	must := func(args ...string) string {
		t.Helper()
		out, err := git(args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return out
	}

	sink := newSSMHookSink(t)
	if _, err := c.CreateHook(ctx, &ssmpb.CreateHookRequest{Parent: repo.GetName(), HookId: "Bad_ID", Hook: &ssmpb.Hook{TargetUri: sink.srv.URL}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateHook with an invalid ID: %v", err)
	}
	op, err := c.CreateHook(ctx, &ssmpb.CreateHookRequest{Parent: repo.GetName(), HookId: "ci", Hook: &ssmpb.Hook{
		TargetUri: sink.srv.URL + "/webhook", SensitiveQueryString: "key=k1&secret=s1",
		Events:     []ssmpb.Hook_HookEventType{ssmpb.Hook_PUSH, ssmpb.Hook_PULL_REQUEST, ssmpb.Hook_PULL_REQUEST_COMMENT},
		PushOption: &ssmpb.Hook_PushOption{BranchFilter: "{main,release-*}"}}})
	if err != nil {
		t.Fatalf("CreateHook: %v", err)
	}
	hook, err := op.Wait(ctx)
	if err != nil || hook.GetName() != repo.GetName()+"/hooks/ci" || hook.GetUid() == "" || hook.GetCreateTime() == nil ||
		hook.GetSensitiveQueryString() == "key=k1&secret=s1" {
		t.Fatalf("CreateHook = %v, %v", hook, err)
	}
	if got, err := c.GetHook(ctx, &ssmpb.GetHookRequest{Name: hook.GetName()}); err != nil || got.GetTargetUri() != sink.srv.URL+"/webhook" ||
		len(got.GetEvents()) != 3 || got.GetPushOption().GetBranchFilter() != "{main,release-*}" {
		t.Errorf("GetHook = %v, %v", got, err)
	}
	op2, err := c.CreateHook(ctx, &ssmpb.CreateHookRequest{Parent: repo.GetName(), HookId: "off", Hook: &ssmpb.Hook{TargetUri: sink.srv.URL + "/off",
		Events: []ssmpb.Hook_HookEventType{ssmpb.Hook_PUSH}}})
	if err != nil {
		t.Fatalf("CreateHook off: %v", err)
	}
	off, err := op2.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	uop, err := c.UpdateHook(ctx, &ssmpb.UpdateHookRequest{Hook: &ssmpb.Hook{Name: off.GetName(), Disabled: true},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"disabled"}}})
	if err != nil {
		t.Fatalf("UpdateHook: %v", err)
	}
	if off, err = uop.Wait(ctx); err != nil || !off.GetDisabled() {
		t.Fatalf("UpdateHook = %v, %v", off, err)
	}
	var hooks []string
	for it := c.ListHooks(ctx, &ssmpb.ListHooksRequest{Parent: repo.GetName()}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListHooks: %v", err)
		}
		hooks = append(hooks, x.GetName())
	}
	if len(hooks) != 2 {
		t.Errorf("ListHooks = %v", hooks)
	}

	// A push to main is delivered; one to feature is not (the filter).
	_ = os.WriteFile(filepath.Join(ssmWorkDir(t, git), "README.md"), []byte("hello\nagain\n"), 0o644)
	must("commit", "--quiet", "-am", "again")
	must("push", "--quiet", "origin", "main")
	push := sink.wait(t, "push", 1)[0]
	if push.query != "key=k1&secret=s1" || push.header.Get("Content-Type") != "application/json" ||
		len(push.header.Get("X-SecureSourceManager-Delivery")) != 36 || push.body["ref"] != "refs/heads/main" ||
		push.body["repository"].(map[string]any)["clone_url"] != repo.GetUris().GetGitHttps() {
		t.Fatalf("push delivery: %v %v %v", push.query, push.header, push.body["ref"])
	}
	if _, ok := push.header["X-Securesourcemanager-Signature"]; !ok {
		t.Errorf("no X-SecureSourceManager-Signature header")
	}
	if cm := push.body["commits"].([]any); len(cm) != 1 || cm[0].(map[string]any)["modified"].([]any)[0] != "README.md" {
		t.Errorf("push commits = %v", cm)
	}

	// The service account opens a pull request of feature into main.
	pop, err := dev.CreatePullRequest(ctx, &ssmpb.CreatePullRequestRequest{Parent: repo.GetName(), PullRequest: &ssmpb.PullRequest{
		Title: "Add b", Base: &ssmpb.PullRequest_Branch{Ref: "main"}, Head: &ssmpb.PullRequest_Branch{Ref: "feature"}}})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	pr, err := pop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	opened := sink.wait(t, "pull_request", 1)[0]
	if opened.body["action"] != "opened" || opened.body["pull_request"].(map[string]any)["user"].(map[string]any)["email"] != devEmail ||
		opened.body["pull_request"].(map[string]any)["head"].(map[string]any)["ref"] != "feature" {
		t.Fatalf("pull_request opened = %v", opened.body)
	}

	// A general comment, edited and deleted, as three events.
	cop, err := c.CreatePullRequestComment(ctx, &ssmpb.CreatePullRequestCommentRequest{Parent: pr.GetName(),
		PullRequestComment: &ssmpb.PullRequestComment{CommentDetail: &ssmpb.PullRequestComment_Comment_{Comment: &ssmpb.PullRequestComment_Comment{Body: "thanks"}}}})
	if err != nil {
		t.Fatalf("CreatePullRequestComment: %v", err)
	}
	general, err := cop.Wait(ctx)
	if err != nil || general.GetName() != pr.GetName()+"/pullRequestComments/1" || general.GetComment().GetBody() != "thanks" {
		t.Fatalf("CreatePullRequestComment = %v, %v", general, err)
	}
	if _, err := dev.UpdatePullRequestComment(ctx, &ssmpb.UpdatePullRequestCommentRequest{PullRequestComment: &ssmpb.PullRequestComment{Name: general.GetName(),
		CommentDetail: &ssmpb.PullRequestComment_Comment_{Comment: &ssmpb.PullRequestComment_Comment{Body: "x"}}}}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("another account editing the comment: %v", err)
	}
	eop, err := c.UpdatePullRequestComment(ctx, &ssmpb.UpdatePullRequestCommentRequest{PullRequestComment: &ssmpb.PullRequestComment{Name: general.GetName(),
		CommentDetail: &ssmpb.PullRequestComment_Comment_{Comment: &ssmpb.PullRequestComment_Comment{Body: "thanks!"}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"body"}}})
	if err != nil {
		t.Fatalf("UpdatePullRequestComment: %v", err)
	}
	if edited, err := eop.Wait(ctx); err != nil || edited.GetComment().GetBody() != "thanks!" {
		t.Fatalf("UpdatePullRequestComment = %v, %v", edited, err)
	}
	dop, err := c.DeletePullRequestComment(ctx, &ssmpb.DeletePullRequestCommentRequest{Name: general.GetName()})
	if err != nil || dop.Wait(ctx) != nil {
		t.Fatalf("DeletePullRequestComment: %v", err)
	}
	if _, err := c.GetPullRequestComment(ctx, &ssmpb.GetPullRequestCommentRequest{Name: general.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetPullRequestComment after delete: %v", err)
	}
	var actions []string
	for _, d := range sink.wait(t, "pull_request_comment", 3) {
		actions = append(actions, d.body["action"].(string))
		if d.body["is_pull"] != true || d.body["issue"].(map[string]any)["number"] != float64(1) {
			t.Errorf("pull_request_comment = %v", d.body)
		}
	}
	if strings.Join(actions, ",") != "created,edited,deleted" {
		t.Errorf("pull_request_comment actions = %v", actions)
	}

	// Start review: a review requesting changes with a code comment.
	if _, err := c.CreatePullRequestComment(ctx, &ssmpb.CreatePullRequestCommentRequest{Parent: pr.GetName(),
		PullRequestComment: ssmCode("README.md", 1, "x")}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a code comment on a file the pull request does not change: %v", err)
	}
	bop, err := c.BatchCreatePullRequestComments(ctx, &ssmpb.BatchCreatePullRequestCommentsRequest{Parent: pr.GetName(),
		Requests: []*ssmpb.CreatePullRequestCommentRequest{{PullRequestComment: ssmReview(ssmpb.PullRequestComment_Review_CHANGE_REQUESTED, "see the comment")},
			{PullRequestComment: ssmCode("src/b.go", 1, "rename the package")}}})
	if err != nil {
		t.Fatalf("BatchCreatePullRequestComments: %v", err)
	}
	batch, err := bop.Wait(ctx)
	if err != nil || len(batch.GetPullRequestComments()) != 2 {
		t.Fatalf("BatchCreatePullRequestComments = %v, %v", batch, err)
	}
	var root *ssmpb.PullRequestComment
	for _, x := range batch.GetPullRequestComments() {
		if x.GetCode() != nil {
			root = x
		} else if x.GetReview().GetEffectiveCommitSha() == "" {
			t.Errorf("the review has no effective_commit_sha: %v", x)
		}
	}
	if root == nil || root.GetCode().GetResolved() || root.GetCode().GetEffectiveCommitSha() == "" {
		t.Fatalf("the code comment = %v", root)
	}
	if _, err := c.BatchCreatePullRequestComments(ctx, &ssmpb.BatchCreatePullRequestCommentsRequest{Parent: pr.GetName(),
		Requests: []*ssmpb.CreatePullRequestCommentRequest{{PullRequestComment: ssmCode("src/b.go", 1, "x")}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a batch with no review: %v", err)
	}
	rop, err := dev.CreatePullRequestComment(ctx, &ssmpb.CreatePullRequestCommentRequest{Parent: pr.GetName(), PullRequestComment: &ssmpb.PullRequestComment{
		CommentDetail: &ssmpb.PullRequestComment_Code_{Code: &ssmpb.PullRequestComment_Code{Body: "done", Reply: root.GetName()}}}})
	if err != nil {
		t.Fatalf("a reply: %v", err)
	}
	reply, err := rop.Wait(ctx)
	if err != nil || reply.GetCode().GetEffectiveRootComment() != root.GetName() || reply.GetCode().GetPosition().GetPath() != "src/b.go" {
		t.Fatalf("the reply = %v, %v", reply, err)
	}

	// The author may not approve; the rule refuses until an approval of
	// head and a resolved conversation.
	if _, err := dev.CreatePullRequestComment(ctx, &ssmpb.CreatePullRequestCommentRequest{Parent: pr.GetName(),
		PullRequestComment: ssmReview(ssmpb.PullRequestComment_Review_APPROVED, "")}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("the author approving: %v", err)
	}
	brop, err := c.CreateBranchRule(ctx, &ssmpb.CreateBranchRuleRequest{Parent: repo.GetName(), BranchRuleId: "main",
		BranchRule: &ssmpb.BranchRule{IncludePattern: "main", MinimumReviewsCount: 1, MinimumApprovalsCount: 1, RequireCommentsResolved: true}})
	if err != nil {
		t.Fatalf("CreateBranchRule: %v", err)
	}
	if _, err := brop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	merge := func() error {
		op, err := c.MergePullRequest(ctx, &ssmpb.MergePullRequestRequest{Name: pr.GetName()})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}
	if err := merge(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("merge with no approval: %v", err)
	}
	aop, err := c.CreatePullRequestComment(ctx, &ssmpb.CreatePullRequestCommentRequest{Parent: pr.GetName(),
		PullRequestComment: ssmReview(ssmpb.PullRequestComment_Review_APPROVED, "LGTM")})
	if err != nil {
		t.Fatalf("approving: %v", err)
	}
	if _, err := aop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := merge(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("merge with an unresolved conversation: %v", err)
	}
	if _, err := c.ResolvePullRequestComments(ctx, &ssmpb.ResolvePullRequestCommentsRequest{Parent: pr.GetName(), Names: []string{root.GetName()}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("resolving part of a conversation: %v", err)
	}
	resop, err := c.ResolvePullRequestComments(ctx, &ssmpb.ResolvePullRequestCommentsRequest{Parent: pr.GetName(), Names: []string{root.GetName()}, AutoFill: true})
	if err != nil {
		t.Fatalf("ResolvePullRequestComments: %v", err)
	}
	res, err := resop.Wait(ctx)
	if err != nil || len(res.GetPullRequestComments()) != 2 || !res.GetPullRequestComments()[1].GetCode().GetResolved() {
		t.Fatalf("ResolvePullRequestComments = %v, %v", res, err)
	}
	unop, err := c.UnresolvePullRequestComments(ctx, &ssmpb.UnresolvePullRequestCommentsRequest{Parent: pr.GetName(), Names: []string{root.GetName(), reply.GetName()}})
	if err != nil {
		t.Fatalf("UnresolvePullRequestComments: %v", err)
	}
	if un, err := unop.Wait(ctx); err != nil || un.GetPullRequestComments()[0].GetCode().GetResolved() {
		t.Fatalf("UnresolvePullRequestComments = %v, %v", un, err)
	}
	if got, err := c.GetPullRequestComment(ctx, &ssmpb.GetPullRequestCommentRequest{Name: reply.GetName()}); err != nil || got.GetCode().GetResolved() {
		t.Errorf("GetPullRequestComment = %v, %v", got, err)
	}
	if resop, err = c.ResolvePullRequestComments(ctx, &ssmpb.ResolvePullRequestCommentsRequest{Parent: pr.GetName(), Names: []string{reply.GetName()}, AutoFill: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := resop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	// The change request is still the owner's latest decisive review?
	// No: its approval came later, so the merge goes through.
	if err := merge(); err != nil {
		t.Fatalf("merge: %v", err)
	}
	var kinds []string
	for it := c.ListPullRequestComments(ctx, &ssmpb.ListPullRequestCommentsRequest{Parent: pr.GetName(), PageSize: 2}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListPullRequestComments: %v", err)
		}
		switch {
		case x.GetReview() != nil:
			kinds = append(kinds, x.GetReview().GetActionType().String())
		case x.GetCode() != nil:
			kinds = append(kinds, "code")
		}
	}
	if strings.Join(kinds, ",") != "code,CHANGE_REQUESTED,code,APPROVED" {
		t.Errorf("ListPullRequestComments = %v", kinds)
	}
	pushes := sink.wait(t, "push", 2)
	if p := pushes[len(pushes)-1]; p.body["ref"] != "refs/heads/main" || len(p.body["commits"].([]any)) == 0 {
		t.Errorf("the merge's push = %v", p.body["ref"])
	}
	var merged bool
	for _, d := range sink.wait(t, "pull_request", 2) {
		merged = merged || (d.body["action"] == "closed" && d.body["pull_request"].(map[string]any)["merged"] == true)
	}
	if !merged {
		t.Errorf("no closed and merged pull_request delivery")
	}
	sink.mu.Lock()
	for _, x := range sink.calls {
		if x.header.Get("X-SecureSourceManager-Event") == "" {
			t.Errorf("a delivery without an event header: %v", x.header)
		}
	}
	sink.mu.Unlock()

	delop, err := c.DeleteHook(ctx, &ssmpb.DeleteHookRequest{Name: hook.GetName()})
	if err != nil || delop.Wait(ctx) != nil {
		t.Fatalf("DeleteHook: %v", err)
	}
	if _, err := c.GetHook(ctx, &ssmpb.GetHookRequest{Name: hook.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetHook after delete: %v", err)
	}
}

// ssmWorkDir is the directory of the clone git runs in.
func ssmWorkDir(t *testing.T, git func(args ...string) (string, error)) string {
	t.Helper()
	dir, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestSecureSourceManagerPullRequestTriggers (#1927), with the official
// Secure Source Manager and Cloud Build clients: a pull_request trigger of
// .cloudbuild/triggers.yaml builds a pull request's head when it is
// opened and again when its head branch is pushed; a branch rule
// requiring that trigger's status check refuses the merge until its build
// of the head has succeeded; merging runs the base branch's push trigger.
func TestSecureSourceManagerPullRequestTriggers(t *testing.T) {
	h := New(t)
	_ = runClient(t, h)
	for _, tool := range []string{"docker", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required", tool)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	cb := cloudBuildClient(ctx, t, h)
	c := ssmClient(t, h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	repo, git := ssmPushed(t, h, suffix)
	work := ssmWorkDir(t, git)
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
	sa := "projects/" + h.Project() + "/serviceAccounts/builder@" + h.Project() + ".iam.gserviceaccount.com"
	write(".cloudbuild/triggers.yaml", `triggers:
- name: presubmit
  eventType: pull_request
  includedGitRefs: ^main$
  serviceAccount: `+sa+`
- name: postsubmit
  includedGitRefs: ^main$
  serviceAccount: `+sa+`
`)
	write(".cloudbuild/cloudbuild.yaml", `steps:
- name: gcr.io/cloud-builders/docker
  args: [build, -t, 'us-central1-docker.pkg.dev/$PROJECT_ID/ssm/${REPO_NAME}:${TRIGGER_NAME}-${SHORT_SHA}', .]
`)
	write("Dockerfile", "FROM scratch\nCOPY README.md /README.md\n")
	must("add", ".")
	must("commit", "--quiet", "-m", "triggers")
	must("push", "--quiet", "origin", "main")
	must("checkout", "--quiet", "-B", "feature", "main")
	write("README.md", "hello\nfrom feature\n")
	must("commit", "--quiet", "-am", "feature")
	must("push", "--quiet", "--force", "origin", "feature")
	head := must("rev-parse", "HEAD")

	builds := func(rev string) []*cbpb.Build {
		var out []*cbpb.Build
		it := cb.ListBuilds(ctx, &cbpb.ListBuildsRequest{Parent: runParent(h), ProjectId: h.Project()})
		for {
			b, err := it.Next()
			if err == iterator.Done {
				return out
			}
			if err != nil {
				t.Fatalf("ListBuilds: %v", err)
			}
			// The built-in substitutions are not the build's own, so a
			// trigger's builds are told apart by the commit they build: a
			// pull request's head is on no branch the push trigger takes.
			if b.GetSource().GetGitSource().GetRevision() == rev && b.GetSource().GetGitSource().GetUrl() == repo.GetUris().GetGitHttps() {
				out = append(out, b)
			}
		}
	}
	finished := func(trigger, rev string) *cbpb.Build {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
			if bs := builds(rev); len(bs) > 0 && bs[0].GetFinishTime() != nil {
				return bs[0]
			}
		}
		t.Fatalf("no finished %s build of %s", trigger, rev)
		return nil
	}

	brop, err := c.CreateBranchRule(ctx, &ssmpb.CreateBranchRuleRequest{Parent: repo.GetName(), BranchRuleId: "checks",
		BranchRule: &ssmpb.BranchRule{IncludePattern: "main", RequiredStatusChecks: []*ssmpb.BranchRule_Check{{Context: "presubmit"}}}})
	if err != nil {
		t.Fatalf("CreateBranchRule: %v", err)
	}
	if _, err := brop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	pop, err := c.CreatePullRequest(ctx, &ssmpb.CreatePullRequestRequest{Parent: repo.GetName(), PullRequest: &ssmpb.PullRequest{
		Title: "feature", Base: &ssmpb.PullRequest_Branch{Ref: "main"}, Head: &ssmpb.PullRequest_Branch{Ref: "feature"}}})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	pr, err := pop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if b := finished("presubmit", head); b.GetStatus() != cbpb.Build_SUCCESS || len(builds(head)) != 1 {
		t.Fatalf("the presubmit build = %v (%d builds of the head)", b.GetStatus(), len(builds(head)))
	}

	// A push to the head builds the new head; until it succeeds the rule
	// holds the merge.
	write("README.md", "hello\nfrom feature, again\n")
	must("commit", "--quiet", "-am", "again")
	must("push", "--quiet", "origin", "feature")
	head2 := must("rev-parse", "HEAD")
	if b := finished("presubmit", head2); b.GetStatus() != cbpb.Build_SUCCESS {
		t.Fatalf("the presubmit build of the new head = %v", b.GetStatus())
	}
	mop, err := c.MergePullRequest(ctx, &ssmpb.MergePullRequestRequest{Name: pr.GetName()})
	if err != nil {
		t.Fatalf("MergePullRequest: %v", err)
	}
	merged, err := mop.Wait(ctx)
	if err != nil || merged.GetState() != ssmpb.PullRequest_MERGED {
		t.Fatalf("MergePullRequest = %v, %v", merged, err)
	}
	if b := finished("postsubmit", merged.GetBase().GetSha()); b.GetStatus() != cbpb.Build_SUCCESS {
		t.Errorf("the base branch's push trigger after the merge = %v", b.GetStatus())
	}

	// A second pull request whose check has not run is refused.
	must("checkout", "--quiet", "-b", "other", "main")
	must("push", "--quiet", "origin", "other")
	must("checkout", "--quiet", "-b", "other2")
	write("other.txt", "x\n")
	must("add", ".")
	must("commit", "--quiet", "-m", "other")
	must("push", "--quiet", "origin", "other2")
	pop2, err := c.CreatePullRequest(ctx, &ssmpb.CreatePullRequestRequest{Parent: repo.GetName(), PullRequest: &ssmpb.PullRequest{
		Title: "other", Base: &ssmpb.PullRequest_Branch{Ref: "other"}, Head: &ssmpb.PullRequest_Branch{Ref: "other2"}}})
	if err != nil {
		t.Fatal(err)
	}
	pr2, err := pop2.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rule2, err := c.CreateBranchRule(ctx, &ssmpb.CreateBranchRuleRequest{Parent: repo.GetName(), BranchRuleId: "other",
		BranchRule: &ssmpb.BranchRule{IncludePattern: "other", RequiredStatusChecks: []*ssmpb.BranchRule_Check{{Context: "presubmit"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rule2.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.MergePullRequest(ctx, &ssmpb.MergePullRequestRequest{Name: pr2.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("merging a pull request whose status check never ran (its trigger takes only main): %v", err)
	}
}

// TestGcloudSourceManagerHooks (#1927): `gcloud alpha source-manager hooks
// create --target-uri --events`, describe, list, update --disabled and
// delete, through the configuration `cloudburrow gcloud-setup` writes,
// egress blocked; the official client reads what gcloud made, and a push
// reaches the hook's local target.
func TestGcloudSourceManagerHooks(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	c := ssmClient(t, h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	parent := "projects/" + g.project + "/locations/us-central1"
	iop, err := c.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "hi-" + suffix})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	in, err := iop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &ssmpb.DeleteInstanceRequest{Name: in.GetName(), Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	rop, err := c.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "hr-" + suffix,
		Repository: &ssmpb.Repository{Instance: in.GetName()}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	repo, err := rop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sink := newSSMHookSink(t)
	where := []string{"--region=us-central1", "--repository=" + repo.GetName()[strings.LastIndex(repo.GetName(), "/")+1:]}
	g.must(append([]string{"alpha", "source-manager", "hooks", "create", "ci", "--target-uri=" + sink.srv.URL + "/hook", "--events=PUSH,PULL_REQUEST"}, where...)...)
	got, err := c.GetHook(ctx, &ssmpb.GetHookRequest{Name: repo.GetName() + "/hooks/ci"})
	if err != nil || got.GetTargetUri() != sink.srv.URL+"/hook" || len(got.GetEvents()) != 2 || got.GetDisabled() {
		t.Fatalf("GetHook after hooks create = %v, %v", got, err)
	}
	if out := g.must(append([]string{"alpha", "source-manager", "hooks", "describe", "ci", "--format=value(targetUri)"}, where...)...); lastLine(out) != sink.srv.URL+"/hook" {
		t.Errorf("hooks describe = %q", out)
	}
	if out := g.must(append([]string{"alpha", "source-manager", "hooks", "list", "--format=value(name)"}, where...)...); lastLine(out) != "ci" { // gcloud shows the name's last segment
		t.Errorf("hooks list = %q", out)
	}
	if out, err := exec.LookPath("git"); err == nil && out != "" {
		work := t.TempDir()
		for _, args := range [][]string{{"clone", "--quiet", repo.GetUris().GetGitHttps(), "."}, {"checkout", "--quiet", "-b", "main"},
			{"commit", "--quiet", "--allow-empty", "-m", "first"}, {"push", "--quiet", "origin", "main"}} {
			cmd := exec.Command("git", append([]string{"-C", work, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, b)
			}
		}
		if p := sink.wait(t, "push", 1)[0]; p.body["ref"] != "refs/heads/main" {
			t.Errorf("the push delivery = %v", p.body["ref"])
		}
	}
	g.must(append([]string{"alpha", "source-manager", "hooks", "update", "ci", "--disabled"}, where...)...)
	if got, err := c.GetHook(ctx, &ssmpb.GetHookRequest{Name: repo.GetName() + "/hooks/ci"}); err != nil || !got.GetDisabled() {
		t.Errorf("GetHook after hooks update --disabled = %v, %v", got, err)
	}
	g.must(append([]string{"alpha", "source-manager", "hooks", "delete", "ci"}, where...)...)
	if _, err := c.GetHook(ctx, &ssmpb.GetHookRequest{Name: repo.GetName() + "/hooks/ci"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetHook after hooks delete: %v", err)
	}
}
