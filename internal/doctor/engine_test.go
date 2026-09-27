package doctor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture is a recorded `docker info --format '{{json .}}'` from
// testdata/dockerinfo. Each file's _provenance says whether it was captured
// or constructed; only docker-desktop-macos.json was captured.
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "dockerinfo", name))
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Provenance string `json:"_provenance"`
	}
	if err := json.Unmarshal(b, &meta); err != nil || meta.Provenance == "" {
		t.Fatalf("%s: every fixture must say where it came from (_provenance): %v", name, err)
	}
	return string(b)
}

// Every engine is classified from its `docker info` alone, through Env, and
// doctor names it, warning where Cloud Run pods may not reach this machine.
func TestDoctorClassifiesTheEngineFromDockerInfo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		fixture, goos string
		kind          EngineKind
		inVM          bool
		rootless      bool
		level         Level
		detail        string // a substring the doctor row must carry
		remedy        string // a substring of the remedy, when it warns
	}{
		{"docker-desktop-macos.json", "darwin", EngineDockerDesktop, true, false, LevelOK, "Docker Desktop (in a VM)", ""},
		{"docker-engine-linux.json", "linux", EngineDockerEngine, false, false, LevelOK, "Docker Engine (rootful, on this host)", ""},
		{"docker-desktop-linux.json", "linux", EngineDockerDesktop, true, false, LevelWarn, "unverified on linux", "Container engines"},
		{"colima-macos.json", "darwin", EngineColima, true, false, LevelWarn, "colima (in a VM); unverified", "colima runs the daemon in a VM"},
		{"orbstack-macos.json", "darwin", EngineOrbStack, true, false, LevelWarn, "OrbStack (in a VM); unverified", "OrbStack runs the daemon in a VM"},
		{"podman-machine-macos.json", "darwin", EnginePodman, true, true, LevelWarn, "Podman (in a VM, rootless); unsupported", "Podman is unsupported"},
		{"rootless-docker-linux.json", "linux", EngineRootlessDocker, false, true, LevelWarn, "rootless Docker; unsupported", "rootless daemon's network namespace"},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			t.Parallel()
			f := healthy()
			f.dockerJS = fixture(t, c.fixture)
			f.goos = c.goos

			info, _, err := dockerInfo(context.Background(), f.env())
			if err != nil {
				t.Fatal(err)
			}
			e := ClassifyEngine(info, c.goos)
			if e.Kind != c.kind || e.InVM != c.inVM || e.Rootless != c.rootless {
				t.Errorf("ClassifyEngine = %+v; want kind %q, inVM %v, rootless %v", e, c.kind, c.inVM, c.rootless)
			}

			r := Run(context.Background(), f.env(), Options{})
			got := find(t, r, "docker engine")
			if got.Level != c.level {
				t.Errorf("level = %s, want %s (%s)", got.Level, c.level, got.Detail)
			}
			if !strings.Contains(got.Detail, c.detail) {
				t.Errorf("detail %q lacks %q", got.Detail, c.detail)
			}
			if c.level != LevelOK && !strings.Contains(got.Remedy, c.remedy) {
				t.Errorf("remedy %q lacks %q", got.Remedy, c.remedy)
			}
			if got.Level == LevelFail {
				t.Error("an engine warning must never block: the engine may work")
			}
		})
	}
}

// Rancher Desktop and plain lima VMs are named by their hostname, and any
// other daemon a macOS client talks to is in some VM.
func TestClassifyEngineByHostname(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		info DockerInfo
		goos string
		want EngineKind
	}{
		{DockerInfo{Name: "lima-rancher-desktop", OperatingSystem: "Alpine Linux v3.20"}, "darwin", EngineRancherDesktop},
		{DockerInfo{Name: "lima-docker", OperatingSystem: "Ubuntu 24.04 LTS"}, "linux", EngineLima},
		{DockerInfo{Name: "colima-work", OperatingSystem: "Ubuntu 24.04 LTS"}, "linux", EngineColima},
		{DockerInfo{Name: "buildbox", OperatingSystem: "Ubuntu 24.04 LTS"}, "darwin", EngineOtherVM},
	} {
		if got := ClassifyEngine(c.info, c.goos); got.Kind != c.want || !got.InVM {
			t.Errorf("ClassifyEngine(%+v, %s) = %+v; want %q in a VM", c.info, c.goos, got, c.want)
		}
	}
}

// With no answer from the daemon the engine is unknown, not assumed.
func TestEngineWithoutDockerInfoIsUnknown(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.dockerJS = "Cannot connect to the Docker daemon"
	f.dockerErr = context.DeadlineExceeded
	got := find(t, Run(context.Background(), f.env(), Options{}), "docker engine")
	if got.Level != LevelUnknown {
		t.Errorf("engine with no docker info = %s, want unknown", got.Level)
	}
}

// A Linux client talking to Docker Desktop's VM must not measure the host's
// filesystem under the daemon's /var/lib/docker, which is a path in the VM:
// it measures the home volume holding the VM disk, and says so (#712).
func TestDiskOnDockerDesktopForLinuxIsLabelled(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.dockerJS = fixture(t, "docker-desktop-linux.json")
	f.goos = "linux"
	var asked []string
	e := f.env()
	e.DiskFree = func(path string) (uint64, string, error) {
		asked = append(asked, path)
		return f.free, path, nil
	}
	got := find(t, Run(context.Background(), e, Options{}), "disk space")
	if len(asked) != 1 || asked[0] != "/home/dev" {
		t.Errorf("measured %v; want the home volume, not the VM's /var/lib/docker", asked)
	}
	if !strings.Contains(got.Detail, "Docker Desktop VM disk, not the VM filesystem") {
		t.Errorf("the figure is not labelled with what it measured: %q", got.Detail)
	}
}

// A daemon on this host is measured exactly at its root, rootless included,
// whose root is under the user's home on the host.
func TestDiskOnAHostDaemonMeasuresItsRoot(t *testing.T) {
	t.Parallel()
	for fx, want := range map[string]string{
		"docker-engine-linux.json":   "/var/lib/docker",
		"rootless-docker-linux.json": "/home/dev/.local/share/docker",
	} {
		f := healthy()
		f.dockerJS = fixture(t, fx)
		got := find(t, Run(context.Background(), f.env(), Options{}), "disk space")
		if !strings.Contains(got.Detail, "free on "+want) || strings.Contains(got.Detail, "(") {
			t.Errorf("%s: disk detail %q; want an unlabelled exact figure on %s", fx, got.Detail, want)
		}
	}
}

// On Linux with no answer from the daemon, the home directory is measured
// and labelled as a stand-in, not claimed to be a VM disk or the daemon root.
func TestDiskWithoutDockerInfoOnLinuxIsLabelledAsAStandIn(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.dockerJS = ""
	f.dockerErr = context.DeadlineExceeded
	got := find(t, Run(context.Background(), f.env(), Options{}), "disk space")
	if !strings.Contains(got.Detail, "/home/dev") || !strings.Contains(got.Detail, "did not say where it stores images") {
		t.Errorf("disk detail %q", got.Detail)
	}
}
