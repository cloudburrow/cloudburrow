package doctor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// fakeEnv builds an Env whose every probe answers from memory, so a test can
// describe a machine that does not exist.
type fakeEnv struct {
	missing   map[string]bool
	dockerJS  string
	dockerErr error
	kindOut   string
	kindErr   error
	busy      map[int]bool
	free      uint64
	freeErr   error
	home      string
	goos      string
}

func (f fakeEnv) env() Env {
	return Env{
		LookPath: func(bin string) (string, error) {
			if f.missing[bin] {
				return "", exec.ErrNotFound
			}
			return "/usr/bin/" + bin, nil
		},
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			switch {
			case name == "docker" && len(args) > 0 && args[0] == "info":
				return []byte(f.dockerJS), f.dockerErr
			case name == "kind":
				return []byte(f.kindOut), f.kindErr
			}
			return nil, fmt.Errorf("unexpected command %s %v", name, args)
		},
		PortFree: func(_ string, port int) error {
			if f.busy[port] {
				return errors.New("address already in use")
			}
			return nil
		},
		DiskFree: func(path string) (uint64, string, error) {
			return f.free, path, f.freeErr
		},
		HomeDir: func() (string, error) { return f.home, nil },
		GOOS:    f.goos,
	}
}

func healthy() fakeEnv {
	return fakeEnv{
		dockerJS: `{"NCPU":8,"MemTotal":8589934592,"DockerRootDir":"/var/lib/docker",` +
			`"ServerVersion":"29.0.0","OperatingSystem":"Docker Desktop"}`,
		kindOut: "kind " + KindVersion + " go1.27 darwin/arm64",
		free:    100 << 30,
		home:    "/home/dev",
		goos:    "linux",
	}
}

func find(t *testing.T, r Report, name string) Result {
	t.Helper()
	for _, res := range r.Results {
		if res.Name == name {
			return res
		}
	}
	t.Fatalf("no result named %q in %v", name, names(r))
	return Result{}
}

func names(r Report) []string {
	out := make([]string, 0, len(r.Results))
	for _, res := range r.Results {
		out = append(out, res.Name)
	}
	return out
}

func TestHealthyMachinePassesEverything(t *testing.T) {
	t.Parallel()
	r := Run(context.Background(), healthy().env(), Options{Ports: map[string]int{"control": 9000}})
	for _, res := range r.Results {
		if res.Level != LevelOK {
			t.Errorf("%s = %s (%s) on a healthy machine", res.Name, res.Level, res.Detail)
		}
	}
	if r.Blocking() {
		t.Error("a healthy machine reported blocking problems")
	}
}

// A missing binary must block: every one of them is executed directly, so
// continuing would fail later with an exec error from inside a component.
func TestMissingBinaryBlocks(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.missing = map[string]bool{"kind": true}
	r := Run(context.Background(), f.env(), Options{})

	got := find(t, r, "kind")
	if got.Level != LevelFail {
		t.Errorf("missing kind = %s, want FAIL", got.Level)
	}
	if got.Remedy == "" {
		t.Error("a failure with no remedy just moves the problem")
	}
	if !r.Blocking() {
		t.Error("a missing binary must block")
	}
}

// A different kind release may work perfectly well. Reporting it as a failure
// would be inventing a result nothing measured.
func TestUnpinnedKindWarnsRatherThanFails(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.kindOut = "kind v0.99.0 go1.27 linux/amd64"
	r := Run(context.Background(), f.env(), Options{})

	got := find(t, r, "kind")
	if got.Level != LevelWarn {
		t.Errorf("unpinned kind = %s, want warn", got.Level)
	}
	if !strings.Contains(got.Detail, "v0.99.0") {
		t.Errorf("detail should name the version found: %q", got.Detail)
	}
	if r.Blocking() {
		t.Error("an untested kind version must not block")
	}
}

func TestUnreportedKindVersionIsUnknownNotOK(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.kindErr = errors.New("exec failed")
	r := Run(context.Background(), f.env(), Options{})

	if got := find(t, r, "kind").Level; got != LevelUnknown {
		t.Errorf("unreadable kind version = %s, want unknown: an unanswerable "+
			"question is not a passing answer", got)
	}
}

func TestStoppedDaemonBlocks(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.dockerJS = "Cannot connect to the Docker daemon at unix:///var/run/docker.sock."
	f.dockerErr = errors.New("exit status 1")
	r := Run(context.Background(), f.env(), Options{})

	got := find(t, r, "docker daemon")
	if got.Level != LevelFail {
		t.Errorf("stopped daemon = %s, want FAIL", got.Level)
	}
	if !strings.Contains(got.Detail, "Cannot connect") {
		t.Errorf("the daemon's own message was lost: %q", got.Detail)
	}
}

