package run

import (
	"context"
	"log/slog"
	"os/exec"
	"sort"
	"strings"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"google.golang.org/protobuf/proto"
)

// What a pull request's life runs (#1927). The triggers file's
// pull_request triggers ("pull_request to trigger on a pull request to the
// specified branch(es)", Google's triggers file schema) run when a pull
// request is opened or reopened, when its base branch changes, and when
// its head branch is pushed: the ref filters match the pull request's
// base branch, the file filters the files it changes, and the build is of
// the head commit ("Secure Source Manager will read the commit SHA or Git
// ref when the pull request changes are pulled from"), with REF_NAME the
// head branch. The builds are kept with the pull request: a branch rule's
// required_status_checks name triggers, and each named trigger's latest
// build of the head commit must have succeeded before the pull request
// merges. Merging moves the base branch as a push does, so the base
// branch's push triggers run and the hooks get a push event.

// ssmPullBuild is one build a pull request's trigger ran.
type ssmPullBuild struct {
	Trigger string `json:"trigger"`
	SHA     string `json:"sha"`
	Build   string `json:"build"`
}

// runPullTriggers runs the pull_request triggers for an open pull request.
func (m *SecureSourceManagerServer) runPullTriggers(ctx context.Context, pull string) {
	builds := m.builds
	if builds == nil {
		return
	}
	ssmState.mu.Lock()
	pr, dir, err := ssmPullLocked(pull)
	var repo *ssmpb.Repository
	if err == nil {
		pr = proto.Clone(pr).(*ssmpb.PullRequest)
		repo = proto.Clone(ssmState.repos[ssmRepoOf(pull)]).(*ssmpb.Repository)
	}
	ssmState.mu.Unlock()
	if err != nil || pr.GetState() != ssmpb.PullRequest_OPEN {
		return
	}
	tipped := withTips(ctx, dir, pr)
	head := tipped.GetHead().GetSha()
	if head == "" {
		return
	}
	raw, err := exec.CommandContext(ctx, "git", "-C", dir, "show", "HEAD:"+ssmTriggersFile).Output()
	if err != nil {
		return
	}
	triggers, err := ParseSourceManagerTriggers(raw)
	if err != nil {
		slog.Warn("securesourcemanager: the triggers file does not parse", "repository", repo.GetName(), "err", err)
		return
	}
	var files []string
	for f := range prChangedFiles(ctx, dir, pr) {
		files = append(files, f)
	}
	sort.Strings(files)
	project, location, _ := resource.ParseLocation(strings.SplitN(repo.GetName(), "/repositories/", 2)[0])
	id := lastSegment(repo.GetName())
	for _, t := range triggers {
		if !t.acceptsEvent("pull_request", "refs/heads/"+pr.GetBase().GetRef(), files) {
			continue
		}
		if t.ServiceAccount == "" {
			slog.Warn("securesourcemanager: trigger skipped: serviceAccount is required", "repository", repo.GetName(), "trigger", t.Name)
			continue
		}
		if link := t.DevConnectGitRepositoryLink; link != "" {
			if named, err := devConnectLinkRepo(link); err != nil || named != repo.GetName() {
				slog.Warn("securesourcemanager: trigger skipped: its git repository link does not name this repository", "trigger", t.Name)
				continue
			}
		}
		name, err := m.runPushTrigger(ctx, builds, repo, dir, project, location, id, "refs/heads/"+pr.GetHead().GetRef(), head, t)
		if err != nil {
			slog.Warn("securesourcemanager: pull request trigger's build was refused", "pull_request", pull, "trigger", t.Name, "err", err)
			continue
		}
		ssmState.mu.Lock()
		if _, ok := ssmState.pulls[pull]; ok {
			ssmState.prBuilds[pull] = append(ssmState.prBuilds[pull], ssmPullBuild{Trigger: t.Name, SHA: head, Build: name})
			saveSSMLocked()
		}
		ssmState.mu.Unlock()
	}
}

// headPushed runs the pull_request triggers of the open pull requests
// whose head branch a push moved, in the background.
func (m *SecureSourceManagerServer) headPushed(ctx context.Context, repo, ref string) {
	if !strings.HasPrefix(ref, "refs/heads/") {
		return
	}
	branch := strings.TrimPrefix(ref, "refs/heads/")
	ssmState.mu.Lock()
	var pulls []string
	for _, k := range sortedByNumber(ssmState.pulls, repo, "pullRequests") {
		if pr := ssmState.pulls[k]; pr.GetState() == ssmpb.PullRequest_OPEN && pr.GetHead().GetRef() == branch {
			pulls = append(pulls, k)
		}
	}
	ssmState.mu.Unlock()
	for _, p := range pulls {
		m.runPullTriggers(ctx, p)
	}
}

// SourceManagerPullBuild is one build a pull request's trigger ran, for
// the console's checks.
type SourceManagerPullBuild struct{ Trigger, SHA, Build string }

// SourceManagerPullBuilds are the builds a pull request's triggers ran,
// oldest first.
func SourceManagerPullBuilds(pull string) []SourceManagerPullBuild {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	var out []SourceManagerPullBuild
	for _, b := range ssmState.prBuilds[pull] {
		out = append(out, SourceManagerPullBuild{Trigger: b.Trigger, SHA: b.SHA, Build: b.Build})
	}
	return out
}

// statusChecksLocked refuses a merge whose required status checks have not
// passed on the head commit: each check names a trigger, whose latest
// build of head must have succeeded.
func statusChecksLocked(rule *ssmpb.BranchRule, pull, head, branch string) error {
	for _, c := range rule.GetRequiredStatusChecks() {
		build := ""
		for _, b := range ssmState.prBuilds[pull] {
			if b.Trigger == c.GetContext() && b.SHA == head {
				build = b.Build
			}
		}
		id := lastSegment(rule.GetName())
		if build == "" {
			return apierror.FailedPrecondition("branch rule %q on %s requires the status check %q, which has not run on %s", id, branch, c.GetContext(), shortSHA(head))
		}
		cloudBuilds.mu.Lock()
		b, ok := cloudBuilds.builds[build]
		st := cbpb.Build_STATUS_UNKNOWN
		if ok {
			st = b.GetStatus()
		}
		cloudBuilds.mu.Unlock()
		if st != cbpb.Build_SUCCESS {
			return apierror.FailedPrecondition("branch rule %q on %s requires the status check %q to succeed: its build of %s is %s", id, branch, c.GetContext(), shortSHA(head), st)
		}
	}
	return nil
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
