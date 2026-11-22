package hostai

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func writeRunner(t *testing.T, dir string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, LiteRTLMRunnerName)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func onMac(t *testing.T) {
	t.Helper()
	o, a := goos, goarch
	t.Cleanup(func() { goos, goarch = o, a })
	goos, goarch = "darwin", "arm64"
}

// The runner is found beside the CLI in litert-lm/, the release archive's
// layout, through a link to the CLI as Homebrew's bin/ has.
func TestLiteRTLMRunnerBesideTheCLI(t *testing.T) {
	onMac(t)
	t.Setenv(LiteRTLMDirEnv, "")
	root := t.TempDir()
	want := writeRunner(t, filepath.Join(root, "dist", "litert-lm"), 0o755)
	cli := filepath.Join(root, "dist", "cloudburrow")
	if err := os.WriteFile(cli, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "bin", "cloudburrow")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cli, link); err != nil {
		t.Fatal(err)
	}
	defer func(e func() (string, error)) { executable = e }(executable)
	executable = func() (string, error) { return link, nil }
	got, err := LiteRTLMRunner()
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := filepath.EvalSymlinks(want); got != r && got != want {
		t.Errorf("LiteRTLMRunner() = %s; want %s", got, want)
	}
}

// Homebrew's layout: libexec/cloudburrow/litert-lm beside bin/.
func TestLiteRTLMRunnerInLibexec(t *testing.T) {
	onMac(t)
	t.Setenv(LiteRTLMDirEnv, "")
	root := t.TempDir()
	writeRunner(t, filepath.Join(root, "libexec", "cloudburrow", "litert-lm"), 0o755)
	defer func(e func() (string, error)) { executable = e }(executable)
	executable = func() (string, error) { return filepath.Join(root, "bin", "cloudburrow"), nil }
	got, err := LiteRTLMRunner()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, filepath.Join("libexec", "cloudburrow", "litert-lm", LiteRTLMRunnerName)) {
		t.Errorf("LiteRTLMRunner() = %s", got)
	}
}

// The variable, when set, is the only place looked in; a file that is not
// executable is not a runner; and the error says how to get one.
func TestLiteRTLMRunnerFromTheVariableOnly(t *testing.T) {
	onMac(t)
	root := t.TempDir()
	writeRunner(t, filepath.Join(root, "litert-lm"), 0o755)
	defer func(e func() (string, error)) { executable = e }(executable)
	executable = func() (string, error) { return filepath.Join(root, "cloudburrow"), nil }

	elsewhere := filepath.Join(root, "elsewhere")
	writeRunner(t, elsewhere, 0o644)
	t.Setenv(LiteRTLMDirEnv, elsewhere)
	_, err := LiteRTLMRunner()
	if !errors.Is(err, ErrNoLiteRTLMRunner) || !strings.Contains(err.Error(), elsewhere) ||
		!strings.Contains(err.Error(), "build-macos.sh") || !strings.Contains(err.Error(), LiteRTLMDirEnv) {
		t.Fatalf("LiteRTLMRunner() with a non-executable file = %v", err)
	}
	want := writeRunner(t, elsewhere, 0o755)
	if got, err := LiteRTLMRunner(); err != nil || got != want {
		t.Errorf("LiteRTLMRunner() = %s, %v; want %s", got, err, want)
	}
}

// Off a Mac with Apple silicon there is no Metal runner to find.
func TestLiteRTLMRunnerNeedsMetal(t *testing.T) {
	o, a := goos, goarch
	defer func() { goos, goarch = o, a }()
	goos, goarch = "linux", "arm64"
	t.Setenv(LiteRTLMDirEnv, t.TempDir())
	if _, err := LiteRTLMRunner(); !errors.Is(err, ErrNoMetal) {
		t.Errorf("LiteRTLMRunner() on linux = %v; want ErrNoMetal", err)
	}
}

// The macOS build (deploy/litert-lm/build-macos.sh) pins the LiteRT-LM
// commit the Linux image's Dockerfile does, the bazelisk version it does,
// and the darwin-arm64 sum dependencies.json records (#2159).
func TestMacOSBuildPinsTheInventory(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", p))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	script, docker, inv := read("deploy/litert-lm/build-macos.sh"), read("deploy/litert-lm/Dockerfile"), read("dependencies.json")
	pin := func(src, re string) string {
		m := regexp.MustCompile(re).FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("no %s", re)
		}
		return m[1]
	}
	if a, b := pin(script, `(?m)^LITERT_LM_REF=([0-9a-f]{40})$`), pin(docker, `(?m)^ARG LITERT_LM_REF=([0-9a-f]{40})$`); a != b {
		t.Errorf("build-macos.sh builds LiteRT-LM %s; the Dockerfile %s", a, b)
	}
	if a, b := pin(script, `(?m)^BAZELISK_VERSION=(\S+)$`), pin(docker, `(?m)^ARG BAZELISK_VERSION=(\S+)$`); a != b {
		t.Errorf("build-macos.sh fetches bazelisk %s; the Dockerfile %s", a, b)
	}
	sum := pin(script, `(?m)^BAZELISK_SHA256=([0-9a-f]{64})$`)
	if !strings.Contains(inv, `"bazelisk-darwin-arm64": "sha256:`+sum+`"`) {
		t.Errorf("dependencies.json localai.bazelisk does not record bazelisk-darwin-arm64 sha256:%s", sum)
	}
}
