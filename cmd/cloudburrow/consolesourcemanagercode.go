package main

// Secure Source Manager's web interface, round 3 (#2071), in the order
// Google's pages give it
// (https://docs.cloud.google.com/secure-source-manager/docs/create-repository,
// https://docs.cloud.google.com/secure-source-manager/docs/work-with-issues-pull-requests,
// https://docs.cloud.google.com/secure-source-manager/docs/set-up-webhooks,
// read 2026-11-20):
//   - the code view's branch and tag selector, its History (the commits
//     that touched the folder or file, each opening the tree at it), a
//     file's Blame and its syntax highlighted by its language;
//   - the Issues and Pull requests tabs' Sort menu, and the Issues tab's
//     Labels, Milestones and Assignee menus and Assigned to me;
//   - Files changed drawn line by line, with the Whitespace, Split
//     view/Unified view and diff options menus, the plus icon beside each
//     line, Add single comment and Start review (comments kept Pending for
//     the viewer until Review sends them with it);
//   - an issue's Labels, Milestones and Assignees, and the repository's
//     Labels and Milestones tabs that define them;
//   - a webhook's Enable service account authentication.
// History, blame, labels, milestones, assignees, pending comments and the
// webhook's authentication are read from the repository and CloudBurrow's
// state (internal/adapter/run/securesourcemanagerhistory.go,
// securesourcemanagerextras.go): the pinned API holds none of them.

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The path segments of a repository's labels and milestones.
const (
	ssmLabelsSegment     = "labels"
	ssmMilestonesSegment = "milestones"
)

// ssmLanguages are the languages the code view highlights, by file
// extension or name; console.js's codeTokens knows each.
var ssmLanguages = map[string]string{
	".go": "go", ".js": "javascript", ".mjs": "javascript", ".cjs": "javascript", ".jsx": "javascript", ".ts": "typescript", ".tsx": "typescript",
	".py": "python", ".java": "java", ".kt": "kotlin", ".kts": "kotlin", ".c": "c", ".h": "c", ".cc": "cpp", ".cpp": "cpp", ".hpp": "cpp",
	".cs": "csharp", ".rs": "rust", ".rb": "ruby", ".php": "php", ".swift": "swift", ".scala": "scala", ".sh": "shell", ".bash": "shell",
	".zsh": "shell", ".yaml": "yaml", ".yml": "yaml", ".json": "json", ".toml": "toml", ".sql": "sql", ".css": "css", ".scss": "css",
	".html": "html", ".htm": "html", ".xml": "html", ".md": "markdown", ".proto": "proto", ".tf": "hcl", ".hcl": "hcl", ".dart": "dart",
	"Dockerfile": "dockerfile", "Makefile": "makefile", ".gitignore": "shell", "go.mod": "go",
}

// ssmLanguage is the language a file's name says it is in, or "text".
func ssmLanguage(file string) string {
	base := path.Base(file)
	if l, ok := ssmLanguages[base]; ok {
		return l
	}
	if l, ok := ssmLanguages[path.Ext(base)]; ok {
		return l
	}
	return "text"
}

// refPicker is the code view's branch and tag selector at ref, each ref
// opening dir at it.
func (p sourceManagerProvider) refPicker(ctx context.Context, in *ssmpb.Instance, r *ssmpb.Repository, ref, dir string) *console.RefPicker {
	repo := lastSegment(r.GetName())
	at := func(name string) []string {
		segs := []string{ssmTreeSegment, name}
		if dir != "" {
			segs = append(segs, strings.Split(dir, "/")...)
		}
		return repoPath(in, repo, segs...)
	}
	rp := &console.RefPicker{Current: ref}
	for _, b := range p.branchNames(ctx, r.GetName()) {
		rp.Branches = append(rp.Branches, console.RefOption{Name: b, Path: at(b)})
	}
	tags, _ := p.refs(ctx, r.GetName(), ssmpb.Ref_REF_TYPE_TAG)
	for _, t := range tags {
		name := strings.TrimPrefix(t.GetName(), "refs/tags/")
		rp.Tags = append(rp.Tags, console.RefOption{Name: name, Path: at(name)})
	}
	return rp
}

