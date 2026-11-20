package run

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TestMain lets a repository's pre-receive hook run this test binary for
// the CODEOWNERS presubmit check, as it runs the cloudburrow executable.
func TestMain(m *testing.M) {
	RunSourceManagerPresubmitIfAsked()
	os.Exit(m.Run())
}

// googleExample is the example file of Google's "Manage pull request
// approvals with CODEOWNERS" page.
const googleExample = `# Un-sectioned rules; 1 approve required from any of owners of last matching line.
# Format: [BRANCH_GLOB] PATH_GLOB [OWNERS...]
# Make repo-admin the owner of all files of the current branch.
*   repo-admin@example.com      # No slash prefix; matches in sub-dirs too.
/**/* repo-admin@example.com      # Slash-prefix equivalent of prior line.
# Match all py files (including sub-dirs). Note repo-admin must be re-added.
*.py  repo-admin@example.com python-owner@example.com
# With repo-admin not included here, repo-admin no longer owns README.
/README.md readme-owner@example.com
/scratchpad.txt             # Can also override a path to have zero owners (path exclusion).
# Branch-specific syntax.
dev-* *             # Remove all owners reqs on dev branches.
dev-* /experimental/ exp-owner@example.com   # dev-specific owners.
# Make repo admin own all CODEOWNERS files, regardless of any prior rules.
CODEOWNERS repo-admin@example.com
# Section; 1 approval required *in addition* to owners outside this section.
[Security Team]
/secure-directory/ security-expert@example.com security-reviewer@example.com
/**/*secure\ me* security-expert@example.com security-reviewer@example.com
# Section w/ req approval count of 2 instead of 1.
[Doc Team][2]
/docs doc-expert@example.com doc-reviewer@example.com
*.md doc-expert@example.com doc-reviewer@example.com
`

func ownersString(os []coOwnership) string {
	var parts []string
	for _, o := range os {
		parts = append(parts, o.section+"/"+strings.Join(o.owners, ",")+"/"+string(rune('0'+o.count)))
	}
	return strings.Join(parts, " | ")
}

// TestCodeOwnersGoogleExample: Google's example parses with no problems
// and gives each path the owners the page's comments describe.
func TestCodeOwnersGoogleExample(t *testing.T) {
	f := parseCodeOwners("CODEOWNERS", googleExample)
	if len(f.notes) != 0 {
		t.Fatalf("notes = %v", f.notes)
	}
	files := []coFile{f}
	for _, c := range []struct{ branch, path, want string }{
		{"main", "a/b.go", "/repo-admin@example.com/1"},
		{"main", "lib/x.py", "/repo-admin@example.com,python-owner@example.com/1"},
		{"main", "README.md", "/readme-owner@example.com/1 | doc team/doc-expert@example.com,doc-reviewer@example.com/2"},
		{"main", "sub/README.md", "/repo-admin@example.com/1 | doc team/doc-expert@example.com,doc-reviewer@example.com/2"},
		{"main", "scratchpad.txt", ""},
		{"main", "docs/guide/a.txt", "/repo-admin@example.com/1 | doc team/doc-expert@example.com,doc-reviewer@example.com/2"},
		{"main", "src/CODEOWNERS", "/repo-admin@example.com/1"},
		{"main", "secure-directory/k.go", "/repo-admin@example.com/1 | security team/security-expert@example.com,security-reviewer@example.com/1"},
		{"main", "x/my secure me.txt", "/repo-admin@example.com/1 | security team/security-expert@example.com,security-reviewer@example.com/1"},
		{"dev-1", "a/b.go", ""},
		{"dev-1", "experimental/e.go", "/exp-owner@example.com/1"},
		{"main", "experimental/e.go", "/repo-admin@example.com/1"},
	} {
		if got := ownersString(ownersOf(files, c.branch, c.path)); got != c.want {
			t.Errorf("%s on %s: %q, want %q", c.path, c.branch, got, c.want)
		}
	}
}

