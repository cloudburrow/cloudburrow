package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// The release CLIs are built with CGO_ENABLED=0 for all four targets (#714).
// Left to Go's default, the native linux/amd64 build linked the runner's
// glibc while the three cross builds did not, so the two Linux archives
// differed. The build sets it, and a step checks what each binary records.

const cgoCheckStep = "Check the CLI is built with CGO_ENABLED=0"

// buildSteps returns the build job's steps, in order.
func buildSteps(t *testing.T) []string {
	t.Helper()
	return strings.Split(job(t, releaseJobs(t), "build"), "\n      - ")[1:]
}

// stepRun returns a step's run: block, unindented.
func stepRun(t *testing.T, step string) string {
	t.Helper()
	_, script, ok := strings.Cut(step, "        run: |\n")
	if !ok {
		t.Fatalf("the step has no run block:\n%s", step)
	}
	var b strings.Builder
	for _, line := range strings.Split(script, "\n") {
		if line != "" && !strings.HasPrefix(line, "          ") {
			break
		}
		b.WriteString(strings.TrimPrefix(line, "          ") + "\n")
	}
	return b.String()
}

// stepIndex returns the index of the step named name, or -1.
func stepIndex(steps []string, name string) int {
	for i, s := range steps {
		if strings.HasPrefix(s, "name: "+name+"\n") {
			return i
		}
	}
	return -1
}

// TestReleaseCLIBuildSetsCGOEnabled: the go build of ./cmd/cloudburrow says
// CGO_ENABLED=0 itself, rather than inheriting whatever the runner defaults
// to.
func TestReleaseCLIBuildSetsCGOEnabled(t *testing.T) {
	steps := buildSteps(t)
	i := stepIndex(steps, "Build")
	if i < 0 {
		t.Fatal("the build job has no Build step")
	}
	// One go build command, with its continuation lines, ending in the CLI's
	// package (not ./cmd/cloudburrow-storage).
	cmd := regexp.MustCompile(`(?m)^(.*go build .*\\\n(?:.*\\\n)*.*\s\./cmd/cloudburrow)$`)
	m := cmd.FindStringSubmatch(stepRun(t, steps[i]))
	if m == nil {
		t.Fatal("the Build step has no go build of ./cmd/cloudburrow")
	}
	if !strings.HasPrefix(m[1], "CGO_ENABLED=0 go build ") {
		t.Errorf("the CLI build does not set CGO_ENABLED=0:\n%s", m[1])
	}
}

// TestReleaseChecksTheCLIsCGOSetting: a step between the build and the
// upload asserts each binary's recorded CGO_ENABLED, on every target.
func TestReleaseChecksTheCLIsCGOSetting(t *testing.T) {
	steps := buildSteps(t)
	build, check := stepIndex(steps, "Build"), stepIndex(steps, cgoCheckStep)
	upload := -1
	for i, s := range steps {
		if strings.HasPrefix(s, "uses: actions/upload-artifact@") {
			upload = i
		}
	}
	if check < 0 {
		t.Fatalf("the build job has no %q step", cgoCheckStep)
	}
	if !(build < check && check < upload) {
		t.Errorf("steps out of order: Build %d, check %d, upload %d; the check must run after the build and before the upload", build, check, upload)
	}
	if regexp.MustCompile(`(?m)^        if:`).MatchString(steps[check]) {
		t.Errorf("the check is conditional, so some targets skip it:\n%s", steps[check])
	}
	if !strings.Contains(stepRun(t, steps[check]), "go version -m") {
		t.Error("the check does not read go version -m")
	}
}

// runCGOCheck runs the check step as Actions would (bash -e) in a directory
// whose dist/ holds a CLI built with the given CGO_ENABLED, or none for "".
func runCGOCheck(t *testing.T, cgo string) (string, error) {
	t.Helper()
	for _, tool := range []string{"bash", "go", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not found")
		}
	}
	steps := buildSteps(t)
	i := stepIndex(steps, cgoCheckStep)
	if i < 0 {
		t.Fatalf("the build job has no %q step", cgoCheckStep)
	}
	dir := t.TempDir()
	if cgo != "" {
		src := filepath.Join(dir, "src")
		if err := os.MkdirAll(src, 0o755); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{
			"go.mod":  "module cgocheck\n\ngo 1.21\n",
			"main.go": "package main\n\nfunc main() {}\n",
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		out := filepath.Join(dir, "dist", "cloudburrow_v0.0.0_"+runtime.GOOS+"_"+runtime.GOARCH, "cloudburrow")
		build := exec.Command("go", "build", "-trimpath", "-o", out, ".")
		build.Dir = src
		build.Env = append(os.Environ(), "CGO_ENABLED="+cgo, "GOFLAGS=")
		if b, err := build.CombinedOutput(); err != nil {
			t.Skipf("cannot build a CGO_ENABLED=%s binary here: %v\n%s", cgo, err, b)
		}
	}
	cmd := exec.Command("bash", "-e", "-c", stepRun(t, steps[i]))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH)
	b, err := cmd.CombinedOutput()
	return string(b), err
}

func TestCGOCheckPassesAStaticCLI(t *testing.T) {
	out, err := runCGOCheck(t, "0")
	if err != nil {
		t.Fatalf("the check refused a CGO_ENABLED=0 binary: %v\n%s", err, out)
	}
	if !strings.Contains(out, "CGO_ENABLED=0") {
		t.Errorf("the check did not report the setting:\n%s", out)
	}
}

func TestCGOCheckRefusesACgoCLI(t *testing.T) {
	out, err := runCGOCheck(t, "1")
	if err == nil {
		t.Fatalf("the check passed a CGO_ENABLED=1 binary:\n%s", out)
	}
	if !strings.Contains(out, "::error::") || !strings.Contains(out, "CGO_ENABLED=1") {
		t.Errorf("the refusal does not say what the binary records:\n%s", out)
	}
}

func TestCGOCheckRefusesAMissingCLI(t *testing.T) {
	out, err := runCGOCheck(t, "")
	if err == nil {
		t.Fatalf("the check passed with no binary:\n%s", out)
	}
	if !strings.Contains(out, "::error::") {
		t.Errorf("no ::error:: for a missing binary:\n%s", out)
	}
}
