package run

import (
	"bytes"
	"context"
	"encoding/base64"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// A repository's content (#1856): FetchTree, FetchBlob and FetchRefs read
// the bare repository with git, as Google's API reference describes them.
// FetchTree's ref is a commit SHA, a branch or a tag (the default branch
// when unset); recursive lists every subfolder's entries, the folders
// included; a page is at most 10,000 entries by default. FetchRefs pages
// 30 by default and 100 at most; a tag's target is the commit it names.

var ssmSHARE = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// ssmGit runs git in a repository's directory.
func ssmGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, &gitError{err: err, stderr: strings.TrimSpace(stderr.String())}
	}
	return out, nil
}

type gitError struct {
	err    error
	stderr string
}

func (e *gitError) Error() string { return e.err.Error() + ": " + e.stderr }

// resolveSSMCommit resolves a ref (a SHA, a branch, a tag, or "" for the
// default branch) to its commit.
func resolveSSMCommit(ctx context.Context, dir, ref string) (string, error) {
	if ref == "" {
		ref = "HEAD"
	}
	if strings.HasPrefix(ref, "-") {
		return "", apierror.InvalidArgument("invalid ref %q", ref)
	}
	out, err := ssmGit(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", apierror.NotFound("ref %q not found in the repository", ref)
	}
	return strings.TrimSpace(string(out)), nil
}

// FetchTree lists the entries of a ref's tree.
func (m *SecureSourceManagerServer) FetchTree(ctx context.Context, req *ssmpb.FetchTreeRequest) (*ssmpb.FetchTreeResponse, error) {
	_, dir, err := ssmRepoDir(req.GetRepository())
	if err != nil {
		return nil, err
	}
	if req.GetPageSize() < 0 {
		return nil, apierror.InvalidArgument("page_size must not be negative")
	}
	commit, err := resolveSSMCommit(ctx, dir, req.GetRef())
	if err != nil {
		return nil, err
	}
	args := []string{"ls-tree", "-l", "-z", "--full-tree"}
	if req.GetRecursive() {
		args = append(args, "-r", "-t")
	}
	out, err := ssmGit(ctx, dir, append(args, commit)...)
	if err != nil {
		return nil, apierror.Internal(err, "git ls-tree")
	}
	entries := parseLsTree(out)
	size := req.GetPageSize()
	if size == 0 {
		size = 10000
	}
	page, next, err := pageOf(entries, size, req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &ssmpb.FetchTreeResponse{TreeEntries: page, NextPageToken: next}, nil
}

// parseLsTree reads `git ls-tree -l -z`: "mode type sha size\tpath\0".
func parseLsTree(out []byte) []*ssmpb.TreeEntry {
	var entries []*ssmpb.TreeEntry
	for _, rec := range strings.Split(string(out), "\x00") {
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta)
		if len(f) != 4 {
			continue
		}
		e := &ssmpb.TreeEntry{Mode: f[0], Sha: f[2], Path: path}
		switch f[1] {
		case "tree":
			e.Type = ssmpb.TreeEntry_TREE
		case "blob":
			e.Type = ssmpb.TreeEntry_BLOB
			e.Size, _ = strconv.ParseInt(f[3], 10, 64)
		case "commit":
			e.Type = ssmpb.TreeEntry_COMMIT
		}
		entries = append(entries, e)
	}
	return entries
}

// FetchBlob reads a blob's content, base64-encoded.
func (m *SecureSourceManagerServer) FetchBlob(ctx context.Context, req *ssmpb.FetchBlobRequest) (*ssmpb.FetchBlobResponse, error) {
	_, dir, err := ssmRepoDir(req.GetRepository())
	if err != nil {
		return nil, err
	}
	sha := req.GetSha()
	if !ssmSHARE.MatchString(sha) {
		return nil, apierror.InvalidArgument("invalid sha %q: want a full hexadecimal object ID", sha)
	}
	t, err := ssmGit(ctx, dir, "cat-file", "-t", sha)
	if err != nil {
		return nil, apierror.NotFound("blob %q not found in the repository", sha)
	}
	if strings.TrimSpace(string(t)) != "blob" {
		return nil, apierror.InvalidArgument("object %q is a %s, not a blob", sha, strings.TrimSpace(string(t)))
	}
	b, err := ssmGit(ctx, dir, "cat-file", "blob", sha)
	if err != nil {
		return nil, apierror.Internal(err, "git cat-file")
	}
	return &ssmpb.FetchBlobResponse{Sha: sha, Content: base64.StdEncoding.EncodeToString(b)}, nil
}

// FetchRefs lists a repository's branches and tags, by name.
func (m *SecureSourceManagerServer) FetchRefs(ctx context.Context, req *ssmpb.FetchRefsRequest) (*ssmpb.FetchRefsResponse, error) {
	_, dir, err := ssmRepoDir(req.GetRepository())
	if err != nil {
		return nil, err
	}
	patterns := []string{"refs/heads", "refs/tags"}
	switch req.GetType() {
	case ssmpb.Ref_REF_TYPE_BRANCH:
		patterns = patterns[:1]
	case ssmpb.Ref_REF_TYPE_TAG:
		patterns = patterns[1:]
	}
	out, err := ssmGit(ctx, dir, append([]string{"for-each-ref", "--sort=refname", "--format=%(refname)%00%(objectname)%00%(*objectname)"}, patterns...)...)
	if err != nil {
		return nil, apierror.Internal(err, "git for-each-ref")
	}
	var refs []*ssmpb.Ref
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\x00")
		if len(f) != 3 {
			continue
		}
		r := &ssmpb.Ref{Name: f[0], Target: f[1], Type: ssmpb.Ref_REF_TYPE_BRANCH}
		if strings.HasPrefix(f[0], "refs/tags/") {
			r.Type = ssmpb.Ref_REF_TYPE_TAG
			if f[2] != "" { // an annotated tag: the commit it names
				r.Target = f[2]
			}
		}
		refs = append(refs, r)
	}
	page, next, err := ssmPage(refs, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &ssmpb.FetchRefsResponse{Refs: page, NextPageToken: next}, nil
}

// SourceManagerBranch is one branch of a repository, as the console shows
// it (#1856).
type SourceManagerBranch struct {
	Name, Commit, Subject string
	When                  time.Time
}

// SourceManagerBranches are a repository's branches with their last
// commits, newest commit first.
func SourceManagerBranches(ctx context.Context, repoName string) ([]SourceManagerBranch, error) {
	_, dir, err := ssmRepoDir(repoName)
	if err != nil {
		return nil, err
	}
	out, err := ssmGit(ctx, dir, "for-each-ref", "--sort=-committerdate", "--format=%(refname:short)%00%(objectname)%00%(subject)%00%(committerdate:unix)", "refs/heads")
	if err != nil {
		return nil, err
	}
	var bs []SourceManagerBranch
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\x00")
		if len(f) != 4 {
			continue
		}
		sec, _ := strconv.ParseInt(f[3], 10, 64)
		bs = append(bs, SourceManagerBranch{Name: f[0], Commit: f[1], Subject: f[2], When: time.Unix(sec, 0)})
	}
	return bs, nil
}

// branchSHA is a branch's tip commit, or "" when there is no such branch.
func branchSHA(ctx context.Context, dir, branch string) string {
	out, err := ssmGit(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
