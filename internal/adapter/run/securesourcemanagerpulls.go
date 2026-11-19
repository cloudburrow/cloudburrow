package run

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// A repository's pull requests (#1856), as Google's API reference and
// "Work with issues and pull requests" describe them: a pull request asks
// to merge its head branch into its base branch, both branches of the
// repository; it is numbered by the server from 1 in each repository
// (its own sequence: Google documents none shared with issues); it is
// OPEN, CLOSED (close_time set, cleared when reopened) or MERGED.
// Merge pull request makes a merge commit of the two tips on the base
// branch, refused with FAILED_PRECONDITION when they conflict, when there
// is nothing to merge, or when the base branch's rule is not met
// (securesourcemanagerbranchrules.go). A rule requiring linear history
// fast-forwards the base branch instead, and refuses a head that is not
// ahead of it. ListPullRequestFileDiffs is the change from the branches'
// merge base to the head, one entry per file with its git patch.
// Reviews and pull request comments are not served.

// ssmMergeIdentity is the merge commit's author and committer: the
// instance authenticates no caller, so it names none.
var ssmMergeIdentity = []string{"GIT_AUTHOR_NAME=Secure Source Manager", "GIT_AUTHOR_EMAIL=noreply@sourcemanager.localhost",
	"GIT_COMMITTER_NAME=Secure Source Manager", "GIT_COMMITTER_EMAIL=noreply@sourcemanager.localhost"}

func shortBranch(ref string) string { return strings.TrimPrefix(ref, "refs/heads/") }

