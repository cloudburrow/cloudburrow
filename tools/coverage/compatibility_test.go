package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// compatSections maps every generated coverage page to the heading that
// opens its section of docs/compatibility.md. A section runs to the next
// `## ` heading, so its `###` subsections (Control plane, Version plane, Jobs
// and executions, ...) are part of it. A page with no entry here fails the
// test, so a new generated page cannot go unchecked.
var compatSections = map[string]string{
	"storage":         "## Cloud Storage — JSON API v1",
	"pubsub":          "## Pub/Sub — `google.pubsub.v1`",
	"tasks":           "## Cloud Tasks — `google.cloud.tasks.v2`",
	"run":             "## Cloud Run — `google.cloud.run.v2`",
	"scheduler":       "## Cloud Scheduler — `google.cloud.scheduler.v1`",
	"logging":         "## Cloud Logging — `google.logging.v2`",
	"kms":             "## Cloud KMS — `google.cloud.kms.v1`",
	"secretmanager":   "## Secret Manager — `google.cloud.secretmanager.v1`",
	"resourcemanager": "## Resource Manager — `google.cloud.resourcemanager.v3`",
	// The opt-in emulators share one section (#717). Its per-service rows
	// only link their pages, so the section also has a table whose rows name
	// each Verified RPC as Service.Method: the qualifier keeps Firestore's
	// Commit from matching Datastore's or Spanner's, and the
	// every-Verified-method-is-named check applies to them as to any page.
	"firestore": "## Optional services",
	"datastore": "## Optional services",
	"bigtable":  "## Optional services",
	"spanner":   "## Optional services",
}

// acceptedStatuses maps a status cell of docs/compatibility.md to the
// generated statuses it agrees with. Emphasis is ignored, and a cell is
// matched by its leading words, so "**Verified**, *stored, not enforced*"
// and "**Verified, measured**" are Verified.
//
//	compatibility.md                      docs/coverage
//	Verified ...                          Verified
//	Verified unsupported, Verified refused Unimplemented or Refused
//	Partial                               Verified or Implemented (served, gaps named in the row)
//	Implemented                           Implemented
//	Unimplemented, Refused, Not supported Unimplemented or Refused (both answer UNIMPLEMENTED:
//	                                      Refused is an upstream emulator's refusal proven by a
//	                                      compat test, Unimplemented CloudBurrow's own, proven
//	                                      in-process)
//	Planned                               Unimplemented, Refused or Not served
//	Not served                            Not served
//
// Any other status (Differs, Storage fact, ...) is fine on a row that names
// no method, and an error on one that does: a row that names a method must
// say where it stands in these terms.
func acceptedStatuses(cell string) ([]string, bool) {
	s := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(cell, "*", "")))
	switch {
	case strings.HasPrefix(s, "verified unsupported"), strings.HasPrefix(s, "verified refused"):
		return []string{"Unimplemented", "Refused"}, true
	case strings.HasPrefix(s, "verified"):
		return []string{"Verified"}, true
	case strings.HasPrefix(s, "partial"):
		return []string{"Verified", "Implemented"}, true
	case strings.HasPrefix(s, "implemented"):
		return []string{"Implemented"}, true
	case strings.HasPrefix(s, "unimplemented"), strings.HasPrefix(s, "refused"), strings.HasPrefix(s, "not supported"):
		return []string{"Unimplemented", "Refused"}, true
	case strings.HasPrefix(s, "planned"):
		return []string{"Unimplemented", "Refused", "Not served"}, true
	case strings.HasPrefix(s, "not served"):
		return []string{"Not served"}, true
	}
	return nil, false
}

// compatSection returns the section of doc that heading opens.
func compatSection(doc, heading string) (string, bool) {
	start := strings.Index(doc, "\n"+heading)
	if start < 0 {
		return "", false
	}
	section := doc[start+1:]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	return section, true
}

