package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// staleDenial is a sentence the docs or the CLI once said, which a Verified
// row of docs/compatibility.md (or a coverage page) now contradicts. Each
// came back after its feature landed, because nothing tied the sentence to
// the row (2026-11-18: README, status.md and SECURITY.md still said "No IAM
// enforcement, anywhere" and "no sqladmin endpoint" after both were built).
type staleDenial struct {
	phrase   *regexp.Regexp
	evidence string // a substring of the compatibility.md row that disproves it
}

var staleDenials = []staleDenial{
	{regexp.MustCompile(`(?i)no IAM enforcement,? anywhere`),
		"| Memorystore for Redis Cluster and for Valkey: IAM auth checks the token | **Verified**"},
	{regexp.MustCompile(`(?i)no (identity|permission) is checked and no policy is evaluated|no policy is evaluated`),
		"| Memorystore for Redis Cluster and for Valkey: IAM auth checks the token | **Verified**"},
	{regexp.MustCompile("(?i)no `?sqladmin`? endpoint"),
		"| The Cloud SQL Admin API: `sql.instances.list`, `sql.instances.get` | **Verified**"},
	{regexp.MustCompile("(?i)IAM Credentials `signBlob`\\*\\*: refused"),
		"| IAM Credentials `signBlob` | **Verified**"},
	{regexp.MustCompile(`(?i)signed URLs are accepted on shape alone`),
		"| Signed URLs (V4 HMAC and RSA, V2) | **Verified"},
	{regexp.MustCompile(`(?i)over gRPC only, so .gcloud run jobs`),
		"| Terraform `google_cloud_run_v2_service`, `google_cloud_run_v2_job` | **Verified**"},
	{regexp.MustCompile(`(?i)No sinks, exclusions, buckets, log-based metrics`),
		"| Sinks: `CreateSink`"},
}

// claimFiles are the pages and sources whose words a reader takes for the
// product's: the README, SECURITY.md, the docs (not compatibility.md, the
// evidence, nor the ADRs, whose Context sections record what was true when
// they were decided), and the CLI's Go sources, whose strings are its help
// text and console notes.
func claimFiles(t *testing.T, dir string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "ls-files", "-z",
		"README.md", "SECURITY.md", "docs/*.md", "cmd/cloudburrow/*.go", "internal/netfwd/*.go", "internal/console/*.go").Output()
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f == "" || f == "docs/compatibility.md" || strings.HasSuffix(f, "_test.go") {
			continue
		}
		files = append(files, f)
	}
	return files
}

// TestDocsDoNotDenyVerifiedFeatures fails when a page or the CLI still says
// a feature is missing that compatibility.md marks Verified. A phrase stays
// allowed only while its evidence row is absent, so the list cannot rot into
// a ban on words: delete the entry once the row it names is gone.
func TestDocsDoNotDenyVerifiedFeatures(t *testing.T) {
	dir := repoRoot(t)
	compat, err := os.ReadFile(filepath.Join(dir, "docs", "compatibility.md"))
	if err != nil {
		t.Fatal(err)
	}
	files := claimFiles(t, dir)
	for _, d := range staleDenials {
		if !strings.Contains(string(compat), d.evidence) {
			t.Errorf("compatibility.md no longer has %q: drop or update the stale-denial entry for %s", d.evidence, d.phrase)
			continue
		}
		for _, f := range files {
			b, err := os.ReadFile(filepath.Join(dir, f))
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(b), "\n") {
				if d.phrase.MatchString(line) {
					t.Errorf("%s:%d says %q, but compatibility.md has %q", f, i+1, d.phrase.FindString(line), d.evidence)
				}
			}
		}
	}
}

// coverageRow is a method's row on a generated coverage page.
var coverageRow = regexp.MustCompile("^\\| `([^`]+)` \\| (Verified|Unimplemented|[A-Za-z ]+) \\|")

// refusalWords mark a sentence that says something is not served.
var refusalWords = regexp.MustCompile(`\b501\b|\bUNIMPLEMENTED\b|` + "`Unimplemented`" + `|\bnot implemented\b`)

