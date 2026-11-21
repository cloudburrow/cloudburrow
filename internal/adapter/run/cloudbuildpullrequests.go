package run

import (
	"context"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"google.golang.org/protobuf/proto"
)

// Cloud Build triggers on pull request events (#2077): a BuildTrigger on a
// Developer Connect link to one of this instance's Secure Source Manager
// repositories (cloudbuilddevconnect.go) whose repositoryEventConfig has a
// pullRequest filter, as Google's BuildTrigger reference documents it:
//
//   - branch, an RE2 matched against the pull request's base branch (the
//     branch it would merge into), invertRegex reversing it;
//   - commentControl: COMMENTS_DISABLED builds on every event;
//     COMMENTS_ENABLED builds only when the author has repository write
//     permission and /gcbrun is in the pull request's description, or when
//     a user with write permission comments /gcbrun;
//     COMMENTS_ENABLED_FOR_EXTERNAL_CONTRIBUTORS_ONLY builds when the
//     author has write permission, else only when a writer comments
//     /gcbrun. A writer's /gcbrun comment builds under every setting,
//     COMMENTS_DISABLED too ("Only PR comments that contain /gcbrun will
//     trigger builds").
//
// The events are the ones a Secure Source Manager pull request's own
// triggers file runs on (securesourcemanagerpullevents.go): opened or
// reopened, its base branch changed, its head branch pushed; and a /gcbrun
// comment. The build is of the head commit, with the trigger's
// includedFiles, ignoredFiles and filter applied to the files the pull
// request changes and to its substitutions, and with Google's pull request
// substitutions _HEAD_BRANCH, _BASE_BRANCH, _HEAD_REPO_URL and _PR_NUMBER
// beside the repository ones (COMMIT_SHA, SHORT_SHA, REVISION_ID,
// REPO_NAME, REPO_FULL_NAME, and BRANCH_NAME and REF_NAME the head
// branch). A pull request here is within one repository, so the head
// repository's URL is the link's clone URI.
//
// Repository write permission: the instance's own account, or a principal
// bound, unconditionally, to Secure Source Manager's repoWriter, repoAdmin
// or instanceOwner role on the repository's or its instance's stored
// policy (securesourcemanageriam.go).

// gcbrun is the comment command that runs a pull request's builds.
const gcbrun = "/gcbrun"

// ssmWriterRoles grant write permission on a repository or more.
var ssmWriterRoles = map[string]bool{"roles/securesourcemanager.repoWriter": true,
	"roles/securesourcemanager.repoAdmin": true, "roles/securesourcemanager.instanceOwner": true}

// ssmRepoWriterLocked reports whether an account has write permission on a
// repository.
func ssmRepoWriterLocked(repo, account string) bool {
	if account == ssmOwnerAccount {
		return true
	}
	if account == "" {
		return false
	}
	members := map[string]bool{"user:" + account: true, "serviceAccount:" + account: true, "allUsers": true, "allAuthenticatedUsers": true}
	for _, res := range []string{repo, ssmState.repos[repo].GetInstance()} {
		for _, b := range ssmState.policies[res].Bindings {
			if !ssmWriterRoles[b.Role] || b.Condition != nil {
				continue
			}
			for _, m := range b.Members {
				if kind, email, ok := strings.Cut(m, ":"); ok {
					m = kind + ":" + strings.ToLower(email)
				}
				if members[m] {
					return true
				}
			}
		}
	}
	return false
}

// pullFilterAccepts reports whether a pull request filter matches a base
// branch.
func pullFilterAccepts(f *cbpb.PullRequestFilter, base string) bool {
	re, err := regexp.Compile(f.GetBranch())
	return err == nil && re.MatchString(base) != f.GetInvertRegex()
}

// pullEventBuilds reports whether a pull request event that is not a
// /gcbrun comment builds under a comment control.
func pullEventBuilds(c cbpb.PullRequestFilter_CommentControl, authorWrites bool, body string) bool {
	switch c {
	case cbpb.PullRequestFilter_COMMENTS_ENABLED:
		return authorWrites && strings.Contains(body, gcbrun)
	case cbpb.PullRequestFilter_COMMENTS_ENABLED_FOR_EXTERNAL_CONTRIBUTORS_ONLY:
		return authorWrites
	}
	return true
}