// tableRows returns the cells of each table row of a section, header rows
// included (their status cell, "Status", is no status and names no method).
func tableRows(section string) [][]string {
	var rows [][]string
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "| ") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 4 {
			continue
		}
		rows = append(rows, cells[1:len(cells)-1])
	}
	return rows
}

var (
	// A Storage JSON API method: `buckets.insert`, `projects.hmacKeys.get`.
	storageToken = regexp.MustCompile("`([a-z][A-Za-z]*(?:\\.[a-zA-Z]+)+)`")
	// A gRPC method: `CreateQueue`, or qualified by its service,
	// `Publisher.CreateTopic`.
	grpcToken = regexp.MustCompile("`(?:([A-Z][A-Za-z0-9]*)\\.)?([A-Z][A-Za-z0-9]*)`")
)

// namedMethods returns the report methods a row's first cell names. Only
// the first cell counts: the notes mention other methods in passing. A
// backticked token that is no method (a field, `/admin/reset`) is skipped;
// a bare gRPC name two services of the page share is an error, so the row
// must qualify it.
func namedMethods(key, op string, report map[string]string) (ids, ambiguous []string) {
	if key == "storage" {
		for _, tok := range storageToken.FindAllStringSubmatch(op, -1) {
			if _, ok := report["storage."+tok[1]]; ok {
				ids = append(ids, "storage."+tok[1])
			}
		}
		return ids, nil
	}
	for _, tok := range grpcToken.FindAllStringSubmatch(op, -1) {
		svc, method := tok[1], tok[2]
		var match []string
		for id := range report {
			full, m, ok := strings.Cut(id, "/")
			if !ok || m != method {
				continue
			}
			if svc == "" || full[strings.LastIndex(full, ".")+1:] == svc {
				match = append(match, id)
			}
		}
		switch len(match) {
		case 0:
		case 1:
			ids = append(ids, match[0])
		default:
			sort.Strings(match)
			ambiguous = append(ambiguous, method+" ("+strings.Join(match, ", ")+")")
		}
	}
	return ids, ambiguous
}

