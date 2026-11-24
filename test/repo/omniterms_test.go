package repo

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNothingAcceptsTheAlloyDBOmniTerms (#2207): Google's AlloyDB Omni
// licence terms are accepted only by the end user, so no workflow,
// Makefile target, script or test that CloudBurrow's CI or a worker runs
// may accept them: none passes --accept-alloydb-omni-terms, sets
// CLOUDBURROW_ACCEPT_ALLOYDB_OMNI_TERMS, writes alloydb.acceptOmniTerms
// true, or sends the acceptance header. docs/alloydb.md's command for the
// user to run themselves is the one place it is written down.
func TestNothingAcceptsTheAlloyDBOmniTerms(t *testing.T) {
	root := filepath.Join("..", "..")
	run := []*regexp.Regexp{
		regexp.MustCompile(`--accept-alloydb-omni-terms`),
		regexp.MustCompile(`ACCEPT_ALLOYDB_OMNI_TERMS["']?\s*[:=]`),
		regexp.MustCompile(`"acceptOmniTerms"\s*:\s*true`),
	}
	// In Go tests a mention of the flag is text (a refusal's wording); what
	// would accept is setting the variable, or sending the header as Y.
	goTests := []*regexp.Regexp{
		regexp.MustCompile(`Setenv\([^)]*ACCEPT_ALLOYDB_OMNI_TERMS`),
		regexp.MustCompile(`(?i)(omniTermsHeader|Accept-AlloyDB-Omni-Terms"?)\s*,\s*"Y"`),
		regexp.MustCompile(`"acceptOmniTerms"\s*:\s*true`),
	}
	check := func(path string, pats []*regexp.Regexp) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range pats {
			if loc := re.FindIndex(b); loc != nil {
				line := strings.Count(string(b[:loc[0]]), "\n") + 1
				t.Errorf("%s:%d accepts Google's AlloyDB Omni terms (%s); only the user may", path, line, re)
			}
		}
	}
	for _, dir := range []string{".github", "scripts"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				check(p, run)
			}
			return nil
		})
	}
	check(filepath.Join(root, "Makefile"), run)
	for _, dir := range []string{filepath.Join("test", "compat"), filepath.Join("test", "browser")} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, "_test.go") {
				check(p, goTests)
			}
			return nil
		})
	}
}