// commentRunsBuilds reports whether a pull request comment is a /gcbrun.
func commentRunsBuilds(c *ssmpb.PullRequestComment) bool {
	return strings.Contains(c.GetComment().GetBody(), gcbrun) || strings.Contains(c.GetReview().GetBody(), gcbrun)
}

// runDevConnectPullTriggers runs the pull request triggers on links to a
// pull request's repository: for an event, when commenter is empty, or for
// commenter's /gcbrun comment.
func (m *SecureSourceManagerServer) runDevConnectPullTriggers(ctx context.Context, pull, commenter string) {
	builds := m.builds
	if builds == nil {
		return
	}
	repo := ssmRepoOf(pull)
	ssmState.mu.Lock()
	pr, dir, err := ssmPullLocked(pull)
	authorWrites, commenterWrites := false, false
	if err == nil {
		pr = proto.Clone(pr).(*ssmpb.PullRequest)
		authorWrites = ssmRepoWriterLocked(repo, ssmState.authors[pull])
		commenterWrites = commenter != "" && ssmRepoWriterLocked(repo, commenter)
	}
	ssmState.mu.Unlock()
	if err != nil || pr.GetState() != ssmpb.PullRequest_OPEN || (commenter != "" && !commenterWrites) {
		return
	}
	triggers := devConnectTriggers(repo)
	if len(triggers) == 0 {
		return
	}
	head := withTips(ctx, dir, pr).GetHead().GetSha()
	if head == "" {
		return
	}
	headRef := "refs/heads/" + pr.GetHead().GetRef()
	var files []string
	filesRead := false
	for _, t := range triggers {
		f := t.GetRepositoryEventConfig().GetPullRequest()
		if t.GetDisabled() || f == nil || !pullFilterAccepts(f, pr.GetBase().GetRef()) {
			continue
		}
		if commenter == "" && !pullEventBuilds(f.GetCommentControl(), authorWrites, pr.GetBody()) {
			continue
		}
		if len(t.GetIncludedFiles()) > 0 || len(t.GetIgnoredFiles()) > 0 {
			if !filesRead {
				for name := range prChangedFiles(ctx, dir, pr) {
					files = append(files, name)
				}
				sort.Strings(files)
				filesRead = true
			}
			if !triggerFilesAccept(t, files) {
				continue
			}
		}
		ssmState.mu.Lock()
		cloneURI := dcState.links[t.GetRepositoryEventConfig().GetRepository()].GetCloneUri()
		ssmState.mu.Unlock()
		extra := map[string]string{"_HEAD_BRANCH": pr.GetHead().GetRef(), "_BASE_BRANCH": pr.GetBase().GetRef(),
			"_HEAD_REPO_URL": cloneURI, "_PR_NUMBER": lastSegment(pull)}
		if t.GetFilter() != "" {
			p, _, _ := resource.ParseLocation(strings.SplitN(t.GetResourceName(), "/triggers/", 2)[0])
			subs := map[string]string{"PROJECT_ID": p}
			for k, v := range t.GetSubstitutions() {
				subs[k] = v
			}
			for k, v := range devConnectBuiltins(t, repo, headRef, head) {
				subs[k] = v
			}
			for k, v := range extra {
				subs[k] = v
			}
			if ok, err := triggerFilterAccepts(t, subs); err != nil || !ok {
				continue
			}
		}
		op, err := builds.runDevConnectTrigger(ctx, t, dir, headRef, head, extra)
		if err != nil {
			slog.Warn("cloudbuild: pull request trigger's build was refused", "pull_request", pull, "trigger", t.GetName(), "err", err)
			continue
		}
		// Kept with the pull request, as its triggers file's builds are:
		// a branch rule's required status check names the trigger.
		ssmState.mu.Lock()
		if _, ok := ssmState.pulls[pull]; ok {
			ssmState.prBuilds[pull] = append(ssmState.prBuilds[pull],
				ssmPullBuild{Trigger: t.GetName(), SHA: head, Build: strings.Replace(op.GetName(), "/operations/", "/builds/", 1)})
			saveSSMLocked()
		}
		ssmState.mu.Unlock()
	}
}
