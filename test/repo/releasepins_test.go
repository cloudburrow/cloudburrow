package repo

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A release pin is a place where a page tells the reader which release to
// install or verify, as opposed to a page recording what an older release
// did ("v0.1.0 has no trust gate"). After v0.2.0, install.md, ci.md and
// SECURITY.md still pinned v0.1.0, and cloudburrow.com copied them (#1806).
// These are the shapes a pin takes in this repository.
var releasePins = []*regexp.Regexp{
	// "Current release: [vX](.../releases/tag/vX)", "currently [vX](...)".
	regexp.MustCompile(`cloudburrow/cloudburrow/releases/tag/(v\d+\.\d+\.\d+)\b`),
	// The install script's "--version vX".
	regexp.MustCompile(`--version (v\d+\.\d+\.\d+)\b`),
	// An archive's name, as in "gh attestation verify cloudburrow_vX_darwin_arm64.tar.gz".
	regexp.MustCompile(`\bcloudburrow_(v\d+\.\d+\.\d+)_(?:darwin|linux)_`),
	// The action's input in docs/ci.md's workflow, whose shape cloudburrow.com
	// reads: "version: vX   # or latest, ...".
	regexp.MustCompile(`(?m)^\s+version: (v\d+\.\d+\.\d+)\s+# or latest`),
	// action.yml's "Release tag to install (such as vX)".
	regexp.MustCompile(`Release tag to install \(such as (v\d+\.\d+\.\d+)\)`),
}

// pinFiles are the pages a reader installs from: the README, SECURITY.md,
// every page under docs/, the install script's usage and the action's inputs.
// CHANGELOG.md is the history, not a pin.
func pinFiles(t *testing.T, root string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files", "-z",
		"README.md", "SECURITY.md", "docs/*.md", "examples", "scripts/install.sh", "action.yml").Output()
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	seen := map[string]bool{}
	for _, f := range strings.Split(string(out), "\x00") {
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		files = append(files, f)
	}
	return files
}

// latestRelease is the newest "## [x.y.z]" heading of CHANGELOG.md, which
// the release step adds when it tags (CHANGELOG.md's own instructions).
func latestRelease(t *testing.T, root string) [3]int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	var newest [3]int
	found := false
	for _, m := range regexp.MustCompile(`(?m)^## \[(\d+)\.(\d+)\.(\d+)\]`).FindAllStringSubmatch(string(b), -1) {
		v := [3]int{atoi(m[1]), atoi(m[2]), atoi(m[3])}
		if !found || less(newest, v) {
			newest, found = v, true
		}
	}
	if !found {
		t.Fatal("CHANGELOG.md has no ## [x.y.z] release heading")
	}
	return newest
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		panic(err)
	}
	return n
}

func less(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func parseTag(tag string) [3]int {
	m := regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`).FindStringSubmatch(tag)
	return [3]int{atoi(m[1]), atoi(m[2]), atoi(m[3])}
}

// TestDocsPinTheLatestRelease fails when a page pins a release older than
// the newest one CHANGELOG.md records, so the release step cannot leave a
// stale install command, attestation example or "current release" link.
func TestDocsPinTheLatestRelease(t *testing.T) {
	root := repoRoot(t)
	newest := latestRelease(t, root)
	want := fmt.Sprintf("v%d.%d.%d", newest[0], newest[1], newest[2])
	pins := 0
	for _, f := range pinFiles(t, root) {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, re := range releasePins {
			for _, m := range re.FindAllStringSubmatchIndex(src, -1) {
				pins++
				tag := src[m[2]:m[3]]
				if less(parseTag(tag), newest) {
					line := strings.Count(src[:m[0]], "\n") + 1
					t.Errorf("%s:%d pins %s; the latest release in CHANGELOG.md is %s", f, line, tag, want)
				}
			}
		}
	}
	// The pins this test exists for: if none is found, a pattern has drifted
	// from the pages and the test checks nothing.
	if pins < 6 {
		t.Errorf("found %d release pins in the docs; want at least 6 (README, SECURITY.md, install.md, ci.md, install.sh, action.yml)", pins)
	}
}

// TestReleasePinPatternsCatchAStalePin keeps the patterns honest: each one
// must find the old release in the shape it is written for.
func TestReleasePinPatternsCatchAStalePin(t *testing.T) {
	samples := []string{
		"**Released: [v0.1.0](https://github.com/cloudburrow/cloudburrow/releases/tag/v0.1.0).**",
		"install.sh | sh -s -- --version v0.1.0 --prefix /usr/local",
		"gh attestation verify cloudburrow_v0.1.0_darwin_arm64.tar.gz --repo cloudburrow/cloudburrow",
		"        with:\n          version: v0.1.0            # or latest, or source (needs actions/setup-go)\n",
		"      Release tag to install (such as v0.1.0), `latest`, or `source` to build",
	}
	for i, re := range releasePins {
		m := re.FindStringSubmatch(samples[i])
		if m == nil || m[1] != "v0.1.0" {
			t.Errorf("pattern %s does not find v0.1.0 in %q", re, samples[i])
		}
	}
	// What a page may say about an older release is not a pin.
	for _, s := range []string{
		"It needs v0.1.0 or later, and refuses an older CLI before `up`",
		"| `up --trust` (#598, #619) | v0.2.0 | left out. v0.1.0 has no trust gate",
		"          version: v0.33.0", // kind's, in the same workflow
	} {
		for _, re := range releasePins {
			if re.MatchString(s) {
				t.Errorf("pattern %s takes %q for a pin", re, s)
			}
		}
	}
}
