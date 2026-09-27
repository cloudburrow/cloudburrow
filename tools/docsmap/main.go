// Command docsmap fails `make docs-check` when two hand-kept tables drift
// from the repository (#588):
//
//   - docs/architecture.md §3's module map must list every package directory
//     under internal/ (a directory holding only other directories, such as
//     internal/service, is covered by listing each child), every listed path
//     must exist, and a row marked **Planned** must name a path that does not
//     exist yet — once it lands, the row has to be updated.
//
//   - docs/compatibility.md's Console section must not mark the same subject
//     both Verified and Not supported.
//
//     go run ./tools/docsmap
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func main() {
	var problems []string
	arch, err := os.ReadFile("docs/architecture.md")
	if err != nil {
		fail(err)
	}
	p, err := checkModuleMap(string(arch), ".")
	if err != nil {
		fail(err)
	}
	problems = append(problems, p...)
	compat, err := os.ReadFile("docs/compatibility.md")
	if err != nil {
		fail(err)
	}
	problems = append(problems, checkConsole(string(compat))...)
	if len(problems) > 0 {
		fmt.Fprintf(os.Stderr, "docsmap: %d problem(s):\n  %s\n", len(problems), strings.Join(problems, "\n  "))
		os.Exit(1)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "docsmap:", err)
	os.Exit(2)
}

// row is one module-map table row: its path and whether it is Planned.
type row struct {
	path    string
	planned bool
}

var mapRow = regexp.MustCompile("^\\| `([^`]+)` \\|(.*)\\|\\s*$")

// moduleMap returns the rows of the table in architecture.md's
// "## 3. Module boundaries" section.
func moduleMap(doc string) []row {
	var rows []row
	in := false
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "## ") {
			in = strings.HasPrefix(line, "## 3. Module boundaries")
			continue
		}
		if !in {
			continue
		}
		if m := mapRow.FindStringSubmatch(line); m != nil {
			rows = append(rows, row{path: strings.TrimSuffix(m[1], "/"), planned: strings.Contains(m[2], "**Planned**")})
		}
	}
	return rows
}

// checkModuleMap compares the module map with the directories under root.
func checkModuleMap(doc, root string) ([]string, error) {
	rows := moduleMap(doc)
	if len(rows) == 0 {
		return []string{"docs/architecture.md: no module map table found under \"## 3. Module boundaries\""}, nil
	}
	var problems []string
	listed := map[string]bool{}
	for _, r := range rows {
		listed[r.path] = true
		_, err := os.Stat(filepath.Join(root, r.path))
		exists := err == nil
		switch {
		case r.planned && exists:
			problems = append(problems, fmt.Sprintf("docs/architecture.md: %s is marked Planned but exists; describe it", r.path))
		case !r.planned && !exists:
			problems = append(problems, fmt.Sprintf("docs/architecture.md: the module map lists %s, which does not exist", r.path))
		}
	}
	pkgs, err := packageDirs(filepath.Join(root, "internal"), "internal")
	if err != nil {
		return nil, err
	}
	for _, d := range pkgs {
		if !listed[d] {
			problems = append(problems, fmt.Sprintf("docs/architecture.md: the module map does not list %s/", d))
		}
	}
	return problems, nil
}

// packageDirs lists the package directories under dir: a directory with Go
// files is one; a directory with none is a container, and its children are
// listed instead. testdata and hidden directories are skipped.
func packageDirs(dir, rel string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "testdata" || strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		sub, subRel := filepath.Join(dir, e.Name()), rel+"/"+e.Name()
		hasGo, err := hasGoFiles(sub)
		if err != nil {
			return nil, err
		}
		if hasGo {
			out = append(out, subRel)
			continue
		}
		children, err := packageDirs(sub, subRel)
		if err != nil {
			return nil, err
		}
		out = append(out, children...)
	}
	sort.Strings(out)
	return out, nil
}

func hasGoFiles(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true, nil
		}
	}
	return false, nil
}

var tableRow = regexp.MustCompile(`^\|([^|]+)\|([^|]+)\|`)
var emphasis = regexp.MustCompile("[*_`]")

// checkConsole finds Console-section subjects marked both Verified and
// Not supported. Subjects are compared without Markdown emphasis, case or
// surrounding space.
func checkConsole(doc string) []string {
	status := map[string]map[string]int{} // subject -> "verified"/"unsupported" -> line
	in := false
	for i, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "## ") {
			in = strings.TrimSpace(line) == "## Console"
			continue
		}
		if !in {
			continue
		}
		m := tableRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		subject := strings.ToLower(strings.TrimSpace(emphasis.ReplaceAllString(m[1], "")))
		st := strings.ToLower(emphasis.ReplaceAllString(m[2], ""))
		kind := ""
		switch {
		case strings.Contains(st, "not supported"):
			kind = "unsupported"
		case strings.Contains(st, "verified"):
			kind = "verified"
		default:
			continue
		}
		if status[subject] == nil {
			status[subject] = map[string]int{}
		}
		status[subject][kind] = i + 1
	}
	var problems []string
	for subject, kinds := range status {
		if v, ok := kinds["verified"]; ok {
			if u, ok := kinds["unsupported"]; ok {
				problems = append(problems, fmt.Sprintf("docs/compatibility.md: Console subject %q is Verified at line %d and Not supported at line %d", subject, v, u))
			}
		}
	}
	sort.Strings(problems)
	return problems
}