// historySection is History: the commits at ref that touched file ("" for
// the whole tree), newest first, each opening the tree at it.
func historySection(ctx context.Context, in *ssmpb.Instance, r *ssmpb.Repository, ref, file string) console.Section {
	sec := console.Section{ID: "history", Label: "History", Listing: console.Listing{Columns: []string{"Message", "Author", "Committed"},
		NameColumn: "Commit", Noun: "commits"}}
	cs, err := runadapter.SourceManagerCommits(ctx, r.GetName(), ref, file, 100)
	if err != nil {
		sec.Unavailable = consoleErr(err)
		return sec
	}
	for _, c := range cs {
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: shortSHA(c.SHA),
			Opens:  repoPath(in, lastSegment(r.GetName()), ssmTreeSegment, c.SHA),
			Fields: map[string]string{"Message": c.Subject, "Author": c.Author + " <" + c.Email + ">", "Committed": stampAge(timestamppb.New(c.When))}})
	}
	sec.Listing.Total = len(sec.Listing.Items)
	if len(cs) == 100 {
		sec.Note = "The 100 newest commits. Clone the repository for the rest: git log"
	}
	return sec
}

// blameRuns are a file's blame as runs of lines from one commit.
func blameRuns(in *ssmpb.Instance, repo string, lines []runadapter.SourceManagerBlameLine) []console.BlameLine {
	var out []console.BlameLine
	for _, l := range lines {
		if n := len(out); n > 0 && out[n-1].Commit == shortSHA(l.Commit.SHA) && out[n-1].Line+out[n-1].Count == l.Line {
			out[n-1].Count++
			continue
		}
		out = append(out, console.BlameLine{Line: l.Line, Count: 1, Commit: shortSHA(l.Commit.SHA), Author: l.Commit.Author,
			When: l.Commit.When.UTC().Format(time.RFC3339), Summary: l.Commit.Subject,
			Opens: repoPath(in, repo, ssmTreeSegment, l.Commit.SHA)})
	}
	return out
}

// Issue and pull request sorts: the hidden fields they order by.
const (
	ssmSortCreated = "_created"
	ssmSortUpdated = "_updated"
	ssmSortNumber  = "_number"
)

// ssmSorts are the Issues and Pull requests tabs' Sort menu. Google's page
// names the menu but not its choices; these are CloudBurrow's.
func ssmSorts() []console.ListSort {
	return []console.ListSort{{Label: "Newest", Key: ssmSortNumber, Desc: true}, {Label: "Oldest", Key: ssmSortNumber},
		{Label: "Recently updated", Key: ssmSortUpdated, Desc: true}, {Label: "Least recently updated", Key: ssmSortUpdated}}
}

// sortFields are a row's hidden sort keys.
func sortFields(fields map[string]string, name string, created, updated *timestamppb.Timestamp) {
	fields[ssmSortNumber] = fmt.Sprintf("%09d", ssmNumber(name))
	fields[ssmSortCreated] = created.AsTime().UTC().Format(time.RFC3339Nano)
	fields[ssmSortUpdated] = updated.AsTime().UTC().Format(time.RFC3339Nano)
}

// ssmTypeField is the hidden field the Type menu reads: "Created by you",
// "Assigned to you", either or both.
const ssmTypeField = "_type"

// typeField sets a row's Type: the console's account opened it, is
// assigned to it, or both. Google's page names the Type menu but not its
// choices; these are the web interface's issue filters (UNVERIFIED,
// re-read 2026-11-20: secure-source-manager/docs/work-with-issues-pull-requests
// names Assignee, Type and Sort but not Type's choices).
func typeField(fields map[string]string, author string, m runadapter.SourceManagerIssueMeta) {
	me := runadapter.SourceManagerConsoleAccount()
	var t []string
	// An unauthenticated caller is the owner, as the console is.
	if strings.EqualFold(author, me) || author == "owner" {
		t = append(t, "Created by you")
	}
	if containsStr(m.Assignees, me) {
		t = append(t, "Assigned to you")
	}
	fields[ssmTypeField] = strings.Join(t, ", ")
}

