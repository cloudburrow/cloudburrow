package dataform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// internalBranch is the branch of a repository without a git remote,
// Dataform's own git repository.
const internalBranch = "main"

// gitCmd runs git in dir with extra -c settings, and returns its stdout.
func gitCmd(ctx context.Context, dir string, conf []string, args ...string) (string, error) {
	var full []string
	for _, c := range conf {
		full = append(full, "-c", c)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	// Never read the user's git configuration, prompts or credentials.
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=", "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return stdout.String(), &gitError{args: args, msg: msg, err: err}
	}
	return stdout.String(), nil
}

type gitError struct {
	args []string
	msg  string
	err  error
}

func (e *gitError) Error() string { return "git " + e.args[0] + ": " + e.msg }
func (e *gitError) Unwrap() error { return e.err }

func initBare(ctx context.Context, dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	_, err := gitCmd(ctx, dir, nil, "init", "-q", "--bare", "-b", internalBranch)
	return err
}

// remote is how a repository's git is reached: its URL, its branch, and the
// git settings carrying its access token.
type remote struct {
	url    string
	branch string
	conf   []string
	// internal is a repository without a git remote.
	internal bool
}

// remoteOf resolves a repository's remote, reading its access token.
func (s *Server) remoteOf(ctx context.Context, name string) (remote, error) {
	s.mu.Lock()
	r, err := s.repo(name)
	if err != nil {
		s.mu.Unlock()
		return remote{}, err
	}
	gr := clone(r).GetGitRemoteSettings()
	s.mu.Unlock()
	if gr.GetUrl() == "" {
		return remote{url: s.repoDir(name), branch: internalBranch, internal: true}, nil
	}
	if err := s.checkRemoteURL(gr.GetUrl()); err != nil {
		return remote{}, err
	}
	rm := remote{url: gr.GetUrl(), branch: gr.GetDefaultBranch()}
	if v := gr.GetAuthenticationTokenSecretVersion(); v != "" {
		tok, err := s.secret(ctx, v)
		if err != nil {
			return remote{}, apierror.FailedPrecondition("git remote access token %s: %v", v, err)
		}
		rm.conf = []string{"http.extraHeader=Authorization: Bearer " + strings.TrimSpace(string(tok))}
	}
	return rm, nil
}

func (s *Server) secret(ctx context.Context, name string) ([]byte, error) {
	if s.opts.AccessSecret == nil {
		return nil, errors.New("Secret Manager is not served on this instance")
	}
	return s.opts.AccessSecret(ctx, name)
}

func gitFailure(err error) error {
	var ge *gitError
	if errors.As(err, &ge) {
		return apierror.FailedPrecondition("%s", ge.Error())
	}
	return apierror.Wrap(err)
}

// ---- repository-level files (repositories without a remote) ----

func (s *Server) internalRepo(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.repo(name)
	if err != nil {
		return "", err
	}
	if r.GetGitRemoteSettings().GetUrl() != "" {
		return "", apierror.FailedPrecondition("Repository %s has a git remote; this method needs a repository without git_remote_settings.url", name)
	}
	return s.repoDir(name), nil
}

func revOrHead(ctx context.Context, dir, sha string) (string, error) {
	if sha == "" {
		sha = internalBranch
	}
	out, err := gitCmd(ctx, dir, nil, "rev-parse", "--verify", "-q", sha+"^{commit}")
	if err != nil {
		if sha == internalBranch {
			return "", apierror.FailedPrecondition("the repository has no commits")
		}
		return "", apierror.NotFound("commit %q not found", sha)
	}
	return strings.TrimSpace(out), nil
}

func (s *Server) CommitRepositoryChanges(ctx context.Context, req *dfpb.CommitRepositoryChangesRequest) (*dfpb.CommitRepositoryChangesResponse, error) {
	dir, err := s.internalRepo(req.GetName())
	if err != nil {
		return nil, err
	}
	a := req.GetCommitMetadata().GetAuthor()
	if a.GetName() == "" || a.GetEmailAddress() == "" {
		return nil, apierror.InvalidArgument("commit_metadata.author name and email_address are required")
	}
	if len(req.GetFileOperations()) == 0 {
		return nil, apierror.InvalidArgument("file_operations must not be empty")
	}
	defer s.lock(dir)()
	work, err := os.MkdirTemp(s.opts.DataDir, "commit-")
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	defer os.RemoveAll(work)
	if _, err := gitCmd(ctx, work, nil, "init", "-q", "-b", internalBranch); err != nil {
		return nil, gitFailure(err)
	}
	head := ""
	if out, err := gitCmd(ctx, dir, nil, "rev-parse", "--verify", "-q", internalBranch); err == nil {
		head = strings.TrimSpace(out)
		if _, err := gitCmd(ctx, work, nil, "fetch", "-q", dir, internalBranch); err != nil {
			return nil, gitFailure(err)
		}
		if _, err := gitCmd(ctx, work, nil, "reset", "-q", "--hard", "FETCH_HEAD"); err != nil {
			return nil, gitFailure(err)
		}
	}
	if want := req.GetRequiredHeadCommitSha(); want != "" && want != head {
		return nil, apierror.FailedPrecondition("required_head_commit_sha %s is not the repository's head commit %s", want, head)
	}
	paths := make([]string, 0, len(req.GetFileOperations()))
	for p := range req.GetFileOperations() {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		clean, err := cleanPath(p, false)
		if err != nil {
			return nil, err
		}
		op := req.GetFileOperations()[p]
		full := filepath.Join(work, filepath.FromSlash(clean))
		switch {
		case op.GetWriteFile() != nil:
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return nil, apierror.Wrap(err)
			}
			if err := os.WriteFile(full, op.GetWriteFile().GetContents(), 0o644); err != nil {
				return nil, apierror.Wrap(err)
			}
		case op.GetDeleteFile() != nil:
			if err := os.Remove(full); err != nil {
				return nil, apierror.NotFound("file %q not found", p)
			}
		default:
			return nil, apierror.InvalidArgument("file_operations[%q] must be write_file or delete_file", p)
		}
	}
	if _, err := gitCmd(ctx, work, nil, "add", "-A"); err != nil {
		return nil, gitFailure(err)
	}
	if _, err := gitCmd(ctx, work, identity(a.GetName(), a.GetEmailAddress()), "commit", "-q", "--allow-empty", "-m", commitMessage(req.GetCommitMetadata().GetCommitMessage())); err != nil {
		return nil, gitFailure(err)
	}
	if _, err := gitCmd(ctx, work, nil, "push", "-q", dir, "HEAD:refs/heads/"+internalBranch); err != nil {
		return nil, gitFailure(err)
	}
	out, err := gitCmd(ctx, work, nil, "rev-parse", "HEAD")
	if err != nil {
		return nil, gitFailure(err)
	}
	return &dfpb.CommitRepositoryChangesResponse{CommitSha: strings.TrimSpace(out)}, nil
}

