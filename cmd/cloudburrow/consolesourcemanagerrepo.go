package main

// A Secure Source Manager repository's pages (#1856), as Google's web
// interface has them (https://docs.cloud.google.com/secure-source-manager/docs/work-with-issues-pull-requests,
// https://docs.cloud.google.com/secure-source-manager/docs/configure-branch-protection,
// read 2026-11-19): Code (the tree at a ref, a file's content), Branches,
// Issues (New issue; an issue's page with its comments, Close and Reopen),
// Pull requests (New pull request from a branch into another; its
// Conversation and Files changed with their reviews and comments, Merge
// pull request, Close and Reopen; consolesourcemanagerreview.go) and
// Settings with its General, Branch rules (Add branch rule) and Webhooks
// tabs, then
// CloudBurrow's Triggers, Builds, Git repository links and Permissions.
// Everything is read and changed through the official client on the Run
// port; the branches, which no Secure Source Manager method lists, are read
// from the repository itself.

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	securesourcemanager "cloud.google.com/go/securesourcemanager/apiv1"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The path segments under a repository's page.
const (
	ssmTreeSegment        = "tree"
	ssmIssuesSegment      = "issues"
	ssmPullsSegment       = "pulls"
	ssmBranchRulesSegment = "branchrules"
)

// ssmMaxBlob is the largest file the Code tab shows.
const ssmMaxBlob = 1 << 20

func (p sourceManagerProvider) repoName(in *ssmpb.Instance, id string) string {
	return in.GetName()[:strings.Index(in.GetName(), "/instances/")] + "/repositories/" + id
}

// repoPath is a console path under a repository's page.
func repoPath(in *ssmpb.Instance, repo string, rest ...string) []string {
	return append([]string{lastSegment(in.GetName()), repo}, rest...)
}

func (p sourceManagerProvider) repositoryDetail(ctx context.Context, project string, in *ssmpb.Instance, id string) (console.Detail, error) {
	r, err := p.repository(ctx, in, id)
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	branch := ssmDefaultBranch(r)
	d := console.Detail{Summary: dropDashes([]console.Property{{Label: "Name", Value: r.GetName()},
		{Label: "Description", Value: r.GetDescription()}, {Label: "Clone URL", Value: r.GetUris().GetGitHttps(), Copy: "Copy URL"},
		{Label: "Default branch", Value: branch}, {Label: "Instance", Value: lastSegment(r.GetInstance())},
		{Label: "Created", Value: stampAge(r.GetCreateTime())}, {Label: "Updated", Value: stampAge(r.GetUpdateTime())}})}

	d.Sections = append(d.Sections, p.codeSection(ctx, in, r, branch, ""))
	d.Sections = append(d.Sections, p.branchesSection(ctx, in, r), p.tagsSection(ctx, in, r))
	d.Sections = append(d.Sections, p.issuesSection(ctx, in, r))
	d.Sections = append(d.Sections, p.pullsSection(ctx, in, r))
	d.Sections = append(d.Sections, p.triggersSection(ctx, r, branch), p.linksSection(project, in, r), p.buildsSection(ctx, project, r),
		console.Section{ID: "permissions", Kind: console.KindPermissions}, p.settingsSection(ctx, r))
	return d, nil
}

// tree is a ref's every entry, through FetchTree.
func (p sourceManagerProvider) tree(ctx context.Context, repo, ref string) ([]*ssmpb.TreeEntry, error) {
	var out []*ssmpb.TreeEntry
	err := p.withClient(ctx, func(c *securesourcemanager.Client) error {
		it := c.FetchTree(ctx, &ssmpb.FetchTreeRequest{Repository: repo, Ref: ref, Recursive: true})
		for {
			e, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				return err
			}
			out = append(out, e)
		}
	})
	return out, err
}

// codeSection lists the entries of dir at ref, folders first: each folder
// opens the next level, each file its content. An empty repository shows
// how to push to it.
func (p sourceManagerProvider) codeSection(ctx context.Context, in *ssmpb.Instance, r *ssmpb.Repository, ref, dir string) console.Section {
	sec := console.Section{ID: "code", Label: "Code", Listing: console.Listing{Columns: []string{"Type", "Size"},
		NameColumn: "Name", Noun: "files"}, Note: "At " + ref + ". Clone it with: git clone " + r.GetUris().GetGitHttps()}
	branches, _ := runadapter.SourceManagerBranches(ctx, r.GetName())
	if len(branches) == 0 {
		return console.Section{ID: "code", Label: "Code", Kind: console.KindText, Text: pushHelp(r)}
	}
	entries, err := p.tree(ctx, r.GetName(), ref)
	if err != nil {
		sec.Unavailable = consoleErr(err)
		return sec
	}
	repo := lastSegment(r.GetName())
	var rows []console.Resource
	for _, e := range entries {
		if path.Dir(e.GetPath()) != orDot(dir) {
			continue
		}
		kind, size := "File", fmt.Sprint(e.GetSize())
		switch e.GetType() {
		case ssmpb.TreeEntry_TREE:
			kind, size = "Folder", "-"
		case ssmpb.TreeEntry_COMMIT:
			kind, size = "Submodule", "-"
		}
		row := console.Resource{Name: path.Base(e.GetPath()), Fields: map[string]string{"Type": kind, "Size": size}}
		if kind != "Submodule" {
			row.Opens = repoPath(in, repo, append([]string{ssmTreeSegment, ref}, strings.Split(e.GetPath(), "/")...)...)
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		fi, fj := rows[i].Fields["Type"] == "Folder", rows[j].Fields["Type"] == "Folder"
		if fi != fj {
			return fi
		}
		return rows[i].Name < rows[j].Name
	})
	sec.Listing.Items, sec.Listing.Total = rows, len(rows)
	return sec
}

