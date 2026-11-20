package run

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func gitDep(url, revision, dest string, depth int64) *cbpb.Dependency {
	return &cbpb.Dependency{Dep: &cbpb.Dependency_GitSource{GitSource: &cbpb.Dependency_GitSourceDependency{
		Repository: &cbpb.Dependency_GitSourceRepository{Repotype: &cbpb.Dependency_GitSourceRepository_Url{Url: url}},
		Revision:   revision, DestPath: dest, Depth: depth}}}
}

// TestPlanDependenciesChecksAsGoogleDocuments (#2074): revision, destPath
// (relative to the working directory) and the repository are required, a
// depth below -1 is refused, and a URL on a host this instance cannot reach
// is refused with Google's requirement, before anything dials it.
func TestPlanDependenciesChecksAsGoogleDocuments(t *testing.T) {
	local := "http://127.0.0.1:1/sourcemanager/git/p/us-central1/missing.git"
	for name, tc := range map[string]struct {
		d    *cbpb.Dependency
		code codes.Code
		want string
	}{
		"no revision":     {gitDep(local, "", "dep", 0), codes.InvalidArgument, "revision is required"},
		"no destPath":     {gitDep(local, "main", "", 0), codes.InvalidArgument, "destPath is required"},
		"absolute":        {gitDep(local, "main", "/etc", 0), codes.InvalidArgument, "relative to the working directory"},
		"escaping":        {gitDep(local, "main", "../x", 0), codes.InvalidArgument, "relative to the working directory"},
		"bad depth":       {gitDep(local, "main", "dep", -2), codes.InvalidArgument, "-1 for all history"},
		"no repository":   {&cbpb.Dependency{Dep: &cbpb.Dependency_GitSource{GitSource: &cbpb.Dependency_GitSourceDependency{Revision: "main", DestPath: "d"}}}, codes.InvalidArgument, "url or a developerConnect"},
		"hosted":          {gitDep("https://github.com/googleapis/googleapis.git", "master", "dep", 0), codes.FailedPrecondition, "cannot be reached from this instance"},
		"not https":       {gitDep("git@github.com:googleapis/googleapis.git", "master", "dep", 0), codes.FailedPrecondition, "not an HTTPS URL"},
		"missing locally": {gitDep(local, "main", "dep", 0), codes.FailedPrecondition, "not found"},
		"unknown link": {&cbpb.Dependency{Dep: &cbpb.Dependency_GitSource{GitSource: &cbpb.Dependency_GitSourceDependency{
			Repository: &cbpb.Dependency_GitSourceRepository{Repotype: &cbpb.Dependency_GitSourceRepository_DeveloperConnect{
				DeveloperConnect: "projects/p/locations/us-central1/connections/c/gitRepositoryLinks/l"}},
			Revision: "main", DestPath: "d"}}}, codes.FailedPrecondition, "not found"},
	} {
		_, _, err := planDependencies([]*cbpb.Dependency{tc.d})
		if status.Code(err) != tc.code || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v; want %v with %q", name, err, tc.code, tc.want)
		}
	}
	if !strings.Contains(dependencyRequirement, "public repository or linked to Cloud Build using Developer Connect") {
		t.Error("the refusal does not quote Google's requirement")
	}
	if _, empty, err := planDependencies([]*cbpb.Dependency{{Dep: &cbpb.Dependency_Empty{Empty: true}}}); err != nil || !empty {
		t.Errorf("{empty: true} = %v, %v", empty, err)
	}
	for host, want := range map[string]bool{"127.0.0.1:8080": true, "localhost:1": true, "[::1]:2": true,
		"github.com": false, "source.developers.google.com": false, "10.0.0.1:80": false} {
		if got := isLocalGitHost(host); got != want {
			t.Errorf("isLocalGitHost(%s) = %v", host, got)
		}
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestCloneDependencyAtRevisionAndDepth (#2074): a dependency is cloned at
// its revision (a branch, a tag or a short hash), with its depth of
// history, and tags only with fetchTags; its tree and .git go under
// destPath in the archive /workspace is unpacked from.
func TestCloneDependencyAtRevisionAndDepth(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	work, bare := filepath.Join(root, "work"), filepath.Join(root, "repo.git")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, work, "init", "-q", "-b", "main")
	for i, text := range []string{"one", "two", "three"} {
		if err := os.WriteFile(filepath.Join(work, "hello.txt"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, work, "add", "hello.txt")
		git(t, work, "commit", "-q", "-m", text)
		if i == 0 {
			git(t, work, "tag", "v1")
		}
	}
	git(t, root, "clone", "-q", "--bare", work, bare)
	first := git(t, work, "rev-list", "--max-parents=0", "HEAD")
	ctx := context.Background()
	for name, tc := range []struct {
		revision string
		depth    int64
		tags     bool
		text     string
		commits  string
	}{
		{"main", 1, false, "three", "1"},
		{"main", 2, false, "three", "2"},
		{"main", 0, true, "three", "3"},
		{"v1", 1, false, "one", "1"},
		{first[:8], 1, false, "one", "1"},
	} {
		dst := filepath.Join(t.TempDir(), "clone")
		d := gitDependency{repo: "r", dir: bare, revision: tc.revision, depth: tc.depth, tags: tc.tags, destPath: "deps/r"}
		if err := cloneDependency(ctx, d, dst); err != nil {
			t.Fatalf("%d: %v", name, err)
		}
		if b, _ := os.ReadFile(filepath.Join(dst, "hello.txt")); string(b) != tc.text {
			t.Errorf("%d: hello.txt = %q, want %q", name, b, tc.text)
		}
		if n := git(t, dst, "rev-list", "--count", "HEAD"); n != tc.commits {
			t.Errorf("%d: %s commits of history, want %s", name, n, tc.commits)
		}
		if tags := git(t, dst, "tag"); (tags == "v1") != tc.tags {
			t.Errorf("%d: tags %q with fetchTags %v", name, tags, tc.tags)
		}
		if name == 0 {
			raw, err := dependencyArchive(dst, d.destPath)
			if err != nil {
				t.Fatal(err)
			}
			names := archiveNames(t, raw)
			if !names["deps/r/hello.txt"] || !names["deps/r/.git/HEAD"] {
				t.Errorf("the archive holds %v", names)
			}
		}
	}
	d := gitDependency{repo: "r", dir: bare, revision: "nope", depth: 1, destPath: "x"}
	if err := cloneDependency(ctx, d, filepath.Join(t.TempDir(), "c")); err == nil || !strings.Contains(err.Error(), `revision "nope" not found`) {
		t.Errorf("an unknown revision = %v", err)
	}
}

func archiveNames(t *testing.T, raw []byte) map[string]bool {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	out := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = true
	}
}