// issueMenus are the Issues and Pull requests tabs' Labels, Milestones,
// Assignee and Type menus, and Assigned to me, over the hidden fields each
// row carries.
func issueMenus(labels []runadapter.SourceManagerLabel, milestones []runadapter.SourceManagerMilestone, assignees []string) []console.ListMenu {
	var ln, mn []string
	for _, l := range labels {
		ln = append(ln, l.Name)
	}
	for _, m := range milestones {
		mn = append(mn, m.Title)
	}
	return []console.ListMenu{{Label: "Labels", Key: "Labels", Options: ln}, {Label: "Milestones", Key: "Milestone", Options: mn},
		{Label: "Assignee", Key: "Assignees", Options: assignees},
		{Label: "Type", Key: ssmTypeField, Options: []string{"Created by you", "Assigned to you"}},
		{Label: "Assigned to me", Key: "Assignees", Toggle: runadapter.SourceManagerConsoleAccount()}}
}

// labelsSection is the repository's Labels tab: New label, Edit, Delete.
func labelsSection(r *ssmpb.Repository) console.Section {
	sec := console.Section{ID: "labels", Label: "Labels", Actions: []console.Action{labelAction("new-label", "New label", "Create label", runadapter.SourceManagerLabel{Color: "#1a73e8"})},
		Listing: console.Listing{Columns: []string{"Color", "Description", "Issues", "Pull requests"}, NameColumn: "Label", Noun: "labels"}}
	ls, err := runadapter.SourceManagerLabels(r.GetName())
	if err != nil {
		sec.Unavailable = consoleErr(err)
		return sec
	}
	labelsOf := func(m runadapter.SourceManagerIssueMeta) []string { return m.Labels }
	uses, pulls := ssmIssueCounts(r.GetName(), "issues", labelsOf), ssmIssueCounts(r.GetName(), "pullRequests", labelsOf)
	for _, l := range ls {
		at := []string{lastSegment(r.GetInstance()), lastSegment(r.GetName()), ssmLabelsSegment, l.Name}
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: l.Name, ActsOn: at,
			Fields: map[string]string{"Color": l.Color, "Description": orDash(l.Description), "Issues": strconv.Itoa(uses[l.Name]),
				"Pull requests": strconv.Itoa(pulls[l.Name])},
			Actions: []console.Action{labelAction("edit-label", "Edit", "Save", l),
				{ID: "delete-label", Label: "Delete", Destructive: true, Confirm: "the label; the issues holding it lose it"}}})
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return sec
}

func labelAction(id, label, submit string, l runadapter.SourceManagerLabel) console.Action {
	return console.Action{ID: id, Label: label, Submit: submit, Fields: []console.Field{
		{Name: "name", Label: "Label name", Type: "text", Required: true, Default: l.Name, Pattern: "^[^,]{1,50}$",
			Help: "At most 50 characters, no comma."},
		{Name: "color", Label: "Color", Type: "text", Required: true, Default: l.Color, Pattern: "^#[0-9a-fA-F]{6}$", Help: "A colour as #rrggbb."},
		{Name: "description", Label: "Description", Type: "text", Default: l.Description}}}
}