// TestShortPagesDoNotCallVerifiedMethodsUnserved checks the two summary
// pages a reader trusts most, README.md and docs/status.md: a bullet or
// table row that says 501 or UNIMPLEMENTED may not name, in backticks, a
// method every coverage page that has it marks Verified (`bulkRestore`,
// `managedFolders.update`). A name that some page marks otherwise is left
// alone, as it may be the refused case the sentence means.
func TestShortPagesDoNotCallVerifiedMethodsUnserved(t *testing.T) {
	dir := repoRoot(t)
	pages, err := filepath.Glob(filepath.Join(dir, "docs", "coverage", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	verified := map[string]bool{} // suffix -> every method ending with it is Verified
	for _, p := range pages {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, line := range strings.Split(string(b), "\n") {
			m := coverageRow.FindStringSubmatch(line)
			if m == nil || seen[m[1]] {
				continue // the error-code table repeats methods; the first row is the method's status
			}
			seen[m[1]] = true
			parts := strings.Split(m[1], ".")
			for i := 1; i < len(parts); i++ {
				suffix := strings.Join(parts[i:], ".")
				ok, known := verified[suffix]
				verified[suffix] = (ok || !known) && m[2] == "Verified"
			}
		}
	}
	backticked := regexp.MustCompile("`([A-Za-z][A-Za-z0-9.]*)`")
	for _, f := range []string{"README.md", "docs/status.md"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		for i, block := range bullets(string(b)) {
			if !refusalWords.MatchString(block.text) {
				continue
			}
			for _, m := range backticked.FindAllStringSubmatch(block.text, -1) {
				// One-word names only where they read as methods (camelCase);
				// `run` or `logging` are commands, not methods.
				if !strings.Contains(m[1], ".") && strings.ToLower(m[1]) == m[1] {
					continue
				}
				if verified[m[1]] {
					t.Errorf("%s:%d (item %d) calls `%s` unserved, but every coverage row for it is Verified", f, block.line, i, m[1])
				}
			}
		}
	}
}

type block struct {
	line int
	text string
}

// bullets splits a page into its list items and table rows, each with the
// lines that continue it.
func bullets(s string) []block {
	var out []block
	for i, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "- "), strings.HasPrefix(trimmed, "| "):
			out = append(out, block{line: i + 1, text: trimmed})
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
			out = append(out, block{line: i + 1})
		case len(out) > 0:
			out[len(out)-1].text += " " + trimmed
		}
	}
	return out
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	return strings.TrimSpace(string(root))
}

// TestPersistenceTableMatchesTheRestartProbes ties install.md's "What
// survives a restart" table to the restart probes CI runs (#1801): a row
// that says **Yes** names its probes, each of which is in PROBE_TESTS of
// scripts/compat-shards.sh, and every probe there is named by a **Yes** row.
// A row that says anything else names no probe, so the table never claims a
// survival that no stop/up measured, nor leaves out one that is.
func TestPersistenceTableMatchesTheRestartProbes(t *testing.T) {
	root := repoRoot(t)
	doc, err := os.ReadFile(filepath.Join(root, "docs", "install.md"))
	if err != nil {
		t.Fatal(err)
	}
	shards, err := os.ReadFile(filepath.Join(root, "scripts", "compat-shards.sh"))
	if err != nil {
		t.Fatal(err)
	}
	probes := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*PROBE_TESTS="([^"]*)"`).FindAllStringSubmatch(string(shards), -1) {
		for _, name := range strings.Fields(m[1]) {
			probes[name] = true
		}
	}
	if len(probes) == 0 {
		t.Fatal("scripts/compat-shards.sh sets no PROBE_TESTS")
	}

	text := string(doc)
	start := strings.Index(text, "\n## What survives a restart\n")
	if start < 0 {
		t.Fatal(`docs/install.md has no "## What survives a restart" section`)
	}
	section := text[start+1:]
	if end := strings.Index(section[3:], "\n## "); end >= 0 {
		section = section[:end+3]
	}
	named := map[string]bool{}
	rows := 0
	for _, line := range strings.Split(section, "\n") {
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if len(cells) != 3 || strings.HasPrefix(strings.TrimSpace(cells[0]), "---") || strings.TrimSpace(cells[0]) == "Service" {
			continue
		}
		rows++
		service, claim, cited := strings.TrimSpace(cells[0]), strings.TrimSpace(cells[1]), cells[2]
		tests := regexp.MustCompile("`(Test[A-Za-z0-9_]+)`").FindAllStringSubmatch(cited, -1)
		yes := strings.HasPrefix(claim, "**Yes**")
		if !yes {
			for _, m := range tests {
				if probes[m[1]] {
					t.Errorf("%s: says %q but cites the restart probe %s", service, claim[:min(len(claim), 20)], m[1])
				}
			}
			continue
		}
		if len(tests) == 0 {
			t.Errorf("%s: says it survives stop/up but cites no restart probe", service)
		}
		for _, m := range tests {
			if !probes[m[1]] {
				t.Errorf("%s: cites %s, which no shard runs as a restart probe (PROBE_TESTS)", service, m[1])
			}
			named[m[1]] = true
		}
	}
	if rows == 0 {
		t.Fatal("found no rows in install.md's persistence table")
	}
	for p := range probes {
		if !named[p] {
			t.Errorf("the restart probe %s is in PROBE_TESTS but no **Yes** row of install.md's table names it", p)
		}
	}
}
