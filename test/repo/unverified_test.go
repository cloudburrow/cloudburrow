package repo

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// unverifiedMarker is an UNVERIFIED marker: a choice CloudBurrow made where
// Google's documentation gave no answer. The compat tests' `// unverified:`
// annotations are markers too (annotationLine). The word in lower case is
// prose ("an unverified engine", "contents are unverified"), and the word
// quoted, or followed by "annotation" or "marker", names the convention
// rather than making a choice.
var unverifiedMarker = regexp.MustCompile("\\bUNVERIFIED\\b")

// namesTheMarker is the word naming the convention, not a marker.
var namesTheMarker = regexp.MustCompile("^(`| annotations?\\b| markers?\\b)")

// reReadDate is the date Google's docs were last re-read for a marker
// (#1593): "Docs re-read 2026-10-17, still unanswered: <url>" in code,
// "UNVERIFIED (docs re-read 2026-10-17: <url>)" in docs.
var reReadDate = regexp.MustCompile(`(?i)re-read \d{4}-\d{2}-\d{2}`)

// annotationLine is a compat test's `// unverified:` annotation.
var annotationLine = regexp.MustCompile(`^\s*//\s*unverified:`)

// unverifiedExempt are the files that name the marker without making a
// choice: the coverage tool, which parses the annotations, the pages it
// generates from them, and docs/gaps.md, which sums up the open choices and
// points at where each is written (and dated), and this test.
var unverifiedExempt = []string{
	"tools/coverage/",
	"docs/coverage/",
	"docs/gaps.md",
	"test/repo/unverified_test.go",
}

// TestUnverifiedMarkersAreDated (#1593): every UNVERIFIED marker carries the
// date Google's docs were last re-read for it, so the next audit can skip
// what was read recently and a new marker comes with the page read.
//
// In Go, a `// unverified:` annotation is dated on its own line or the next
// one; any other marker by a date in the comment block that holds it, or,
// on a code line, in the comment block just above. Elsewhere (docs/, READMEs),
// each marker needs a date of its own after it and before the next marker,
// within its table row or paragraph.
func TestUnverifiedMarkersAreDated(t *testing.T) {
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	dir := strings.TrimSpace(string(root))
	out, err := exec.Command("git", "-C", dir, "ls-files", "-z").Output()
	if err != nil {
		t.Fatal(err)
	}
	undated := 0
	for _, f := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if f == "" || strings.HasPrefix(f, "third_party/") || exempt(f) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue // deleted in the working tree
		}
		if bytes.IndexByte(b, 0) >= 0 || !unverifiedMarker.Match(b) && !(strings.HasSuffix(f, ".go") && bytes.Contains(b, []byte("unverified:"))) {
			continue
		}
		lines := strings.Split(string(b), "\n")
		var bad []int
		if strings.HasSuffix(f, ".go") {
			bad = undatedInGo(lines)
		} else {
			bad = undatedInText(lines)
		}
		for _, i := range bad {
			undated++
			t.Errorf("%s:%d: UNVERIFIED with no \"re-read YYYY-MM-DD\" date: %s", f, i+1, strings.TrimSpace(lines[i]))
		}
	}
	if undated > 0 {
		t.Logf("%d undated UNVERIFIED markers: re-read Google's page or proto for each, then settle it or add the date and the URL read", undated)
	}
}

func exempt(f string) bool {
	for _, e := range unverifiedExempt {
		if f == e || strings.HasSuffix(e, "/") && strings.HasPrefix(f, e) {
			return true
		}
	}
	return false
}

// markers returns where s holds a marker, leaving out the word naming the
// convention.
func markers(s string) [][]int {
	var out [][]int
	for _, at := range unverifiedMarker.FindAllStringIndex(s, -1) {
		if namesTheMarker.MatchString(s[at[1]:]) || at[0] > 0 && s[at[0]-1] == '`' {
			continue
		}
		out = append(out, at)
	}
	return out
}

func isComment(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "//")
}

