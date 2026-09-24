//go:build compat

package compat

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// curlImage is pinned by digest, with linux/amd64 and linux/arm64.
const curlImage = "curlimages/curl@sha256:463eaf6072688fe96ac64fa623fe73e1dbe25d8ad6c34404a669ad3ce1f104b6"

// TestAContainerListsBucketsFromPlainEnvFile.
//
// `cloudburrow env --format plain` is documented as the `docker run
// --env-file` format (#284). A container started with it, and nothing else,
// lists this instance's buckets.
//
// Host networking gives the container the host's loopback, where CloudBurrow
// binds. That holds on Linux, where CI runs. On Docker Desktop host
// networking does not reach the host's loopback, so the test skips there;
// docs/configuration.md gives the check to run by hand.
func TestAContainerListsBucketsFromPlainEnvFile(t *testing.T) {
	h := New(t)
	if runtime.GOOS != "linux" {
		t.Skip("host networking reaches the host's loopback only on Linux; see docs/configuration.md for the manual check")
	}
	if exec.Command("docker", "info").Run() != nil {
		t.Skip("docker is not available")
	}
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	flags := strings.Fields(os.Getenv(EnvCLIArgs))

	bucket := h.Project() + "-envfile"
	sc := storageClient(t, h)
	// The instance's own project, as `env` exports it, so the container
	// lists the same project the bucket is made in.
	plain, err := exec.Command(cli, append([]string{"env", "--format", "plain"}, flags...)...).Output()
	if err != nil {
		t.Fatalf("cloudburrow env --format plain: %v", err)
	}
	project := ""
	for _, l := range strings.Split(string(plain), "\n") {
		if v, ok := strings.CutPrefix(l, "GOOGLE_CLOUD_PROJECT="); ok {
			project = v
		}
	}
	if project == "" {
		t.Fatalf("env --format plain exported no GOOGLE_CLOUD_PROJECT:\n%s", plain)
	}
	if err := sc.Bucket(bucket).Create(h.Context(), project, nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = sc.Bucket(bucket).Delete(h.Context()) })

	envFile := filepath.Join(t.TempDir(), "cloudburrow.env")
	if err := os.WriteFile(envFile, plain, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("docker", "run", "--rm", "--network", "host", "--env-file", envFile,
		"--entrypoint", "sh", curlImage, "-c",
		`curl -fsS "$STORAGE_EMULATOR_HOST/storage/v1/b?project=$GOOGLE_CLOUD_PROJECT"`).CombinedOutput()
	if err != nil {
		t.Fatalf("the container could not list buckets: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"`+bucket+`"`) {
		t.Errorf("the container's bucket list lacks %s:\n%s", bucket, out)
	}
}
