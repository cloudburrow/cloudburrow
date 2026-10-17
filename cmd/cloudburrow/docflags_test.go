package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// The commands a user types from the docs must run as written (#1583): every
// `cloudburrow <command> --flag` in README.md and the user-facing docs names
// a command the CLI has and a flag that command takes. A renamed flag, or a
// command that never existed (docs/cloudsql.md named `cloudburrow snapshot`),
// fails here rather than in a new user's shell.

// docFlagsSkipped are the Markdown files whose commands are not instructions:
// the generated coverage pages, the decision records and the changelog, which
// keep history, and vendored trees.
func docFlagsSkipped(path string) bool {
	for _, p := range []string{"docs/coverage/", "docs/adr/", "third_party/", "internal/console/assets/"} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return path == "CHANGELOG.md"
}

// docInvocation is `cloudburrow <command> <args>`, where the CLI is the
// command a shell would run: at the start of a line or a code span, after a
// prompt, `./bin/`, `$(`, a pipe or a separator.
var docInvocation = regexp.MustCompile(`(?:^|[\s"'(|;&$])(?:\./bin/|\$ )?cloudburrow[ \t]+([a-z][a-z-]*)([^|;&#)\x60\n]*)`)

// docCommandStart is a line or code span that is itself a cloudburrow command
// line, so its first word must be a command the CLI has.
var docCommandStart = regexp.MustCompile(`^(?:\$ |eval "\$\()?(?:\./bin/)?cloudburrow[ \t]+([a-z][a-z-]*)\b`)

var docFlag = regexp.MustCompile(`^--?([a-zA-Z][a-zA-Z0-9-]*)(?:=.*)?$`)

var helpFlag = regexp.MustCompile(`(?m)(?:^|[\s\[])--?([a-z][a-z0-9-]*)`)

var inlineCode = regexp.MustCompile("`([^`\n]+)`")

// docCodeLines returns the code a Markdown file shows: each fenced block's
// lines, with a trailing backslash continuation joined, and each inline code
// span outside the fences, with the line number each starts on.
func docCodeLines(md string) (lines []string, at []int) {
	inFence := false
	pending, start := "", 0
	for i, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if !inFence {
			for _, m := range inlineCode.FindAllStringSubmatch(line, -1) {
				lines, at = append(lines, strings.TrimSpace(m[1])), append(at, i+1)
			}
			continue
		}
		if pending == "" {
			start = i + 1
		}
		if cont, ok := strings.CutSuffix(trimmed, `\`); ok {
			pending += cont + " "
			continue
		}
		lines, at = append(lines, pending+trimmed), append(at, start)
		pending = ""
	}
	return lines, at
}

// cliCommands are the commands the top-level usage lists.
func cliCommands() map[string]bool {
	cmds := map[string]bool{}
	_, list, _ := strings.Cut(usage, "Commands:\n")
	list, _, _ = strings.Cut(list, "\n\n")
	for _, line := range strings.Split(list, "\n") {
		if f := strings.Fields(line); len(f) > 0 && strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") {
			cmds[f[0]] = true
		}
	}
	return cmds
}

// cliFlags are the flags a command takes: those its help names, and the
// configuration flags every command reads through config.Load.
func cliFlags(t *testing.T, cmd string) map[string]bool {
	var help bytes.Buffer
	config.Usage(&help)
	switch cmd {
	case "terraform":
		// `terraform --help` is terraform's own; ours is --binary.
		help.WriteString(" --binary")
	default:
		var out bytes.Buffer
		_ = run([]string{cmd, "--help"}, &out, &out)
		help.Write(out.Bytes())
	}
	flags := map[string]bool{"h": true, "help": true}
	for _, m := range helpFlag.FindAllStringSubmatch(help.String(), -1) {
		flags[m[1]] = true
	}
	return flags
}

// docFlagProblems checks every file's cloudburrow invocations, keyed by path.
func docFlagProblems(t *testing.T, files map[string]string) []string {
	cmds := cliCommands()
	flagsOf := map[string]map[string]bool{}
	var problems []string
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		lines, at := docCodeLines(files[path])
		for i, line := range lines {
			if m := docCommandStart.FindStringSubmatch(line); m != nil && !cmds[m[1]] {
				problems = append(problems, path+":"+strconv.Itoa(at[i])+": `cloudburrow "+m[1]+"` is not a command (cloudburrow help lists them)")
			}
			for _, m := range docInvocation.FindAllStringSubmatch(line, -1) {
				cmd, rest := m[1], m[2]
				if !cmds[cmd] {
					continue // prose: "the cloudburrow namespace"
				}
				if cmd == "terraform" {
					rest, _, _ = strings.Cut(rest+" ", " -- ") // terraform's own arguments
				}
				if flagsOf[cmd] == nil {
					flagsOf[cmd] = cliFlags(t, cmd)
				}
				for _, word := range strings.Fields(rest) {
					fm := docFlag.FindStringSubmatch(word)
					if fm == nil || flagsOf[cmd][fm[1]] {
						continue
					}
					problems = append(problems, path+":"+strconv.Itoa(at[i])+": `cloudburrow "+cmd+"` has no flag "+word+" (cloudburrow "+cmd+" --help)")
				}
			}
		}
	}
	return problems
}

// TestDocumentedCommandsAndFlagsExist reads every tracked user-facing
// Markdown file.
func TestDocumentedCommandsAndFlagsExist(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", root, "ls-files", "*.md").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	files := map[string]string{}
	for _, f := range strings.Fields(string(out)) {
		if docFlagsSkipped(f) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			continue // deleted in the working tree
		}
		files[f] = string(b)
	}
	if len(files) < 10 {
		t.Fatalf("read %d Markdown files from %s; expected the docs", len(files), root)
	}
	for _, p := range docFlagProblems(t, files) {
		t.Error(p)
	}
}

// TestDocFlagCheckCatchesAPlantedWrongFlag proves the check above can fail:
// a renamed flag, an unknown command and a flag after terraform's `--`.
func TestDocFlagCheckCatchesAPlantedWrongFlag(t *testing.T) {
	doc := "```sh\n" +
		"cloudburrow up --detach \\\n  --port-bse 9100\n" + // a typo, on a continuation line
		"eval \"$(cloudburrow env --name beta --format json)\"\n" +
		"cloudburrow terraform --binary tofu -- plan -refresh=false\n" +
		"```\n" +
		"Run `cloudburrow snapshot` or `cloudburrow logs --folow`; the `cloudburrow` namespace is prose.\n"
	got := docFlagProblems(t, map[string]string{"planted.md": doc})
	want := []string{
		"planted.md:2: `cloudburrow up` has no flag --port-bse",
		"planted.md:7: `cloudburrow snapshot` is not a command",
		"planted.md:7: `cloudburrow logs` has no flag --folow",
	}
	if len(got) != len(want) {
		t.Fatalf("problems = %q, want %d", got, len(want))
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("problem %d = %q, want %q", i, got[i], want[i])
		}
	}
}