func orDot(dir string) string {
	if dir == "" {
		return "."
	}
	return dir
}

// codeDetail is the page of a folder or a file at a ref:
// [instance, repository, tree, REF, path...]; the ref alone is its root.
func (p sourceManagerProvider) codeDetail(ctx context.Context, in *ssmpb.Instance, cpath []string) (console.Detail, error) {
	r, err := p.repository(ctx, in, cpath[1])
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	ref, file := cpath[3], strings.Join(cpath[4:], "/")
	trail := []console.Crumb{{Label: cpath[0], Path: cpath[:1]}, {Label: lastSegment(r.GetName()), Path: cpath[:2]}, {Label: ref, Path: cpath[:4]}}
	for i := 4; i < len(cpath)-1; i++ {
		trail = append(trail, console.Crumb{Label: cpath[i], Path: cpath[:i+1]})
	}
	if len(cpath) > 4 {
		trail = append(trail, console.Crumb{Label: cpath[len(cpath)-1]})
	}
	d := console.Detail{Title: orRoot(file, lastSegment(r.GetName())), Trail: trail,
		Summary: []console.Property{{Label: "Ref", Value: ref}, {Label: "Path", Value: "/" + file}}}
	if file == "" {
		d.Sections = []console.Section{p.codeSection(ctx, in, r, ref, "")}
		return d, nil
	}
	entries, err := p.tree(ctx, r.GetName(), ref)
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	for _, e := range entries {
		if e.GetPath() != file {
			continue
		}
		if e.GetType() == ssmpb.TreeEntry_TREE {
			d.Sections = []console.Section{p.codeSection(ctx, in, r, ref, file)}
			return d, nil
		}
		d.Summary = append(d.Summary, console.Property{Label: "Size", Value: fmt.Sprint(e.GetSize()) + " bytes"},
			console.Property{Label: "Blob", Value: e.GetSha()})
		sec := console.Section{ID: "file", Label: path.Base(file), Kind: console.KindText}
		if e.GetSize() > ssmMaxBlob {
			sec.Text = fmt.Sprintf("This file is %d bytes, larger than the %d the page shows. Clone the repository to read it.", e.GetSize(), ssmMaxBlob)
		} else if text, err := p.blob(ctx, r.GetName(), e.GetSha()); err != nil {
			sec.Unavailable = consoleErr(err)
		} else if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
			sec.Text = "This is a binary file."
		} else {
			sec.Text = text
		}
		d.Sections = []console.Section{sec}
		return d, nil
	}
	return console.Detail{Unavailable: fmt.Sprintf("%s is not in %s at %s", file, lastSegment(r.GetName()), ref)}, nil
}

func orRoot(file, repo string) string {
	if file == "" {
		return repo
	}
	return path.Base(file)
}

// blob is a file's content, through FetchBlob.
func (p sourceManagerProvider) blob(ctx context.Context, repo, sha string) (string, error) {
	var out string
	err := p.withClient(ctx, func(c *securesourcemanager.Client) error {
		b, err := c.FetchBlob(ctx, &ssmpb.FetchBlobRequest{Repository: repo, Sha: sha})
		if err != nil {
			return err
		}
		raw, err := base64.StdEncoding.DecodeString(b.GetContent())
		if err != nil {
			return fmt.Errorf("the blob's content is not base64: %w", err)
		}
		out = string(raw)
		return nil
	})
	return out, err
}

// branchesSection lists the repository's branches; each opens its code.
func (p sourceManagerProvider) branchesSection(ctx context.Context, in *ssmpb.Instance, r *ssmpb.Repository) console.Section {
	sec := console.Section{ID: "branches", Label: "Branches", Listing: console.Listing{Columns: []string{"Commit", "Message", "Updated"},
		NameColumn: "Branch", Noun: "branches"}}
	bs, err := runadapter.SourceManagerBranches(ctx, r.GetName())
	if err != nil {
		sec.Unavailable = consoleErr(err)
	}
	def := ssmDefaultBranch(r)
	for _, b := range bs {
		st := ""
		if b.Name == def {
			st = "Default"
		}
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: b.Name, Status: st,
			Opens:  repoPath(in, lastSegment(r.GetName()), ssmTreeSegment, b.Name),
			Fields: map[string]string{"Commit": shortSHA(b.Commit), "Message": orDash(b.Subject), "Updated": stampAge(timestamppb.New(b.When))}})
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return sec
}

// issues are the repository's issues, through ListIssues.
func (p sourceManagerProvider) issues(ctx context.Context, repo string) ([]*ssmpb.Issue, error) {
	var out []*ssmpb.Issue
	err := p.withClient(ctx, func(c *securesourcemanager.Client) error {
		it := c.ListIssues(ctx, &ssmpb.ListIssuesRequest{Parent: repo})
		for {
			is, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				return err
			}
			out = append(out, is)
		}
	})
	return out, err
}

// ssmNumber is an issue's or pull request's number, the last segment of
// its name.
func ssmNumber(name string) int {
	n, _ := strconv.Atoi(lastSegment(name))
	return n
}

func newIssueAction() console.Action {
	return console.Action{ID: "new-issue", Label: "New issue", Submit: "Create Issue", Fields: []console.Field{
		{Name: "title", Label: "Title", Type: "text", Required: true},
		{Name: "body", Label: "Description", Type: "textarea"}}}
}