// TestCodeOwnersGlobs: gitignore-style globs as Google's page lists them.
func TestCodeOwnersGlobs(t *testing.T) {
	for _, c := range []struct {
		glob, path string
		want       bool
	}{
		{"*.js", "a/b/c.js", true},
		{"build/", "x/build/y.go", true},
		{"build/", "build", false},
		{"docs/*", "docs/a.md", true},
		{"docs/*", "docs/a/b.md", false},
		{"docs/*", "x/docs/a.md", false},
		{"/*.js", "a.js", true},
		{"/*.js", "x/a.js", false},
		{"build-*/", "src/build-app/a", true},
		{"/**/*.py", "a/b/c.py", true},
		{"/**.py", "a.py", true},
		{"/**.py", "a/b.py", false},
		{"*.py", "x.py/a.txt", false},
		{"*.py/", "x.py/a.txt", true},
		{"/docs", "docs/a/b", true},
		{"docs/**", "docs/a/b", true},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"Readme.md", "README.md", false},
		{"a?.txt", "ab.txt", true},
		{"a?.txt", "a/.txt", false},
	} {
		p, err := coPath(c.glob)
		if err != nil {
			t.Fatalf("%s: %v", c.glob, err)
		}
		if got := p.matches(c.path); got != c.want {
			t.Errorf("%s matches %s = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
}

// TestCodeOwnersPrecedenceAndErrors: a deeper file overrides a shallower
// one per section; the dedicated file replaces all; lines that do not
// parse are reported and ignored, a negative count being 0.
func TestCodeOwnersPrecedenceAndErrors(t *testing.T) {
	root := parseCodeOwners("CODEOWNERS", "* root@example.com\n[Sec]\n* sec@example.com\n")
	sub := parseCodeOwners("lib/CODEOWNERS", "*.go lib@example.com\n")
	files := []coFile{root, sub}
	if got := ownersString(ownersOf(files, "main", "lib/a.go")); got != "/lib@example.com/1 | sec/sec@example.com/1" {
		t.Errorf("lib/a.go = %q", got)
	}
	if got := ownersString(ownersOf(files, "main", "lib/a.txt")); got != "/root@example.com/1 | sec/sec@example.com/1" {
		t.Errorf("lib/a.txt = %q", got)
	}
	if got := ownersString(ownersOf(files, "main", "a.go")); got != "/root@example.com/1 | sec/sec@example.com/1" {
		t.Errorf("a.go = %q (the lib file covers only lib)", got)
	}
	ded := parseCodeOwners(ssmCodeOwnersDedicated, "/lib/ ded@example.com\n")
	if got := ownersString(ownersOf([]coFile{ded}, "main", "lib/a.go")); got != "/ded@example.com/1" {
		t.Errorf("dedicated file: %q", got)
	}
	bad := parseCodeOwners("CODEOWNERS", strings.Join([]string{
		"* @team",                 // not an email
		"a b c x@example.com",     // three globs
		"x@example.com *",         // glob after the owners
		"[Docs] y@example.com",    // default owners on a section
		"[Neg][-1]",               // negative count: 0
		"/a[b] z@example.com",     // reserved [
		"!/vendor/",               // GitLab negation
		"[Bad][two]",              // count not a number
		"ok/ z@example.com",       // a good line
		"/tail\\",                 // lone backslash
		"   # comment",            // comment
		"*.md   docs@example.com", // a good line, in the Neg section
	}, "\n"))
	var lines []int
	for _, n := range bad.notes {
		if !n.Error {
			t.Errorf("a note that is not an error: %v", n)
		}
		lines = append(lines, n.Line)
	}
	if got := fmt.Sprint(lines); got != "[1 2 3 4 5 6 7 8 10]" {
		t.Errorf("error lines = %v", got)
	}
	if len(bad.rules) != 2 || bad.rules[1].count != 0 || bad.rules[1].section != "neg" {
		t.Errorf("rules = %+v", bad.rules)
	}
	notes := CheckSourceManagerCodeOwners("CODEOWNERS", "dev-* /x/ a@example.com\nmain /y/ b@example.com\n", "main")
	if len(notes) != 1 || notes[0].Line != 1 || notes[0].Error {
		t.Errorf("branch notes = %v", notes)
	}
}

// TestSourceManagerCodeOwnersMerge (#1928): with a rule requiring code
// owner approval, a merge needs an approval of the head commit from each
// changed file's owners in every section, read from the base branch's
// CODEOWNERS; a push of a CODEOWNERS file that does not parse is refused.
func TestSourceManagerCodeOwnersMerge(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if err := UseSourceManagerRoot(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	SetSourceManagerCaller(func(b string) (string, bool) { return b + "@example.com", true }, "owner@example.com")
	defer SetSourceManagerCaller(nil, "")
	as := func(who string) context.Context {
		return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+who))
	}
	ctx := context.Background()
	s := NewServer(&Knative{}, "t", time.Minute)
	var srv *httptest.Server
	m := s.SecureSourceManager(func() string { return srv.URL })
	srv = httptest.NewServer(SourceManagerHandlerOr(m, nil, func(g grpc.ServiceRegistrar) { m.Register(g) }, nil))
	defer srv.Close()
	parent := "projects/ssm-co/locations/us-central1"
	repo := parent + "/repositories/r"
	if _, err := m.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "i"}); err != nil {
		t.Fatal(err)
	}
	op, err := m.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "r",
		Repository: &ssmpb.Repository{Instance: parent + "/instances/i"}})
	r := opResult(t, op, err, &ssmpb.Repository{})
	work := t.TempDir()
	run := func(args ...string) (string, error) {
		out, err := exec.Command("git", append([]string{"-C", work, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput()
		return string(out), err
	}
	git := func(args ...string) {
		t.Helper()
		if out, err := run(args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, body string) {
		t.Helper()
		_ = os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755)
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("clone", "--quiet", r.GetUris().GetGitHttps(), ".")
	git("checkout", "--quiet", "-b", "main")
	write("CODEOWNERS", "* bob@example.com\n[Docs]\n*.md carol@example.com\n")
	write("lib/CODEOWNERS", "*.go dave@example.com\n")
	write("lib/a.go", "package a\n")
	write("README.md", "hi\n")
	git("add", ".")
	git("commit", "--quiet", "-m", "first")
	git("push", "--quiet", "origin", "main")

	op, err = m.CreateBranchRule(ctx, &ssmpb.CreateBranchRuleRequest{Parent: repo, BranchRuleId: "main", BranchRule: &ssmpb.BranchRule{
		IncludePattern: "main", RequireCodeOwnerApproval: true}})
	opResult(t, op, err, &ssmpb.BranchRule{})

	// The presubmit check: a CODEOWNERS that does not parse is refused on
	// main, and taken on another branch.
	git("checkout", "--quiet", "-b", "bad")
	write("CODEOWNERS", "* @bob\n")
	git("commit", "--quiet", "-am", "bad owners")
	if out, err := run("push", "--quiet", "origin", "bad:main"); err == nil || !strings.Contains(out, "CODEOWNERS:1") {
		t.Fatalf("a push of a bad CODEOWNERS to main: %v\n%s", err, out)
	}
	git("push", "--quiet", "origin", "bad")

	// alice changes lib/a.go and README.md.
	git("checkout", "--quiet", "main")
	git("checkout", "--quiet", "-b", "feature")
	write("lib/a.go", "package a // 2\n")
	write("README.md", "hello\n")
	write("CODEOWNERS", "* alice@example.com\n") // the head's CODEOWNERS does not count
	git("commit", "--quiet", "-am", "change")
	git("push", "--quiet", "origin", "feature")
	op, err = m.CreatePullRequest(as("alice"), &ssmpb.CreatePullRequestRequest{Parent: repo, PullRequest: &ssmpb.PullRequest{Title: "t",
		Base: &ssmpb.PullRequest_Branch{Ref: "main"}, Head: &ssmpb.PullRequest_Branch{Ref: "feature"}}})
	pr := opResult(t, op, err, &ssmpb.PullRequest{})
	approve := func(who string) {
		t.Helper()
		op, err := m.CreatePullRequestComment(as(who), &ssmpb.CreatePullRequestCommentRequest{Parent: pr.GetName(), PullRequestComment: &ssmpb.PullRequestComment{
			CommentDetail: &ssmpb.PullRequestComment_Review_{Review: &ssmpb.PullRequestComment_Review{ActionType: ssmpb.PullRequestComment_Review_APPROVED}}}})
		opResult(t, op, err, &ssmpb.PullRequestComment{})
	}
	merge := func() error {
		_, err := m.MergePullRequest(ctx, &ssmpb.MergePullRequestRequest{Name: pr.GetName()})
		return err
	}
	st, err := SourceManagerCodeOwners(ctx, pr.GetName())
	if err != nil || !st.Enforced || st.Rule != "main" || len(st.Files) != 2 || len(st.Requirements) != 3 || st.Satisfied() {
		t.Fatalf("SourceManagerCodeOwners = %+v, %v", st, err)
	}
	if err := merge(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "code owner") {
		t.Fatalf("merge with no owner approval: %v", err)
	}
	approve("bob")  // CODEOWNERS and README.md, unsectioned
	approve("dave") // lib/a.go: the deeper file wins
	if err := merge(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "[Docs]") {
		t.Fatalf("merge without the Docs section: %v", err)
	}
	approve("carol")
	// A new commit makes the approvals stale.
	write("lib/a.go", "package a // 3\n")
	git("commit", "--quiet", "-am", "again")
	git("push", "--quiet", "origin", "feature")
	if err := merge(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("merge with stale approvals: %v", err)
	}
	st, _ = SourceManagerCodeOwners(ctx, pr.GetName())
	for _, q := range st.Requirements {
		if q.Satisfied || len(q.ApprovedBy) != 0 {
			t.Errorf("a stale approval counted: %+v", q)
		}
	}
	approve("bob")
	approve("dave")
	approve("carol")
	if st, _ = SourceManagerCodeOwners(ctx, pr.GetName()); !st.Satisfied() {
		t.Fatalf("satisfied = %+v", st)
	}
	if err := merge(); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if _, err := m.DeleteRepository(ctx, &ssmpb.DeleteRepositoryRequest{Name: repo}); err != nil {
		t.Fatal(err)
	}
}
