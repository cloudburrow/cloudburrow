package run

// A build's git source dependencies (#2074), as Google's "Manage build
// dependencies" page has them: each gitSource names its repository (an
// HTTPS url, or a Developer Connect gitRepositoryLink), a revision, and the
// destPath under /workspace it is cloned into, with its depth (1 by
// default, -1 for all history), recurseSubmodules and fetchTags.
// "Dependencies are cloned in the order that you specify them", before any
// step runs, and once a build lists dependencies "only those listed in
// dependencies are cloned": a triggered build's own repository is not.
// {empty: true} "disable[s] all dependency fetching (ignoring the default
// source as well)".
//
// A repository must be "either a public repository or linked to Cloud
// Build using Developer Connect". Here that is this instance's Secure
// Source Manager repositories, by their git URL or through a Developer
// Connect link on one: the dependency is cloned from the repository's own
// directory, with git restricted to the file transport, so a build never
// fetches from another host. A URL on any other host is refused when the
// build is created, as a local instance cannot reach it.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// gitDependency is one gitSource, resolved to the repository it clones.
type gitDependency struct {
	index      int
	repo       string // the url or link, as the build names it
	baseURL    string // its git URL, which relative submodule URLs resolve against
	dir        string // the repository's bare git directory
	revision   string
	destPath   string // cleaned, relative to /workspace
	depth      int64  // 0: all history
	submodules bool
	tags       bool
}

// ssmGitBase is the Secure Source Manager port's base URL, whose host a
// dependency URL may name.
var ssmGitBase atomic.Value // func() string

// WorkspaceUnpacker is a BuildWorkspace that unpacks more into /workspace
// after it is made: each dependency's clone, under its destPath.
type WorkspaceUnpacker interface {
	Unpack(ctx context.Context, archive io.Reader) error
}

// dependencyRequirement is Google's requirement for a dependency's repository
// (manage-dependencies).
const dependencyRequirement = "A Git repository that is either a public repository or linked to Cloud Build using Developer Connect"

// planDependencies checks a build's dependencies and resolves each git
// source to its repository; empty reports {empty: true}.
func planDependencies(ds []*cbpb.Dependency) (deps []gitDependency, empty bool, err error) {
	seen := map[string]bool{}
	for i, d := range ds {
		if d.GetEmpty() {
			empty = true
		}
		g := d.GetGitSource()
		if g == nil {
			continue
		}
		f := fmt.Sprintf("dependencies[%d].gitSource", i)
		if g.GetRevision() == "" {
			return nil, false, apierror.InvalidArgument("invalid build: %s.revision is required", f)
		}
		dest := g.GetDestPath()
		if dest == "" {
			return nil, false, apierror.InvalidArgument("invalid build: %s.destPath is required", f)
		}
		clean := path.Clean(dest)
		if path.IsAbs(dest) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, false, apierror.InvalidArgument("invalid build: %s.destPath %q: the dest_path value must be a path relative to the working directory of the build", f, dest)
		}
		if seen[clean] {
			return nil, false, apierror.InvalidArgument("invalid build: %s.destPath %q is another dependency's", f, dest)
		}
		seen[clean] = true
		depth := g.GetDepth()
		switch {
		case depth == 0:
			depth = 1
		case depth == -1:
			depth = 0
		case depth < -1:
			return nil, false, apierror.InvalidArgument("invalid build: %s.depth %d: use a positive depth, or -1 for all history", f, g.GetDepth())
		}
		dep := gitDependency{index: i, revision: g.GetRevision(), destPath: clean, depth: depth,
			submodules: g.GetRecurseSubmodules(), tags: g.GetFetchTags()}
		r := g.GetRepository()
		switch {
		case r.GetDeveloperConnect() != "":
			dep.repo = r.GetDeveloperConnect()
			repo, err := devConnectLinkRepo(dep.repo)
			if err != nil {
				return nil, false, apierror.FailedPrecondition("%s.repository.developerConnect: %v", f, err)
			}
			dep.dir, dep.baseURL, err = localRepository(repo)
			if err != nil {
				return nil, false, apierror.FailedPrecondition("%s.repository.developerConnect %q: %v", f, dep.repo, err)
			}
		case r.GetUrl() != "":
			dep.repo = r.GetUrl()
			repo, err := localRepositoryURL(dep.repo)
			if err != nil {
				return nil, false, apierror.FailedPrecondition("%s.repository.url %q: %v", f, dep.repo, err)
			}
			if dep.dir, _, err = localRepository(repo); err != nil {
				return nil, false, apierror.FailedPrecondition("%s.repository.url %q: %v", f, dep.repo, err)
			}
			dep.baseURL = dep.repo
		default:
			return nil, false, apierror.InvalidArgument("invalid build: %s.repository needs a url or a developerConnect link", f)
		}
		deps = append(deps, dep)
	}
	return deps, empty, nil
}