// issuesSection is the Issues tab: open issues first, newest first.
func (p sourceManagerProvider) issuesSection(ctx context.Context, in *ssmpb.Instance, r *ssmpb.Repository) console.Section {
	sec := console.Section{ID: "issues", Label: "Issues", Actions: []console.Action{newIssueAction()}, Listing: console.Listing{
		Columns: []string{"Title", "Created", "Updated"}, NameColumn: "Issue", Noun: "issues", AlwaysStatus: true}}
	is, err := p.issues(ctx, r.GetName())
	if err != nil {
		sec.Unavailable = consoleErr(err)
	}
	sort.SliceStable(is, func(i, j int) bool {
		if oi, oj := is[i].GetState() == ssmpb.Issue_OPEN, is[j].GetState() == ssmpb.Issue_OPEN; oi != oj {
			return oi
		}
		return ssmNumber(is[i].GetName()) > ssmNumber(is[j].GetName())
	})
	for _, x := range is {
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: lastSegment(x.GetName()), Status: stateWord(x.GetState().String()),
			Opens:  repoPath(in, lastSegment(r.GetName()), ssmIssuesSegment, lastSegment(x.GetName())),
			Fields: map[string]string{"Title": x.GetTitle(), "Created": stampAge(x.GetCreateTime()), "Updated": stampAge(x.GetUpdateTime())}})
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return sec
}

func (p sourceManagerProvider) issue(ctx context.Context, name string) (*ssmpb.Issue, error) {
	var out *ssmpb.Issue
	err := p.withClient(ctx, func(c *securesourcemanager.Client) error {
		var e error
		out, e = c.GetIssue(ctx, &ssmpb.GetIssueRequest{Name: name})
		return e
	})
	return out, err
}

// issueDetail is an issue's page: its description and its comments.
func (p sourceManagerProvider) issueDetail(ctx context.Context, in *ssmpb.Instance, ipath []string) (console.Detail, error) {
	name := p.repoName(in, ipath[1]) + "/issues/" + ipath[3]
	x, err := p.issue(ctx, name)
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	d := console.Detail{Title: x.GetTitle() + " #" + ipath[3], Trail: []console.Crumb{{Label: ipath[0], Path: ipath[:1]}, {Label: ipath[1], Path: ipath[:2]},
		{Label: "Issues", Path: ipath[:3]}, {Label: "#" + ipath[3]}},
		Summary: dropDashes([]console.Property{{Label: "State", Value: stateWord(x.GetState().String())},
			{Label: "Created", Value: stampAge(x.GetCreateTime())}, {Label: "Updated", Value: stampAge(x.GetUpdateTime())},
			{Label: "Closed", Value: closedAge(x.GetCloseTime())}})}
	body := x.GetBody()
	if strings.TrimSpace(body) == "" {
		body = "No description provided."
	}
	conv := console.Section{ID: "conversation", Label: "Conversation", Kind: console.KindText, Text: body}
	comments := console.Section{ID: "comments", Label: "Comments", Actions: []console.Action{addCommentAction()}, Listing: console.Listing{
		Columns: []string{"Comment", "Created", "Updated"}, NameColumn: "ID", Noun: "comments"}}
	err = p.withClient(ctx, func(c *securesourcemanager.Client) error {
		it := c.ListIssueComments(ctx, &ssmpb.ListIssueCommentsRequest{Parent: name})
		for {
			cm, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				return err
			}
			comments.Listing.Items = append(comments.Listing.Items, console.Resource{Name: lastSegment(cm.GetName()),
				Fields:  map[string]string{"Comment": cm.GetBody(), "Created": stampAge(cm.GetCreateTime()), "Updated": stampAge(cm.GetUpdateTime())},
				Actions: commentActions(cm.GetBody())})
		}
	})
	if err != nil {
		comments.Unavailable = consoleErr(err)
	}
	comments.Listing.Total = len(comments.Listing.Items)
	d.Sections = []console.Section{conv, comments}
	return d, nil
}

// closedAge is when an issue closed, or nothing while it is open.
func closedAge(t *timestamppb.Timestamp) string {
	if t == nil {
		return ""
	}
	return stampAge(t)
}

func addCommentAction() console.Action {
	return console.Action{ID: "add-comment", Label: "Comment", Submit: "Comment", Fields: []console.Field{
		{Name: "body", Label: "Comment", Type: "textarea", Required: true}}}
}

func commentActions(body string) []console.Action {
	return []console.Action{
		{ID: "edit-comment", Label: "Edit", Submit: "Update comment", Fields: []console.Field{
			{Name: "body", Label: "Comment", Type: "textarea", Required: true, Default: body}}},
		{ID: "delete-comment", Label: "Delete", Destructive: true, Confirm: "the comment"},
	}
}

// pulls are the repository's pull requests, through ListPullRequests.
func (p sourceManagerProvider) pulls(ctx context.Context, repo string) ([]*ssmpb.PullRequest, error) {
	var out []*ssmpb.PullRequest
	err := p.withClient(ctx, func(c *securesourcemanager.Client) error {
		it := c.ListPullRequests(ctx, &ssmpb.ListPullRequestsRequest{Parent: repo})
		for {
			pr, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				return err
			}
			out = append(out, pr)
		}
	})
	return out, err
}

// newPullAction is New pull request: the branch to merge into and the one
// to pull from, then its title and description.
func newPullAction(branches []string, def string) console.Action {
	head := def
	for _, b := range branches {
		if b != def {
			head = b
			break
		}
	}
	return console.Action{ID: "new-pull", Label: "New pull request", Submit: "Create pull request", Fields: []console.Field{
		{Name: "base", Label: "merge into", Type: "select", Required: true, Options: branches, Default: def},
		{Name: "head", Label: "pull from", Type: "select", Required: true, Options: branches, Default: head},
		{Name: "title", Label: "Title", Type: "text", Required: true},
		{Name: "body", Label: "Description", Type: "textarea"}}}
}

