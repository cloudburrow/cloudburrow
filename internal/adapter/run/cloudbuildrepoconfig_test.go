package run

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
)

// commitFiles makes a git repository holding files and returns its
// directory and the commit's SHA.
func commitFiles(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "-q", "-m", "files")
	return dir, git("rev-parse", "HEAD")
}

// TestTriggerRepoBuildReadsTheCommit (#1854): a trigger on a Developer
// Connect link with a filename reads that config file from the pushed
// commit; autodetect finds cloudbuild.yaml, else the Dockerfile.
func TestTriggerRepoBuildReadsTheCommit(t *testing.T) {
	ctx := context.Background()
	config := "steps:\n- name: alpine\n  args: [echo, hi]\n"
	dir, sha := commitFiles(t, map[string]string{"cloudbuild.yaml": config})

	b, path, err := triggerRepoBuild(ctx, &cbpb.BuildTrigger{BuildTemplate: &cbpb.BuildTrigger_Filename{Filename: "/cloudbuild.yaml"}}, dir, sha)
	if err != nil || path != "cloudbuild.yaml" || len(b.GetSteps()) != 1 || b.GetSteps()[0].GetName() != "alpine" {
		t.Fatalf("filename: %v %q %v", b, path, err)
	}
	if _, _, err := triggerRepoBuild(ctx, &cbpb.BuildTrigger{BuildTemplate: &cbpb.BuildTrigger_Filename{Filename: "missing.yaml"}}, dir, sha); err == nil {
		t.Fatal("a missing config file was accepted")
	}
	auto := &cbpb.BuildTrigger{BuildTemplate: &cbpb.BuildTrigger_Autodetect{Autodetect: true}}
	if b, path, err = triggerRepoBuild(ctx, auto, dir, sha); err != nil || path != "cloudbuild.yaml" || b.GetSteps()[0].GetName() != "alpine" {
		t.Fatalf("autodetect config: %v %q %v", b, path, err)
	}
	dir, sha = commitFiles(t, map[string]string{"Dockerfile": "FROM alpine\n"})
	if b, _, err = triggerRepoBuild(ctx, auto, dir, sha); err != nil || b.GetSteps()[0].GetName() != "gcr.io/cloud-builders/docker" || len(b.GetImages()) != 1 {
		t.Fatalf("autodetect Dockerfile: %v %v", b, err)
	}
	dir, sha = commitFiles(t, map[string]string{"README": "x"})
	if _, _, err = triggerRepoBuild(ctx, auto, dir, sha); err == nil {
		t.Fatal("autodetect with nothing to build was accepted")
	}
	// Off a link, both are still refused: nothing holds the repository.
	if err := unsupportedTrigger(&cbpb.BuildTrigger{Name: "x", BuildTemplate: &cbpb.BuildTrigger_Filename{Filename: "cloudbuild.yaml"}}); err == nil {
		t.Fatal("filename without a repository link was accepted")
	}
}