func commitMessage(m string) string {
	if strings.TrimSpace(m) == "" {
		return "Commit"
	}
	return m
}

func identity(name, email string) []string {
	return []string{"user.name=" + name, "user.email=" + email}
}

func (s *Server) ReadRepositoryFile(ctx context.Context, req *dfpb.ReadRepositoryFileRequest) (*dfpb.ReadRepositoryFileResponse, error) {
	dir, err := s.internalRepo(req.GetName())
	if err != nil {
		return nil, err
	}
	p, err := cleanPath(req.GetPath(), false)
	if err != nil {
		return nil, err
	}
	rev, err := revOrHead(ctx, dir, req.GetCommitSha())
	if err != nil {
		return nil, err
	}
	out, err := gitCmd(ctx, dir, nil, "cat-file", "blob", rev+":"+p)
	if err != nil {
		return nil, apierror.NotFound("file %q not found at %s", req.GetPath(), rev)
	}
	return &dfpb.ReadRepositoryFileResponse{Contents: []byte(out)}, nil
}

func (s *Server) QueryRepositoryDirectoryContents(ctx context.Context, req *dfpb.QueryRepositoryDirectoryContentsRequest) (*dfpb.QueryRepositoryDirectoryContentsResponse, error) {
	dir, err := s.internalRepo(req.GetName())
	if err != nil {
		return nil, err
	}
	p, err := cleanPath(req.GetPath(), true)
	if err != nil {
		return nil, err
	}
	rev, err := revOrHead(ctx, dir, req.GetCommitSha())
	if err != nil {
		return nil, err
	}
	spec := rev
	if p != "" {
		spec = rev + ":" + p
	}
	out, err := gitCmd(ctx, dir, nil, "ls-tree", "-z", spec)
	if err != nil {
		return nil, apierror.NotFound("directory %q not found at %s", req.GetPath(), rev)
	}
	var entries []*dfpb.DirectoryEntry
	for _, line := range strings.Split(out, "\x00") {
		meta, name, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		full := name
		if p != "" {
			full = p + "/" + name
		}
		if strings.Contains(meta, " tree ") {
			entries = append(entries, &dfpb.DirectoryEntry{Entry: &dfpb.DirectoryEntry_Directory{Directory: full}})
		} else {
			entries = append(entries, &dfpb.DirectoryEntry{Entry: &dfpb.DirectoryEntry_File{File: full}})
		}
	}
	start, end, next, err := page("repodir:"+req.GetName()+rev+p, req.GetPageToken(), req.GetPageSize(), len(entries))
	if err != nil {
		return nil, err
	}
	return &dfpb.QueryRepositoryDirectoryContentsResponse{DirectoryEntries: entries[start:end], NextPageToken: next}, nil
}

func (s *Server) FetchRepositoryHistory(ctx context.Context, req *dfpb.FetchRepositoryHistoryRequest) (*dfpb.FetchRepositoryHistoryResponse, error) {
	dir, err := s.internalRepo(req.GetName())
	if err != nil {
		return nil, err
	}
	commits, err := history(ctx, dir, internalBranch)
	if err != nil {
		return nil, err
	}
	start, end, next, err := page("history:"+req.GetName(), req.GetPageToken(), req.GetPageSize(), len(commits))
	if err != nil {
		return nil, err
	}
	return &dfpb.FetchRepositoryHistoryResponse{Commits: commits[start:end], NextPageToken: next}, nil
}

