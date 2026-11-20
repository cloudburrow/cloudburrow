package repo

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// releaseTargets are the four platforms release.yml builds.
var releaseTargets = []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64"}

// TestMakeCrossCoversReleaseTargets (#713): `make cross` builds and vets
// every package, without cgo, for each release target.
func TestMakeCrossCoversReleaseTargets(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	mk := string(raw)
	m := regexp.MustCompile(`(?m)^CROSS_TARGETS := (.+)$`).FindStringSubmatch(mk)
	if m == nil {
		t.Fatal("Makefile has no CROSS_TARGETS")
	}
	got := strings.Fields(m[1])
	for _, want := range releaseTargets {
		found := false
		for _, g := range got {
			found = found || g == want
		}
		if !found {
			t.Errorf("CROSS_TARGETS = %v, missing %s", got, want)
		}
	}
	_, recipe, ok := strings.Cut(mk, "\ncross:\n")
	if !ok {
		t.Fatal("Makefile has no cross target")
	}
	recipe, _, _ = strings.Cut(recipe, "\n\n")
	for _, want := range []string{
		"CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build ./... || exit 1",
		"CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go vet ./... || exit 1",
	} {
		if !strings.Contains(recipe, want) {
			t.Errorf("cross recipe does not contain %q", want)
		}
	}
}

// TestCICrossJobBuildsEveryReleaseTarget (#713): CI's cross job runs
// `make cross` for each release target, one a row, and gates ci-green, so a
// PR that breaks a release target fails before it lands. It left the check
// job for its own (PR #1987: the check job ran out of time in make cross).
func TestCICrossJobBuildsEveryReleaseTarget(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	_, cross, ok := strings.Cut(string(raw), "\n  cross:\n")
	if !ok {
		t.Fatal("ci.yml has no cross job")
	}
	if i := regexp.MustCompile(`\n  [a-z0-9-]+:\n`).FindStringIndex(cross); i != nil {
		cross = cross[:i[0]]
	}
	if !strings.Contains(cross, "run: make cross CROSS_TARGETS=${{ matrix.target }}") {
		t.Error("the cross job does not run make cross for its row's target")
	}
	mk, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^CROSS_TARGETS := (.+)$`).FindStringSubmatch(string(mk))
	if m == nil {
		t.Fatal("the Makefile has no CROSS_TARGETS")
	}
	want := "target: [" + strings.Join(strings.Fields(m[1]), ", ") + "]"
	if !strings.Contains(cross, want) {
		t.Errorf("the cross job's matrix is not the Makefile's CROSS_TARGETS: want %q", want)
	}
	_, green, _ := strings.Cut(string(raw), "\n  ci-green:\n")
	if !regexp.MustCompile(`needs: \[[^\]]*\bcross\b`).MatchString(green) {
		t.Error("ci-green does not need the cross job")
	}
}