// refs are the repository's branches or tags, through FetchRefs, by short
// name.
func (p sourceManagerProvider) refs(ctx context.Context, repo string, typ ssmpb.Ref_RefType) ([]*ssmpb.Ref, error) {
	var out []*ssmpb.Ref
	err := p.withClient(ctx, func(c *securesourcemanager.Client) error {
		it := c.FetchRefs(ctx, &ssmpb.FetchRefsRequest{Repository: repo, Type: typ})
		for {
			r, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				return err
			}
			out = append(out, r)
		}
	})
	return out, err
}

// branchNames are the repository's branches, which New pull request
// chooses from.
func (p sourceManagerProvider) branchNames(ctx context.Context, repo string) []string {
	rs, _ := p.refs(ctx, repo, ssmpb.Ref_REF_TYPE_BRANCH)
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, strings.TrimPrefix(r.GetName(), "refs/heads/"))
	}
	return out
}

// tagsSection lists the repository's tags; each opens its code.
func (p sourceManagerProvider) tagsSection(ctx context.Context, in *ssmpb.Instance, r *ssmpb.Repository) console.Section {
	sec := console.Section{ID: "tags", Label: "Tags", Listing: console.Listing{Columns: []string{"Commit"}, NameColumn: "Tag", Noun: "tags"}}
	rs, err := p.refs(ctx, r.GetName(), ssmpb.Ref_REF_TYPE_TAG)
	if err != nil {
		sec.Unavailable = consoleErr(err)
	}
	for _, ref := range rs {
		tag := strings.TrimPrefix(ref.GetName(), "refs/tags/")
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: tag, Fields: map[string]string{"Commit": shortSHA(ref.GetTarget())},
			Opens: repoPath(in, lastSegment(r.GetName()), ssmTreeSegment, tag)})
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return sec
}

// pullsSection is the Pull requests tab. An empty repository has none, as
// Google's has no Pull requests tab.
func (p sourceManagerProvider) pullsSection(ctx context.Context, in *ssmpb.Instance, r *ssmpb.Repository) console.Section {
	sec := console.Section{ID: "pulls", Label: "Pull requests", Listing: console.Listing{
		Columns: []string{"Title", "Branches", "Created", "Updated"}, NameColumn: "Pull request", Noun: "pull requests", AlwaysStatus: true}}
	if names := p.branchNames(ctx, r.GetName()); len(names) > 0 {
		sec.Actions = []console.Action{newPullAction(names, ssmDefaultBranch(r))}
	} else {
		sec.Note = "The repository is empty: push a branch to open a pull request."
	}
	prs, err := p.pulls(ctx, r.GetName())
	if err != nil {
		sec.Unavailable = consoleErr(err)
	}
	sort.SliceStable(prs, func(i, j int) bool {
		if oi, oj := prs[i].GetState() == ssmpb.PullRequest_OPEN, prs[j].GetState() == ssmpb.PullRequest_OPEN; oi != oj {
			return oi
		}
		return ssmNumber(prs[i].GetName()) > ssmNumber(prs[j].GetName())
	})
	for _, pr := range prs {
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: lastSegment(pr.GetName()), Status: stateWord(pr.GetState().String()),
			Opens: repoPath(in, lastSegment(r.GetName()), ssmPullsSegment, lastSegment(pr.GetName())),
			Fields: map[string]string{"Title": pr.GetTitle(), "Branches": pr.GetHead().GetRef() + " → " + pr.GetBase().GetRef(),
				"Created": stampAge(pr.GetCreateTime()), "Updated": stampAge(pr.GetUpdateTime())}})
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return sec
}

func (p sourceManagerProvider) pull(ctx context.Context, name string) (*ssmpb.PullRequest, error) {
	var out *ssmpb.PullRequest
	err := p.withClient(ctx, func(c *securesourcemanager.Client) error {
		var e error
		out, e = c.GetPullRequest(ctx, &ssmpb.GetPullRequestRequest{Name: name})
		return e
	})
	return out, err
}

// pullDetail is a pull request's page: Conversation and Files changed.
func (p sourceManagerProvider) pullDetail(ctx context.Context, in *ssmpb.Instance, ppath []string) (console.Detail, error) {
	name := p.repoName(in, ppath[1]) + "/pullRequests/" + ppath[3]
	pr, err := p.pull(ctx, name)
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	d := console.Detail{Title: pr.GetTitle() + " #" + ppath[3], Trail: []console.Crumb{{Label: ppath[0], Path: ppath[:1]}, {Label: ppath[1], Path: ppath[:2]},
		{Label: "Pull requests", Path: ppath[:3]}, {Label: "#" + ppath[3]}},
		Summary: dropDashes([]console.Property{{Label: "State", Value: stateWord(pr.GetState().String())},
			{Label: "Merge into", Value: pr.GetBase().GetRef()}, {Label: "Pull from", Value: pr.GetHead().GetRef()},
			{Label: "Head commit", Value: shortSHA(pr.GetHead().GetSha())},
			{Label: "Created", Value: stampAge(pr.GetCreateTime())}, {Label: "Updated", Value: stampAge(pr.GetUpdateTime())}})}
	diffs, derr := p.fileDiffs(ctx, name)
	cms, cerr := p.prComments(ctx, name)
	conv := p.conversationSection(pr, cms, cerr, diffNames(diffs))
	// Comment and Review on Conversation; Add line comment and Review on
	// Files changed, as Google's has them.
	all := conv.Actions
	conv.Actions = append([]console.Action{all[0]}, all[2:]...)
	fileActs := all[1:]
	d.Sections = []console.Section{conv, filesSection(diffs, derr, cms, fileActs)}
	return d, nil
}

