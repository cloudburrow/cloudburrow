package dataform

// v1beta1's workspace branches: a workspace created with
// enable_branch_management checks out, creates and deletes branches of its
// git working copy; SyncWorkspaceRefs fetches the remote's refs (deepening
// a shallow clone); FetchWorkspaceHistory is the log of the checked-out
// branch; DeleteRepositoryLongRunning deletes a repository as an operation.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	b1pb "cloud.google.com/go/dataform/apiv1beta1/dataformpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

func checkBranchName(field, b string) error {
	if b == "" {
		return apierror.InvalidArgument("%s is required", field)
	}
	if _, err := gitCmd(context.Background(), os.TempDir(), nil, "check-ref-format", "--branch", b); err != nil || strings.HasPrefix(b, "-") {
		return apierror.InvalidArgument("%s %q is not a valid git branch name", field, b)
	}
	return nil
}

func (s *Server) extraOf(name string) workspaceExtra {
	s.mu.Lock()
	defer s.mu.Unlock()
	if x := s.wsExtras[name]; x != nil {
		return *x
	}
	return workspaceExtra{}
}

// workspaceBranch is the branch a workspace pulls from and pushes to when
// the request names none: the checked-out branch of a workspace with
// branch management, else its original_branch, else the repository's
// default branch.
func (s *Server) workspaceBranch(ctx context.Context, name, dir string, rm remote) string {
	x := s.extraOf(name)
	if x.BranchManagement {
		if out, err := gitCmd(ctx, dir, nil, "symbolic-ref", "--short", "-q", "HEAD"); err == nil && strings.TrimSpace(out) != "" {
			return strings.TrimSpace(out)
		}
	}
	if x.OriginalBranch != "" {
		return x.OriginalBranch
	}
	return rm.branch
}

func currentBranch(ctx context.Context, dir string) (string, error) {
	out, err := gitCmd(ctx, dir, nil, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		return "", apierror.FailedPrecondition("the workspace has no branch checked out")
	}
	return strings.TrimSpace(out), nil
}

func hasRef(ctx context.Context, dir, ref string) bool {
	_, err := gitCmd(ctx, dir, nil, "rev-parse", "--verify", "-q", ref+"^{commit}")
	return err == nil
}

func (s *Server) fetchCurrentWorkspaceBranch(ctx context.Context, req *b1pb.FetchCurrentWorkspaceBranchRequest) (*b1pb.FetchCurrentWorkspaceBranchResponse, error) {
	_, dir, err := s.workspace(req.GetName())
	if err != nil {
		return nil, err
	}
	defer s.lock(dir)()
	b, err := currentBranch(ctx, dir)
	if err != nil {
		return nil, err
	}
	return &b1pb.FetchCurrentWorkspaceBranchResponse{BranchName: b}, nil
}

func (s *Server) fetchWorkspaceBranches(ctx context.Context, req *b1pb.FetchWorkspaceBranchesRequest) (*b1pb.FetchWorkspaceBranchesResponse, error) {
	_, dir, err := s.workspace(req.GetName())
	if err != nil {
		return nil, err
	}
	var refs []string
	switch req.GetFilter() {
	case b1pb.FetchWorkspaceBranchesRequest_LOCAL_ONLY:
		refs = []string{"refs/heads"}
	case b1pb.FetchWorkspaceBranchesRequest_REMOTE_ONLY:
		refs = []string{"refs/remotes/origin"}
	case b1pb.FetchWorkspaceBranchesRequest_ALL, b1pb.FetchWorkspaceBranchesRequest_BRANCH_FILTER_UNSPECIFIED:
		refs = []string{"refs/heads", "refs/remotes/origin"}
	default:
		return nil, apierror.InvalidArgument("filter %v is not a BranchFilter", req.GetFilter())
	}
	size := req.GetPageSize()
	if size > 1000 {
		size = 1000
	}
	defer s.lock(dir)()
	args := append([]string{"for-each-ref", "--sort=refname",
		"--format=%(refname:short)%00%(objectname)%00%(authorname)%00%(authoremail)%00%(authordate:unix)%00%(contents)%1e"}, refs...)
	out, err := gitCmd(ctx, dir, nil, args...)
	if err != nil {
		return nil, gitFailure(err)
	}
	var all []*b1pb.BranchMetadata
	for _, rec := range strings.Split(out, "\x1e") {
		f := strings.SplitN(strings.TrimLeft(rec, "\n"), "\x00", 6)
		if len(f) < 6 || f[0] == "origin" || f[0] == "origin/HEAD" {
			continue
		}
		sec, _ := strconv.ParseInt(f[4], 10, 64)
		all = append(all, &b1pb.BranchMetadata{BranchName: f[0], LastCommit: &b1pb.CommitLogEntry{
			CommitSha: f[1], Author: &b1pb.CommitAuthor{Name: f[2], EmailAddress: strings.Trim(f[3], "<>")},
			CommitTime: timestamppb.New(time.Unix(sec, 0)), CommitMessage: strings.TrimRight(f[5], "\n")}})
	}
	start, end, next, err := page("branches:"+req.GetName()+req.GetFilter().String(), req.GetPageToken(), size, len(all))
	if err != nil {
		return nil, err
	}
	return &b1pb.FetchWorkspaceBranchesResponse{Branches: all[start:end], NextPageToken: next}, nil
}