// undatedInGo returns the indexes of the Go lines whose marker has no date.
func undatedInGo(lines []string) []int {
	var bad []int
	for i, line := range lines {
		if len(markers(line)) == 0 && !annotationLine.MatchString(line) || reReadDate.MatchString(line) {
			continue
		}
		if annotationLine.MatchString(line) {
			if i+1 < len(lines) && isComment(lines[i+1]) && !annotationLine.MatchString(lines[i+1]) && reReadDate.MatchString(lines[i+1]) {
				continue
			}
			bad = append(bad, i)
			continue
		}
		// The comment block holding the line, or the one just above a
		// code line.
		lo, hi := i, i
		if !isComment(line) {
			hi = i - 1
			lo = i
		}
		for lo > 0 && isComment(lines[lo-1]) {
			lo--
		}
		if isComment(line) {
			for hi+1 < len(lines) && isComment(lines[hi+1]) {
				hi++
			}
		}
		dated := false
		for j := lo; j <= hi; j++ {
			if reReadDate.MatchString(lines[j]) {
				dated = true
				break
			}
		}
		if !dated {
			bad = append(bad, i)
		}
	}
	return bad
}

// undatedInText returns the indexes of the lines holding a marker with no
// date after it before the next marker, within its table row (a line
// starting with "|") or its paragraph.
func undatedInText(lines []string) []int {
	var bad []int
	for i := 0; i < len(lines); {
		// One unit: a table row, or a paragraph of non-blank, non-table lines.
		j := i + 1
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), "|") && strings.TrimSpace(lines[i]) != "" {
			for j < len(lines) && strings.TrimSpace(lines[j]) != "" && !strings.HasPrefix(strings.TrimSpace(lines[j]), "|") {
				j++
			}
		}
		// Offsets of each line's start within the unit's text.
		var text strings.Builder
		starts := make([]int, 0, j-i)
		for k := i; k < j; k++ {
			starts = append(starts, text.Len())
			text.WriteString(lines[k])
			text.WriteString("\n")
		}
		s := text.String()
		marks := markers(s)
		for m, at := range marks {
			end := len(s)
			if m+1 < len(marks) {
				end = marks[m+1][0]
			}
			if reReadDate.MatchString(s[at[1]:end]) {
				continue
			}
			line := i
			for k, st := range starts {
				if st <= at[0] {
					line = i + k
				}
			}
			if len(bad) == 0 || bad[len(bad)-1] != line {
				bad = append(bad, line)
			}
		}
		i = j
	}
	return bad
}

// TestUnverifiedDateRules pins what counts as dated.
func TestUnverifiedDateRules(t *testing.T) {
	goSrc := []string{
		"// unverified: a/B NOT_FOUND: dated below",       // 0
		"// Docs re-read 2026-10-17, still unanswered: u", // 1
		"// unverified: a/B NOT_FOUND: not dated",         // 2
		"func f() {}",                        // 3
		"// A choice (UNVERIFIED), and",      // 4
		"// docs re-read 2026-10-17: u",      // 5
		"",                                   // 6
		"// Another choice (UNVERIFIED).",    // 7
		`return "x" // UNVERIFIED wording`,   // 8
		"// names the UNVERIFIED annotation", // 9
	}
	if got := undatedInGo(goSrc); len(got) != 3 || got[0] != 2 || got[1] != 7 || got[2] != 8 {
		t.Errorf("undatedInGo = %v, want [2 7 8]", got)
	}
	text := []string{
		"| a | x (UNVERIFIED (docs re-read 2026-10-17: <u>)); y (UNVERIFIED) |", // 0
		"| b | UNVERIFIED (docs re-read 2026-10-17: <u>) |",                     // 1
		"",                                // 2
		"A paragraph UNVERIFIED, wrapped", // 3
		"and dated (docs re-read 2026-10-17: <u>).", // 4
		"", // 5
		"An `UNVERIFIED` word, and an unverified engine.", // 6
	}
	if got := undatedInText(text); len(got) != 1 || got[0] != 0 {
		t.Errorf("undatedInText = %v, want [0]", got)
	}
}
