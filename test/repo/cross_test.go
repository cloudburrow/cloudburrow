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

// TestCICheckRunsMakeCross (#713): the check job runs `make cross` on the
// Linux Go 1.27 row, the one row that also runs in merge groups, so a PR
// that breaks a release target fails before it lands.
func TestCICheckRunsMakeCross(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	_, check, ok := strings.Cut(string(raw), "\n  check:\n")
	if !ok {
		t.Fatal("ci.yml has no check job")
	}
	if i := regexp.MustCompile(`\n  [a-z0-9-]+:\n`).FindStringIndex(check); i != nil {
		check = check[:i[0]]
	}
	var live []string
	for _, line := range strings.Split(check, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			live = append(live, line)
		}
	}
	step := regexp.MustCompile(`(?m)^      - name: [^\n]+\n        if: runner\.os == 'Linux' && matrix\.go == '1\.27'\n        run: make cross$`)
	if !step.MatchString(strings.Join(live, "\n")) {
		t.Error("the check job has no `make cross` step on the Linux Go 1.27 row")
	}
	if !strings.Contains(check, `'[{"go":"1.27","os":"ubuntu-latest"}]'`) {
		t.Error("the merge-group matrix no longer includes the Linux Go 1.27 row that runs make cross")
	}
}