// branchRules are the repository's branch rules, through ListBranchRules.
func (p sourceManagerProvider) branchRules(ctx context.Context, repo string) ([]*ssmpb.BranchRule, error) {
	var out []*ssmpb.BranchRule
	err := p.withClient(ctx, func(c *securesourcemanager.Client) error {
		it := c.ListBranchRules(ctx, &ssmpb.ListBranchRulesRequest{Parent: repo})
		for {
			br, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				return err
			}
			out = append(out, br)
		}
	})
	return out, err
}

// branchRuleFields are Add branch rule's fields, as Google's
// configure-branch-protection page names them, prefilled from br.
func branchRuleFields(br *ssmpb.BranchRule) []console.Field {
	b := func(v bool) string { return strconv.FormatBool(v) }
	checks := make([]string, 0, len(br.GetRequiredStatusChecks()))
	for _, c := range br.GetRequiredStatusChecks() {
		checks = append(checks, c.GetContext())
	}
	sec := "Branch protection rule details"
	return []console.Field{
		{Name: "filter", Label: "Branch filter", Type: "text", Required: true, Default: br.GetIncludePattern(),
			Help: "A branch name, or .* for every branch. Full regular expressions are not supported."},
		{Name: "requirePullRequest", Label: "Require a pull request before merging", Type: "checkbox", Section: sec, Default: b(br.GetRequirePullRequest()),
			Help: "Blocks direct pushes to the branch. Not allowed with .*."},
		{Name: "minReviews", Label: "Required number of reviewers", Type: "number", Section: sec, Default: fmt.Sprint(br.GetMinimumReviewsCount()),
			ShowWhen: "requirePullRequest"},
		{Name: "minApprovals", Label: "Required number of approvers", Type: "number", Section: sec, Default: fmt.Sprint(br.GetMinimumApprovalsCount()),
			ShowWhen: "requirePullRequest"},
		{Name: "blockStale", Label: "Block merge on stale reviews and approvals", Type: "checkbox", Section: sec,
			Default: b(br.GetRequirePullRequest() && !br.GetAllowStaleReviews()), ShowWhen: "requirePullRequest"},
		{Name: "requireCommentsResolved", Label: "Require conversation resolution before merging", Type: "checkbox", Section: sec,
			Default: b(br.GetRequireCommentsResolved()), ShowWhen: "requirePullRequest"},
		{Name: "requireLinearHistory", Label: "Require linear history", Type: "checkbox", Section: sec, Default: b(br.GetRequireLinearHistory())},
		{Name: "requireStatusChecks", Label: "Require status checks", Type: "checkbox", Section: sec, Default: b(len(checks) > 0)},
		{Name: "statusChecks", Label: "Status checks", Type: "textarea", Section: sec, Default: strings.Join(checks, "\n"),
			ShowWhen: "requireStatusChecks", Help: "One per line: the names of triggers in " + runadapter.SourceManagerTriggersFile + "."},
		{Name: "disabled", Label: "Disabled", Type: "checkbox", Section: sec, Default: b(br.GetDisabled())},
	}
}

// branchRuleFrom reads the branch rule fields.
func branchRuleFrom(v map[string]string) (*ssmpb.BranchRule, error) {
	br := &ssmpb.BranchRule{IncludePattern: strings.TrimSpace(v["filter"]), RequirePullRequest: v["requirePullRequest"] == "true",
		RequireLinearHistory: v["requireLinearHistory"] == "true", Disabled: v["disabled"] == "true"}
	if br.RequirePullRequest {
		for _, f := range []struct {
			key string
			to  *int32
		}{{"minReviews", &br.MinimumReviewsCount}, {"minApprovals", &br.MinimumApprovalsCount}} {
			if s := strings.TrimSpace(v[f.key]); s != "" {
				n, err := strconv.ParseInt(s, 10, 32)
				if err != nil || n < 0 {
					return nil, fmt.Errorf("%s: want a number of 0 or more, got %q", f.key, s)
				}
				*f.to = int32(n)
			}
		}
		br.AllowStaleReviews = v["blockStale"] != "true"
		br.RequireCommentsResolved = v["requireCommentsResolved"] == "true"
	}
	if v["requireStatusChecks"] == "true" {
		for _, line := range strings.Split(v["statusChecks"], "\n") {
			if line = strings.TrimSpace(line); line != "" {
				br.RequiredStatusChecks = append(br.RequiredStatusChecks, &ssmpb.BranchRule_Check{Context: line})
			}
		}
	}
	return br, nil
}

// settingsSection is Settings: General (what the repository holds) and
// Branch rules, each a tab of its own as Google's are.
func (p sourceManagerProvider) settingsSection(ctx context.Context, r *ssmpb.Repository) console.Section {
	general := console.Section{ID: "general", Label: "General", Kind: console.KindProperties, Groups: []console.PropertyGroup{{
		Properties: dropDashes([]console.Property{{Label: "Repository ID", Value: lastSegment(r.GetName())},
			{Label: "Description", Value: orDash(r.GetDescription())}, {Label: "Default branch", Value: ssmDefaultBranch(r)},
			{Label: "Service account", Value: r.GetServiceAccount()}, {Label: "Etag", Value: r.GetEtag()}})}}}
	rules := console.Section{ID: "branchrules", Label: "Branch rules", Listing: console.Listing{
		Columns: []string{"Branch filter", "Pull request", "Approvals", "Linear history", "Status checks"}, NameColumn: "Branch rule ID",
		Noun: "branch rules", AlwaysStatus: true},
		Actions: []console.Action{addBranchRuleAction()}}
	brs, err := p.branchRules(ctx, r.GetName())
	if err != nil {
		rules.Unavailable = consoleErr(err)
	}
	yes := func(v bool) string {
		if v {
			return "Required"
		}
		return "-"
	}
	for _, br := range brs {
		st := "Enabled"
		if br.GetDisabled() {
			st = "Disabled"
		}
		rules.Listing.Items = append(rules.Listing.Items, console.Resource{Name: lastSegment(br.GetName()), Status: st,
			ActsOn: []string{lastSegment(r.GetInstance()), lastSegment(r.GetName()), ssmBranchRulesSegment, lastSegment(br.GetName())},
			Fields: map[string]string{"Branch filter": br.GetIncludePattern(), "Pull request": yes(br.GetRequirePullRequest()),
				"Approvals": fmt.Sprint(br.GetMinimumApprovalsCount()), "Linear history": yes(br.GetRequireLinearHistory()),
				"Status checks": fmt.Sprint(len(br.GetRequiredStatusChecks()))},
			Actions: branchRuleRowActions(br)})
	}
	rules.Listing.Total = len(rules.Listing.Items)
	webhooks := p.webhooksSection(ctx, r)
	return console.Section{ID: "settings", Label: "Settings", Tabs: []console.Section{general, rules, webhooks},
		Actions: []console.Action{ssmEditRepositoryAction(r)}}
}

