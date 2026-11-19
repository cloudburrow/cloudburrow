//go:build compat

package compat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestSecureSourceManagerCodeOwners (#1928), with the official client over
// gRPC on the Run port, as three accounts (the instance's own and two
// service accounts whose tokens IAM Credentials mints), as Google's
// "Manage pull request approvals with CODEOWNERS" describes code owners:
// a branch rule's require_code_owner_approval makes a merge wait for each
// changed file's owners from the base branch's CODEOWNERS files (a nested
// file overriding the root one for its directory, a section adding its own
// approval), an owner's approval counting only for the head commit it
// reviewed; a push to the branch of a CODEOWNERS file that does not parse
// is refused, and the pull request's own CODEOWNERS changes do not count.
func TestSecureSourceManagerCodeOwners(t *testing.T) {
	h := New(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_RESOURCEMANAGER", EnvResourceManager)
	h.Endpoint(EnvMetadata)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	repo, git := ssmPushed(t, h, suffix)
	work := ssmWorkDir(t, git)
	c := ssmClient(t, h)
	me, _ := instanceAccount(t)
	dev, _ := ssmClientAs(t, h, "ssm-dev")
	rev, revEmail := ssmClientAs(t, h, "ssm-rev")
	ctx := h.Context()
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

	// main's CODEOWNERS: the instance's account owns everything, but
	// src/CODEOWNERS gives its Go files to the reviewer, and the Security
	// section needs the reviewer for src/ as well.
	write("CODEOWNERS", "# owners\n* "+me+"\n[Security][1]\n/src/ "+revEmail+"\n")
	write("src/CODEOWNERS", "*.go "+revEmail+"   # more local wins\n")
	must("add", ".")
	must("commit", "--quiet", "-m", "owners")
	must("push", "--quiet", "origin", "main")
	op, err := c.CreateBranchRule(ctx, &ssmpb.CreateBranchRuleRequest{Parent: repo.GetName(), BranchRuleId: "owners",
		BranchRule: &ssmpb.BranchRule{IncludePattern: "main", RequireCodeOwnerApproval: true}})
	if err != nil {
		t.Fatalf("CreateBranchRule: %v", err)
	}
	if br, err := op.Wait(ctx); err != nil || !br.GetRequireCodeOwnerApproval() {
		t.Fatalf("CreateBranchRule = %v, %v", br, err)
	}

	// The presubmit check refuses a CODEOWNERS with a syntax error on main.
	must("checkout", "--quiet", "-b", "broken")
	write("CODEOWNERS", "* @everyone\n")
	must("commit", "--quiet", "-am", "broken owners")
	if out, err := git("push", "origin", "broken:main"); err == nil || !strings.Contains(out, "CODEOWNERS:1") {
		t.Fatalf("pushing a CODEOWNERS that does not parse to main: %v\n%s", err, out)
	}

	// feature (src/b.go) also changes README.md, and adds a dedicated
	// .securesourcemanager/CODEOWNERS, which would replace every other
	// file were the head's files read.
	must("checkout", "--quiet", "feature")
	write("README.md", "hello from feature\n")
	write(".securesourcemanager/CODEOWNERS", "* nobody@example.com\n")
	must("add", ".")
	must("commit", "--quiet", "-m", "readme")
	must("push", "--quiet", "origin", "feature")
	pop, err := dev.CreatePullRequest(ctx, &ssmpb.CreatePullRequestRequest{Parent: repo.GetName(), PullRequest: &ssmpb.PullRequest{
		Title: "b and a readme", Base: &ssmpb.PullRequest_Branch{Ref: "main"}, Head: &ssmpb.PullRequest_Branch{Ref: "feature"}}})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	pr, err := pop.Wait(ctx)
	if err != nil {
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
	approve := func(who string) {
		t.Helper()
		cl := c
		if who == "rev" {
			cl = rev
		}
		op, err := cl.CreatePullRequestComment(ctx, &ssmpb.CreatePullRequestCommentRequest{Parent: pr.GetName(),
			PullRequestComment: ssmReview(ssmpb.PullRequestComment_Review_APPROVED, "LGTM")})
		if err != nil {
			t.Fatalf("%s approving: %v", who, err)
		}
		if _, err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := merge(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "code owner") {
		t.Fatalf("merge with no code owner approval: %v", err)
	}
	approve("me")
	if err := merge(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), revEmail) {
		t.Fatalf("merge without the reviewer's approval: %v", err)
	}
	approve("rev")
	// A new commit leaves both approvals stale.
	write("src/b.go", "package a\n\n// B.\n")
	must("commit", "--quiet", "-am", "doc")
	must("push", "--quiet", "origin", "feature")
	if err := merge(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "code owner") {
		t.Fatalf("merge with stale approvals: %v", err)
	}
	approve("rev")
	if err := merge(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "README.md") || strings.Contains(err.Error(), "nobody") {
		t.Fatalf("merge without the root owner's fresh approval: %v", err)
	}
	approve("me")
	if err := merge(); err != nil {
		t.Fatalf("merge: %v", err)
	}
	got, err := c.GetPullRequest(ctx, &ssmpb.GetPullRequestRequest{Name: pr.GetName()})
	if err != nil || got.GetState() != ssmpb.PullRequest_MERGED {
		t.Fatalf("GetPullRequest = %v, %v", got, err)
	}
}