// localRepositoryURL is the Secure Source Manager repository a dependency
// URL names on this instance, or why it cannot be fetched.
func localRepositoryURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("not an HTTPS URL of a Git repository")
	}
	repo := ""
	if isLocalGitHost(u.Host) {
		repo = ssmRepoOfURI(raw)
	}
	if repo == "" {
		return "", fmt.Errorf("host %s cannot be reached from this instance, which fetches build dependencies only from its own "+
			"Secure Source Manager repositories, by their git URL or a Developer Connect link (%s)", u.Host, dependencyRequirement)
	}
	return repo, nil
}

// isLocalGitHost reports whether host:port is this instance's git port, or
// a loopback address.
func isLocalGitHost(hostPort string) bool {
	if f, ok := ssmGitBase.Load().(func() string); ok && f != nil {
		if b, err := url.Parse(f()); err == nil && b.Host != "" && strings.EqualFold(b.Host, hostPort) {
			return true
		}
	}
	host := hostPort
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		host = h
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// localRepository is a Secure Source Manager repository's directory and
// git URL.
func localRepository(repo string) (dir, gitURL string, err error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 6 {
		return "", "", fmt.Errorf("not a Secure Source Manager repository of this instance")
	}
	r, dir, ok := lookupSSMRepo(parts[1], parts[3], parts[5])
	if !ok {
		return "", "", fmt.Errorf("repository %s not found", repo)
	}
	return dir, r.GetUris().GetGitHttps(), nil
}

// localGit runs git with only the file transport allowed, so nothing it
// does reaches another host.
func localGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "protocol.allow=never", "-c", "protocol.file.allow=always"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_ALLOW_PROTOCOL=file", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// cloneDependency clones d into dst: its revision, at its depth, with its
// tags and submodules when asked, as a worker's git fetch does.
func cloneDependency(ctx context.Context, d gitDependency, dst string) error {
	return cloneLocal(ctx, d.dir, d.baseURL, d.revision, d.depth, d.tags, d.submodules, dst, 0)
}

func cloneLocal(ctx context.Context, src, baseURL, revision string, depth int64, tags, submodules bool, dst string, level int) error {
	if level > 8 {
		return fmt.Errorf("submodules nest more than 8 deep")
	}
	// The revision, "a version, commit hash, tag, or branch name", is
	// resolved in the repository first, so a short hash is fetched too.
	out, err := localGit(ctx, src, "rev-parse", "--verify", "--quiet", revision+"^{commit}")
	if err != nil {
		return fmt.Errorf("revision %q not found", revision)
	}
	sha := strings.TrimSpace(string(out))
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	if _, err := localGit(ctx, dst, "init", "-q"); err != nil {
		return err
	}
	if _, err := localGit(ctx, dst, "remote", "add", "origin", "file://"+src); err != nil {
		return err
	}
	fetch := []string{"fetch", "-q", "--upload-pack=git -c uploadpack.allowAnySHA1InWant=true upload-pack"}
	if depth > 0 {
		fetch = append(fetch, "--depth="+strconv.FormatInt(depth, 10))
	}
	if tags {
		fetch = append(fetch, "--tags")
	} else {
		fetch = append(fetch, "--no-tags")
	}
	if _, err := localGit(ctx, dst, append(fetch, "origin", sha)...); err != nil {
		return err
	}
	if _, err := localGit(ctx, dst, "-c", "advice.detachedHead=false", "checkout", "-q", sha); err != nil {
		return err
	}
	if !submodules {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dst, ".gitmodules")); err != nil {
		return nil
	}
	cfg, _ := localGit(ctx, dst, "config", "-f", ".gitmodules", "--get-regexp", `^submodule\..*\.path$`)
	for _, line := range strings.Split(strings.TrimSpace(string(cfg)), "\n") {
		key, subPath, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(key, "submodule."), ".path")
		rawURL, err := localGit(ctx, dst, "config", "-f", ".gitmodules", "submodule."+name+".url")
		if err != nil {
			return fmt.Errorf("submodule %s has no url", name)
		}
		subURL := strings.TrimSpace(string(rawURL))
		if strings.HasPrefix(subURL, "./") || strings.HasPrefix(subURL, "../") {
			b, err := url.Parse(strings.TrimSuffix(baseURL, "/") + "/")
			if err != nil {
				return fmt.Errorf("submodule %s: %v", name, err)
			}
			rel, _ := url.Parse(subURL)
			subURL = b.ResolveReference(rel).String()
		}
		repo, err := localRepositoryURL(subURL)
		if err != nil {
			return fmt.Errorf("submodule %s, %s: %v", name, subURL, err)
		}
		subDir, _, err := localRepository(repo)
		if err != nil {
			return fmt.Errorf("submodule %s: %v", name, err)
		}
		cleanSub := filepath.Clean(subPath)
		if filepath.IsAbs(cleanSub) || cleanSub == ".." || strings.HasPrefix(cleanSub, ".."+string(filepath.Separator)) {
			return fmt.Errorf("submodule %s's path %q escapes its repository", name, subPath)
		}
		// The commit the superproject records for the submodule's path.
		tree, err := localGit(ctx, dst, "ls-tree", "HEAD", "--", subPath)
		fields := strings.Fields(string(tree))
		if err != nil || len(fields) < 3 || fields[1] != "commit" {
			return fmt.Errorf("submodule %s: %s is not a submodule at %s", name, subPath, sha)
		}
		target := filepath.Join(dst, cleanSub)
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		if err := cloneLocal(ctx, subDir, subURL, fields[2], depth, false, true, target, level+1); err != nil {
			return fmt.Errorf("submodule %s: %w", name, err)
		}
	}
	return nil
}

