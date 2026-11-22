package vertexai

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner writes a shell script standing in for cloudburrow_generate:
// it records its arguments and either prints text or fails as a GPU
// delegate that cannot start does.
func fakeHostRunner(t *testing.T, fail bool) (string, string) {
	t.Helper()
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + args + "\n"
	if fail {
		body += "echo 'ERROR: Failed to create the GPU delegate' >&2\nexit 3\n"
	} else {
		body += "printf 'hello'\necho 'cloudburrow_generate: decoded_tokens=1' >&2\n"
	}
	p := filepath.Join(dir, "cloudburrow_generate")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p, args
}

// A host generator runs the native runner with the backend and the model
// path, as the runner takes options (#2159), and says where it runs.
func TestHostGeneratorRunsTheRunnerOnItsBackend(t *testing.T) {
	runner, args := fakeHostRunner(t, false)
	g := NewHostGenerator(runner, "/models/m.litertlm", "m", BackendGPU)
	if !g.HonoursOptions() || g.Model() != "m" {
		t.Fatalf("HonoursOptions %v, Model %q", g.HonoursOptions(), g.Model())
	}
	if got := collect(t, g, "hi"); got != "hello" {
		t.Errorf("text = %q", got)
	}
	b, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(got) < 3 || got[0] != "--backend=gpu" || got[1] != "--model_path=/models/m.litertlm" || got[len(got)-1] != "--input_prompt=hi" {
		t.Errorf("runner args = %q", got)
	}
	if a := g.Accelerator(); !strings.Contains(a, "Apple GPU") || !strings.Contains(a, "host process") {
		t.Errorf("Accelerator() = %q", a)
	}
	if a := NewHostGenerator(runner, "m", "m", BackendCPU).Accelerator(); a != "CPU, a host process" {
		t.Errorf("CPU host Accelerator() = %q", a)
	}
	if a := NewDockerGenerator("img", "/d", "f", "m").Accelerator(); a != "CPU, in the runtime container" {
		t.Errorf("container Accelerator() = %q", a)
	}
}

// Probe passes for a runner that generates and returns the runner's own
// error for one that fails.
func TestProbe(t *testing.T) {
	ok, _ := fakeHostRunner(t, false)
	if err := Probe(context.Background(), NewHostGenerator(ok, "m", "m", BackendGPU)); err != nil {
		t.Errorf("Probe of a working runner = %v", err)
	}
	bad, args := fakeHostRunner(t, true)
	err := Probe(context.Background(), NewHostGenerator(bad, "m", "m", BackendGPU))
	if err == nil || !strings.Contains(err.Error(), "GPU delegate") {
		t.Errorf("Probe of a failing runner = %v; want its message", err)
	}
	if b, _ := os.ReadFile(args); !strings.Contains(string(b), "--max_output_tokens=1") {
		t.Errorf("the probe asked for more than one token: %s", b)
	}
}