// milestonesSection is the repository's Milestones tab.
func milestonesSection(r *ssmpb.Repository) console.Section {
	sec := console.Section{ID: "milestones", Label: "Milestones",
		Actions: []console.Action{milestoneAction("new-milestone", "New milestone", "Create milestone", runadapter.SourceManagerMilestone{})},
		Listing: console.Listing{Columns: []string{"Due date", "Description", "Issues", "Pull requests"}, NameColumn: "Milestone", Noun: "milestones", AlwaysStatus: true}}
	ms, err := runadapter.SourceManagerMilestones(r.GetName())
	if err != nil {
		sec.Unavailable = consoleErr(err)
		return sec
	}
	msOf := func(m runadapter.SourceManagerIssueMeta) []string { return []string{m.Milestone} }
	uses, pulls := ssmIssueCounts(r.GetName(), "issues", msOf), ssmIssueCounts(r.GetName(), "pullRequests", msOf)
	for _, m := range ms {
		st, toggle := "Open", console.Action{ID: "close-milestone", Label: "Close"}
		if m.Closed {
			st, toggle = "Closed", console.Action{ID: "reopen-milestone", Label: "Reopen"}
		}
		at := []string{lastSegment(r.GetInstance()), lastSegment(r.GetName()), ssmMilestonesSegment, m.Title}
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: m.Title, Status: st, ActsOn: at,
			Fields: map[string]string{"Due date": orDash(m.Due), "Description": orDash(m.Description), "Issues": strconv.Itoa(uses[m.Title]),
				"Pull requests": strconv.Itoa(pulls[m.Title])},
			Actions: []console.Action{milestoneAction("edit-milestone", "Edit", "Save", m), toggle,
				{ID: "delete-milestone", Label: "Delete", Destructive: true, Confirm: "the milestone; its issues lose it"}}})
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return sec
}

func milestoneAction(id, label, submit string, m runadapter.SourceManagerMilestone) console.Action {
	return console.Action{ID: id, Label: label, Submit: submit, Fields: []console.Field{
		{Name: "title", Label: "Title", Type: "text", Required: true, Default: m.Title, Pattern: "^.{1,50}$", Help: "At most 50 characters."},
		{Name: "due", Label: "Due date", Type: "date", Default: m.Due, Help: "Optional."},
		{Name: "description", Label: "Description", Type: "textarea", Default: m.Description}}}
}

// ssmIssueCounts counts the repository's issues (kind "issues") or pull
// requests ("pullRequests") holding each value of what of returns.
func ssmIssueCounts(repo, kind string, of func(runadapter.SourceManagerIssueMeta) []string) map[string]int {
	out := map[string]int{}
	for _, m := range runadapter.SourceManagerIssueMetaAll(repo, kind) {
		for _, v := range of(m) {
			if v != "" {
				out[v]++
			}
		}
	}
	return out
}

// issueMetaActions are an issue's Labels, Milestones and Assignees, each
// the gear beside its detail on the issue's page; a pull request's
// Reviewers and Due date too.
func issueMetaActions(repo string, m runadapter.SourceManagerIssueMeta, pull bool) []console.Action {
	ls, _ := runadapter.SourceManagerLabels(repo)
	// A checkbox per label, named by its place (a label's name may hold
	// anything an element ID cannot); labelNames says which is which.
	var lf []console.Field
	var names []string
	for i, l := range ls {
		lf = append(lf, console.Field{Name: fmt.Sprintf("label-%d", i), Label: l.Name, Type: "checkbox", Default: strconv.FormatBool(containsStr(m.Labels, l.Name)),
			Help: l.Description})
		names = append(names, l.Name)
	}
	if len(lf) > 0 {
		lf = append(lf, console.Field{Name: "labelNames", Type: "hidden", Default: strings.Join(names, "\n")})
	}
	labels := console.Action{ID: "set-labels", Label: "Labels", Submit: "Save", Fields: lf, Linked: true}
	if len(lf) == 0 {
		labels.Unavailable = "The repository has no labels yet: make them on its Labels tab."
	}
	ms, _ := runadapter.SourceManagerMilestones(repo)
	opts := []string{""}
	for _, x := range ms {
		if !x.Closed || x.Title == m.Milestone {
			opts = append(opts, x.Title)
		}
	}
	milestone := console.Action{ID: "set-milestone", Label: "Milestones", Submit: "Save", Linked: true, Fields: []console.Field{
		{Name: "milestone", Label: "Milestone", Type: "select", Options: opts, Default: m.Milestone}}}
	if len(opts) == 1 {
		milestone.Unavailable = "The repository has no open milestones: make one on its Milestones tab."
	}
	assignees := console.Action{ID: "set-assignees", Label: "Assignees", Submit: "Save", Linked: true, Fields: []console.Field{
		{Name: "assignees", Label: "Assignees", Type: "textarea", Default: strings.Join(m.Assignees, "\n"),
			Help: "One email per line. You are " + runadapter.SourceManagerConsoleAccount() + "."}}}
	if !pull {
		return []console.Action{labels, milestone, assignees}
	}
	reviewers := console.Action{ID: "set-reviewers", Label: "Reviewers", Submit: "Save", Linked: true, Fields: []console.Field{
		{Name: "reviewers", Label: "Reviewers", Type: "textarea", Default: strings.Join(m.Reviewers, "\n"), Help: "One email per line."}}}
	due := console.Action{ID: "set-due", Label: "Due date", Submit: "Save", Linked: true, Fields: []console.Field{
		{Name: "due", Label: "Due date", Type: "date", Default: m.Due, Help: "Leave it empty for none."}}}
	return []console.Action{reviewers, labels, milestone, assignees, due}
}