// TestCoverageMatchesCompatibility (#520, #688): for every page tools/coverage
// generates, each method a docs/compatibility.md row of that service's
// section names (in the row's first cell) has a status that agrees with the
// report (see acceptedStatuses), and every method the report calls Verified
// or Implemented is named in one of those rows, so neither document can drift
// from the other. Removing a `// covers:` annotation turns a Verified method
// Implemented (or Unknown), which then disagrees with its **Verified** row.
func TestCoverageMatchesCompatibility(t *testing.T) {
	pages, err := build("../..")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("../../docs/compatibility.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)

	for _, p := range pages {
		heading, ok := compatSections[p.Key]
		if !ok {
			t.Errorf("coverage page %s has no docs/compatibility.md section in compatSections", p.Key)
			continue
		}
		section, ok := compatSection(doc, heading)
		if !ok {
			t.Errorf("docs/compatibility.md has no %q section for coverage page %s", heading, p.Key)
			continue
		}
		report := map[string]string{}
		for _, r := range p.Rows {
			report[r.Method] = r.Status
		}
		named := map[string]bool{}
		for _, cells := range tableRows(section) {
			ids, ambiguous := namedMethods(p.Key, cells[0], report)
			for _, a := range ambiguous {
				t.Errorf("%s: the row %q names %s, which is ambiguous; qualify it as Service.Method", p.Key, strings.TrimSpace(cells[0]), a)
			}
			if len(ids) == 0 {
				continue
			}
			want, ok := acceptedStatuses(cells[1])
			if !ok {
				t.Errorf("%s: the row %q names %s but its status %q is not one acceptedStatuses knows", p.Key, strings.TrimSpace(cells[0]), strings.Join(ids, ", "), strings.TrimSpace(cells[1]))
				continue
			}
			for _, id := range ids {
				named[id] = true
				got := report[id]
				agrees := false
				for _, w := range want {
					agrees = agrees || w == got
				}
				if !agrees {
					t.Errorf("docs/compatibility.md calls %s %q; the coverage report says %s", id, strings.TrimSpace(cells[1]), got)
				}
			}
		}
		for _, r := range p.Rows {
			if (r.Status == "Verified" || r.Status == "Implemented") && !named[r.Method] {
				t.Errorf("%s is %s in the coverage report but no row of %q in docs/compatibility.md names it", r.Method, r.Status, heading)
			}
		}
	}

	// The Generated coverage paragraph links every page.
	start := strings.Index(doc, "\n### Generated coverage\n")
	if start < 0 {
		t.Fatal("docs/compatibility.md has no Generated coverage section")
	}
	gen := doc[start+1:]
	if end := strings.Index(gen[1:], "\n#"); end >= 0 {
		gen = gen[:end+1]
	}
	for _, p := range pages {
		if !strings.Contains(gen, "(coverage/"+p.Key+".md)") {
			t.Errorf("docs/compatibility.md's Generated coverage section does not link coverage/%s.md", p.Key)
		}
	}
}

var (
	goTestFunc = regexp.MustCompile(`(?m)^func (Test\w+)\(`)
	pyTestFunc = regexp.MustCompile(`(?m)^\s*(?:async\s+)?def (test_\w+)\(`)
	// A cited Go test, `TestX` or a prefix `TestX*`, and a cited pytest.
	goTestToken = regexp.MustCompile("`(Test[A-Za-z0-9_]*)(\\*?)`")
	pyTestToken = regexp.MustCompile("`(test_[a-z0-9_]+)`")
)

// definedTests returns every Go test function in a *_test.go file of the
// repository, whatever its build tags, and every pytest function in a .py
// file (test/compat-python).
func definedTests(t *testing.T, root string) (goTests, pyTests map[string]bool) {
	goTests, pyTests = map[string]bool{}, map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".venv", "venv":
				return filepath.SkipDir
			}
			return nil
		}
		var re *regexp.Regexp
		var into map[string]bool
		switch {
		case strings.HasSuffix(p, "_test.go"):
			re, into = goTestFunc, goTests
		case strings.HasSuffix(p, ".py"):
			re, into = pyTestFunc, pyTests
		default:
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range re.FindAllSubmatch(b, -1) {
			into[string(m[1])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return goTests, pyTests
}

// TestCompatibilityCitesOnlyDefinedTests (#688): every backticked `Test*`
// name (or `TestX*` prefix) and `test_*` pytest name in a docs/compatibility.md
// table row is a test defined in the repository, so a row cannot cite
// evidence that does not exist. A name that is a method of a generated page
// (`TestIamPermissions`) is a method, not a test.
func TestCompatibilityCitesOnlyDefinedTests(t *testing.T) {
	pages, err := build("../..")
	if err != nil {
		t.Fatal(err)
	}
	methods := map[string]bool{}
	for _, p := range pages {
		for _, r := range p.Rows {
			methods[r.Method[strings.LastIndex(r.Method, "/")+1:]] = true
		}
	}
	goTests, pyTests := definedTests(t, "../..")
	if len(goTests) == 0 || len(pyTests) == 0 {
		t.Fatalf("found %d Go and %d Python tests; the walk is wrong", len(goTests), len(pyTests))
	}
	b, err := os.ReadFile("../../docs/compatibility.md")
	if err != nil {
		t.Fatal(err)
	}
	missing := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		for _, m := range goTestToken.FindAllStringSubmatch(line, -1) {
			name, prefix := m[1], m[2] == "*"
			if methods[name] {
				continue
			}
			found := goTests[name]
			if prefix {
				for n := range goTests {
					found = found || strings.HasPrefix(n, name)
				}
			}
			if !found {
				missing[m[1]+m[2]] = true
			}
		}
		for _, m := range pyTestToken.FindAllStringSubmatch(line, -1) {
			if !pyTests[m[1]] {
				missing[m[1]] = true
			}
		}
	}
	var names []string
	for n := range missing {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t.Errorf("docs/compatibility.md cites %s, which no test in the repository defines", n)
	}
}