// Too little memory is the failure this command exists for: without it,
// Knative's control plane is OOM-killed and the symptom is pods stuck
// Pending, which points at nothing.
func TestInsufficientMemoryBlocks(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.dockerJS = `{"NCPU":8,"MemTotal":2147483648,"ServerVersion":"29.0.0"}`
	r := Run(context.Background(), f.env(), Options{})

	got := find(t, r, "docker memory")
	if got.Level != LevelFail {
		t.Fatalf("2 GiB = %s, want FAIL", got.Level)
	}
	if !strings.Contains(got.Detail, "2.0 GiB") || !strings.Contains(got.Detail, "4.0 GiB") {
		t.Errorf("detail should state what is available and what is required: %q", got.Detail)
	}
	if !r.Blocking() {
		t.Error("insufficient memory must block")
	}
}

func TestInsufficientCPUsBlocks(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.dockerJS = `{"NCPU":1,"MemTotal":8589934592,"ServerVersion":"29.0.0"}`
	r := Run(context.Background(), f.env(), Options{})
	if got := find(t, r, "docker cpus").Level; got != LevelFail {
		t.Errorf("1 CPU = %s, want FAIL", got)
	}
}

// A daemon that does not report a figure must not be reported as passing it.
func TestUnreportedResourcesAreUnknown(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.dockerJS = `{"ServerVersion":"29.0.0"}`
	r := Run(context.Background(), f.env(), Options{})

	for _, name := range []string{"docker memory", "docker cpus"} {
		if got := find(t, r, name).Level; got != LevelUnknown {
			t.Errorf("%s = %s, want unknown", name, got)
		}
	}
	if r.Blocking() {
		t.Error("an unmeasurable resource must not block")
	}
}

// Low disk warns rather than blocks: it is a prediction about images not yet
// pulled, and a developer who knows better should not be stopped.
func TestLowDiskWarnsAndNamesTheMargin(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.free = 5 << 30
	r := Run(context.Background(), f.env(), Options{})

	got := find(t, r, "disk space")
	if got.Level != LevelWarn {
		t.Errorf("5 GiB free = %s, want warn", got.Level)
	}
	if !strings.Contains(got.Detail, "20.0 GiB") {
		t.Errorf("detail should name the margin: %q", got.Detail)
	}
	if strings.Contains(got.Remedy, "prune -a") || strings.Contains(got.Remedy, "system prune") {
		t.Errorf("the remedy must not tell CloudBurrow to prune Docker globally: %q", got.Remedy)
	}
	if r.Blocking() {
		t.Error("low disk must not block")
	}
}

// On a machine where the daemon's root is inside a VM, the number reported
// must say what it actually measured.
func TestDiskOnAVMHostLabelsWhatItMeasured(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.goos = "darwin"
	r := Run(context.Background(), f.env(), Options{})

	got := find(t, r, "disk space")
	if !strings.Contains(got.Detail, "/home/dev") {
		t.Errorf("detail should name the path measured: %q", got.Detail)
	}
	if !strings.Contains(got.Detail, "not the VM filesystem") {
		t.Errorf("a number whose meaning is unclear is worse than one labelled honestly: %q", got.Detail)
	}
}

func TestBusyPortBlocksAndSuggestsTheEscape(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.busy = map[int]bool{9002: true}
	r := Run(context.Background(), f.env(), Options{
		BindAddress: "127.0.0.1",
		Ports:       map[string]int{"control": 9000, "pubsub": 9002},
	})

	got := find(t, r, "port pubsub")
	if got.Level != LevelFail {
		t.Fatalf("busy port = %s, want FAIL", got.Level)
	}
	if !strings.Contains(got.Remedy, "--port-pubsub 0") {
		t.Errorf("remedy should offer the OS-assigned escape: %q", got.Remedy)
	}
	if find(t, r, "port control").Level != LevelOK {
		t.Error("a free port was reported as taken")
	}
}

// Port 0 asks the OS to choose, so there is nothing to collide with and
// nothing worth printing.
func TestOSAssignedPortsAreNotChecked(t *testing.T) {
	t.Parallel()
	r := Run(context.Background(), healthy().env(), Options{Ports: map[string]int{"control": 0}})
	for _, res := range r.Results {
		if strings.HasPrefix(res.Name, "port ") {
			t.Errorf("an OS-assigned port produced a result: %+v", res)
		}
	}
}

func TestReportRendersRemediesOnlyForProblems(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	Report{Results: []Result{
		{Name: "fine", Level: LevelOK, Detail: "all good", Remedy: "unused"},
		{Name: "broken", Level: LevelFail, Detail: "nope", Remedy: "do the thing"},
	}}.Write(&sb)

	out := sb.String()
	if strings.Contains(out, "unused") {
		t.Errorf("a passing check printed a remedy:\n%s", out)
	}
	if !strings.Contains(out, "do the thing") {
		t.Errorf("a failure printed no remedy:\n%s", out)
	}
	if !strings.Contains(out, "1 blocking problem") {
		t.Errorf("the summary did not count the failure:\n%s", out)
	}
}

func TestHumanMatchesDockerDesktopUnits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   uint64
		want string
	}{
		{512, "512 B"},
		{4 << 30, "4.0 GiB"},
		{1536 << 20, "1.5 GiB"},
		{2 << 40, "2.0 TiB"},
	} {
		if got := human(tc.in); got != tc.want {
			t.Errorf("human(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