// issueMetaSummary are an issue's or pull request's Labels, Milestone and
// Assignees details (a pull request's Reviewers and Due date first and
// last), each with its gear.
func issueMetaSummary(m runadapter.SourceManagerIssueMeta, pull bool) []console.Property {
	props := []console.Property{{Label: "Labels", Value: orNone(strings.Join(m.Labels, ", ")), Action: "set-labels", ActionText: "Edit labels"},
		{Label: "Milestone", Value: orNone(m.Milestone), Action: "set-milestone", ActionText: "Edit milestone"},
		{Label: "Assignees", Value: orNone(strings.Join(m.Assignees, ", ")), Action: "set-assignees", ActionText: "Edit assignees"}}
	if !pull {
		return props
	}
	return append(append([]console.Property{{Label: "Reviewers", Value: orNone(strings.Join(m.Reviewers, ", ")), Action: "set-reviewers", ActionText: "Edit reviewers"}},
		props...), console.Property{Label: "Due date", Value: orNone(m.Due), Action: "set-due", ActionText: "Edit due date"})
}

// orNone is a detail the issue does not hold yet, said as such.
func orNone(v string) string {
	if v == "" {
		return "None yet"
	}
	return v
}

func containsStr(in []string, s string) bool {
	for _, x := range in {
		if x == s {
			return true
		}
	}
	return false
}

// issueMetaAct performs an issue's Labels, Milestones and Assignees.
func issueMetaAct(name, action string, v map[string]string) (bool, error) {
	m := runadapter.SourceManagerIssueMetaOf(name)[name]
	switch action {
	case "set-labels":
		m.Labels = nil
		for i, l := range strings.Split(v["labelNames"], "\n") {
			if v[fmt.Sprintf("label-%d", i)] == "true" && l != "" {
				m.Labels = append(m.Labels, l)
			}
		}
	case "set-milestone":
		m.Milestone = v["milestone"]
	case "set-assignees":
		m.Assignees = strings.FieldsFunc(v["assignees"], func(r rune) bool { return r == '\n' || r == ',' || r == ' ' })
	case "set-reviewers":
		m.Reviewers = strings.FieldsFunc(v["reviewers"], func(r rune) bool { return r == '\n' || r == ',' || r == ' ' })
	case "set-due":
		m.Due = v["due"]
	default:
		return false, nil
	}
	return true, runadapter.SetSourceManagerIssueMeta(name, m)
}

