package run

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
)

// CODEOWNERS files (#1928), as Google's "Manage pull request approvals
// with CODEOWNERS" (Preview) describes them
// (https://docs.cloud.google.com/secure-source-manager/docs/codeowners,
// read 2026-11-19):
//
//   - Location and precedence: a CODEOWNERS file in any directory but
//     .securesourcemanager covers that directory and its subdirectories,
//     its paths relative to it; a deeper (more local) file overrides the
//     matching rules of a shallower one, the root file having the lowest
//     precedence. A .securesourcemanager/CODEOWNERS file, when there is
//     one, is the only file read, its paths relative to the root. Only
//     the base branch's files count for a pull request.
//   - A rule line is `[BRANCH_GLOB] PATH_GLOB [OWNERS...]`: fields with
//     an @ are owners (individual email addresses; Google Groups are not
//     supported); one other field is the path glob, two are the branch
//     glob then the path glob; a rule with no owners is a path exclusion.
//     A line whose branch glob does not match the branch is ignored.
//   - Path globs are gitignore's: a pattern with no slash but a trailing
//     one matches in any directory, any other is relative to the file; a
//     trailing slash matches directories (and so everything in them), and
//     a directory is matched only so or when the pattern's last part has
//     no wildcard; `**` matches across slashes, but mixed with other
//     characters is a plain `*`; `*` and `?` stop at a slash; matching is
//     case sensitive; `[ ] space @ * ? \ { } !` are escaped with a
//     backslash to be literal.
//   - `[Section]` and `[Section][N]` start a section: an independent set
//     of approvals, N (default 1; 0 for optional owners) from the owners
//     of each file's last matching rule in it. Rules before any section
//     are one section needing one approval. Sections of one name (case
//     insensitive) merge across files. Within a section the last matching
//     line wins; different sections all apply.
//   - With a branch rule's require_code_owner_approval, a pull request
//     merges only when every changed file's owners have approved in every
//     section, an approval counting as the minimum reviewer and approver
//     counts do (the account's latest decisive review, of the head commit
//     unless the rule allows stale reviews). A presubmit check refuses a
//     push (or a merge) to such a branch that brings a CODEOWNERS file
//     with lines that do not parse; elsewhere those lines are ignored, a
//     negative approval count being read as 0.

// SourceManagerCodeOwnersFile is the name of a CODEOWNERS file.
const SourceManagerCodeOwnersFile = "CODEOWNERS"

// ssmCodeOwnersDedicated is the file that, when present, is the only one.
const ssmCodeOwnersDedicated = ".securesourcemanager/CODEOWNERS"

// SourceManagerCodeOwnersNote is a note on one line of a CODEOWNERS file:
// an error for a line that does not parse (and is ignored), or
// information, such as a rule for other branches.
type SourceManagerCodeOwnersNote struct {
	Line    int
	Error   bool
	Message string
}

// coPattern is a compiled branch or path glob.
type coPattern struct {
	re       *regexp.Regexp
	anchored bool // relative to the file's directory (it has a slash)
	dirOnly  bool // a trailing slash: directories only
	dirMatch bool // may match a directory, and so all it holds
}

type coRule struct {
	line    int
	branch  *regexp.Regexp // nil: every branch
	path    coPattern
	owners  []string
	section string // lower-cased section name; "" before any section
	label   string // the section's name as written
	count   int    // approvals the section asks for
}

// coFile is a parsed CODEOWNERS file.
type coFile struct {
	path  string // in the repository
	dir   string // the directory it covers, "" for the root
	rules []coRule
	notes []SourceManagerCodeOwnersNote
}

var (
	coSectionRE = regexp.MustCompile(`^\[([^\[\]]*)\](?:\[([^\[\]]*)\])?(.*)$`)
	coEmailRE   = regexp.MustCompile(`^[^@]+@[^@\s]+\.[^@\s]+$`)
)

