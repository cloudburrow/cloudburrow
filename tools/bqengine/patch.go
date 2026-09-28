package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// filePatch is one file's changes in a unified diff.
type filePatch struct {
	// path is the file, relative to the tree the patch applies to.
	path string
	// create is a patch of /dev/null: the file must not exist.
	create bool
	hunks  []hunk
}

type hunk struct {
	// oldStart is the 1-based first line of old, 0 for a new file.
	oldStart int
	old, new []string
}

// parsePatch reads a unified diff, as diff -u and git diff write it. Any
// text before the first "--- " line is a description and is ignored, as
// are git's "diff --git" and "index" lines. A deletion, a rename or a
// binary change is refused: none is needed, and refusing keeps the applier
// small enough to trust.
func parsePatch(b []byte) ([]filePatch, error) {
	lines := strings.Split(string(b), "\n")
	var out []filePatch
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if strings.HasPrefix(l, "Binary files ") || strings.HasPrefix(l, "rename from ") || strings.HasPrefix(l, "deleted file mode") {
			return nil, fmt.Errorf("line %d: %q: only text changes to existing or new files are supported", i+1, l)
		}
		if !strings.HasPrefix(l, "--- ") {
			continue
		}
		if i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "+++ ") {
			return nil, fmt.Errorf("line %d: \"--- \" without \"+++ \"", i+1)
		}
		oldName, newName := diffName(l[4:]), diffName(lines[i+1][4:])
		if newName == "/dev/null" {
			return nil, fmt.Errorf("line %d: deleting %s is not supported", i+1, oldName)
		}
		fp := filePatch{path: strings.TrimPrefix(newName, "b/"), create: oldName == "/dev/null"}
		if !fp.create && strings.TrimPrefix(oldName, "a/") != fp.path {
			return nil, fmt.Errorf("line %d: renaming %s to %s is not supported", i+1, oldName, newName)
		}
		if fp.path == "" || filepath.IsAbs(fp.path) || strings.Contains("/"+fp.path+"/", "/../") {
			return nil, fmt.Errorf("line %d: unsafe path %q", i+1, newName)
		}
		i += 2
		for i < len(lines) && strings.HasPrefix(lines[i], "@@ ") {
			h, next, err := parseHunk(lines, i)
			if err != nil {
				return nil, err
			}
			fp.hunks = append(fp.hunks, h)
			i = next
		}
		if len(fp.hunks) == 0 {
			return nil, fmt.Errorf("%s: no hunks", fp.path)
		}
		out = append(out, fp)
		i-- // the loop's i++ lands on the line after the last hunk
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no file changes found")
	}
	return out, nil
}

// diffName is a ---/+++ line's file name, without the tab-separated
// timestamp diff -u appends.
func diffName(s string) string {
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// parseHunk reads the hunk whose header is lines[i] and returns the index of
// the line after it.
func parseHunk(lines []string, i int) (hunk, int, error) {
	var h hunk
	hdr := lines[i]
	end := strings.Index(hdr[3:], " @@")
	if end < 0 {
		return h, 0, fmt.Errorf("line %d: bad hunk header %q", i+1, hdr)
	}
	f := strings.Fields(hdr[3 : 3+end])
	if len(f) != 2 || !strings.HasPrefix(f[0], "-") || !strings.HasPrefix(f[1], "+") {
		return h, 0, fmt.Errorf("line %d: bad hunk header %q", i+1, hdr)
	}
	oldStart, oldCount, err := hunkRange(f[0][1:])
	if err != nil {
		return h, 0, fmt.Errorf("line %d: %w", i+1, err)
	}
	_, newCount, err := hunkRange(f[1][1:])
	if err != nil {
		return h, 0, fmt.Errorf("line %d: %w", i+1, err)
	}
	h.oldStart = oldStart
	i++
	for len(h.old) < oldCount || len(h.new) < newCount {
		if i >= len(lines) {
			return h, 0, fmt.Errorf("hunk at line %d is truncated", i)
		}
		l := lines[i]
		switch {
		case strings.HasPrefix(l, " "):
			h.old, h.new = append(h.old, l[1:]), append(h.new, l[1:])
		case l == "":
			// Some tools drop the space of an empty context line.
			h.old, h.new = append(h.old, ""), append(h.new, "")
		case strings.HasPrefix(l, "-"):
			h.old = append(h.old, l[1:])
		case strings.HasPrefix(l, "+"):
			h.new = append(h.new, l[1:])
		case strings.HasPrefix(l, `\`):
			return h, 0, fmt.Errorf("line %d: files without a final newline are not supported", i+1)
		default:
			return h, 0, fmt.Errorf("line %d: unexpected %q inside a hunk", i+1, l)
		}
		i++
	}
	if len(h.old) != oldCount || len(h.new) != newCount {
		return h, 0, fmt.Errorf("hunk ending at line %d has %d/%d lines, header says %d/%d", i, len(h.old), len(h.new), oldCount, newCount)
	}
	if i < len(lines) && strings.HasPrefix(lines[i], `\`) {
		return h, 0, fmt.Errorf("line %d: files without a final newline are not supported", i+1)
	}
	return h, i, nil
}

func hunkRange(s string) (start, count int, err error) {
	count = 1
	a, b, ok := strings.Cut(s, ",")
	if start, err = strconv.Atoi(a); err != nil {
		return 0, 0, fmt.Errorf("bad hunk range %q", s)
	}
	if ok {
		if count, err = strconv.Atoi(b); err != nil {
			return 0, 0, fmt.Errorf("bad hunk range %q", s)
		}
	}
	return start, count, nil
}

// applyPatch applies a unified diff to the tree at root, strictly: every
// hunk's old lines must be exactly at the line its header names (after the
// shift of the file's earlier hunks), with no fuzz and no search. A patch
// written against another version of a file fails rather than landing
// somewhere plausible.
func applyPatch(root string, patch []byte) error {
	files, err := parsePatch(patch)
	if err != nil {
		return err
	}
	for _, fp := range files {
		path := filepath.Join(root, filepath.FromSlash(fp.path))
		var lines []string
		mode := os.FileMode(0o644)
		if fp.create {
			if _, err := os.Lstat(path); err == nil {
				return fmt.Errorf("%s: patch creates it, but it exists", fp.path)
			}
		} else {
			st, err := os.Stat(path)
			if err != nil {
				return fmt.Errorf("%s: %w", fp.path, err)
			}
			mode = st.Mode().Perm()
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if len(b) > 0 && !bytes.HasSuffix(b, []byte("\n")) {
				return fmt.Errorf("%s: no final newline", fp.path)
			}
			lines = strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
			if len(b) == 0 {
				lines = nil
			}
		}
		shift := 0
		for n, h := range fp.hunks {
			at := h.oldStart - 1 + shift
			if len(h.old) == 0 {
				// A pure insertion's start names the line it follows.
				at = h.oldStart + shift
			}
			if at < 0 || at+len(h.old) > len(lines) {
				return fmt.Errorf("%s: hunk %d is outside the file", fp.path, n+1)
			}
			for k, want := range h.old {
				if lines[at+k] != want {
					return fmt.Errorf("%s: hunk %d does not apply: line %d is %q, the patch expects %q", fp.path, n+1, at+k+1, lines[at+k], want)
				}
			}
			rest := append([]string{}, lines[at+len(h.old):]...)
			lines = append(append(lines[:at], h.new...), rest...)
			shift += len(h.new) - len(h.old)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), mode); err != nil {
			return err
		}
	}
	return nil
}