func (s *Server) branchManaged(name string) error {
	if !s.extraOf(name).BranchManagement {
		return apierror.FailedPrecondition("Workspace %s was not created with enable_branch_management", name)
	}
	return nil
}

func (s *Server) checkoutWorkspaceBranch(ctx context.Context, req *b1pb.CheckoutWorkspaceBranchRequest) (*emptypb.Empty, error) {
	_, dir, err := s.workspace(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := s.branchManaged(req.GetName()); err != nil {
		return nil, err
	}
	b := req.GetBranch()
	if err := checkBranchName("branch", b); err != nil {
		return nil, err
	}
	if req.GetSourceBranch() != "" && !req.GetCreateIfNotExists() {
		return nil, apierror.InvalidArgument("source_branch can be set only with create_if_not_exists")
	}
	defer s.lock(dir)()
	switch {
	case hasRef(ctx, dir, "refs/heads/"+b) || hasRef(ctx, dir, "refs/remotes/origin/"+b):
		// An existing branch, or the remote's, which git checks out as a
		// local branch tracking it.
		if _, err := gitCmd(ctx, dir, nil, "checkout", "-q", b, "--"); err != nil {
			return nil, gitFailure(err)
		}
	case !req.GetCreateIfNotExists():
		return nil, apierror.NotFound("branch %q not found in the workspace or its remote", b)
	case req.GetSourceBranch() != "":
		src := req.GetSourceBranch()
		ref := "refs/heads/" + src
		if !hasRef(ctx, dir, ref) {
			ref = "refs/remotes/" + src
			if !strings.HasPrefix(src, "origin/") || !hasRef(ctx, dir, ref) {
				return nil, apierror.InvalidArgument("source_branch %q is not a branch FetchWorkspaceBranches returns", src)
			}
		}
		if _, err := gitCmd(ctx, dir, nil, "checkout", "-q", "--no-track", "-b", b, ref); err != nil {
			return nil, gitFailure(err)
		}
	case hasRef(ctx, dir, "HEAD"):
		if _, err := gitCmd(ctx, dir, nil, "checkout", "-q", "-b", b); err != nil {
			return nil, gitFailure(err)
		}
	default:
		// No commit yet: the new branch is the unborn HEAD's.
		if _, err := gitCmd(ctx, dir, nil, "symbolic-ref", "HEAD", "refs/heads/"+b); err != nil {
			return nil, gitFailure(err)
		}
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) deleteBranch(ctx context.Context, req *b1pb.DeleteBranchRequest) (*b1pb.DeleteBranchResponse, error) {
	_, dir, err := s.workspace(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := s.branchManaged(req.GetName()); err != nil {
		return nil, err
	}
	b := req.GetBranch()
	if err := checkBranchName("branch", b); err != nil {
		return nil, err
	}
	defer s.lock(dir)()
	if !hasRef(ctx, dir, "refs/heads/"+b) {
		return nil, apierror.NotFound("branch %q not found in the workspace", b)
	}
	if cur, _ := currentBranch(ctx, dir); cur == b {
		return nil, apierror.FailedPrecondition("branch %q is checked out; check out another branch first", b)
	}
	flag := "-d"
	if req.GetForce() {
		flag = "-D"
	}
	if _, err := gitCmd(ctx, dir, nil, "branch", "-q", flag, b); err != nil {
		return nil, gitFailure(err)
	}
	return &b1pb.DeleteBranchResponse{}, nil
}

func (s *Server) syncWorkspaceRefs(ctx context.Context, req *b1pb.SyncWorkspaceRefsRequest) (*b1pb.SyncWorkspaceRefsResponse, error) {
	if req.GetDeepen() < 0 {
		return nil, apierror.InvalidArgument("deepen must not be negative")
	}
	if b := req.GetRemoteBranchName(); b != "" {
		if err := checkBranchName("remote_branch_name", b); err != nil {
			return nil, err
		}
	}
	dir, rm, unlock, err := s.wsGit(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	defer unlock()
	args := []string{"fetch", "-q"}
	if req.GetDeepen() > 0 {
		if _, err := os.Stat(filepath.Join(dir, ".git", "shallow")); err == nil {
			args = append(args, "--deepen="+strconv.Itoa(int(req.GetDeepen())))
		}
	}
	if b := req.GetRemoteBranchName(); b != "" {
		args = append(args, "origin", "+refs/heads/"+b+":refs/remotes/origin/"+b)
	} else {
		args = append(args, "--prune", "origin")
	}
	if _, err := gitCmd(ctx, dir, rm.conf, args...); err != nil {
		return nil, gitFailure(err)
	}
	return &b1pb.SyncWorkspaceRefsResponse{}, nil
}

func (s *Server) fetchWorkspaceHistory(ctx context.Context, req *b1pb.FetchWorkspaceHistoryRequest) (*b1pb.FetchWorkspaceHistoryResponse, error) {
	_, dir, err := s.workspace(req.GetName())
	if err != nil {
		return nil, err
	}
	defer s.lock(dir)()
	commits, err := history(ctx, dir, "HEAD")
	if err != nil {
		return nil, err
	}
	start, end, next, err := page("wshistory:"+req.GetName(), req.GetPageToken(), req.GetPageSize(), len(commits))
	if err != nil {
		return nil, err
	}
	out := &b1pb.FetchWorkspaceHistoryResponse{NextPageToken: next}
	for _, c := range commits[start:end] {
		e := &b1pb.CommitLogEntry{}
		if err := reread(c, e); err != nil {
			return nil, err
		}
		out.Commits = append(out.Commits, e)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "shallow")); err == nil && end == len(commits) && len(commits) > 0 {
		out.ReachesShallowBoundary = true
	}
	return out, nil
}

// deleteRepositoryLongRunning deletes a repository as an operation. Its
// force deletes compilation results and workflow invocations only: "This
// flag doesn't support deletion of workspaces, release configs or
// workflow configs. If any of such resources exists in the repository,
// the request will fail."
func (s *Server) deleteRepositoryLongRunning(_ context.Context, req *b1pb.DeleteRepositoryLongRunningRequest) (*longrunningpb.Operation, error) {
	name := req.GetName()
	m := repositoryRE.FindStringSubmatch(name)
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/*/locations/*/repositories/*", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.repo(name); err != nil {
		return nil, err
	}
	in := func(k string) bool { return strings.HasPrefix(k, name+"/") }
	kept := 0
	for _, keys := range [][]string{sortedKeys(s.workspaces), sortedKeys(s.releases), sortedKeys(s.workflows)} {
		for _, k := range keys {
			if in(k) {
				kept++
			}
		}
	}
	if kept > 0 {
		return nil, apierror.FailedPrecondition("Repository %s has %d workspaces, release configs or workflow configs; delete them first (force does not delete them)", name, kept)
	}
	var children int64
	for k := range s.compilations {
		if in(k) {
			children++
		}
	}
	for k := range s.invocations {
		if in(k) {
			children++
		}
	}
	if children > 0 && !req.GetForce() {
		return nil, apierror.FailedPrecondition("Repository %s has %d compilation results and workflow invocations; set force to delete them", name, children)
	}
	start := s.now()
	if err := s.deleteRepoLocked(name, true); err != nil {
		return nil, err
	}
	md := &b1pb.DeleteRepositoryLongRunningMetadata{CreateTime: start, EndTime: s.now(), Target: name,
		State: b1pb.DeleteRepositoryLongRunningMetadata_SUCCEEDED, PercentComplete: 100, ChildResourcesCount: children}
	op, err := s.done(m[1], md, &b1pb.DeleteRepositoryLongRunningResponse{})
	if err != nil {
		return nil, err
	}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return op, nil
}