// CreatePullRequest opens a pull request of head into base.
func (m *SecureSourceManagerServer) CreatePullRequest(ctx context.Context, req *ssmpb.CreatePullRequestRequest) (*longrunningpb.Operation, error) {
	in := req.GetPullRequest()
	if strings.TrimSpace(in.GetTitle()) == "" {
		return nil, apierror.InvalidArgument("pull_request.title is required")
	}
	base, head := shortBranch(in.GetBase().GetRef()), shortBranch(in.GetHead().GetRef())
	if base == "" || head == "" {
		return nil, apierror.InvalidArgument("pull_request.base.ref and pull_request.head.ref are required")
	}
	if base == head {
		return nil, apierror.InvalidArgument("the head branch %q is the base branch", head)
	}
	_, dir, err := ssmRepoDir(req.GetParent())
	if err != nil {
		return nil, err
	}
	for _, b := range []string{base, head} {
		if branchSHA(ctx, dir, b) == "" {
			return nil, apierror.NotFound("branch %q not found in the repository", b)
		}
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	if _, _, err := ssmRepoLocked(req.GetParent()); err != nil {
		return nil, err
	}
	now := timestamppb.Now()
	pr := &ssmpb.PullRequest{Name: req.GetParent() + "/pullRequests/" + strconv.Itoa(nextSSMNumberLocked(req.GetParent(), "pullRequests")),
		Title: in.GetTitle(), Body: in.GetBody(), Base: &ssmpb.PullRequest_Branch{Ref: base}, Head: &ssmpb.PullRequest_Branch{Ref: head},
		State: ssmpb.PullRequest_OPEN, CreateTime: now, UpdateTime: now}
	ssmState.pulls[pr.Name] = pr
	saveSSMLocked()
	return m.done(req.GetParent(), pr.Name, "create", withTips(ctx, dir, pr))
}

// withTips is a copy of a pull request with its branches' tips: an open
// one's as they are now, a closed or merged one's as they were.
func withTips(ctx context.Context, dir string, pr *ssmpb.PullRequest) *ssmpb.PullRequest {
	out := proto.Clone(pr).(*ssmpb.PullRequest)
	if out.GetState() == ssmpb.PullRequest_OPEN || out.GetBase().GetSha() == "" {
		out.Base.Sha, out.Head.Sha = branchSHA(ctx, dir, out.GetBase().GetRef()), branchSHA(ctx, dir, out.GetHead().GetRef())
	}
	return out
}

func ssmPullLocked(name string) (*ssmpb.PullRequest, string, error) {
	parent, _, err := ssmChildName(name, "pullRequests")
	if err != nil {
		return nil, "", err
	}
	_, dir, err := ssmRepoLocked(parent)
	if err != nil {
		return nil, "", err
	}
	pr, ok := ssmState.pulls[name]
	if !ok {
		return nil, "", apierror.NotFound("pull request %q not found", name)
	}
	return pr, dir, nil
}

// GetPullRequest reads a pull request.
func (m *SecureSourceManagerServer) GetPullRequest(ctx context.Context, req *ssmpb.GetPullRequestRequest) (*ssmpb.PullRequest, error) {
	ssmState.mu.Lock()
	pr, dir, err := ssmPullLocked(req.GetName())
	if err != nil {
		ssmState.mu.Unlock()
		return nil, err
	}
	pr = proto.Clone(pr).(*ssmpb.PullRequest)
	ssmState.mu.Unlock()
	return withTips(ctx, dir, pr), nil
}

// ListPullRequests lists a repository's pull requests, by number.
func (m *SecureSourceManagerServer) ListPullRequests(ctx context.Context, req *ssmpb.ListPullRequestsRequest) (*ssmpb.ListPullRequestsResponse, error) {
	ssmState.mu.Lock()
	_, dir, err := ssmRepoLocked(req.GetParent())
	if err != nil {
		ssmState.mu.Unlock()
		return nil, err
	}
	var all []*ssmpb.PullRequest
	for _, k := range sortedByNumber(ssmState.pulls, req.GetParent(), "pullRequests") {
		all = append(all, proto.Clone(ssmState.pulls[k]).(*ssmpb.PullRequest))
	}
	ssmState.mu.Unlock()
	page, next, err := ssmPage(all, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	for i, pr := range page {
		page[i] = withTips(ctx, dir, pr)
	}
	return &ssmpb.ListPullRequestsResponse{PullRequests: page, NextPageToken: next}, nil
}

// UpdatePullRequest changes a pull request's title, body or base branch.
func (m *SecureSourceManagerServer) UpdatePullRequest(ctx context.Context, req *ssmpb.UpdatePullRequestRequest) (*longrunningpb.Operation, error) {
	in := req.GetPullRequest()
	paths, err := ssmMaskPaths(req.GetUpdateMask().GetPaths(), []string{"title", "body", "base"}, func(f string) bool {
		return (f == "title" && in.GetTitle() != "") || (f == "body" && in.GetBody() != "") || (f == "base" && in.GetBase().GetRef() != "")
	})
	if err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	pr, dir, err := ssmPullLocked(in.GetName())
	if err != nil {
		return nil, err
	}
	next := proto.Clone(pr).(*ssmpb.PullRequest)
	for _, p := range paths {
		switch p {
		case "title":
			if strings.TrimSpace(in.GetTitle()) == "" {
				return nil, apierror.InvalidArgument("pull_request.title is required")
			}
			next.Title = in.GetTitle()
		case "body":
			next.Body = in.GetBody()
		case "base":
			if pr.GetState() != ssmpb.PullRequest_OPEN {
				return nil, apierror.FailedPrecondition("the base branch of a %s pull request cannot change", strings.ToLower(pr.GetState().String()))
			}
			b := shortBranch(in.GetBase().GetRef())
			if b == pr.GetHead().GetRef() {
				return nil, apierror.InvalidArgument("the head branch %q is the base branch", b)
			}
			if branchSHA(ctx, dir, b) == "" {
				return nil, apierror.NotFound("branch %q not found in the repository", b)
			}
			next.Base = &ssmpb.PullRequest_Branch{Ref: b}
		}
	}
	next.UpdateTime = timestamppb.Now()
	ssmState.pulls[pr.GetName()] = next
	saveSSMLocked()
	return m.done(ssmRepoOf(pr.GetName()), pr.GetName(), "update", withTips(ctx, dir, next))
}

// OpenPullRequest reopens a closed pull request.
func (m *SecureSourceManagerServer) OpenPullRequest(ctx context.Context, req *ssmpb.OpenPullRequestRequest) (*longrunningpb.Operation, error) {
	return m.setPullState(ctx, req.GetName(), ssmpb.PullRequest_OPEN)
}

// ClosePullRequest closes an open pull request without merging it.
func (m *SecureSourceManagerServer) ClosePullRequest(ctx context.Context, req *ssmpb.ClosePullRequestRequest) (*longrunningpb.Operation, error) {
	return m.setPullState(ctx, req.GetName(), ssmpb.PullRequest_CLOSED)
}

func (m *SecureSourceManagerServer) setPullState(ctx context.Context, name string, st ssmpb.PullRequest_State) (*longrunningpb.Operation, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	pr, dir, err := ssmPullLocked(name)
	if err != nil {
		return nil, err
	}
	if pr.GetState() == ssmpb.PullRequest_MERGED {
		return nil, apierror.FailedPrecondition("pull request %q is merged", name)
	}
	if st == ssmpb.PullRequest_OPEN && pr.GetState() == ssmpb.PullRequest_CLOSED {
		for _, b := range []string{pr.GetBase().GetRef(), pr.GetHead().GetRef()} {
			if branchSHA(ctx, dir, b) == "" {
				return nil, apierror.FailedPrecondition("branch %q no longer exists", b)
			}
		}
	}
	if pr.GetState() != st {
		now := timestamppb.Now()
		if st == ssmpb.PullRequest_CLOSED {
			tipped := withTips(ctx, dir, pr)
			pr.Base, pr.Head, pr.CloseTime = tipped.Base, tipped.Head, now
		} else {
			pr.CloseTime = nil
			pr.Base.Sha, pr.Head.Sha = "", ""
		}
		pr.State, pr.UpdateTime = st, now
		saveSSMLocked()
	}
	return m.done(ssmRepoOf(name), name, "update", withTips(ctx, dir, pr))
}

// MergePullRequest merges an open pull request's head into its base.
func (m *SecureSourceManagerServer) MergePullRequest(ctx context.Context, req *ssmpb.MergePullRequestRequest) (*longrunningpb.Operation, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	pr, dir, err := ssmPullLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	if pr.GetState() != ssmpb.PullRequest_OPEN {
		return nil, apierror.FailedPrecondition("pull request %q is %s", req.GetName(), strings.ToLower(pr.GetState().String()))
	}
	baseRef, headRef := pr.GetBase().GetRef(), pr.GetHead().GetRef()
	base, head := branchSHA(ctx, dir, baseRef), branchSHA(ctx, dir, headRef)
	if base == "" || head == "" {
		return nil, apierror.FailedPrecondition("branch %q or %q no longer exists", baseRef, headRef)
	}
	if isAncestor(ctx, dir, head, base) {
		return nil, apierror.FailedPrecondition("nothing to merge: %s is already in %s", headRef, baseRef)
	}
	rule := effectiveBranchRuleLocked(ssmRepoOf(pr.GetName()), baseRef)
	if err := ruleAllowsMerge(rule, baseRef); err != nil {
		return nil, err
	}
	merged := head
	if rule.GetRequireLinearHistory() {
		if !isAncestor(ctx, dir, base, head) {
			return nil, apierror.FailedPrecondition("branch rule %q requires linear history: %s is not ahead of %s, so it cannot be fast-forwarded; rebase it first",
				lastSegment(rule.GetName()), headRef, baseRef)
		}
	} else {
		tree, err := ssmGit(ctx, dir, "merge-tree", "--write-tree", "--no-messages", base, head)
		if err != nil {
			var ge *gitError
			var xe *exec.ExitError
			if errors.As(err, &ge) && errors.As(ge.err, &xe) && xe.ExitCode() == 1 {
				return nil, apierror.FailedPrecondition("%s cannot be merged into %s: they conflict", headRef, baseRef)
			}
			return nil, apierror.Internal(err, "git merge-tree")
		}
		msg := "Merge pull request #" + lastSegment(pr.GetName()) + " from " + headRef + "\n\n" + pr.GetTitle()
		cmd := exec.CommandContext(ctx, "git", "-C", dir, "commit-tree", strings.TrimSpace(strings.SplitN(string(tree), "\n", 2)[0]),
			"-p", base, "-p", head, "-m", msg)
		cmd.Env = append(cmd.Environ(), ssmMergeIdentity...)
		out, err := cmd.Output()
		if err != nil {
			return nil, apierror.Internal(err, "git commit-tree")
		}
		merged = strings.TrimSpace(string(out))
	}
	if _, err := ssmGit(ctx, dir, "update-ref", "-m", "merge pull request #"+lastSegment(pr.GetName()), "refs/heads/"+baseRef, merged, base); err != nil {
		return nil, apierror.Aborted("branch %q moved while the pull request was merged; try again", baseRef)
	}
	now := timestamppb.Now()
	pr.State, pr.UpdateTime, pr.CloseTime = ssmpb.PullRequest_MERGED, now, now
	pr.Base.Sha, pr.Head.Sha = merged, head
	saveSSMLocked()
	return m.done(ssmRepoOf(pr.GetName()), pr.GetName(), "update", proto.Clone(pr))
}

// isAncestor reports whether commit a is an ancestor of (or is) b.
func isAncestor(ctx context.Context, dir, a, b string) bool {
	_, err := ssmGit(ctx, dir, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// ListPullRequestFileDiffs lists the files a pull request changes.
func (m *SecureSourceManagerServer) ListPullRequestFileDiffs(ctx context.Context, req *ssmpb.ListPullRequestFileDiffsRequest) (*ssmpb.ListPullRequestFileDiffsResponse, error) {
	ssmState.mu.Lock()
	pr, dir, err := ssmPullLocked(req.GetName())
	if err != nil {
		ssmState.mu.Unlock()
		return nil, err
	}
	pr = proto.Clone(pr).(*ssmpb.PullRequest)
	ssmState.mu.Unlock()
	pr = withTips(ctx, dir, pr)
	base, head := pr.GetBase().GetSha(), pr.GetHead().GetSha()
	if pr.GetState() == ssmpb.PullRequest_MERGED {
		// After the merge the base holds the head: diff from the base's
		// first parent, the tip the head was merged into.
		if out, err := ssmGit(ctx, dir, "rev-parse", "--verify", "--quiet", base+"^1"); err == nil && base != head {
			base = strings.TrimSpace(string(out))
		}
	}
	if base == "" || head == "" {
		return nil, apierror.FailedPrecondition("branch %q or %q no longer exists", pr.GetBase().GetRef(), pr.GetHead().GetRef())
	}
	mb, err := ssmGit(ctx, dir, "merge-base", base, head)
	if err != nil {
		return nil, apierror.FailedPrecondition("%s and %s share no history", pr.GetBase().GetRef(), pr.GetHead().GetRef())
	}
	from := strings.TrimSpace(string(mb))
	raw, err := ssmGit(ctx, dir, "diff", "--raw", "-z", "--no-renames", from, head)
	if err != nil {
		return nil, apierror.Internal(err, "git diff")
	}
	var diffs []*ssmpb.FileDiff
	f := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		meta := strings.Fields(f[i])
		if len(meta) < 5 {
			continue
		}
		d := &ssmpb.FileDiff{Name: f[i+1], Sha: head, Action: ssmpb.FileDiff_MODIFIED}
		switch meta[4] {
		case "A":
			d.Action = ssmpb.FileDiff_ADDED
		case "D":
			d.Action = ssmpb.FileDiff_DELETED
		}
		diffs = append(diffs, d)
	}
	page, next, err := ssmPage(diffs, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	for _, d := range page {
		patch, err := ssmGit(ctx, dir, "diff", "--no-renames", from, head, "--", d.GetName())
		if err != nil {
			return nil, apierror.Internal(err, "git diff")
		}
		d.Patch = string(patch)
	}
	return &ssmpb.ListPullRequestFileDiffsResponse{FileDiffs: page, NextPageToken: next}, nil
}