// coFields splits a line at unescaped whitespace, keeping escapes, and
// drops a trailing comment (a field starting with an unescaped #).
func coFields(line string) ([]string, error) {
	var out []string
	var cur strings.Builder
	in := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case ch == '\\':
			if i+1 >= len(line) {
				return nil, fmt.Errorf("the line ends with a lone backslash")
			}
			cur.WriteByte(ch)
			cur.WriteByte(line[i+1])
			i++
			in = true
		case ch == ' ' || ch == '\t':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		case ch == '#' && !in:
			return out, nil
		default:
			cur.WriteByte(ch)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out, nil
}

// coUnescape drops a field's escaping backslashes.
func coUnescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// coHasUnescaped reports whether s holds one of chars unescaped.
func coHasUnescaped(s, chars string) (byte, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if strings.IndexByte(chars, s[i]) >= 0 {
			return s[i], true
		}
	}
	return 0, false
}

// coSegment turns one slash-free glob segment into a regular expression.
func coSegment(seg string) (re string, wild bool) {
	var b strings.Builder
	for i := 0; i < len(seg); i++ {
		switch ch := seg[i]; ch {
		case '\\':
			i++
			b.WriteString(regexp.QuoteMeta(seg[i : i+1]))
		case '*':
			for i+1 < len(seg) && seg[i+1] == '*' { // ** among other characters is *
				i++
			}
			b.WriteString(`[^/]*`)
			wild = true
		case '?':
			b.WriteString(`[^/]`)
			wild = true
		default:
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	return b.String(), wild
}

// coSplit splits a glob at its unescaped slashes.
func coSplit(g string) []string {
	var out []string
	start := 0
	for i := 0; i < len(g); i++ {
		if g[i] == '\\' {
			i++
			continue
		}
		if g[i] == '/' {
			out = append(out, g[start:i])
			start = i + 1
		}
	}
	return append(out, g[start:])
}

// coGlob compiles a glob's segments: a whole ** segment spans any number
// of directories.
func coGlob(segs []string) (*regexp.Regexp, bool) {
	var b strings.Builder
	wild := false
	for i, seg := range segs {
		last := i == len(segs)-1
		if seg == "**" {
			if last {
				b.WriteString(`.*`)
			} else {
				b.WriteString(`(?:.*/)?`)
			}
			wild = wild || last
			continue
		}
		re, w := coSegment(seg)
		b.WriteString(re)
		if last {
			wild = w
		} else {
			b.WriteString("/")
		}
	}
	return regexp.MustCompile(`^` + b.String() + `$`), wild
}

// coPath compiles a path glob.
func coPath(g string) (coPattern, error) {
	if c, bad := coHasUnescaped(g, "[]{}!"); bad {
		return coPattern{}, fmt.Errorf("%q in the path %q is reserved: escape it with a backslash", c, coUnescape(g))
	}
	p := coPattern{}
	for strings.HasSuffix(g, "/") && !strings.HasSuffix(g, `\/`) {
		g, p.dirOnly = strings.TrimSuffix(g, "/"), true
	}
	segs := coSplit(g)
	p.anchored = len(segs) > 1
	if p.anchored && segs[0] == "" {
		segs = segs[1:]
	}
	if len(segs) == 0 || (len(segs) == 1 && segs[0] == "") {
		return coPattern{}, fmt.Errorf("the path is empty")
	}
	for _, s := range segs {
		if s == "" {
			return coPattern{}, fmt.Errorf("the path %q has an empty part", coUnescape(g))
		}
	}
	re, wild := coGlob(segs)
	p.re, p.dirMatch = re, p.dirOnly || !wild
	return p, nil
}

// coBranch compiles a branch glob.
func coBranch(g string) (*regexp.Regexp, error) {
	if c, bad := coHasUnescaped(g, "[]{}!"); bad {
		return nil, fmt.Errorf("%q in the branch %q is reserved: escape it with a backslash", c, coUnescape(g))
	}
	re, _ := coGlob(coSplit(g))
	return re, nil
}

// matches reports whether a path relative to the file's directory
// matches: the file itself, or a directory holding it.
func (p coPattern) matches(rel string) bool {
	parts := strings.Split(rel, "/")
	if !p.anchored {
		if !p.dirOnly && p.re.MatchString(parts[len(parts)-1]) {
			return true
		}
		if p.dirMatch {
			for _, d := range parts[:len(parts)-1] {
				if p.re.MatchString(d) {
					return true
				}
			}
		}
		return false
	}
	if !p.dirOnly && p.re.MatchString(rel) {
		return true
	}
	if p.dirMatch {
		for i := 1; i < len(parts); i++ {
			if p.re.MatchString(strings.Join(parts[:i], "/")) {
				return true
			}
		}
	}
	return false
}

// parseCodeOwners reads a CODEOWNERS file at file in the repository.
func parseCodeOwners(file, text string) coFile {
	f := coFile{path: file}
	if file != ssmCodeOwnersDedicated {
		if d := path.Dir(file); d != "." {
			f.dir = d
		}
	}
	section, label, count := "", "", 1
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		fail := func(format string, a ...any) {
			f.notes = append(f.notes, SourceManagerCodeOwnersNote{Line: n, Error: true, Message: fmt.Sprintf(format, a...) + "; the line is ignored"})
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			m := coSectionRE.FindStringSubmatch(line)
			if m == nil {
				fail("not a section header: want [Section name] or [Section name][N]")
				continue
			}
			name := strings.TrimSpace(m[1])
			rest, err := coFields(m[3])
			switch {
			case name == "":
				fail("the section has no name")
				continue
			case err != nil || len(rest) > 0:
				fail("default owners on a section header are not supported: put the owners on rule lines")
				continue
			}
			c := 1
			if m[2] != "" || strings.Contains(line, "][") {
				v, err := strconv.Atoi(strings.TrimSpace(m[2]))
				if err != nil {
					fail("the approval count %q is not a number", m[2])
					continue
				}
				if v < 0 {
					f.notes = append(f.notes, SourceManagerCodeOwnersNote{Line: n, Error: true, Message: fmt.Sprintf("the approval count %d is negative; it is treated as 0", v)})
					v = 0
				}
				c = v
			}
			section, label, count = strings.ToLower(name), name, c
			continue
		}
		fields, err := coFields(line)
		if err != nil {
			fail("%v", err)
			continue
		}
		var globs, owners []string
		bad := ""
		for _, fd := range fields {
			if _, at := coHasUnescaped(fd, "@"); at {
				o := coUnescape(fd)
				if !coEmailRE.MatchString(o) {
					bad = fmt.Sprintf("%q is not an email address: owners are individual users' email addresses", o)
					break
				}
				owners = append(owners, strings.ToLower(o))
				continue
			}
			if len(owners) > 0 {
				bad = fmt.Sprintf("%q follows the owners: the branch and path come first", coUnescape(fd))
				break
			}
			globs = append(globs, fd)
		}
		if bad == "" && (len(globs) == 0 || len(globs) > 2) {
			bad = fmt.Sprintf("want [BRANCH_GLOB] PATH_GLOB [OWNERS...]: %d fields before the owners", len(globs))
		}
		if bad != "" {
			fail("%s", bad)
			continue
		}
		r := coRule{line: n, owners: owners, section: section, label: label, count: count}
		if len(globs) == 2 {
			if r.branch, err = coBranch(globs[0]); err != nil {
				fail("%v", err)
				continue
			}
			globs = globs[1:]
		}
		if r.path, err = coPath(globs[0]); err != nil {
			fail("%v", err)
			continue
		}
		f.rules = append(f.rules, r)
	}
	return f
}

// CheckSourceManagerCodeOwners checks a CODEOWNERS file's text as the web
// interface shows it on a branch: errors for the lines that do not parse,
// and information for the rules of other branches (branch "" for a ref
// that is not a branch).
func CheckSourceManagerCodeOwners(file, text, branch string) []SourceManagerCodeOwnersNote {
	f := parseCodeOwners(file, text)
	notes := f.notes
	if branch != "" {
		for _, r := range f.rules {
			if r.branch != nil && !r.branch.MatchString(branch) {
				notes = append(notes, SourceManagerCodeOwnersNote{Line: r.line, Message: "this rule does not match the branch " + branch + ", so it does not apply here"})
			}
		}
	}
	sort.SliceStable(notes, func(i, j int) bool { return notes[i].Line < notes[j].Line })
	return notes
}

// isCodeOwnersPath reports whether a repository path is a CODEOWNERS file
// that is read.
func isCodeOwnersPath(p string) bool {
	if path.Base(p) != SourceManagerCodeOwnersFile {
		return false
	}
	return p == ssmCodeOwnersDedicated || !strings.HasPrefix(p, ".securesourcemanager/")
}

// coGit runs git: in dir, or (dir "") in the environment's repository, as
// a hook does.
func coGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if dir != "" {
		return ssmGit(ctx, dir, args...)
	}
	return exec.CommandContext(ctx, "git", args...).Output()
}

