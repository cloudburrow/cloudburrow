// Command doclinks fails when a tracked Markdown file links to a relative
// path that does not exist (#520). External links and in-page anchors are
// not checked; a link's #fragment is ignored.
//
//	go run ./tools/doclinks
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// link is an inline Markdown link or image: [text](target) or ![alt](target).
var link = regexp.MustCompile(`\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)

func main() {
	out, err := exec.Command("git", "ls-files", "*.md").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "doclinks:", err)
		os.Exit(2)
	}
	var broken []string
	for _, f := range strings.Fields(string(out)) {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		inFence := false
		for i, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				inFence = !inFence
				continue
			}
			if inFence {
				continue
			}
			for _, m := range link.FindAllStringSubmatch(line, -1) {
				target := m[1]
				if strings.Contains(target, "://") || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
					continue
				}
				path, _, _ := strings.Cut(target, "#")
				if path == "" {
					continue
				}
				if _, err := os.Stat(filepath.Join(filepath.Dir(f), path)); err != nil {
					broken = append(broken, fmt.Sprintf("%s:%d links to %s, which does not exist", f, i+1, target))
				}
			}
		}
	}
	if len(broken) > 0 {
		fmt.Fprintf(os.Stderr, "doclinks: %d broken link(s):\n  %s\n", len(broken), strings.Join(broken, "\n  "))
		os.Exit(1)
	}
}