func ssmEditRepositoryAction(r *ssmpb.Repository) console.Action {
	return console.Action{ID: "edit-description", Label: "Edit description", Submit: "Save", Fields: []console.Field{
		{Name: "description", Label: "Description", Type: "text", Default: r.GetDescription()}}}
}

func addBranchRuleAction() console.Action {
	f := append([]console.Field{{Name: "id", Label: "Branch rule ID", Type: "text", Required: true,
		Pattern: "^[a-z]([\\-a-z0-9]{0,61}[a-z0-9])?$",
		Help:    "Lowercase letters, numbers and hyphens, starting with a letter and ending with a letter or number."}},
		branchRuleFields(&ssmpb.BranchRule{IncludePattern: ""})...)
	return console.Action{ID: "add-branch-rule", Label: "Add branch rule", Submit: "Submit", Fields: f}
}

func branchRuleRowActions(br *ssmpb.BranchRule) []console.Action {
	return []console.Action{
		{ID: "edit-branch-rule", Label: "Edit", Submit: "Submit", Fields: branchRuleFields(br)},
		{ID: "delete-branch-rule", Label: "Delete", Destructive: true, Confirm: "the branch rule; its branches are no longer protected"},
	}
}

// triggersSection is the push triggers of the triggers file.
func (p sourceManagerProvider) triggersSection(ctx context.Context, r *ssmpb.Repository, branch string) console.Section {
	trig := console.Section{ID: "triggers", Label: "Triggers", Listing: console.Listing{
		Columns: []string{"Event", "Config file", "Branches", "Ignored", "Service account", "Link"}, NameColumn: "Trigger", Noun: "triggers",
		AlwaysStatus: true}, Note: "Read from " + runadapter.SourceManagerTriggersFile + " on " + branch + "; change them by pushing it."}
	ts, err := runadapter.SourceManagerTriggers(ctx, r.GetName())
	if err != nil {
		trig.Unavailable = consoleErr(err)
	}
	for _, t := range ts {
		st := "Enabled"
		if t.Disabled {
			st = "Disabled"
		}
		if t.ServiceAccount == "" {
			st = "No service account"
		}
		trig.Listing.Items = append(trig.Listing.Items, console.Resource{Name: t.Name, Status: st, Fields: map[string]string{
			"Event": t.EventType, "Config file": t.ConfigFilePath, "Branches": orDash(t.IncludedGitRefs),
			"Ignored": orDash(t.IgnoredGitRefs), "Service account": orDash(t.ServiceAccount), "Link": orDash(lastSegment(t.GitRepositoryLink))}})
	}
	trig.Listing.Total = len(trig.Listing.Items)
	return trig
}

// buildsSection is the builds the repository's pushes ran.
func (p sourceManagerProvider) buildsSection(ctx context.Context, project string, r *ssmpb.Repository) console.Section {
	builds := console.Section{ID: "builds", Label: "Builds", Listing: console.Listing{
		Columns: []string{"Commit", "Created"}, NameColumn: "Build", Noun: "builds", AlwaysStatus: true},
		Note: "The builds pushes to this repository ran, newest first; each opens on Cloud Build's Build history."}
	bs, err := p.pushBuilds(ctx, project, r)
	if err != nil {
		builds.Unavailable = consoleErr(err)
	}
	for _, b := range bs {
		builds.Listing.Items = append(builds.Listing.Items, console.Resource{Name: b.GetId(), Status: stateWord(b.GetStatus().String()),
			Link:   "/cloud-build/builds/" + url.PathEscape(b.GetId()) + "?project=" + url.QueryEscape(p.project(project)),
			Fields: map[string]string{"Commit": orDash(shortSHA(b.GetSource().GetGitSource().GetRevision())), "Created": stampAge(b.GetCreateTime())}})
	}
	builds.Listing.Total = len(builds.Listing.Items)
	return builds
}