// loadCodeOwners reads the CODEOWNERS files of a commit: the dedicated
// one alone when it exists, else every other, shallowest first.
func loadCodeOwners(ctx context.Context, dir, commit string) ([]coFile, error) {
	out, err := coGit(ctx, dir, "ls-tree", "-r", "-z", "--name-only", commit)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p == ssmCodeOwnersDedicated {
			paths = []string{p}
			break
		}
		if isCodeOwnersPath(p) {
			paths = append(paths, p)
		}
	}
	sort.SliceStable(paths, func(i, j int) bool {
		di, dj := strings.Count(paths[i], "/"), strings.Count(paths[j], "/")
		if di != dj {
			return di < dj
		}
		return paths[i] < paths[j]
	})
	var files []coFile
	for _, p := range paths {
		text, err := coGit(ctx, dir, "cat-file", "blob", commit+":"+p)
		if err != nil {
			return nil, err
		}
		files = append(files, parseCodeOwners(p, string(text)))
	}
	return files, nil
}

// coOwnership is what one section asks of one file.
type coOwnership struct {
	section, label string
	count          int
	owners         []string
}

// ownersOf is each section's owners of a file on a branch: the last
// matching rule of the deepest file that has one wins in each section.
func ownersOf(files []coFile, branch, file string) []coOwnership {
	won := map[string]coRule{}
	var order []string
	for _, f := range files {
		rel := file
		if f.dir != "" {
			if !strings.HasPrefix(file, f.dir+"/") {
				continue
			}
			rel = strings.TrimPrefix(file, f.dir+"/")
		}
		for _, r := range f.rules {
			if (r.branch != nil && !r.branch.MatchString(branch)) || !r.path.matches(rel) {
				continue
			}
			if _, ok := won[r.section]; !ok {
				order = append(order, r.section)
			}
			won[r.section] = r
		}
	}
	var out []coOwnership
	for _, s := range order {
		r := won[s]
		if len(r.owners) == 0 {
			continue // a path exclusion
		}
		out = append(out, coOwnership{section: s, label: r.label, count: r.count, owners: r.owners})
	}
	return out
}

