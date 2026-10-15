package buildpacks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeTree(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Each runtime is detected from the manifest Google's buildpack for it
// detects on.
func TestDetectFromManifest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ file, runtime string }{
		{"go.mod", "go"},
		{"package.json", "nodejs"},
		{"requirements.txt", "python"},
		{"pyproject.toml", "python"},
		{"pom.xml", "java"},
		{"build.gradle.kts", "java"},
		{"Gemfile", "ruby"},
		{"composer.json", "php"},
	} {
		d, err := Detect(writeTree(t, tc.file), nil)
		if err != nil || d.Runtime != tc.runtime || d.From != tc.file {
			t.Errorf("Detect(%s) = %+v, %v; want %s from %s", tc.file, d, err, tc.runtime, tc.file)
		}
	}
}

// Without a manifest, source files anywhere in the tree decide, as the
// buildpacks' globs do; dependency directories are not the application.
func TestDetectFromSources(t *testing.T) {
	t.Parallel()
	d, err := Detect(writeTree(t, "src/app/main.py", "node_modules/x/index.js"), nil)
	if err != nil || d.Runtime != "python" || d.From != "src/app/main.py" {
		t.Errorf("Detect = %+v, %v; want python from src/app/main.py", d, err)
	}
	d, err = Detect(writeTree(t, "App/App.csproj"), nil)
	if err != nil || d.Runtime != "dotnet" {
		t.Errorf("Detect(.csproj) = %+v, %v; want dotnet", d, err)
	}
}

// A root manifest outweighs stray sources: a Python service with a static
// .js file is a Python service.
func TestDetectManifestOutweighsSources(t *testing.T) {
	t.Parallel()
	d, err := Detect(writeTree(t, "requirements.txt", "main.py", "static/app.js"), nil)
	if err != nil || d.Runtime != "python" || d.From != "requirements.txt" {
		t.Errorf("Detect = %+v, %v; want python from requirements.txt", d, err)
	}
}

// Several manifests are the builder's to settle, in its own order: nothing is
// chosen or forced, and the candidates are reported.
func TestDetectAmbiguousLeavesTheBuilderToChoose(t *testing.T) {
	t.Parallel()
	d, err := Detect(writeTree(t, "package.json", "requirements.txt"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Runtime != "" || !reflect.DeepEqual(d.Candidates, []string{"nodejs", "python"}) {
		t.Errorf("Detect = %+v; want no runtime and candidates nodejs, python", d)
	}
	if !strings.Contains(d.String(), "chosen by the builder among nodejs, python") {
		t.Errorf("String() = %q", d.String())
	}
}

// GOOGLE_RUNTIME wins over every marker, as on Google, and its version
// suffix is kept apart.
func TestDetectGoogleRuntimeWins(t *testing.T) {
	t.Parallel()
	dir := writeTree(t, "go.mod", "package.json")
	for _, tc := range []struct{ env, runtime, version string }{
		{"python", "python", ""},
		{"nodejs22", "nodejs", "22"},
		{"go1.25", "go", "1.25"},
	} {
		d, err := Detect(dir, map[string]string{EnvRuntime: tc.env})
		if err != nil || d.Runtime != tc.runtime || d.Version != tc.version || d.From != EnvRuntime {
			t.Errorf("GOOGLE_RUNTIME=%s: Detect = %+v, %v; want %s version %q", tc.env, d, err, tc.runtime, tc.version)
		}
	}
	// And it reaches pack unchanged.
	args, err := Args(Request{Source: dir, Image: "reg:5000/a:v1", Registry: "reg:5000", Env: map[string]string{EnvRuntime: "nodejs22"}})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlag(args, "--env", "GOOGLE_RUNTIME=nodejs22") {
		t.Errorf("GOOGLE_RUNTIME did not reach pack: %v", args)
	}
}

// Detection never adds GOOGLE_RUNTIME: the builder detects as Google's does.
func TestArgsNeverForcesARuntime(t *testing.T) {
	t.Parallel()
	args, err := Args(Request{Source: writeTree(t, "package.json"), Image: "reg:5000/a:v1", Registry: "reg:5000", FunctionTarget: "Hello"})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range args {
		if strings.HasPrefix(a, EnvRuntime+"=") {
			t.Errorf("Args forced a runtime: %v", args)
		}
	}
}

// A source with no language at all is refused before pack runs, with the
// files the builder looks for and how to choose one.
func TestBuildRefusesASourceWithNoRuntime(t *testing.T) {
	t.Parallel()
	ran := false
	b := &Builder{Runner: runnerFunc(func() { ran = true })}
	err := b.Build(context.Background(), Request{Source: writeTree(t, "README.md", "static/index.html"), Image: "reg:5000/a:v1", Registry: "reg:5000"})
	if !errors.Is(err, ErrNoRuntime) || !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("Build = %v; want ErrNoRuntime and ErrBuildFailed", err)
	}
	for _, want := range []string{"package.json", "requirements.txt", "pom.xml", "GOOGLE_RUNTIME"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
	if ran {
		t.Error("pack ran for a source with no runtime")
	}
}

func TestDetectRejectsAMissingSource(t *testing.T) {
	t.Parallel()
	if _, err := Detect(filepath.Join(t.TempDir(), "absent"), nil); !errors.Is(err, ErrBuildFailed) {
		t.Errorf("Detect(missing) = %v; want ErrBuildFailed", err)
	}
}

// The fixtures the gated Functions tests build are detected as their
// languages, from the files Google's buildpacks detect them by.
func TestDetectTheFunctionFixtures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ dir, runtime, from string }{
		{"function", "go", "go.mod"},
		{"function-nodejs", "nodejs", "package.json"},
		{"function-python", "python", "requirements.txt"},
	} {
		d, err := Detect(filepath.Join("..", "..", "testdata", tc.dir), nil)
		if err != nil || d.Runtime != tc.runtime || d.From != tc.from {
			t.Errorf("Detect(testdata/%s) = %+v, %v; want %s from %s", tc.dir, d, err, tc.runtime, tc.from)
		}
	}
}

type runnerFunc func()

func (f runnerFunc) Run(context.Context, string, ...string) (string, error) {
	f()
	return "", nil
}

type failingRunner struct{}

func (failingRunner) Run(context.Context, string, ...string) (string, error) {
	return "ERROR: failed to build\n", errors.New("exit status 1")
}

// A failed build names the runtime it was built as, after the error that
// TestFunctionsBuildBrokenModulePathFails reads the start of.
func TestBuildFailureNamesTheRuntime(t *testing.T) {
	t.Parallel()
	if ok, _ := SupportedHere(); !ok {
		t.Skip("pack is not installed")
	}
	err := (&Builder{Runner: failingRunner{}}).Build(context.Background(),
		Request{Source: writeTree(t, "package.json"), Image: "reg:5000/a:v1", Registry: "reg:5000"})
	if !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("Build = %v; want ErrBuildFailed", err)
	}
	if msg := err.Error(); !strings.HasPrefix(msg, "build failed: exit status 1 (runtime nodejs, detected from package.json)\n") ||
		!strings.Contains(msg, "ERROR: failed to build") {
		t.Errorf("error = %q", msg)
	}
}