// repoActions are the actions of a repository's page and the pages and
// rows inside it.
func (p sourceManagerProvider) repoActions(ctx context.Context, project string, in *ssmpb.Instance, apath []string) []console.Action {
	r, err := p.repository(ctx, in, apath[1])
	if err != nil {
		return nil
	}
	switch {
	case len(apath) == 2:
		acts := []console.Action{newIssueAction()}
		if names := p.branchNames(ctx, r.GetName()); len(names) > 0 {
			acts = append(acts, newPullAction(names, ssmDefaultBranch(r)))
		}
		acts = append(acts, addBranchRuleAction(), addHookAction(), ssmEditRepositoryAction(r),
			console.Action{ID: "delete-repo", Label: "Delete repository", Destructive: true, Leaves: true,
				Confirm: "the repository and every commit pushed to it"})
		return append(acts, p.linkActions(project, in, r)...)
	case len(apath) == 4 && apath[2] == ssmIssuesSegment:
		x, err := p.issue(ctx, r.GetName()+"/issues/"+apath[3])
		if err != nil {
			return nil
		}
		acts := []console.Action{addCommentAction(), {ID: "edit-issue", Label: "Edit", Submit: "Save", Fields: []console.Field{
			{Name: "title", Label: "Title", Type: "text", Required: true, Default: x.GetTitle()},
			{Name: "body", Label: "Description", Type: "textarea", Default: x.GetBody()}}}}
		if x.GetState() == ssmpb.Issue_OPEN {
			acts = append(acts, console.Action{ID: "close-issue", Label: "Close issue"})
		} else {
			acts = append(acts, console.Action{ID: "reopen-issue", Label: "Reopen issue"})
		}
		return append(acts, console.Action{ID: "delete-issue", Label: "Delete issue", Destructive: true, Leaves: true, Confirm: "the issue and its comments"})
	case len(apath) == 5 && apath[2] == ssmIssuesSegment:
		var body string
		_ = p.withClient(ctx, func(c *securesourcemanager.Client) error {
			cm, err := c.GetIssueComment(ctx, &ssmpb.GetIssueCommentRequest{Name: r.GetName() + "/issues/" + apath[3] + "/issueComments/" + apath[4]})
			body = cm.GetBody()
			return err
		})
		return commentActions(body)
	case len(apath) == 4 && apath[2] == ssmPullsSegment:
		pr, err := p.pull(ctx, r.GetName()+"/pullRequests/"+apath[3])
		if err != nil {
			return nil
		}
		diffs, _ := p.fileDiffs(ctx, pr.GetName())
		acts := append(prCommentActions(pr, diffNames(diffs)), console.Action{ID: "edit-pull", Label: "Edit", Submit: "Save", Fields: []console.Field{
			{Name: "title", Label: "Title", Type: "text", Required: true, Default: pr.GetTitle()},
			{Name: "body", Label: "Description", Type: "textarea", Default: pr.GetBody()}}})
		switch pr.GetState() {
		case ssmpb.PullRequest_OPEN:
			acts = append(acts, console.Action{ID: "merge-pull", Label: "Merge pull request"},
				console.Action{ID: "close-pull", Label: "Close pull request"})
		case ssmpb.PullRequest_CLOSED:
			acts = append(acts, console.Action{ID: "reopen-pull", Label: "Reopen pull request"})
		}
		return acts
	case len(apath) == 5 && apath[2] == ssmPullsSegment:
		var cm *ssmpb.PullRequestComment
		if p.withClient(ctx, func(c *securesourcemanager.Client) error {
			var e error
			cm, e = c.GetPullRequestComment(ctx, &ssmpb.GetPullRequestCommentRequest{Name: r.GetName() + "/pullRequests/" + apath[3] + "/pullRequestComments/" + apath[4]})
			return e
		}) != nil {
			return nil
		}
		return prCommentRowActions(cm)
	case len(apath) == 4 && apath[2] == ssmHooksSegment:
		return p.hookActions(ctx, r.GetName()+"/hooks/"+apath[3])
	case len(apath) == 4 && apath[2] == ssmBranchRulesSegment:
		var br *ssmpb.BranchRule
		if p.withClient(ctx, func(c *securesourcemanager.Client) error {
			var e error
			br, e = c.GetBranchRule(ctx, &ssmpb.GetBranchRuleRequest{Name: r.GetName() + "/branchRules/" + apath[3]})
			return e
		}) != nil {
			return nil
		}
		return branchRuleRowActions(br)
	}
	return nil
}

