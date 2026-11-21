package run

import (
	"bufio"
	"bytes"
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// A repository's commit history and blame (#2071), which the web
// interface's code view shows and no Secure Source Manager method reads:
// they are read from the repository with git, for the console, as its
// branches are (SourceManagerBranches).

// SourceManagerCommit is one commit of a history.
type SourceManagerCommit struct {
	SHA, Author, Email, Subject string
	When                        time.Time
}

// SourceManagerCommits are the commits reachable from ref (a branch, a
// tag or a SHA), newest first, that touch path ("" for every path), at
// most max (0: 100).
func SourceManagerCommits(ctx context.Context, repo, ref, path string, max int) ([]SourceManagerCommit, error) {
	_, dir, err := ssmRepoDir(repo)
	if err != nil {
		return nil, err
	}
	commit, err := resolveSSMCommit(ctx, dir, ref)
	if err != nil {
		return nil, err
	}
	if max <= 0 {
		max = 100
	}
	args := []string{"log", "-z", "--format=%H%x00%an%x00%ae%x00%at%x00%s", "-n", strconv.Itoa(max), commit}
	if path != "" {
		args = append(args, "--", path)
	}
	out, err := ssmGit(ctx, dir, args...)
	if err != nil {
		return nil, apierror.Internal(err, "git log")
	}
	f := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	var cs []SourceManagerCommit
	for i := 0; i+4 < len(f); i += 5 {
		at, _ := strconv.ParseInt(f[i+3], 10, 64)
		cs = append(cs, SourceManagerCommit{SHA: strings.TrimSpace(f[i]), Author: f[i+1], Email: f[i+2], When: time.Unix(at, 0), Subject: f[i+4]})
	}
	return cs, nil
}

// SourceManagerBlameLine is one line of a file's blame: the commit that
// last changed it.
type SourceManagerBlameLine struct {
	Line   int
	Commit SourceManagerCommit
}

// SourceManagerBlame is the blame of a file at ref, line by line.
func SourceManagerBlame(ctx context.Context, repo, ref, path string) ([]SourceManagerBlameLine, error) {
	_, dir, err := ssmRepoDir(repo)
	if err != nil {
		return nil, err
	}
	commit, err := resolveSSMCommit(ctx, dir, ref)
	if err != nil {
		return nil, err
	}
	if path == "" || strings.HasPrefix(path, "-") {
		return nil, apierror.InvalidArgument("invalid path %q", path)
	}
	out, err := ssmGit(ctx, dir, "blame", "--porcelain", commit, "--", path)
	if err != nil {
		return nil, apierror.NotFound("%s is not a file at %s", path, ref)
	}
	return parseBlamePorcelain(out), nil
}

// parseBlamePorcelain reads `git blame --porcelain`: per line, a header
// "SHA ORIG FINAL [COUNT]", the commit's details the first time it is
// named, then the line itself after a tab.
func parseBlamePorcelain(out []byte) []SourceManagerBlameLine {
	commits := map[string]*SourceManagerCommit{}
	var lines []SourceManagerBlameLine
	var cur *SourceManagerCommit
	final := 0
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "\t") {
			if cur != nil {
				lines = append(lines, SourceManagerBlameLine{Line: final, Commit: *cur})
			}
			continue
		}
		f := strings.Fields(l)
		if len(f) >= 3 && len(f[0]) >= 40 && ssmSHARE.MatchString(f[0]) {
			c, ok := commits[f[0]]
			if !ok {
				c = &SourceManagerCommit{SHA: f[0]}
				commits[f[0]] = c
			}
			cur = c
			final, _ = strconv.Atoi(f[2])
			continue
		}
		if cur == nil {
			continue
		}
		k, v, _ := strings.Cut(l, " ")
		switch k {
		case "author":
			cur.Author = v
		case "author-mail":
			cur.Email = strings.Trim(v, "<>")
		case "author-time":
			at, _ := strconv.ParseInt(v, 10, 64)
			cur.When = time.Unix(at, 0)
		case "summary":
			cur.Subject = v
		}
	}
	return lines
}
