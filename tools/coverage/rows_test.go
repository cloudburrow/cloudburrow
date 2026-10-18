package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docTable is one Markdown table of a document: the line each row starts on
// and its cells, header and separator rows left out.
type docTable struct {
	heading string
	lines   []int
	rows    [][]string
}

// docTables splits a document into its tables. A table is a run of lines
// starting `|`; its first two lines are its header and separator.
func docTables(doc string) []docTable {
	var tables []docTable
	var cur *docTable
	heading := ""
	for i, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "#") {
			heading = line
		}
		if !strings.HasPrefix(line, "|") {
			cur = nil
			continue
		}
		if cur == nil {
			tables = append(tables, docTable{heading: heading})
			cur = &tables[len(tables)-1]
			cur.lines = append(cur.lines, -1) // the header
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 4 || strings.Trim(cells[1], " -:") == "" {
			continue // the separator
		}
		cur.lines = append(cur.lines, i+1)
		cur.rows = append(cur.rows, cells[1:len(cells)-1])
	}
	for i := range tables {
		tables[i].lines = tables[i].lines[1:]
	}
	return tables
}

// shingles returns a row's five-word runs.
func shingles(row string) map[string]bool {
	w := strings.Fields(row)
	out := map[string]bool{}
	for i := 0; i+5 <= len(w); i++ {
		out[strings.Join(w[i:i+5], " ")] = true
	}
	return out
}

// duplicateRows returns a message for each pair of rows of one table that
// describe the same feature: the same first cell, or most of the same
// words. Merges kept both sides of a row an edit changed (Landing 107 found
// nine such pairs and a triple, #1566), so one said 501 or "not tested"
// for what the other said was since built.
func duplicateRows(doc string) []string {
	var out []string
	for _, tb := range docTables(doc) {
		sh := make([]map[string]bool, len(tb.rows))
		for i, r := range tb.rows {
			sh[i] = shingles(strings.Join(r, "|"))
		}
		for i := range tb.rows {
			for j := i + 1; j < len(tb.rows); j++ {
				a, b := strings.TrimSpace(tb.rows[i][0]), strings.TrimSpace(tb.rows[j][0])
				if a == b && a != "" {
					out = append(out, fmt.Sprintf("lines %d and %d are two rows for %q: merge them into one that keeps every fact", tb.lines[i], tb.lines[j], a))
					continue
				}
				small := min(len(sh[i]), len(sh[j]))
				if small < 20 {
					continue // too short to compare
				}
				shared := 0
				for s := range sh[i] {
					if sh[j][s] {
						shared++
					}
				}
				if 10*shared > 6*small {
					out = append(out, fmt.Sprintf("lines %d and %d share %d of %d five-word runs: one is another's stale copy (%.60q, %.60q); merge them into one that keeps every fact", tb.lines[i], tb.lines[j], shared, small, a, b))
				}
			}
		}
	}
	return out
}

// TestCompatibilityHasNoDuplicateRows (#1566): no table of
// docs/compatibility.md has two rows for one feature.
func TestCompatibilityHasNoDuplicateRows(t *testing.T) {
	b, err := os.ReadFile("../../docs/compatibility.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range duplicateRows(string(b)) {
		t.Error("docs/compatibility.md: " + m)
	}
}

// TestAPlantedStaleRowFails (#1566): a row a merge left beside its newer
// copy, as the Terraform row was (one copy said the folder and
// organization policies were 501, the other that they were verified), and
// a second row under the same first cell, are both found; distinct rows
// are not.
func TestAPlantedStaleRowFails(t *testing.T) {
	long := "Untested is untested. The other resources of the served services are untested, and the provider has no snapshot resource; verified in the fourth batch: certificates and read replicas, a maintenance policy, folder and organization IAM, and a version's deletion policy;"
	doc := "## S\n\n| Feature | Status | Notes |\n|---|---|---|\n" +
		"| Terraform resources | Planned | " + long + " the folder org policy methods are not served (501). |\n" +
		"| Something else | **Verified** | `TestOther`: a row of its own. |\n" +
		"| Terraform resources beyond these | Planned | " + long + " the folder org policy is verified since #1534 (`TestTerraformFolderAndOrganizationPolicy`). |\n" +
		"| `DeleteQueue` | **Verified** | `TestA` |\n" +
		"| `DeleteQueue` | **Unimplemented** | 501 |\n" +
		"\n| Other table | Status | Notes |\n|---|---|---|\n| `DeleteQueue` | **Verified** | `TestB` |\n"
	got := duplicateRows(doc)
	if len(got) != 2 || !strings.Contains(got[0], "lines 5 and 7 share") || !strings.Contains(got[1], "lines 8 and 9 are two rows") {
		t.Errorf("duplicateRows = %q, want the Terraform pair and the DeleteQueue pair, and not the other table's row", got)
	}
}