// dependencyArchive is a gzipped tarball of dir's tree under prefix, its
// .git included, as a clone leaves it.
func dependencyArchive(dir, prefix string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		name := path.Join(prefix, filepath.ToSlash(rel))
		info, err := e.Info()
		if err != nil {
			return err
		}
		switch {
		case e.IsDir():
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755, ModTime: info.ModTime()})
		case info.Mode().IsRegular():
			mode := int64(0o644)
			if info.Mode()&0o111 != 0 {
				mode = 0o755
			}
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: target, Mode: 0o777, ModTime: info.ModTime()})
		}
		return nil
	})
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = gz.Close()
	}
	return buf.Bytes(), err
}

// fetchDependencies clones each dependency, in order, into /workspace.
func (r *buildRun) fetchDependencies(ctx context.Context) error {
	if len(r.gitDeps) == 0 {
		return nil
	}
	u, ok := r.ws.(WorkspaceUnpacker)
	if !ok {
		return fmt.Errorf("this instance's /workspace cannot take dependencies")
	}
	for _, d := range r.gitDeps {
		depth := "all history"
		if d.depth > 0 {
			depth = "depth " + strconv.FormatInt(d.depth, 10)
		}
		r.logf("Fetching dependency %d: %s at %s (%s) into /workspace/%s\n", d.index, d.repo, d.revision, depth, d.destPath)
		tmp, err := os.MkdirTemp("", "cloudburrow-dep-")
		if err != nil {
			return err
		}
		err = cloneDependency(ctx, d, filepath.Join(tmp, "clone"))
		var archive []byte
		if err == nil {
			archive, err = dependencyArchive(filepath.Join(tmp, "clone"), d.destPath)
		}
		_ = os.RemoveAll(tmp)
		if err == nil {
			err = u.Unpack(ctx, bytes.NewReader(archive))
		}
		if err != nil {
			return fmt.Errorf("failed to fetch dependency %d, %s: %s", d.index, d.repo, firstLine(err.Error()))
		}
	}
	return nil
}
