package install

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The smoke job is the only place a published archive is installed and run
// before users get it (#603). It must cover every archive the build job
// publishes, each on a runner native to that archive's platform, or the
// archive ships without anyone having run even `version` (#689).

// nativeRunner is the platform each GitHub-hosted runner label runs, from
// docs.github.com's list of standard runners for public repositories.
// install.sh picks the archive from `uname -s`/`uname -m`, so a smoke entry
// only tests the platform its runner really is.
var nativeRunner = map[string]string{
	"ubuntu-latest":    "linux/amd64",
	"ubuntu-24.04":     "linux/amd64",
	"ubuntu-24.04-arm": "linux/arm64",
	"macos-latest":     "darwin/arm64",
	"macos-15":         "darwin/arm64",
	"macos-15-intel":   "darwin/amd64",
	"macos-26-intel":   "darwin/amd64",
}

// flowMaps returns the `- { k: v, ... }` matrix entries in a job.
func flowMaps(j string) []map[string]string {
	var out []map[string]string
	entry := regexp.MustCompile(`(?m)^\s+- \{(.*)\}\s*$`)
	for _, m := range entry.FindAllStringSubmatch(j, -1) {
		kv := map[string]string{}
		for _, field := range strings.Split(m[1], ",") {
			k, v, ok := strings.Cut(field, ":")
			if ok {
				kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
		out = append(out, kv)
	}
	return out
}

// builtPlatforms is every GOOS/GOARCH pair the build job publishes.
func builtPlatforms(t *testing.T, jobs map[string]string) []string {
	t.Helper()
	var out []string
	for _, e := range flowMaps(job(t, jobs, "build")) {
		if e["goos"] == "" || e["goarch"] == "" {
			t.Fatalf("build matrix entry without goos/goarch: %v", e)
		}
		out = append(out, e["goos"]+"/"+e["goarch"])
	}
	if len(out) == 0 {
		t.Fatal("found no build matrix entries")
	}
	sort.Strings(out)
	return out
}

// statusRow returns docs/status.md's Shipped platforms row for an archive.
func statusRow(t *testing.T, platform string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "status.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "| **"+platform+"** |") {
			return line
		}
	}
	t.Fatalf("docs/status.md's Shipped platforms table has no row for %s", platform)
	return ""
}

func TestReleaseSmokeRunsEveryPublishedArchiveNatively(t *testing.T) {
	jobs := releaseJobs(t)
	smoke := job(t, jobs, "smoke")

	if !strings.Contains(smoke, "runs-on: ${{ matrix.os }}") {
		t.Error("smoke must run on its matrix's runner")
	}
	covered := map[string]string{}
	for _, e := range flowMaps(smoke) {
		runner, platform := e["os"], e["platform"]
		native, known := nativeRunner[runner]
		switch {
		case runner == "" || platform == "":
			t.Errorf("smoke matrix entry without os/platform: %v", e)
			continue
		case !known:
			t.Errorf("smoke runner %s is not one this test knows the platform of; add it to nativeRunner from docs.github.com", runner)
			continue
		case native != platform:
			t.Errorf("smoke runner %s runs %s, but its entry claims %s: install.sh would install the %s archive", runner, native, platform, native)
		}
		if prev, dup := covered[platform]; dup {
			t.Errorf("%s is smoked twice (%s and %s)", platform, prev, runner)
		}
		covered[platform] = runner
		if e["brew"] != "true" {
			t.Errorf("smoke on %s skips the Homebrew formula; Homebrew is on every hosted macOS and Ubuntu image, and the formula serves %s", runner, platform)
		}
	}

	for _, p := range builtPlatforms(t, jobs) {
		if _, ok := covered[p]; ok {
			continue
		}
		// The one allowed exception: no hosted runner for the platform,
		// and status.md says the archive is built but never run.
		if !strings.Contains(statusRow(t, p), "never run") {
			t.Errorf("the release publishes %s but smoke never installs it, and docs/status.md does not say it is built but never run", p)
		}
	}
	for p := range covered {
		if !contains(builtPlatforms(t, jobs), p) {
			t.Errorf("smoke covers %s, which the build job does not publish", p)
		}
	}
}

func TestReleaseSmokeChecksTheInstalledPlatform(t *testing.T) {
	smoke := job(t, releaseJobs(t), "smoke")
	if !strings.Contains(smoke, "SMOKE_PLATFORM: ${{ matrix.platform }}") {
		t.Error("the install step must pass SMOKE_PLATFORM so smoke-release.sh checks the binary it installed is the runner's platform")
	}
	if !strings.Contains(smoke, "if: matrix.brew") {
		t.Error("the Homebrew step must be gated on matrix.brew, not on one OS")
	}
	if !strings.Contains(smoke, "brew shellenv") {
		t.Error("the Homebrew step must put Linux's /home/linuxbrew brew on PATH")
	}

	raw, err := os.ReadFile(scriptPath(t, "smoke-release.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `*", ${SMOKE_PLATFORM})")`) {
		t.Error("smoke-release.sh must compare `cloudburrow version`'s platform with SMOKE_PLATFORM")
	}
}
