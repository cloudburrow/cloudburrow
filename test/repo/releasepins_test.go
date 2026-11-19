package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A release pin is a place where a page tells the reader which release to
// install or verify, as opposed to a page recording what an older release
// did ("v0.1.0 has no trust gate"). After v0.2.0, install.md, ci.md and
// SECURITY.md still pinned v0.1.0, and cloudburrow.com copied them (#1806).
//
// Since #1898 a release goes out every one to two days, from a commit that
// is already on main, and nothing is committed back: a page on main can
// never name the release cut from it. So there is one source of truth for
// the current version, GitHub's latest release (releases/latest, which the
// release workflow moves only after the smoke installs pass), and a pin
// names it rather than a number: a releases/latest link, the `latest`
// input, or the vX.Y.Z placeholder (#1901). cloudburrow.com resolves
// releases/latest when it builds and shows the tag itself.
//
// These are the shapes a pin takes in this repository. Each captures what
// the pin names: a concrete vN.N.N fails the test.
var releasePins = []*regexp.Regexp{
	// "Current release: [the latest](.../releases/latest)"; a
	// ".../releases/tag/vX" link is a pin to one release.
	regexp.MustCompile(`cloudburrow/cloudburrow/releases/(latest\b|tag/v[\dX]+\.[\dY]+\.[\dZ]+\b)`),
	// The install script's "--version vX".
	regexp.MustCompile(`--version (v[\dX]+\.[\dY]+\.[\dZ]+)\b`),
	// An archive's name, as in "gh attestation verify cloudburrow_vX_darwin_arm64.tar.gz".
	regexp.MustCompile(`\bcloudburrow_(v[\dX]+\.[\dY]+\.[\dZ]+)_(?:darwin|linux)_`),
	// The action's input in docs/ci.md's workflow, whose shape cloudburrow.com
	// reads: "version: latest   # or a release tag (vX.Y.Z), ...".
	regexp.MustCompile(`(?m)^\s+version: (latest|v[\dX]+\.[\dY]+\.[\dZ]+)\s+# or `),
	// action.yml's "Release tag to install (vX)".
	regexp.MustCompile(`Release tag to install \((?:such as )?(v[\dX]+\.[\dY]+\.[\dZ]+)\)`),
}

// concreteRelease is a pin that names one release instead of the latest.
var concreteRelease = regexp.MustCompile(`v\d+\.\d+\.\d+`)

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

// TestDocsPinTheLatestRelease fails when a page pins a numbered release,
// which falls behind as soon as the scheduled release tags the next one, so
// no install command, attestation example or "current release" link can go
// stale (#1806, #1901).
func TestDocsPinTheLatestRelease(t *testing.T) {
	root := repoRoot(t)
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
				if got := src[m[2]:m[3]]; concreteRelease.MatchString(got) {
					line := strings.Count(src[:m[0]], "\n") + 1
					t.Errorf("%s:%d pins %s; a page names the latest release instead (releases/latest, `latest` or vX.Y.Z), since a scheduled release is tagged every one to two days without a commit to main", f, line, concreteRelease.FindString(got))
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
// must find a numbered release in the shape it is written for, and the
// latest-release form too.
func TestReleasePinPatternsCatchAStalePin(t *testing.T) {
	stale := []string{
		"**Released: [v0.1.0](https://github.com/cloudburrow/cloudburrow/releases/tag/v0.1.0).**",
		"install.sh | sh -s -- --version v0.1.0 --prefix /usr/local",
		"gh attestation verify cloudburrow_v0.1.0_darwin_arm64.tar.gz --repo cloudburrow/cloudburrow",
		"        with:\n          version: v0.1.0            # or latest, or source (needs actions/setup-go)\n",
		"      Release tag to install (such as v0.1.0), `latest`, or `source` to build",
	}
	current := []string{
		"**Released: [the latest release](https://github.com/cloudburrow/cloudburrow/releases/latest)**",
		"install.sh | sh -s -- --version vX.Y.Z --prefix /usr/local",
		"gh attestation verify cloudburrow_vX.Y.Z_darwin_arm64.tar.gz --repo cloudburrow/cloudburrow",
		"        with:\n          version: latest            # or a release tag (vX.Y.Z), or source\n",
		"      Release tag to install (vX.Y.Z), `latest`, or `source` to build",
	}
	for i, re := range releasePins {
		if m := re.FindStringSubmatch(stale[i]); m == nil || concreteRelease.FindString(m[1]) != "v0.1.0" {
			t.Errorf("pattern %s does not find v0.1.0 in %q", re, stale[i])
		}
		if m := re.FindStringSubmatch(current[i]); m == nil || concreteRelease.MatchString(m[1]) {
			t.Errorf("pattern %s does not take %q for a latest-release pin", re, current[i])
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