// labelMilestoneAct performs New label, New milestone and a label's or
// milestone's row actions.
func labelMilestoneAct(repo string, apath []string, action string, v map[string]string) (bool, error) {
	label := runadapter.SourceManagerLabel{Name: v["name"], Color: v["color"], Description: v["description"]}
	ms := runadapter.SourceManagerMilestone{Title: v["title"], Due: v["due"], Description: v["description"]}
	switch {
	case len(apath) == 2 && action == "new-label":
		return true, runadapter.PutSourceManagerLabel(repo, "", label)
	case len(apath) == 2 && action == "new-milestone":
		return true, runadapter.PutSourceManagerMilestone(repo, "", ms)
	case len(apath) == 4 && apath[2] == ssmLabelsSegment && action == "edit-label":
		return true, runadapter.PutSourceManagerLabel(repo, apath[3], label)
	case len(apath) == 4 && apath[2] == ssmLabelsSegment && action == "delete-label":
		return true, runadapter.DeleteSourceManagerLabel(repo, apath[3])
	case len(apath) == 4 && apath[2] == ssmMilestonesSegment:
		cur, ok := findMilestone(repo, apath[3])
		if !ok {
			return true, fmt.Errorf("milestone %q not found", apath[3])
		}
		switch action {
		case "edit-milestone":
			ms.Closed = cur.Closed
			return true, runadapter.PutSourceManagerMilestone(repo, apath[3], ms)
		case "close-milestone", "reopen-milestone":
			cur.Closed = action == "close-milestone"
			return true, runadapter.PutSourceManagerMilestone(repo, apath[3], cur)
		case "delete-milestone":
			return true, runadapter.DeleteSourceManagerMilestone(repo, apath[3])
		}
	}
	return false, nil
}

func findMilestone(repo, title string) (runadapter.SourceManagerMilestone, bool) {
	ms, _ := runadapter.SourceManagerMilestones(repo)
	for _, m := range ms {
		if m.Title == title {
			return m, true
		}
	}
	return runadapter.SourceManagerMilestone{}, false
}

// labelMilestoneRowActions are a label's or milestone's row actions, for
// DetailActions.
func labelMilestoneRowActions(r *ssmpb.Repository, apath []string) []console.Action {
	switch apath[2] {
	case ssmLabelsSegment:
		for _, row := range labelsSection(r).Listing.Items {
			if row.Name == apath[3] {
				return row.Actions
			}
		}
	case ssmMilestonesSegment:
		for _, row := range milestonesSection(r).Listing.Items {
			if row.Name == apath[3] {
				return row.Actions
			}
		}
	}
	return nil
}

// parsePatch reads a unified diff's hunks into lines; what precedes the
// first hunk (git's header) is left out.
func parsePatch(patch string) []console.DiffLine {
	var out []console.DiffLine
	old, now := 0, 0
	inHunk := false
	for _, l := range strings.Split(strings.TrimSuffix(patch, "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			inHunk = true
			var a, c int
			// @@ -a[,b] +c[,d] @@ section
			if f := strings.Fields(l); len(f) >= 3 {
				a, _ = strconv.Atoi(strings.SplitN(strings.TrimPrefix(f[1], "-"), ",", 2)[0])
				c, _ = strconv.Atoi(strings.SplitN(strings.TrimPrefix(f[2], "+"), ",", 2)[0])
			}
			old, now = a, c
			out = append(out, console.DiffLine{Kind: "@", Text: l})
		case !inHunk || strings.HasPrefix(l, "\\"):
		case strings.HasPrefix(l, "+"):
			out = append(out, console.DiffLine{Kind: "+", New: now, Text: l[1:]})
			now++
		case strings.HasPrefix(l, "-"):
			out = append(out, console.DiffLine{Kind: "-", Old: old, Text: l[1:]})
			old++
		default:
			out = append(out, console.DiffLine{Kind: " ", Old: old, New: now, Text: strings.TrimPrefix(l, " ")})
			old++
			now++
		}
	}
	return out
}