// SourceManagerCodeOwnerRequirement is one set of code owners a pull
// request needs: in a section (empty for the rules before any section),
// Approvals of Owners for Files.
type SourceManagerCodeOwnerRequirement struct {
	Section    string
	Owners     []string
	Approvals  int
	Files      []string
	ApprovedBy []string
	Satisfied  bool
}

// SourceManagerCodeOwnerStatus is a pull request's code owners: the
// CODEOWNERS files of its base branch, what they require of its changed
// files, and whether its branch rule enforces them.
type SourceManagerCodeOwnerStatus struct {
	Enforced     bool   // the base branch's rule requires code owner approval
	Rule         string // that rule's ID
	Files        []string
	Problems     int // lines of those files that do not parse
	Requirements []SourceManagerCodeOwnerRequirement
}

// Satisfied reports whether every requirement is met.
func (s SourceManagerCodeOwnerStatus) Satisfied() bool {
	for _, r := range s.Requirements {
		if !r.Satisfied {
			return false
		}
	}
	return true
}

// codeOwnerRequirements groups the changed files' owners into
// requirements, each met by approvals from the approving accounts.
func codeOwnerRequirements(files []coFile, branch string, changed []string, approved map[string]bool) []SourceManagerCodeOwnerRequirement {
	byKey := map[string]*SourceManagerCodeOwnerRequirement{}
	var keys []string
	sort.Strings(changed)
	for _, file := range changed {
		for _, o := range ownersOf(files, branch, file) {
			key := o.section + "\x00" + strconv.Itoa(o.count) + "\x00" + strings.Join(o.owners, " ")
			r, ok := byKey[key]
			if !ok {
				r = &SourceManagerCodeOwnerRequirement{Section: o.label, Owners: dedupe(o.owners), Approvals: o.count}
				for _, w := range r.Owners {
					if approved[w] {
						r.ApprovedBy = append(r.ApprovedBy, w)
					}
				}
				r.Satisfied = len(r.ApprovedBy) >= r.Approvals
				byKey[key] = r
				keys = append(keys, key)
			}
			r.Files = append(r.Files, file)
		}
	}
	out := make([]SourceManagerCodeOwnerRequirement, 0, len(keys))
	for _, k := range keys {
		out = append(out, *byKey[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Section) < strings.ToLower(out[j].Section) })
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// codeOwnerStatus is a pull request's code owners at head, its base branch
// being at base: approved holds the emails of the accounts whose approval
// counts.
func codeOwnerStatus(ctx context.Context, dir, branch, base string, changed map[string]bool, approved map[string]bool) (SourceManagerCodeOwnerStatus, error) {
	var st SourceManagerCodeOwnerStatus
	files, err := loadCodeOwners(ctx, dir, base)
	if err != nil {
		return st, err
	}
	for _, f := range files {
		st.Files = append(st.Files, f.path)
		for _, n := range f.notes {
			if n.Error {
				st.Problems++
			}
		}
	}
	st.Requirements = codeOwnerRequirements(files, branch, sortedKeys(changed), approved)
	return st, nil
}

// approvedEmails are the emails of the accounts whose approval of a pull
// request counts at head: their latest decisive review approves it, of
// head unless stale reviews are allowed.
func approvedEmailsLocked(pull, head string, allowStale bool) map[string]bool {
	latest := map[string]*ssmPRReviewRef{}
	for _, k := range sortedByNumber(ssmState.prComments, pull, "pullRequestComments") {
		r := ssmState.prComments[k].GetReview()
		if r == nil || r.GetActionType() == ssmpb.PullRequestComment_Review_COMMENT {
			continue
		}
		latest[ssmState.authors[k]] = &ssmPRReviewRef{approved: r.GetActionType() == ssmpb.PullRequestComment_Review_APPROVED, sha: r.GetEffectiveCommitSha()}
	}
	out := map[string]bool{}
	for a, r := range latest {
		if r.approved && (allowStale || r.sha == head) {
			out[strings.ToLower(ssmCallerEmail(a))] = true
		}
	}
	return out
}

type ssmPRReviewRef struct {
	approved bool
	sha      string
}

// codeOwnersPresubmit checks the CODEOWNERS files a change from old to
// sha brings (every one when old is empty): their lines must parse.
func codeOwnersPresubmit(ctx context.Context, dir, old, sha string) []string {
	var paths []string
	if old == "" || strings.Trim(old, "0") == "" {
		out, err := coGit(ctx, dir, "ls-tree", "-r", "-z", "--name-only", sha)
		if err != nil {
			return []string{"the pushed commit cannot be read: " + err.Error()}
		}
		paths = strings.Split(string(out), "\x00")
	} else {
		out, err := coGit(ctx, dir, "diff", "--name-only", "-z", "--no-renames", "--diff-filter=d", old, sha)
		if err != nil {
			return []string{"the pushed change cannot be read: " + err.Error()}
		}
		paths = strings.Split(string(out), "\x00")
	}
	var problems []string
	for _, p := range paths {
		if !isCodeOwnersPath(p) {
			continue
		}
		text, err := coGit(ctx, dir, "cat-file", "blob", sha+":"+p)
		if err != nil {
			continue
		}
		for _, n := range parseCodeOwners(p, string(text)).notes {
			if n.Error {
				problems = append(problems, fmt.Sprintf("%s:%d: %s", p, n.Line, n.Message))
			}
		}
	}
	return problems
}

// ssmCodeOwnersBranchesFile lists, for the pre-receive hook, the branch
// filters of the enabled rules that require code owner approval.
const ssmCodeOwnersBranchesFile = "cloudburrow-codeowners-branches"

// SourceManagerPresubmitArg is the argument the pre-receive hook runs the
// CloudBurrow executable with to check the CODEOWNERS files of a push.
const SourceManagerPresubmitArg = "__ssm-codeowners-presubmit"

// ssmPresubmitEnv names the executable the hook runs for the check.
const ssmPresubmitEnv = "CLOUDBURROW_SSM_PRESUBMIT"

// RunSourceManagerPresubmitIfAsked runs the CODEOWNERS presubmit check and
// exits when the process was started for it (by a repository's
// pre-receive hook), and returns otherwise. main calls it first.
func RunSourceManagerPresubmitIfAsked() {
	if len(os.Args) < 3 || os.Args[1] != SourceManagerPresubmitArg {
		return
	}
	os.Exit(sourceManagerPresubmit(os.Args[2], os.Stdin))
}

// sourceManagerPresubmit reads the pre-receive hook's "old new ref" lines
// and refuses (1) an update of a branch whose rule requires code owner
// approval that brings a CODEOWNERS file with lines that do not parse.
func sourceManagerPresubmit(gitDir string, in *os.File) int {
	raw, err := os.ReadFile(path.Join(gitDir, ssmCodeOwnersBranchesFile))
	if err != nil {
		return 0
	}
	// Each line is a branch filter, a tab, and whether its rule requires
	// code owner approval: a rule naming the branch wins over .*.
	filters := map[string]bool{}
	for _, l := range strings.Split(string(raw), "\n") {
		if name, req, ok := strings.Cut(l, "\t"); ok {
			filters[name] = req == "true"
		}
	}
	status := 0
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 3 || !strings.HasPrefix(f[2], "refs/heads/") || strings.Trim(f[1], "0") == "" {
			continue
		}
		branch := strings.TrimPrefix(f[2], "refs/heads/")
		req, named := filters[branch]
		if !named {
			req = filters[".*"]
		}
		if !req {
			continue
		}
		for _, p := range codeOwnersPresubmit(context.Background(), "", f[0], f[1]) {
			fmt.Fprintf(os.Stderr, "error: branch %s requires code owner review, and CODEOWNERS has a syntax error: %s\n", branch, p)
			status = 1
		}
	}
	return status
}

