//go:build compat

package compat

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.securesourcemanager.v1.SecureSourceManager/CreateRepository
//
// TestSecureSourceManagerInitialConfig (#2071), with the official client
// over gRPC on the Run port, as Google's "Create a repository" page
// documents initial_config: gitignores (template names), a license
// template and readme "default" make the repository's first commit on its
// default branch, which git clones with .gitignore, LICENSE and README.md;
// FetchTree reads the same three files. A template name the reference does
// not list, or a readme other than default, is INVALID_ARGUMENT.
func TestSecureSourceManagerInitialConfig(t *testing.T) {
	h := New(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	c := ssmClient(t, h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	in, _ := ssmInstanceAndRepo(t, h, c, suffix)
	parent := runParent(h)

	for _, bad := range []*ssmpb.Repository_InitialConfig{{Gitignores: []string{"not-a-template"}}, {License: "not-a-license"}, {Readme: "custom"}} {
		_, err := c.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "bad-" + suffix,
			Repository: &ssmpb.Repository{Instance: in.GetName(), InitialConfig: bad}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateRepository with initial_config %v: %v; want InvalidArgument", bad, err)
		}
	}
	op, err := c.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "init-" + suffix,
		Repository: &ssmpb.Repository{Instance: in.GetName(), Description: "Initialized.", InitialConfig: &ssmpb.Repository_InitialConfig{
			DefaultBranch: "trunk", Gitignores: []string{"go", "visual-studio-code"}, License: "apache-2-0", Readme: "default"}}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	repo, err := op.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := c.FetchTree(ctx, &ssmpb.FetchTreeRequest{Repository: repo.GetName(), Ref: "trunk"}).Next()
	if err != nil || tree.GetPath() != ".gitignore" {
		t.Errorf("FetchTree's first entry = %v, %v", tree, err)
	}
	work := t.TempDir()
	if out, err := exec.Command("git", "clone", "--quiet", repo.GetUris().GetGitHttps(), work).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(work, name))
		if err != nil {
			t.Fatalf("the clone has no %s: %v", name, err)
		}
		return string(b)
	}
	if gi := read(".gitignore"); !strings.HasPrefix(gi, "# go\n") || !strings.Contains(gi, "\n# visual-studio-code\n") {
		t.Errorf(".gitignore = %.120q", gi)
	}
	if l := read("LICENSE"); !strings.Contains(l, "Apache License") || !strings.Contains(l, "Version 2.0") {
		t.Errorf("LICENSE = %.120q", l)
	}
	if r := read("README.md"); r != "# init-"+suffix+"\n\nInitialized.\n" {
		t.Errorf("README.md = %q", r)
	}
	out, err := exec.Command("git", "-C", work, "log", "--format=%s", "-1").Output()
	if err != nil || strings.TrimSpace(string(out)) != "Initial commit" {
		t.Errorf("the first commit's message = %q, %v", out, err)
	}
	if b, err := exec.Command("git", "-C", work, "branch", "--show-current").Output(); err != nil || strings.TrimSpace(string(b)) != "trunk" {
		t.Errorf("the clone checked out %q, %v; want the default branch trunk", b, err)
	}
}