var (
	// A test of test/compat-node: `test('title', ...)`. A title with
	// `${...}` in it is a template: what comes before it is a prefix.
	nodeTestTitle = regexp.MustCompile("\\btest\\(\\s*['\"`]([^'\"`]+)['\"`]")
	backticked    = regexp.MustCompile("`([^`]+)`")
	sameTest      = regexp.MustCompile(`(?i)\bsame tests?\b|\bboth (?:tests|Pub/Sub tests)\b`)
	namedTest     = regexp.MustCompile(`(?:^|[^A-Za-z0-9_./])Test[A-Z0-9_][A-Za-z0-9_]*|` + "`test_[a-z0-9_]+`")
)

// nodeTitles returns the titles of test/compat-node's tests.
func nodeTitles(t *testing.T, root string) (titles, prefixes []string) {
	files, err := filepath.Glob(filepath.Join(root, "test/compat-node/*.test.mjs"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no Node tests under %s: %v", root, err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range nodeTestTitle.FindAllStringSubmatch(string(b), -1) {
			if before, _, ok := strings.Cut(m[1], "${"); ok {
				prefixes = append(prefixes, before)
			} else {
				titles = append(titles, m[1])
			}
		}
	}
	return titles, prefixes
}

// verifiedWithoutTests returns each row of doc whose status is Verified and
// that names no test: no Go test, no pytest, no Node test (a row of the
// Node table names its tests by title) and no "same test" of an earlier row
// of its table that names one.
func verifiedWithoutTests(doc string, nodeTests map[string]bool, nodePrefixes []string) []string {
	var out []string
	for _, tb := range docTables(doc) {
		named := false // an earlier row of the table names a test
		for i, cells := range tb.rows {
			row := strings.Join(cells, "|")
			names := namedTest.MatchString(row)
			for _, m := range backticked.FindAllStringSubmatch(row, -1) {
				if nodeTests[m[1]] || strings.HasSuffix(m[1], ".test.mjs") || strings.HasSuffix(m[1], ".py") {
					names = true
				}
				for _, p := range nodePrefixes {
					names = names || strings.HasPrefix(m[1], p)
				}
			}
			verified := false
			for _, c := range cells[1:min(3, len(cells))] {
				s := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(c, "*", "")))
				verified = verified || strings.HasPrefix(s, "verified")
			}
			if verified && !names && !(named && sameTest.MatchString(row)) {
				out = append(out, fmt.Sprintf("line %d, %.80q, is Verified and names no test", tb.lines[i], strings.TrimSpace(cells[0])))
			}
			named = named || names
		}
	}
	return out
}

// TestEveryVerifiedRowNamesATest (#1566): a Verified row of
// docs/compatibility.md names the test that verifies it (that the test
// exists is TestCompatibilityCitesOnlyDefinedTests's check), or says it is
// the same test as a row above it in its table that names one.
func TestEveryVerifiedRowNamesATest(t *testing.T) {
	titles, prefixes := nodeTitles(t, "../..")
	nodeTests := map[string]bool{}
	for _, n := range titles {
		nodeTests[n] = true
	}
	b, err := os.ReadFile("../../docs/compatibility.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range verifiedWithoutTests(string(b), nodeTests, prefixes) {
		t.Error("docs/compatibility.md: " + m)
	}
	// A Verified Node table row's evidence titles a test of test/compat-node.
	doc := string(b)
	start := strings.Index(doc, "\n### Node.js client libraries\n")
	if start < 0 {
		t.Fatal("docs/compatibility.md has no Node.js client libraries section")
	}
	for _, tb := range docTables(doc[start+1:])[:1] {
		for i, cells := range tb.rows {
			if len(cells) < 4 || !strings.HasPrefix(strings.ToLower(strings.Trim(cells[2], " *")), "verified") {
				continue
			}
			for _, m := range backticked.FindAllStringSubmatch(cells[len(cells)-1], -1) {
				ok := nodeTests[m[1]] || strings.HasSuffix(m[1], ".mjs") || strings.HasPrefix(m[1], "@google-cloud/")
				for _, p := range prefixes {
					ok = ok || strings.HasPrefix(m[1], p)
				}
				if !ok && strings.Contains(m[1], " ") {
					t.Errorf("docs/compatibility.md's Node table cites %q, which no test in test/compat-node is titled (row %d)", m[1], i+1)
				}
			}
		}
	}
}

// TestAVerifiedRowWithoutATestFails (#1566).
func TestAVerifiedRowWithoutATestFails(t *testing.T) {
	doc := "| Feature | Status | Notes |\n|---|---|---|\n" +
		"| A | **Verified** | `TestA` |\n" +
		"| B | **Verified** | Same test, another case. |\n" +
		"| C | **Verified** | Removes the queue's tasks too. |\n" +
		"| D | **Verified** | `queue and task create, get and delete` |\n" +
		"| E | **Verified** | `example: Firestore` |\n" +
		"| F | **Unimplemented** | 501 |\n" +
		"\n| Feature | Status | Notes |\n|---|---|---|\n| G | **Verified** | Same test. |\n"
	got := verifiedWithoutTests(doc, map[string]bool{"queue and task create, get and delete": true}, []string{"example: "})
	if len(got) != 2 || !strings.Contains(got[0], `"C"`) || !strings.Contains(got[1], `"G"`) {
		t.Errorf("verifiedWithoutTests = %q, want C and G (a same test with none above it)", got)
	}
}