// repoAct performs an action of a repository's page or a page inside it.
func (p sourceManagerProvider) repoAct(ctx context.Context, in *ssmpb.Instance, apath []string, action string, v map[string]string) error {
	repo := p.repoName(in, apath[1])
	return p.withClient(ctx, func(c *securesourcemanager.Client) error {
		switch {
		case len(apath) == 2 && action == "edit-description":
			op, err := c.UpdateRepository(ctx, &ssmpb.UpdateRepositoryRequest{
				Repository: &ssmpb.Repository{Name: repo, Description: strings.TrimSpace(v["description"])},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		case len(apath) == 2 && action == "delete-repo":
			op, err := c.DeleteRepository(ctx, &ssmpb.DeleteRepositoryRequest{Name: repo})
			if err != nil {
				return err
			}
			return op.Wait(ctx)
		case len(apath) == 2 && action == "new-issue":
			op, err := c.CreateIssue(ctx, &ssmpb.CreateIssueRequest{Parent: repo, Issue: &ssmpb.Issue{Title: strings.TrimSpace(v["title"]), Body: v["body"]}})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		case len(apath) == 2 && action == "new-pull":
			op, err := c.CreatePullRequest(ctx, &ssmpb.CreatePullRequestRequest{Parent: repo, PullRequest: &ssmpb.PullRequest{
				Title: strings.TrimSpace(v["title"]), Body: v["body"],
				Base: &ssmpb.PullRequest_Branch{Ref: v["base"]}, Head: &ssmpb.PullRequest_Branch{Ref: v["head"]}}})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		case len(apath) == 2 && action == "add-hook":
			h, err := hookFrom(v)
			if err != nil {
				return err
			}
			op, err := c.CreateHook(ctx, &ssmpb.CreateHookRequest{Parent: repo, HookId: strings.TrimSpace(v["id"]), Hook: h})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		case len(apath) == 4 && apath[2] == ssmHooksSegment:
			return hookAct(ctx, c, repo+"/hooks/"+apath[3], action, v)
		case len(apath) == 5 && apath[2] == ssmPullsSegment:
			pr := repo + "/pullRequests/" + apath[3]
			return prCommentRowAct(ctx, c, pr, pr+"/pullRequestComments/"+apath[4], action, v)
		case len(apath) == 2 && action == "add-branch-rule":
			br, err := branchRuleFrom(v)
			if err != nil {
				return err
			}
			op, err := c.CreateBranchRule(ctx, &ssmpb.CreateBranchRuleRequest{Parent: repo, BranchRuleId: strings.TrimSpace(v["id"]), BranchRule: br})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		case len(apath) == 4 && apath[2] == ssmBranchRulesSegment && action == "edit-branch-rule":
			br, err := branchRuleFrom(v)
			if err != nil {
				return err
			}
			br.Name = repo + "/branchRules/" + apath[3]
			op, err := c.UpdateBranchRule(ctx, &ssmpb.UpdateBranchRuleRequest{BranchRule: br, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{
				"include_pattern", "disabled", "require_pull_request", "minimum_reviews_count", "minimum_approvals_count",
				"require_comments_resolved", "allow_stale_reviews", "require_linear_history", "required_status_checks"}}})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		case len(apath) == 4 && apath[2] == ssmBranchRulesSegment && action == "delete-branch-rule":
			op, err := c.DeleteBranchRule(ctx, &ssmpb.DeleteBranchRuleRequest{Name: repo + "/branchRules/" + apath[3]})
			if err != nil {
				return err
			}
			return op.Wait(ctx)
		case len(apath) == 4 && apath[2] == ssmIssuesSegment:
			return p.issueAct(ctx, c, repo+"/issues/"+apath[3], action, v)
		case len(apath) == 5 && apath[2] == ssmIssuesSegment:
			name := repo + "/issues/" + apath[3] + "/issueComments/" + apath[4]
			switch action {
			case "edit-comment":
				op, err := c.UpdateIssueComment(ctx, &ssmpb.UpdateIssueCommentRequest{IssueComment: &ssmpb.IssueComment{Name: name, Body: v["body"]},
					UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"body"}}})
				if err != nil {
					return err
				}
				_, err = op.Wait(ctx)
				return err
			case "delete-comment":
				op, err := c.DeleteIssueComment(ctx, &ssmpb.DeleteIssueCommentRequest{Name: name})
				if err != nil {
					return err
				}
				return op.Wait(ctx)
			}
		case len(apath) == 4 && apath[2] == ssmPullsSegment:
			return p.pullAct(ctx, c, repo+"/pullRequests/"+apath[3], action, v)
		}
		return fmt.Errorf("unknown action %q", action)
	})
}

func (p sourceManagerProvider) issueAct(ctx context.Context, c *securesourcemanager.Client, name, action string, v map[string]string) error {
	var err error
	switch action {
	case "add-comment":
		var op *securesourcemanager.CreateIssueCommentOperation
		if op, err = c.CreateIssueComment(ctx, &ssmpb.CreateIssueCommentRequest{Parent: name, IssueComment: &ssmpb.IssueComment{Body: v["body"]}}); err == nil {
			_, err = op.Wait(ctx)
		}
	case "edit-issue":
		var op *securesourcemanager.UpdateIssueOperation
		if op, err = c.UpdateIssue(ctx, &ssmpb.UpdateIssueRequest{Issue: &ssmpb.Issue{Name: name, Title: strings.TrimSpace(v["title"]), Body: v["body"]},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"title", "body"}}}); err == nil {
			_, err = op.Wait(ctx)
		}
	case "close-issue":
		var op *securesourcemanager.CloseIssueOperation
		if op, err = c.CloseIssue(ctx, &ssmpb.CloseIssueRequest{Name: name}); err == nil {
			_, err = op.Wait(ctx)
		}
	case "reopen-issue":
		var op *securesourcemanager.OpenIssueOperation
		if op, err = c.OpenIssue(ctx, &ssmpb.OpenIssueRequest{Name: name}); err == nil {
			_, err = op.Wait(ctx)
		}
	case "delete-issue":
		var op *securesourcemanager.DeleteIssueOperation
		if op, err = c.DeleteIssue(ctx, &ssmpb.DeleteIssueRequest{Name: name}); err == nil {
			err = op.Wait(ctx)
		}
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	return err
}

func (p sourceManagerProvider) pullAct(ctx context.Context, c *securesourcemanager.Client, name, action string, v map[string]string) error {
	if ok, err := prCommentAct(ctx, c, name, action, v); ok {
		return err
	}
	var err error
	switch action {
	case "edit-pull":
		var op *securesourcemanager.UpdatePullRequestOperation
		if op, err = c.UpdatePullRequest(ctx, &ssmpb.UpdatePullRequestRequest{PullRequest: &ssmpb.PullRequest{Name: name,
			Title: strings.TrimSpace(v["title"]), Body: v["body"]}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"title", "body"}}}); err == nil {
			_, err = op.Wait(ctx)
		}
	case "merge-pull":
		var op *securesourcemanager.MergePullRequestOperation
		if op, err = c.MergePullRequest(ctx, &ssmpb.MergePullRequestRequest{Name: name}); err == nil {
			_, err = op.Wait(ctx)
		}
	case "close-pull":
		var op *securesourcemanager.ClosePullRequestOperation
		if op, err = c.ClosePullRequest(ctx, &ssmpb.ClosePullRequestRequest{Name: name}); err == nil {
			_, err = op.Wait(ctx)
		}
	case "reopen-pull":
		var op *securesourcemanager.OpenPullRequestOperation
		if op, err = c.OpenPullRequest(ctx, &ssmpb.OpenPullRequestRequest{Name: name}); err == nil {
			_, err = op.Wait(ctx)
		}
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	return err
}