func history(ctx context.Context, dir, rev string) ([]*dfpb.CommitLogEntry, error) {
	if _, err := gitCmd(ctx, dir, nil, "rev-parse", "--verify", "-q", rev); err != nil {
		return nil, nil
	}
	out, err := gitCmd(ctx, dir, nil, "log", "--format=%H%x00%an%x00%ae%x00%ct%x00%B%x1e", rev)
	if err != nil {
		return nil, gitFailure(err)
	}
	var commits []*dfpb.CommitLogEntry
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		f := strings.SplitN(rec, "\x00", 5)
		if len(f) < 5 {
			continue
		}
		sec, _ := strconv.ParseInt(f[3], 10, 64)
		commits = append(commits, &dfpb.CommitLogEntry{CommitSha: f[0], Author: &dfpb.CommitAuthor{Name: f[1], EmailAddress: f[2]},
			CommitTime: timestamppb.New(time.Unix(sec, 0)), CommitMessage: strings.TrimRight(f[4], "\n")})
	}
	return commits, nil
}

func (s *Server) FetchRemoteBranches(ctx context.Context, req *dfpb.FetchRemoteBranchesRequest) (*dfpb.FetchRemoteBranchesResponse, error) {
	rm, err := s.remoteOf(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	if rm.internal {
		return nil, apierror.FailedPrecondition("Repository %s has no git remote", req.GetName())
	}
	out, err := gitCmd(ctx, s.opts.DataDir, rm.conf, "ls-remote", "--heads", rm.url)
	if err != nil {
		return nil, gitFailure(err)
	}
	var branches []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if _, ref, ok := strings.Cut(line, "\t"); ok {
			branches = append(branches, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	sort.Strings(branches)
	return &dfpb.FetchRemoteBranchesResponse{Branches: branches}, nil
}

func (s *Server) ComputeRepositoryAccessTokenStatus(ctx context.Context, req *dfpb.ComputeRepositoryAccessTokenStatusRequest) (*dfpb.ComputeRepositoryAccessTokenStatusResponse, error) {
	s.mu.Lock()
	r, err := s.repo(req.GetName())
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	gr := clone(r).GetGitRemoteSettings()
	s.mu.Unlock()
	if gr.GetUrl() == "" {
		return nil, apierror.FailedPrecondition("Repository %s has no git remote", req.GetName())
	}
	out := func(st dfpb.ComputeRepositoryAccessTokenStatusResponse_TokenStatus) (*dfpb.ComputeRepositoryAccessTokenStatusResponse, error) {
		return &dfpb.ComputeRepositoryAccessTokenStatusResponse{TokenStatus: st}, nil
	}
	if gr.GetAuthenticationTokenSecretVersion() == "" {
		return out(dfpb.ComputeRepositoryAccessTokenStatusResponse_NOT_FOUND)
	}
	if _, err := s.secret(ctx, gr.GetAuthenticationTokenSecretVersion()); err != nil {
		if apierror.From(err).Code == 7 {
			return out(dfpb.ComputeRepositoryAccessTokenStatusResponse_PERMISSION_DENIED)
		}
		return out(dfpb.ComputeRepositoryAccessTokenStatusResponse_NOT_FOUND)
	}
	rm, err := s.remoteOf(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	if _, err := gitCmd(ctx, s.opts.DataDir, rm.conf, "ls-remote", "--heads", rm.url); err != nil {
		return out(dfpb.ComputeRepositoryAccessTokenStatusResponse_INVALID)
	}
	return out(dfpb.ComputeRepositoryAccessTokenStatusResponse_VALID)
}

// ---- Workspaces ----

func (s *Server) CreateWorkspace(ctx context.Context, req *dfpb.CreateWorkspaceRequest) (*dfpb.Workspace, error) {
	if err := checkID("workspace_id", req.GetWorkspaceId()); err != nil {
		return nil, err
	}
	rm, err := s.remoteOf(ctx, req.GetParent())
	if err != nil {
		return nil, err
	}
	name := req.GetParent() + "/workspaces/" + req.GetWorkspaceId()
	s.mu.Lock()
	if _, ok := s.workspaces[name]; ok {
		s.mu.Unlock()
		return nil, apierror.AlreadyExists("Workspace %s already exists", name)
	}
	s.mu.Unlock()
	dir := s.workspaceDir(name)
	defer s.lock(dir)()
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, apierror.Wrap(err)
	}
	fail := func(err error) (*dfpb.Workspace, error) {
		_ = os.RemoveAll(dir)
		return nil, gitFailure(err)
	}
	if _, err := gitCmd(ctx, dir, nil, "init", "-q", "-b", rm.branch); err != nil {
		return fail(err)
	}
	if _, err := gitCmd(ctx, dir, nil, "remote", "add", "origin", rm.url); err != nil {
		return fail(err)
	}
	// A workspace starts from the default branch, when it has commits.
	if _, err := gitCmd(ctx, dir, rm.conf, "fetch", "-q", "origin"); err != nil {
		return fail(err)
	}
	if _, err := gitCmd(ctx, dir, nil, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+rm.branch); err == nil {
		if _, err := gitCmd(ctx, dir, nil, "reset", "-q", "--hard", "origin/"+rm.branch); err != nil {
			return fail(err)
		}
	}
	w := clone(req.GetWorkspace())
	if w == nil {
		w = &dfpb.Workspace{}
	}
	w.Name = name
	w.CreateTime = s.now()
	w.DataEncryptionState, w.InternalMetadata = nil, nil
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.repo(req.GetParent()); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	s.workspaces[name] = w
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(w), nil
}

func (s *Server) workspace(name string) (*dfpb.Workspace, string, error) {
	if !workspaceRE.MatchString(name) {
		return nil, "", apierror.InvalidArgument("name %q must be projects/*/locations/*/repositories/*/workspaces/*", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workspaces[name]
	if !ok {
		return nil, "", apierror.NotFound("Workspace %s not found", name)
	}
	return clone(w), s.workspaceDir(name), nil
}

func (s *Server) GetWorkspace(_ context.Context, req *dfpb.GetWorkspaceRequest) (*dfpb.Workspace, error) {
	w, _, err := s.workspace(req.GetName())
	return w, err
}

func (s *Server) ListWorkspaces(_ context.Context, req *dfpb.ListWorkspacesRequest) (*dfpb.ListWorkspacesResponse, error) {
	s.mu.Lock()
	if _, err := s.repo(req.GetParent()); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	var all []*dfpb.Workspace
	for _, k := range childrenOf(s.workspaces, req.GetParent(), "workspaces") {
		all = append(all, clone(s.workspaces[k]))
	}
	s.mu.Unlock()
	all, err := filterBy(all, req.GetFilter(), func(w *dfpb.Workspace) map[string]string { return map[string]string{"name": w.GetName()} })
	if err != nil {
		return nil, err
	}
	if err := orderBy(all, req.GetOrderBy(), func(w *dfpb.Workspace) map[string]string {
		return map[string]string{"name": w.GetName(), "create_time": w.GetCreateTime().AsTime().Format(time.RFC3339Nano)}
	}); err != nil {
		return nil, err
	}
	start, end, next, err := page("ws:"+req.GetParent()+req.GetFilter()+req.GetOrderBy(), req.GetPageToken(), req.GetPageSize(), len(all))
	if err != nil {
		return nil, err
	}
	return &dfpb.ListWorkspacesResponse{Workspaces: all[start:end], NextPageToken: next}, nil
}

func (s *Server) DeleteWorkspace(_ context.Context, req *dfpb.DeleteWorkspaceRequest) (*emptypb.Empty, error) {
	_, dir, err := s.workspace(req.GetName())
	if err != nil {
		return nil, err
	}
	defer s.lock(dir)()
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.workspaces, req.GetName())
	delete(s.policies, req.GetName())
	_ = os.RemoveAll(dir)
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

// cleanPath checks a workspace-relative path: no absolute path, no "..",
// nothing inside .git. dir allows "" (the root).
func cleanPath(p string, dir bool) (string, error) {
	if p == "" || p == "." || p == "/" {
		if dir {
			return "", nil
		}
		return "", apierror.InvalidArgument("path is required")
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return "", apierror.InvalidArgument("path %q must be relative to the workspace root", p)
	}
	c := path.Clean(p)
	for _, part := range strings.Split(c, "/") {
		if part == ".." || part == ".git" {
			return "", apierror.InvalidArgument("path %q is not allowed", p)
		}
	}
	return c, nil
}

// wsPath resolves a workspace and a path in it.
func (s *Server) wsPath(name, p string, dir bool) (root, full, clean string, err error) {
	_, root, err = s.workspace(name)
	if err != nil {
		return "", "", "", err
	}
	clean, err = cleanPath(p, dir)
	if err != nil {
		return "", "", "", err
	}
	return root, filepath.Join(root, filepath.FromSlash(clean)), clean, nil
}

func (s *Server) WriteFile(_ context.Context, req *dfpb.WriteFileRequest) (*dfpb.WriteFileResponse, error) {
	root, full, _, err := s.wsPath(req.GetWorkspace(), req.GetPath(), false)
	if err != nil {
		return nil, err
	}
	defer s.lock(root)()
	if fi, err := os.Stat(full); err == nil && fi.IsDir() {
		return nil, apierror.InvalidArgument("path %q is a directory", req.GetPath())
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return nil, apierror.FailedPrecondition("path %q: a parent is not a directory", req.GetPath())
	}
	if err := os.WriteFile(full, req.GetContents(), 0o644); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &dfpb.WriteFileResponse{}, nil
}

func (s *Server) ReadFile(ctx context.Context, req *dfpb.ReadFileRequest) (*dfpb.ReadFileResponse, error) {
	root, full, clean, err := s.wsPath(req.GetWorkspace(), req.GetPath(), false)
	if err != nil {
		return nil, err
	}
	defer s.lock(root)()
	if rev := req.GetRevision(); rev != "" {
		out, err := gitCmd(ctx, root, nil, "cat-file", "blob", rev+":"+clean)
		if err != nil {
			return nil, apierror.NotFound("file %q not found at revision %s", req.GetPath(), rev)
		}
		return &dfpb.ReadFileResponse{FileContents: []byte(out)}, nil
	}
	fi, err := os.Stat(full)
	if err != nil || fi.IsDir() {
		return nil, apierror.NotFound("file %q not found", req.GetPath())
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return &dfpb.ReadFileResponse{FileContents: b}, nil
}

func (s *Server) RemoveFile(_ context.Context, req *dfpb.RemoveFileRequest) (*dfpb.RemoveFileResponse, error) {
	root, full, _, err := s.wsPath(req.GetWorkspace(), req.GetPath(), false)
	if err != nil {
		return nil, err
	}
	defer s.lock(root)()
	fi, err := os.Stat(full)
	if err != nil || fi.IsDir() {
		return nil, apierror.NotFound("file %q not found", req.GetPath())
	}
	if err := os.Remove(full); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &dfpb.RemoveFileResponse{}, nil
}

func (s *Server) moveEntry(workspace, from, to string, wantDir bool) error {
	root, src, _, err := s.wsPath(workspace, from, false)
	if err != nil {
		return err
	}
	_, dst, _, err := s.wsPath(workspace, to, false)
	if err != nil {
		return err
	}
	defer s.lock(root)()
	fi, err := os.Stat(src)
	if err != nil || fi.IsDir() != wantDir {
		kind := "file"
		if wantDir {
			kind = "directory"
		}
		return apierror.NotFound("%s %q not found", kind, from)
	}
	if _, err := os.Stat(dst); err == nil {
		return apierror.AlreadyExists("path %q already exists", to)
	}
	if wantDir && strings.HasPrefix(dst+string(filepath.Separator), src+string(filepath.Separator)) {
		return apierror.InvalidArgument("directory %q cannot be moved into itself", from)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return apierror.FailedPrecondition("path %q: a parent is not a directory", to)
	}
	if err := os.Rename(src, dst); err != nil {
		return apierror.Wrap(err)
	}
	return nil
}

func (s *Server) MoveFile(_ context.Context, req *dfpb.MoveFileRequest) (*dfpb.MoveFileResponse, error) {
	if err := s.moveEntry(req.GetWorkspace(), req.GetPath(), req.GetNewPath(), false); err != nil {
		return nil, err
	}
	return &dfpb.MoveFileResponse{}, nil
}

func (s *Server) MoveDirectory(_ context.Context, req *dfpb.MoveDirectoryRequest) (*dfpb.MoveDirectoryResponse, error) {
	if err := s.moveEntry(req.GetWorkspace(), req.GetPath(), req.GetNewPath(), true); err != nil {
		return nil, err
	}
	return &dfpb.MoveDirectoryResponse{}, nil
}

func (s *Server) MakeDirectory(_ context.Context, req *dfpb.MakeDirectoryRequest) (*dfpb.MakeDirectoryResponse, error) {
	root, full, _, err := s.wsPath(req.GetWorkspace(), req.GetPath(), false)
	if err != nil {
		return nil, err
	}
	defer s.lock(root)()
	if _, err := os.Stat(full); err == nil {
		return nil, apierror.AlreadyExists("path %q already exists", req.GetPath())
	}
	if err := os.MkdirAll(full, 0o755); err != nil {
		return nil, apierror.FailedPrecondition("path %q: a parent is not a directory", req.GetPath())
	}
	return &dfpb.MakeDirectoryResponse{}, nil
}

func (s *Server) RemoveDirectory(_ context.Context, req *dfpb.RemoveDirectoryRequest) (*dfpb.RemoveDirectoryResponse, error) {
	root, full, _, err := s.wsPath(req.GetWorkspace(), req.GetPath(), false)
	if err != nil {
		return nil, err
	}
	defer s.lock(root)()
	fi, err := os.Stat(full)
	if err != nil || !fi.IsDir() {
		return nil, apierror.NotFound("directory %q not found", req.GetPath())
	}
	if err := os.RemoveAll(full); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &dfpb.RemoveDirectoryResponse{}, nil
}

func (s *Server) QueryDirectoryContents(_ context.Context, req *dfpb.QueryDirectoryContentsRequest) (*dfpb.QueryDirectoryContentsResponse, error) {
	root, full, clean, err := s.wsPath(req.GetWorkspace(), req.GetPath(), true)
	if err != nil {
		return nil, err
	}
	defer s.lock(root)()
	ents, err := os.ReadDir(full)
	if err != nil {
		return nil, apierror.NotFound("directory %q not found", req.GetPath())
	}
	var out []*dfpb.DirectoryEntry
	for _, e := range ents {
		if e.Name() == ".git" {
			continue
		}
		p := e.Name()
		if clean != "" {
			p = clean + "/" + e.Name()
		}
		d := &dfpb.DirectoryEntry{}
		if e.IsDir() {
			d.Entry = &dfpb.DirectoryEntry_Directory{Directory: p}
		} else {
			d.Entry = &dfpb.DirectoryEntry_File{File: p}
		}
		if req.GetView() == dfpb.DirectoryContentsView_DIRECTORY_CONTENTS_VIEW_METADATA {
			if fi, err := e.Info(); err == nil {
				d.Metadata = &dfpb.FilesystemEntryMetadata{UpdateTime: timestamppb.New(fi.ModTime())}
				if !e.IsDir() {
					d.Metadata.SizeBytes = fi.Size()
				}
			}
		}
		out = append(out, d)
	}
	start, end, next, err := page("dir:"+req.GetWorkspace()+clean, req.GetPageToken(), req.GetPageSize(), len(out))
	if err != nil {
		return nil, err
	}
	return &dfpb.QueryDirectoryContentsResponse{DirectoryEntries: out[start:end], NextPageToken: next}, nil
}

// SearchFiles lists the workspace's files and directories whose path
// matches the filter: path:"text" (contains) or path="text" (equals), or
// everything when it is empty.
func (s *Server) SearchFiles(_ context.Context, req *dfpb.SearchFilesRequest) (*dfpb.SearchFilesResponse, error) {
	root, _, _, err := s.wsPath(req.GetWorkspace(), "", true)
	if err != nil {
		return nil, err
	}
	match := func(string) bool { return true }
	if f := strings.TrimSpace(req.GetFilter()); f != "" {
		var op, want string
		for _, o := range []string{"=", ":"} {
			if k, v, ok := strings.Cut(f, o); ok && strings.TrimSpace(k) == "path" {
				op, want = o, strings.Trim(strings.TrimSpace(v), `"`)
				break
			}
		}
		if op == "" {
			return nil, apierror.InvalidArgument("filter %q: only path:\"...\" and path=\"...\" are supported", f)
		}
		if op == "=" {
			match = func(p string) bool { return p == want }
		} else {
			match = func(p string) bool { return strings.Contains(p, want) }
		}
	}
	defer s.lock(root)()
	var results []*dfpb.SearchResult
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if !match(rel) {
			return nil
		}
		if d.IsDir() {
			results = append(results, &dfpb.SearchResult{Entry: &dfpb.SearchResult_Directory{Directory: &dfpb.DirectorySearchResult{Path: rel}}})
		} else {
			results = append(results, &dfpb.SearchResult{Entry: &dfpb.SearchResult_File{File: &dfpb.FileSearchResult{Path: rel}}})
		}
		return nil
	})
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	start, end, next, err := page("search:"+req.GetWorkspace()+req.GetFilter(), req.GetPageToken(), req.GetPageSize(), len(results))
	if err != nil {
		return nil, err
	}
	return &dfpb.SearchFilesResponse{SearchResults: results[start:end], NextPageToken: next}, nil
}

// ---- Workspace git ----

// wsGit is a workspace's directory and its repository's remote, with the
// workspace locked; call unlock when done.
func (s *Server) wsGit(ctx context.Context, name string) (dir string, rm remote, unlock func(), err error) {
	_, dir, err = s.workspace(name)
	if err != nil {
		return "", remote{}, nil, err
	}
	m := workspaceRE.FindStringSubmatch(name)
	rm, err = s.remoteOf(ctx, m[1])
	if err != nil {
		return "", remote{}, nil, err
	}
	unlock = s.lock(dir)
	if _, err := gitCmd(ctx, dir, nil, "remote", "set-url", "origin", rm.url); err != nil {
		unlock()
		return "", remote{}, nil, gitFailure(err)
	}
	return dir, rm, unlock, nil
}

func (s *Server) PullGitCommits(ctx context.Context, req *dfpb.PullGitCommitsRequest) (*dfpb.PullGitCommitsResponse, error) {
	a := req.GetAuthor()
	if a.GetName() == "" || a.GetEmailAddress() == "" {
		return nil, apierror.InvalidArgument("author name and email_address are required")
	}
	dir, rm, unlock, err := s.wsGit(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	defer unlock()
	branch := req.GetRemoteBranch()
	if branch == "" {
		branch = rm.branch
	}
	if _, err := gitCmd(ctx, dir, rm.conf, "fetch", "-q", "origin", "refs/heads/"+branch+":refs/remotes/origin/"+branch); err != nil {
		return nil, apierror.FailedPrecondition("fetch branch %q: %v", branch, err)
	}
	if _, err := gitCmd(ctx, dir, nil, "rev-parse", "--verify", "-q", "HEAD"); err != nil {
		// No commit yet: the workspace takes the branch as it is.
		if _, err := gitCmd(ctx, dir, nil, "reset", "-q", "--hard", "origin/"+branch); err != nil {
			return nil, gitFailure(err)
		}
		return &dfpb.PullGitCommitsResponse{}, nil
	}
	if _, err := gitCmd(ctx, dir, identity(a.GetName(), a.GetEmailAddress()), "merge", "-q", "--no-edit", "origin/"+branch); err != nil {
		return nil, gitFailure(err)
	}
	return &dfpb.PullGitCommitsResponse{}, nil
}

func (s *Server) PushGitCommits(ctx context.Context, req *dfpb.PushGitCommitsRequest) (*dfpb.PushGitCommitsResponse, error) {
	dir, rm, unlock, err := s.wsGit(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, err := gitCmd(ctx, dir, nil, "rev-parse", "--verify", "-q", "HEAD"); err != nil {
		return nil, apierror.FailedPrecondition("the workspace has no commits to push")
	}
	branch := req.GetRemoteBranch()
	if branch == "" {
		branch = rm.branch
	}
	if _, err := gitCmd(ctx, dir, rm.conf, "push", "-q", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return nil, gitFailure(err)
	}
	_, _ = gitCmd(ctx, dir, rm.conf, "fetch", "-q", "origin", "refs/heads/"+branch+":refs/remotes/origin/"+branch)
	return &dfpb.PushGitCommitsResponse{}, nil
}

func (s *Server) FetchFileGitStatuses(ctx context.Context, req *dfpb.FetchFileGitStatusesRequest) (*dfpb.FetchFileGitStatusesResponse, error) {
	_, dir, err := s.workspace(req.GetName())
	if err != nil {
		return nil, err
	}
	defer s.lock(dir)()
	out, err := gitCmd(ctx, dir, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, gitFailure(err)
	}
	resp := &dfpb.FetchFileGitStatusesResponse{}
	add := func(p string, st dfpb.FetchFileGitStatusesResponse_UncommittedFileChange_State) {
		resp.UncommittedFileChanges = append(resp.UncommittedFileChanges, &dfpb.FetchFileGitStatusesResponse_UncommittedFileChange{Path: p, State: st})
	}
	recs := strings.Split(out, "\x00")
	for i := 0; i < len(recs); i++ {
		r := recs[i]
		if len(r) < 4 {
			continue
		}
		xy, p := r[:2], r[3:]
		switch {
		case strings.Contains(xy, "U") || xy == "AA" || xy == "DD":
			add(p, dfpb.FetchFileGitStatusesResponse_UncommittedFileChange_HAS_CONFLICTS)
		case xy == "??" || xy[0] == 'A':
			add(p, dfpb.FetchFileGitStatusesResponse_UncommittedFileChange_ADDED)
		case xy[0] == 'R' || xy[0] == 'C':
			add(p, dfpb.FetchFileGitStatusesResponse_UncommittedFileChange_ADDED)
			if i+1 < len(recs) && xy[0] == 'R' {
				i++
				add(recs[i], dfpb.FetchFileGitStatusesResponse_UncommittedFileChange_DELETED)
			}
		case strings.Contains(xy, "D"):
			add(p, dfpb.FetchFileGitStatusesResponse_UncommittedFileChange_DELETED)
		default:
			add(p, dfpb.FetchFileGitStatusesResponse_UncommittedFileChange_MODIFIED)
		}
	}
	return resp, nil
}

func (s *Server) FetchGitAheadBehind(ctx context.Context, req *dfpb.FetchGitAheadBehindRequest) (*dfpb.FetchGitAheadBehindResponse, error) {
	dir, rm, unlock, err := s.wsGit(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	defer unlock()
	branch := req.GetRemoteBranch()
	if branch == "" {
		branch = rm.branch
	}
	remoteRef := "refs/remotes/origin/" + branch
	if _, err := gitCmd(ctx, dir, rm.conf, "fetch", "-q", "origin", "refs/heads/"+branch+":"+remoteRef); err != nil {
		remoteRef = ""
	}
	hasHead := true
	if _, err := gitCmd(ctx, dir, nil, "rev-parse", "--verify", "-q", "HEAD"); err != nil {
		hasHead = false
	}
	count := func(rangeSpec string) int32 {
		out, err := gitCmd(ctx, dir, nil, "rev-list", "--count", rangeSpec)
		if err != nil {
			return 0
		}
		n, _ := strconv.Atoi(strings.TrimSpace(out))
		return int32(n)
	}
	resp := &dfpb.FetchGitAheadBehindResponse{}
	switch {
	case hasHead && remoteRef != "":
		resp.CommitsAhead = count(remoteRef + "..HEAD")
		resp.CommitsBehind = count("HEAD.." + remoteRef)
	case hasHead:
		resp.CommitsAhead = count("HEAD")
	case remoteRef != "":
		resp.CommitsBehind = count(remoteRef)
	}
	return resp, nil
}

func (s *Server) CommitWorkspaceChanges(ctx context.Context, req *dfpb.CommitWorkspaceChangesRequest) (*dfpb.CommitWorkspaceChangesResponse, error) {
	a := req.GetAuthor()
	if a.GetName() == "" || a.GetEmailAddress() == "" {
		return nil, apierror.InvalidArgument("author name and email_address are required")
	}
	_, dir, err := s.workspace(req.GetName())
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range req.GetPaths() {
		c, err := cleanPath(p, false)
		if err != nil {
			return nil, err
		}
		paths = append(paths, c)
	}
	defer s.lock(dir)()
	args := append([]string{"add", "-A", "--"}, paths...)
	if _, err := gitCmd(ctx, dir, nil, args...); err != nil {
		return nil, gitFailure(err)
	}
	commit := []string{"commit", "-q", "-m", commitMessage(req.GetCommitMessage())}
	if len(paths) > 0 {
		commit = append(append(commit, "--"), paths...)
	}
	if _, err := gitCmd(ctx, dir, identity(a.GetName(), a.GetEmailAddress()), commit...); err != nil {
		return nil, apierror.FailedPrecondition("nothing to commit: %v", err)
	}
	return &dfpb.CommitWorkspaceChangesResponse{}, nil
}

func (s *Server) ResetWorkspaceChanges(ctx context.Context, req *dfpb.ResetWorkspaceChangesRequest) (*dfpb.ResetWorkspaceChangesResponse, error) {
	_, dir, err := s.workspace(req.GetName())
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range req.GetPaths() {
		c, err := cleanPath(p, false)
		if err != nil {
			return nil, err
		}
		paths = append(paths, c)
	}
	defer s.lock(dir)()
	_, headErr := gitCmd(ctx, dir, nil, "rev-parse", "--verify", "-q", "HEAD")
	if headErr == nil {
		if len(paths) == 0 {
			_, _ = gitCmd(ctx, dir, nil, "merge", "--abort")
			if _, err := gitCmd(ctx, dir, nil, "reset", "-q", "--hard", "HEAD"); err != nil {
				return nil, gitFailure(err)
			}
		} else {
			for _, p := range paths {
				_, _ = gitCmd(ctx, dir, nil, "reset", "-q", "HEAD", "--", p)
				// A path HEAD does not have is new: reset leaves it untracked.
				_, _ = gitCmd(ctx, dir, nil, "checkout", "-q", "HEAD", "--", p)
			}
		}
	} else {
		args := append([]string{"rm", "-r", "-q", "--cached", "--ignore-unmatch", "--"}, paths...)
		if len(paths) == 0 {
			args = append(args, ".")
		}
		_, _ = gitCmd(ctx, dir, nil, args...)
	}
	if req.GetClean() {
		args := append([]string{"clean", "-f", "-d", "-q", "--"}, paths...)
		if _, err := gitCmd(ctx, dir, nil, args...); err != nil {
			return nil, gitFailure(err)
		}
	}
	return &dfpb.ResetWorkspaceChangesResponse{}, nil
}

func (s *Server) FetchFileDiff(ctx context.Context, req *dfpb.FetchFileDiffRequest) (*dfpb.FetchFileDiffResponse, error) {
	root, full, clean, err := s.wsPath(req.GetWorkspace(), req.GetPath(), false)
	if err != nil {
		return nil, err
	}
	defer s.lock(root)()
	tracked := false
	if _, err := gitCmd(ctx, root, nil, "rev-parse", "--verify", "-q", "HEAD:"+clean); err == nil {
		tracked = true
	}
	if tracked {
		out, err := gitCmd(ctx, root, nil, "diff", "HEAD", "--", clean)
		if err != nil {
			return nil, gitFailure(err)
		}
		return &dfpb.FetchFileDiffResponse{FormattedDiff: out}, nil
	}
	if _, err := os.Stat(full); err != nil {
		return nil, apierror.NotFound("file %q not found", req.GetPath())
	}
	// An untracked file's diff is against nothing; git exits 1 for it.
	out, err := gitCmd(ctx, root, nil, "diff", "--no-index", "--", "/dev/null", clean)
	var ge *gitError
	if err != nil && !(errors.As(err, &ge) && out != "") {
		return nil, gitFailure(err)
	}
	return &dfpb.FetchFileDiffResponse{FormattedDiff: out}, nil
}

// snapshot copies a git revision of a repository (fetched from its remote)
// into a new directory, and returns the directory and the commit.
func (s *Server) snapshot(ctx context.Context, repo, commitish string) (dir, sha string, err error) {
	rm, err := s.remoteOf(ctx, repo)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Join(s.opts.DataDir, "snapshots"), 0o755); err != nil {
		return "", "", apierror.Wrap(err)
	}
	dir, err = os.MkdirTemp(filepath.Join(s.opts.DataDir, "snapshots"), "c-")
	if err != nil {
		return "", "", apierror.Wrap(err)
	}
	fail := func(e error) (string, string, error) {
		_ = os.RemoveAll(dir)
		return "", "", e
	}
	if _, err := gitCmd(ctx, dir, nil, "init", "-q"); err != nil {
		return fail(gitFailure(err))
	}
	if _, err := gitCmd(ctx, dir, rm.conf, "fetch", "-q", rm.url, "+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*"); err != nil {
		return fail(gitFailure(err))
	}
	if commitish == "" {
		commitish = rm.branch
	}
	var resolved string
	for _, cand := range []string{"refs/remotes/origin/" + commitish, "refs/tags/" + commitish, commitish} {
		if out, err := gitCmd(ctx, dir, nil, "rev-parse", "--verify", "-q", cand+"^{commit}"); err == nil {
			resolved = strings.TrimSpace(out)
			break
		}
	}
	if resolved == "" {
		// A commit SHA no ref reaches yet: fetch it by itself.
		if _, err := gitCmd(ctx, dir, rm.conf, "fetch", "-q", rm.url, commitish); err == nil {
			if out, err := gitCmd(ctx, dir, nil, "rev-parse", "--verify", "-q", "FETCH_HEAD^{commit}"); err == nil {
				resolved = strings.TrimSpace(out)
			}
		}
	}
	if resolved == "" {
		return fail(apierror.InvalidArgument("git_commitish %q does not name a branch, tag or commit of the repository", commitish))
	}
	if _, err := gitCmd(ctx, dir, nil, "checkout", "-q", "--detach", resolved); err != nil {
		return fail(gitFailure(err))
	}
	return dir, resolved, nil
}

// copyTree copies a workspace's files (without .git) into a new directory.
func (s *Server) copyTree(src string) (string, error) {
	if err := os.MkdirAll(filepath.Join(s.opts.DataDir, "snapshots"), 0o755); err != nil {
		return "", err
	}
	dst, err := os.MkdirTemp(filepath.Join(s.opts.DataDir, "snapshots"), "w-")
	if err != nil {
		return "", err
	}
	err = filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" && filepath.Dir(rel) == "." {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, target)
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fi, _ := d.Info()
			mode := os.FileMode(0o644)
			if fi != nil {
				mode = fi.Mode().Perm()
			}
			return os.WriteFile(target, b, mode)
		}
	})
	if err != nil {
		_ = os.RemoveAll(dst)
		return "", fmt.Errorf("copy workspace: %w", err)
	}
	return dst, nil
}
