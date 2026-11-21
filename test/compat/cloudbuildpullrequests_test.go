//go:build compat

package compat

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	securesourcemanager "cloud.google.com/go/securesourcemanager/apiv1"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/google/uuid"
)

// covers: google.devtools.cloudbuild.v1.CloudBuild/CreateBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/ListBuilds, google.cloud.securesourcemanager.v1.SecureSourceManager/CreatePullRequest, google.cloud.securesourcemanager.v1.SecureSourceManager/CreatePullRequestComment
//
// TestCloudBuildPullRequestTriggers (#2077), with the official cloudbuild,
// developerconnect and securesourcemanager clients over gRPC on the Run
// port: BuildTriggers on a Developer Connect link to a Secure Source
// Manager repository, with repositoryEventConfig.pullRequest filters as
// Google's BuildTrigger reference documents them
// (<https://docs.cloud.google.com/build/docs/api/reference/rest/v1/projects.locations.triggers>,
// read 2026-11-20). A pull request of feature into main builds the head
// commit with the pull request substitutions _HEAD_BRANCH, _BASE_BRANCH,
// _HEAD_REPO_URL and _PR_NUMBER, and COMMIT_SHA, SHORT_SHA, BRANCH_NAME and
// REF_NAME of the head (<https://docs.cloud.google.com/build/docs/configuring-builds/substitute-variable-values>).
// COMMENTS_ENABLED builds nothing until the owner comments /gcbrun, then
// builds the head; COMMENTS_ENABLED_FOR_EXTERNAL_CONTRIBUTORS_ONLY builds
// the owner's pull request at once, and an external contributor's (a
// service account with no repository role) only on the owner's /gcbrun,
// not on its own. A pull request into develop builds nothing on the
// ^main$ trigger, and builds on the invertRegex one.
func TestCloudBuildPullRequestTriggers(t *testing.T) {
	h := New(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: builds run with the host's Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	parent := runParent(h)
	l := newSSMLink(ctx, t, h, parent, "cbpr")
	cb := cloudBuildClient(ctx, t, h)
	ssm := ssmClient(t, h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]

	l.git("clone", "--quiet", l.repo.GetUris().GetGitHttps(), ".")
	l.git("checkout", "--quiet", "-b", "main")
	l.commit("first", map[string]string{"README": "app\n"})
	l.git("push", "--quiet", "origin", "main")
	l.git("checkout", "--quiet", "-b", "develop")
	l.git("push", "--quiet", "origin", "develop")

	echo := "echo $_HEAD_BRANCH $_BASE_BRANCH $_PR_NUMBER $_HEAD_REPO_URL $COMMIT_SHA $SHORT_SHA $BRANCH_NAME $REF_NAME"
	build := &cbpb.Build{Steps: []*cbpb.BuildStep{{Name: stepImage(), Entrypoint: "sh", Args: []string{"-c", echo}}}}
	trigger := func(name, branch string, invert bool, cc cbpb.PullRequestFilter_CommentControl) *cbpb.BuildTrigger {
		t.Helper()
		made, err := cb.CreateBuildTrigger(ctx, &cbpb.CreateBuildTriggerRequest{Parent: parent, ProjectId: h.Project(),
			Trigger: &cbpb.BuildTrigger{Name: name + "-" + suffix, BuildTemplate: &cbpb.BuildTrigger_Build{Build: build},
				RepositoryEventConfig: &cbpb.RepositoryEventConfig{Repository: l.link.GetName(),
					Filter: &cbpb.RepositoryEventConfig_PullRequest{PullRequest: &cbpb.PullRequestFilter{
						GitRef: &cbpb.PullRequestFilter_Branch{Branch: branch}, InvertRegex: invert, CommentControl: cc}}}}})
		if err != nil {
			t.Fatalf("CreateBuildTrigger %s with a pull request filter: %v", name, err)
		}
		t.Cleanup(func() {
			_ = cb.DeleteBuildTrigger(context.Background(), &cbpb.DeleteBuildTriggerRequest{Name: made.GetResourceName()})
		})
		if got := made.GetRepositoryEventConfig().GetPullRequest(); got.GetBranch() != branch || got.GetCommentControl() != cc || got.GetInvertRegex() != invert {
			t.Errorf("the trigger's pull request filter = %v", got)
		}
		return made
	}
	plain := trigger("pr", "^main$", false, cbpb.PullRequestFilter_COMMENTS_DISABLED)
	gated := trigger("gated", "^main$", false, cbpb.PullRequestFilter_COMMENTS_ENABLED)
	external := trigger("ext", "^main$", false, cbpb.PullRequestFilter_COMMENTS_ENABLED_FOR_EXTERNAL_CONTRIBUTORS_ONLY)
	inverted := trigger("notmain", "^main$", true, cbpb.PullRequestFilter_COMMENTS_DISABLED)

	openPR := func(c *securesourcemanager.Client, branch, base string) *ssmpb.PullRequest {
		t.Helper()
		l.git("checkout", "--quiet", "-B", branch, "main")
		l.commit(branch, map[string]string{branch + ".txt": branch + "\n"})
		l.git("push", "--quiet", "origin", branch)
		op, err := c.CreatePullRequest(ctx, &ssmpb.CreatePullRequestRequest{Parent: l.repo.GetName(), PullRequest: &ssmpb.PullRequest{
			Title: branch, Base: &ssmpb.PullRequest_Branch{Ref: base}, Head: &ssmpb.PullRequest_Branch{Ref: branch}}})
		if err != nil {
			t.Fatalf("CreatePullRequest %s into %s: %v", branch, base, err)
		}
		pr, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("CreatePullRequest %s into %s: %v", branch, base, err)
		}
		return pr
	}
	comment := func(c *securesourcemanager.Client, pr *ssmpb.PullRequest, body string) {
		t.Helper()
		op, err := c.CreatePullRequestComment(ctx, &ssmpb.CreatePullRequestCommentRequest{Parent: pr.GetName(),
			PullRequestComment: &ssmpb.PullRequestComment{CommentDetail: &ssmpb.PullRequestComment_Comment_{
				Comment: &ssmpb.PullRequestComment_Comment{Body: body}}}})
		if err == nil {
			_, err = op.Wait(ctx)
		}
		if err != nil {
			t.Fatalf("CreatePullRequestComment %q: %v", body, err)
		}
	}
	// waitBuilds waits for a trigger's builds of a commit, until want of
	// them or a short while for none.
	waitBuilds := func(tr *cbpb.BuildTrigger, sha string, want int) []*cbpb.Build {
		t.Helper()
		wait := 3 * time.Minute
		if want == 0 {
			wait = 5 * time.Second
		}
		var got []*cbpb.Build
		for deadline := time.Now().Add(wait); time.Now().Before(deadline); time.Sleep(time.Second) {
			if got = triggerBuilds(ctx, t, cb, parent, h.Project(), tr.GetId())[sha]; want > 0 && len(got) >= want {
				break
			}
		}
		if len(got) != want {
			t.Errorf("trigger %s has %d builds of %s; want %d", tr.GetName(), len(got), shortCommit(sha), want)
		}
		return got
	}

	pr := openPR(ssm, "feature", "main")
	head := l.git("rev-parse", "HEAD")
	number := pr.GetName()[strings.LastIndex(pr.GetName(), "/")+1:]
	if bs := waitBuilds(plain, head, 1); len(bs) == 1 {
		want := "echo feature main " + number + " " + l.link.GetCloneUri() + " " + head + " " + head[:7] + " feature feature"
		if args := bs[0].GetSteps()[0].GetArgs(); len(args) != 2 || args[1] != want {
			t.Errorf("the pull request build's step = %q; want %q", args, want)
		}
		for k, v := range map[string]string{"_HEAD_BRANCH": "feature", "_BASE_BRANCH": "main", "_PR_NUMBER": number, "_HEAD_REPO_URL": l.link.GetCloneUri()} {
			if got := bs[0].GetSubstitutions()[k]; got != v {
				t.Errorf("the build's substitution %s = %q; want %q", k, got, v)
			}
		}
		if src := bs[0].GetSource().GetGitSource(); src.GetRevision() != head || src.GetUrl() != l.link.GetCloneUri() {
			t.Errorf("the build's source = %v; want the head commit %s of %s", src, head, l.link.GetCloneUri())
		}
	}
	waitBuilds(external, head, 1) // the owner writes to the repository
	waitBuilds(gated, head, 0)
	waitBuilds(inverted, head, 0)
	comment(ssm, pr, "lgtm")
	waitBuilds(gated, head, 0)
	comment(ssm, pr, "/gcbrun")
	waitBuilds(gated, head, 1)

	// Into develop: not ^main$, so only the inverted trigger builds.
	openPR(ssm, "topic", "develop")
	devHead := l.git("rev-parse", "HEAD")
	waitBuilds(inverted, devHead, 1)
	for _, tr := range []*cbpb.BuildTrigger{plain, gated, external} {
		waitBuilds(tr, devHead, 0)
	}

	// An external contributor's pull request waits for a writer's /gcbrun.
	outsider, _ := ssmClientAs(t, h, "cbpr-ext")
	ext := openPR(outsider, "outside", "main")
	extHead := l.git("rev-parse", "HEAD")
	waitBuilds(plain, extHead, 1)
	waitBuilds(external, extHead, 0)
	comment(outsider, ext, "/gcbrun") // not a writer: builds nothing
	waitBuilds(external, extHead, 0)
	comment(ssm, ext, "/gcbrun")
	waitBuilds(external, extHead, 1)
}

func shortCommit(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
