package repo

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The harness's Context carries a 60-second deadline, and a context derived
// from it can never outlast it: context.WithTimeout(h.Context(), 30*time.Minute)
// still ends after 60 seconds. Thirty-three compat tests waited that way for
// builds and deploys and failed when a runner was slow (functions.yml run
// 38919482569), so a test that needs longer takes its own context from
// context.Background().
func TestCompatLongContextsDoNotDeriveFromTheHarness(t *testing.T) {
	re := regexp.MustCompile(`WithTimeout\(h\.Context\(\),\s*[0-9]+\s*\*\s*time\.(Minute|Hour)`)
	files, err := filepath.Glob(filepath.Join("..", "compat", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no files in test/compat")
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if re.MatchString(line) {
				t.Errorf("%s:%d derives a context of minutes from h.Context(), which ends after 60s; use context.Background()", filepath.Base(f), i+1)
			}
		}
	}
}