// diffSection is Files changed drawn line by line (Section.Diff), with the
// code comments and the viewer's pending ones under their lines.
func diffSection(pr string, diffs []*ssmpb.FileDiff, err error, cms []*ssmpb.PullRequestComment, acts []console.Action, open bool) console.Section {
	sec := console.Section{ID: "files", Label: "Files changed", Kind: console.KindDiff, Actions: acts}
	if err != nil {
		sec.Unavailable = consoleErr(err)
		return sec
	}
	view := &console.DiffView{Files: []console.DiffFile{}, CommentAction: "add-line-comment"}
	if open {
		view.StartAction, view.ReviewAction = "start-review", "review"
	}
	pending := runadapter.SourceManagerPendingComments(pr, runadapter.SourceManagerConsoleAccount())
	view.Pending = len(pending)
	for _, fd := range diffs {
		f := console.DiffFile{Name: fd.GetName(), Action: strings.ToLower(fd.GetAction().String()), Lines: parsePatch(fd.GetPatch())}
		for _, cm := range cms {
			cd := cm.GetCode()
			if cd == nil || cd.GetPosition().GetPath() != fd.GetName() {
				continue
			}
			st := "Unresolved"
			if cd.GetResolved() {
				st = "Resolved"
			}
			f.Comments = append(f.Comments, console.DiffComment{ID: lastSegment(cm.GetName()), Line: cd.GetPosition().GetLine(), Body: cd.GetBody(), State: st})
		}
		for _, pc := range pending {
			if pc.Path == fd.GetName() {
				f.Comments = append(f.Comments, console.DiffComment{ID: "pending-" + pc.ID, Line: pc.Line, Body: pc.Body, State: "Pending", Pending: true})
			}
		}
		view.Files = append(view.Files, f)
	}
	sec.Diff = view
	if len(diffs) == 0 {
		sec.Kind, sec.Text, sec.Diff = console.KindText, "No files changed.", nil
	}
	return sec
}

// startReviewAction is the plus icon's Start review (and, once begun, Add
// comment): a code comment kept Pending until Review.
func startReviewAction(files []string) console.Action {
	return console.Action{ID: "start-review", Label: "Start review", Submit: "Start review", Linked: true, Fields: lineCommentFields(files, "", true)}
}

// discardReviewAction drops the viewer's pending comments.
func discardReviewAction() console.Action {
	return console.Action{ID: "discard-review", Label: "Discard pending comments", Destructive: true, Confirm: "your pending review comments"}
}

// pendingAct performs Start review and Discard pending comments.
func pendingAct(pr, action string, v map[string]string) (bool, error) {
	switch action {
	case "start-review":
		cm, err := codeComment(v, "lineBody", true)
		if err != nil {
			return true, err
		}
		pos := cm.GetCode().GetPosition()
		return true, runadapter.AddSourceManagerPendingComment(pr, runadapter.SourceManagerConsoleAccount(), pos.GetPath(), pos.GetLine(), cm.GetCode().GetBody())
	case "discard-review":
		runadapter.DeleteSourceManagerPendingComments(pr, runadapter.SourceManagerConsoleAccount())
		return true, nil
	}
	return false, nil
}

// pendingRequests are the viewer's pending comments as the requests a
// Review sends with it.
func pendingRequests(pr string) []*ssmpb.CreatePullRequestCommentRequest {
	var out []*ssmpb.CreatePullRequestCommentRequest
	for _, pc := range runadapter.SourceManagerPendingComments(pr, runadapter.SourceManagerConsoleAccount()) {
		out = append(out, &ssmpb.CreatePullRequestCommentRequest{Parent: pr, PullRequestComment: &ssmpb.PullRequestComment{
			CommentDetail: &ssmpb.PullRequestComment_Code_{Code: &ssmpb.PullRequestComment_Code{Body: pc.Body,
				Position: &ssmpb.PullRequestComment_Position{Path: pc.Path, Line: pc.Line}}}}})
	}
	return out
}

// sortedIssueAssignees are every assignee of the repository's issues (kind
// "issues") or pull requests ("pullRequests").
func sortedIssueAssignees(repo, kind string) []string {
	seen := map[string]bool{}
	for _, m := range runadapter.SourceManagerIssueMetaAll(repo, kind) {
		for _, a := range m.Assignees {
			seen[a] = true
		}
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}
