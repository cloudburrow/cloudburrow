// Command doclinks fails when a tracked Markdown file links to a relative
// path that does not exist (#520), or to a #fragment that no heading or
// explicit anchor of the Markdown file it names defines (#1583). External
// links are not checked, nor fragments into files other than Markdown, nor
// GitHub's #L<line> anchors.
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
	"unicode"
)

// link is an inline Markdown link or image: [text](target) or ![alt](target).
var link = regexp.MustCompile(`\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)

// heading is an ATX heading; anchor an explicit HTML anchor.
var (
	heading    = regexp.MustCompile(`^#{1,6}\s+(.*?)\s*#*\s*$`)
	anchor     = regexp.MustCompile(`<a\s+(?:name|id)="([^"]+)"`)
	headLink   = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	lineAnchor = regexp.MustCompile(`^L\d+(-L?\d+)?$`)
)

func main() {
	out, err := exec.Command("git", "ls-files", "*.md").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "doclinks:", err)
		os.Exit(2)
	}
	anchors := map[string]map[string]bool{}
	anchorsOf := func(f string) map[string]bool {
		if a, ok := anchors[f]; ok {
			return a
		}
		b, _ := os.ReadFile(f)
		anchors[f] = fileAnchors(string(b))
		return anchors[f]
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
				if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
					continue
				}
				path, frag, hasFrag := strings.Cut(target, "#")
				dest := f
				if path != "" {
					dest = filepath.Join(filepath.Dir(f), path)
					if _, err := os.Stat(dest); err != nil {
						broken = append(broken, fmt.Sprintf("%s:%d links to %s, which does not exist", f, i+1, target))
						continue
					}
				}
				if !hasFrag || frag == "" || !strings.HasSuffix(dest, ".md") || lineAnchor.MatchString(frag) {
					continue
				}
				if !anchorsOf(dest)[frag] {
					broken = append(broken, fmt.Sprintf("%s:%d links to %s, but %s has no heading or anchor #%s", f, i+1, target, dest, frag))
				}
			}
		}
	}
	if len(broken) > 0 {
		fmt.Fprintf(os.Stderr, "doclinks: %d broken link(s):\n  %s\n", len(broken), strings.Join(broken, "\n  "))
		os.Exit(1)
	}
}

// fileAnchors are the fragments a Markdown file defines: GitHub's slug of each
// heading outside a fence, with -1, -2... for repeats, and each explicit
// <a name> or <a id>.
func fileAnchors(md string) map[string]bool {
	a := map[string]bool{}
	seen := map[string]int{}
	inFence := false
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if m := heading.FindStringSubmatch(line); m != nil {
			s := slug(headLink.ReplaceAllString(m[1], "$1"))
			if n := seen[s]; n > 0 {
				a[fmt.Sprintf("%s-%d", s, n)] = true
			} else {
				a[s] = true
			}
			seen[s]++
		}
		for _, m := range anchor.FindAllStringSubmatch(line, -1) {
			a[m[1]] = true
		}
	}
	return a
}

// slug is GitHub's heading anchor: lower case, every character but letters,
// digits, spaces, hyphens and underscores dropped, and each space a hyphen.
func slug(h string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(h)) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}
