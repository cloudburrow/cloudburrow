//go:build compat

package compat

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The real tools behind compatibility.md's Verified rows, pinned (#694):
// gcloud (with gsutil and bq), Terraform and OpenTofu are those of the tools
// image (testdata/tools/Dockerfile, #2174), and run only in it, on an
// internal Docker network; the Terraform modules require one exact
// hashicorp/google release, from the image's filesystem mirror, and init
// against a committed lock file. Each test logs the versions that ran, so
// its evidence names them.

// EnvGcloud, when set, says the run requires gcloud: CI sets it in the
// shards whose services the gcloud tests need, so a missing tools container
// fails there rather than skipping. The gcloud that runs is always the tools
// image's, never the one it names or the runner's (#2174).
const EnvGcloud = "CLOUDBURROW_TEST_GCLOUD"

// googleProviderVersion is the hashicorp/google release every Terraform and
// OpenTofu test module requires, and testdata/terraform/.terraform.lock.hcl
// locks. dependencies.json records it as hashicorpGoogleProvider.
const googleProviderVersion = "8.12.0"

// googleProviderRequirement begins every test module.
const googleProviderRequirement = `terraform {
  required_providers {
    google = { source = "hashicorp/google", version = "` + googleProviderVersion + `" }
  }
}
`

//go:embed testdata/terraform/.terraform.lock.hcl
var googleProviderLock []byte

// gcloudBinary is the gcloud a test runs: EnvGcloud when set, which must then
// resolve, otherwise gcloud from PATH. It skips when neither is there, and
// logs `gcloud version`.
func gcloudBinary(t *testing.T) string {
	t.Helper()
	bin := realTool(t, "gcloud")
	logToolVersion(t, bin, "version")
	return bin
}

// gsutilBinary is the gsutil beside gcloudBinary's gcloud when EnvGcloud is
// set, otherwise gsutil from PATH. It logs `gsutil version`.
func gsutilBinary(t *testing.T) string {
	t.Helper()
	bin := realTool(t, "gsutil")
	logToolVersion(t, bin, "version")
	return bin
}

// bqBinary is the bq beside gcloudBinary's gcloud when EnvGcloud is set,
// otherwise bq from PATH (#696). It logs `bq version`.
func bqBinary(t *testing.T) string {
	t.Helper()
	bin := realTool(t, "bq")
	logToolVersion(t, bin, "version")
	return bin
}

// realTool is name's shim, which runs it in the tools container on an
// internal Docker network (toolBinary, #2174), never on the host. EnvGcloud
// set means the run requires it, so a missing Docker fails rather than
// skips; the image's gcloud is the one that runs.
func realTool(t *testing.T, name string) string {
	t.Helper()
	return toolBinary(t, name)
}

// toolVersions caches each tool's version output: the tools are fixed for a
// run, and gcloud and gsutil take a second or two to start. Every test still
// logs it.
var toolVersions sync.Map

// logToolVersion logs `bin args...`, run once per binary in an empty gcloud
// configuration and boto file, behind egressGuard, so the command cannot
// read a developer's configuration or leave the machine.
func logToolVersion(t *testing.T, bin string, args ...string) {
	t.Helper()
	key := bin + " " + strings.Join(args, " ")
	out, ok := toolVersions.Load(key)
	if !ok {
		dir := t.TempDir()
		boto := filepath.Join(dir, "boto")
		if err := os.WriteFile(boto, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bin, args...)
		cmd.Env = append(append(os.Environ(), "CLOUDSDK_CONFIG="+filepath.Join(dir, "gcloud"), "BOTO_CONFIG="+boto,
			"CLOUDSDK_CORE_DISABLE_PROMPTS=1", "CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK=true",
			// An empty configuration would look up metadata.google.internal,
			// which the tools container logs as an escape (#2174).
			"CLOUDSDK_CORE_CHECK_GCE_METADATA=false"), egressGuard(t)...)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", key, err, b)
		}
		out, _ = toolVersions.LoadOrStore(key, strings.TrimSpace(string(b)))
	}
	t.Logf("%s %s (%s):\n%s", filepath.Base(bin), strings.Join(args, " "), bin, out)
}

// useGoogleProviderLock writes the committed lock file into a module's
// directory, before init: init then installs the pinned provider only if its
// package matches a locked hash, and fails otherwise. When the test ends it
// logs `terraform version -json` (or tofu's), run in the directory, so the
// provider selection is in the log too; the tool is the one whose registry
// init installed from.
func useGoogleProviderLock(t *testing.T, dir string) {
	t.Helper()
	toolBinary(t, "terraform")
	if err := os.WriteFile(filepath.Join(dir, ".terraform.lock.hcl"), googleProviderLock, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bin := "terraform"
		if _, err := os.Stat(filepath.Join(dir, ".terraform", "providers", "registry.opentofu.org")); err == nil {
			bin = "tofu"
		}
		cmd := exec.Command(bin, "version", "-json")
		cmd.Dir, cmd.Env = dir, noGoogleEgress()
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("%s version -json: %v\n%s", bin, err, b)
			return
		}
		t.Logf("%s version -json:\n%s", bin, strings.TrimSpace(string(b)))
	})
}

// TestGoogleProviderLockMatchesThePin: the committed lock file locks the
// version the modules require, for Terraform's registry and OpenTofu's.
func TestGoogleProviderLockMatchesThePin(t *testing.T) {
	lock := string(googleProviderLock)
	for _, registry := range []string{"registry.terraform.io", "registry.opentofu.org"} {
		block := `provider "` + registry + `/hashicorp/google" {
  version     = "` + googleProviderVersion + `"
  constraints = "` + googleProviderVersion + `"
  hashes = [
`
		if !strings.Contains(lock, block) {
			t.Errorf("the lock file has no %s/hashicorp/google %s entry with hashes", registry, googleProviderVersion)
		}
	}
	if n := strings.Count(lock, "provider \""); n != 2 {
		t.Errorf("the lock file has %d provider entries; want 2", n)
	}
}
