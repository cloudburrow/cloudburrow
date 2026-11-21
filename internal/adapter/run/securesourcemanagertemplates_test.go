package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestSourceManagerTemplatesCoverTheDocumentedNames: every gitignore and
// license template name InitialConfig's reference lists is in the archive
// (#2071), with content.
func TestSourceManagerTemplatesCoverTheDocumentedNames(t *testing.T) {
	gs, ls := SourceManagerTemplateNames()
	if len(gs) != 230 || len(ls) != 459 {
		t.Fatalf("%d gitignores, %d licenses; want the reference's 230 and 459", len(gs), len(ls))
	}
	for _, want := range []string{"go", "objective--c", "visual-studio-code", "c-make"} {
		if len(ssmTemplates.gitignore[want]) == 0 {
			t.Errorf("gitignore %s is empty or missing", want)
		}
	}
	for _, want := range []string{"apache-2-0", "mit", "license-0bsd", "bsd-3-clause", "crystal-stacker"} {
		if len(ssmTemplates.license[want]) == 0 {
			t.Errorf("license %s is empty or missing", want)
		}
	}
	if !strings.Contains(string(ssmTemplates.license["apache-2-0"]), "Apache License") {
		t.Error("apache-2-0 is not the Apache License's text")
	}
}

// TestSourceManagerInitialConfigIsTheFirstCommit: gitignores, license and
// readme are written as the repository's first commit on its default
// branch; an unknown template is refused and makes nothing.
func TestSourceManagerInitialConfigIsTheFirstCommit(t *testing.T) {
	root := t.TempDir()
	if err := UseSourceManagerRoot(root); err != nil {
		t.Fatal(err)
	}
	s := NewServer(&Knative{}, "t", time.Minute)
	m := s.SecureSourceManager(func() string { return "http://127.0.0.1:1" })
	ctx := context.Background()
	parent := "projects/ssm-init/locations/us-central1"
	if _, err := m.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "i", Instance: &ssmpb.Instance{}}); err != nil {
		t.Fatal(err)
	}
	inst := parent + "/instances/i"
	for _, bad := range []*ssmpb.Repository_InitialConfig{{Gitignores: []string{"no-such"}}, {License: "no-such"}, {Readme: "fancy"}} {
		_, err := m.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "bad",
			Repository: &ssmpb.Repository{Instance: inst, InitialConfig: bad}})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%v: %v; want InvalidArgument", bad, err)
		}
	}
	if _, err := m.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "app",
		Repository: &ssmpb.Repository{Instance: inst, Description: "An app.", InitialConfig: &ssmpb.Repository_InitialConfig{
			DefaultBranch: "trunk", Gitignores: []string{"go", "node"}, License: "apache-2-0", Readme: "default"}}}); err != nil {
		t.Fatal(err)
	}
	dir := repoDir(root, parent+"/repositories/app")
	files, err := ssmGit(ctx, dir, "ls-tree", "--name-only", "trunk")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(files)); strings.Join(got, " ") != ".gitignore LICENSE README.md" {
		t.Fatalf("first commit holds %v", got)
	}
	readme, _ := ssmGit(ctx, dir, "show", "trunk:README.md")
	if string(readme) != "# app\n\nAn app.\n" {
		t.Fatalf("README.md = %q", readme)
	}
	gi, _ := ssmGit(ctx, dir, "show", "trunk:.gitignore")
	if !strings.HasPrefix(string(gi), "# go\n") || !strings.Contains(string(gi), "\n# node\n") {
		t.Fatalf(".gitignore = %.80q", gi)
	}
	msg, _ := ssmGit(ctx, dir, "log", "--format=%s %ae", "trunk")
	if strings.TrimSpace(string(msg)) != "Initial commit "+ssmOwnerAccount {
		t.Fatalf("log = %q", msg)
	}
	if head, _ := ssmGit(ctx, dir, "symbolic-ref", "HEAD"); strings.TrimSpace(string(head)) != "refs/heads/trunk" {
		t.Fatalf("HEAD = %q", head)
	}
	// FetchTree reads it as any pushed commit.
	tr, err := m.FetchTree(ctx, &ssmpb.FetchTreeRequest{Repository: parent + "/repositories/app"})
	if err != nil || len(tr.GetTreeEntries()) != 3 {
		t.Fatalf("FetchTree: %v, %v", tr, err)
	}
}

// TestSourceManagerTemplatesDigest: the embedded archive is the one
// third_party/ssm-templates/PROVENANCE.md records, so a regenerated archive
// updates its provenance too.
func TestSourceManagerTemplatesDigest(t *testing.T) {
	sum := sha256.Sum256(ssmTemplatesArchive)
	if got := hex.EncodeToString(sum[:]); got != "95eaa76dc8e9df2929055ca074df6e619ec593494c0c852ffe9b2bd9233b404b" {
		t.Errorf("ssmtemplates.tar.gz's SHA-256 = %s; update third_party/ssm-templates/PROVENANCE.md with it", got)
	}
}