// SourceManagerCodeOwners is a pull request's code owners, for its page in
// the web interface: the CODEOWNERS files of its base branch, the owners
// its changed files need and who of them has approved its head commit, and
// whether its base branch's rule requires them.
func SourceManagerCodeOwners(ctx context.Context, pull string) (SourceManagerCodeOwnerStatus, error) {
	ssmState.mu.Lock()
	pr, dir, err := ssmPullLocked(pull)
	if err != nil {
		ssmState.mu.Unlock()
		return SourceManagerCodeOwnerStatus{}, err
	}
	pr = withTips(ctx, dir, pr)
	branch, head := pr.GetBase().GetRef(), pr.GetHead().GetSha()
	rule := effectiveBranchRuleLocked(ssmRepoOf(pull), branch)
	approved := approvedEmailsLocked(pull, head, rule.GetAllowStaleReviews())
	ssmState.mu.Unlock()
	base := pr.GetBase().GetSha()
	if pr.GetState() == ssmpb.PullRequest_MERGED {
		// After the merge, the files the base branch had before it.
		if out, err := ssmGit(ctx, dir, "rev-parse", "--verify", "--quiet", base+"^1"); err == nil && base != head {
			base = strings.TrimSpace(string(out))
		}
	}
	st, err := codeOwnerStatus(ctx, dir, branch, base, prChangedFiles(ctx, dir, pr), approved)
	if err != nil {
		return st, err
	}
	st.Enforced, st.Rule = rule.GetRequireCodeOwnerApproval(), lastSegment(rule.GetName())
	return st, nil
}
