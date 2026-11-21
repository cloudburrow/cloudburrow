package run

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// What Google's web interface keeps beside the API (#2071), as "Work with
// issues and pull requests" documents it
// (https://docs.cloud.google.com/secure-source-manager/docs/work-with-issues-pull-requests,
// read 2026-11-20): "On the Issue details page you can assign Labels,
// Milestones, Assignees", and a pull request's line comments go into a
// review begun with "Start review" and sent with "Review". The pinned
// google.cloud.securesourcemanager.v1 API (v1.12.0, and v1.13.1) holds none
// of an issue's labels, milestone or assignees, nor a review's pending
// comments, so CloudBurrow keeps them here, in the instance's state file
// with the repository's other children, for the console: they are not
// served through the API, and an issue read through it carries none.
//
// A label has a name (unique in its repository, at most 50 characters), a
// colour (#rrggbb) and a description; a milestone a title (unique, at most
// 50), a description and an optional due date (YYYY-MM-DD), and is open or
// closed. An issue holds any of its repository's labels, at most one of
// its milestones and its assignees, each an email. A pending comment is a
// code comment of the caller's on a pull request, kept until their Review
// sends them all with it (BatchCreatePullRequestComments) or they are
// discarded. A webhook's "Enable service account authentication" is kept
// here too (securesourcemanagerhooks.go): the pinned Hook has no field for
// it. CloudBurrow's choices where Google documents none: the limits above,
// and that deleting a label or milestone takes it off every issue.

// SourceManagerLabel is one of a repository's issue labels.
type SourceManagerLabel struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description,omitempty"`
}

// SourceManagerMilestone is one of a repository's milestones.
type SourceManagerMilestone struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	// Due is YYYY-MM-DD, or "".
	Due    string `json:"due,omitempty"`
	Closed bool   `json:"closed,omitempty"`
}

// SourceManagerIssueMeta is what an issue or a pull request holds beside
// the API; a pull request has its Reviewers and Due date too.
type SourceManagerIssueMeta struct {
	Labels    []string `json:"labels,omitempty"`
	Milestone string   `json:"milestone,omitempty"`
	Assignees []string `json:"assignees,omitempty"`
	Reviewers []string `json:"reviewers,omitempty"`
	// Due is YYYY-MM-DD, or "".
	Due string `json:"due,omitempty"`
}

// SourceManagerPendingComment is a code comment kept for a review.
type SourceManagerPendingComment struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Line int64  `json:"line"`
	Body string `json:"body"`
}

type ssmSavedExtras struct {
	// Labels and Milestones are by repository, IssueMeta by issue,
	// Pending by "{pull request}|{account}", HookAuth by hook.
	Labels     map[string][]SourceManagerLabel          `json:"labels,omitempty"`
	Milestones map[string][]SourceManagerMilestone      `json:"milestones,omitempty"`
	IssueMeta  map[string]SourceManagerIssueMeta        `json:"issueMeta,omitempty"`
	Pending    map[string][]SourceManagerPendingComment `json:"pending,omitempty"`
	HookAuth   map[string]bool                          `json:"hookServiceAccountAuth,omitempty"`
}

// ssmExtra is guarded by ssmState.mu.
var ssmExtra = ssmSavedExtras{Labels: map[string][]SourceManagerLabel{}, Milestones: map[string][]SourceManagerMilestone{},
	IssueMeta: map[string]SourceManagerIssueMeta{}, Pending: map[string][]SourceManagerPendingComment{}, HookAuth: map[string]bool{}}

func loadSSMExtrasLocked(e *ssmSavedExtras) {
	if e == nil {
		return
	}
	keep := func(k string) bool { return ssmState.repos[ssmRepoOf(k)] != nil }
	for k, v := range e.Labels {
		if keep(k) {
			ssmExtra.Labels[k] = v
		}
	}
	for k, v := range e.Milestones {
		if keep(k) {
			ssmExtra.Milestones[k] = v
		}
	}
	for k, v := range e.IssueMeta {
		if ssmState.issues[k] != nil || ssmState.pulls[k] != nil {
			ssmExtra.IssueMeta[k] = v
		}
	}
	for k, v := range e.Pending {
		if ssmState.pulls[strings.SplitN(k, "|", 2)[0]] != nil {
			ssmExtra.Pending[k] = v
		}
	}
	for k, v := range e.HookAuth {
		if ssmState.hooks[k] != nil {
			ssmExtra.HookAuth[k] = v
		}
	}
}

func deleteSSMExtrasLocked(prefix string) {
	deletePrefix(ssmExtra.Labels, prefix)
	deletePrefix(ssmExtra.Milestones, prefix)
	deletePrefix(ssmExtra.IssueMeta, prefix)
	deletePrefix(ssmExtra.Pending, prefix)
	deletePrefix(ssmExtra.HookAuth, prefix)
}

var ssmColorRE = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func ssmRepoExistsLocked(repo string) error {
	_, _, err := ssmRepoLocked(repo)
	return err
}

// SourceManagerLabels are a repository's labels, by name.
func SourceManagerLabels(repo string) ([]SourceManagerLabel, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	if err := ssmRepoExistsLocked(repo); err != nil {
		return nil, err
	}
	return append([]SourceManagerLabel(nil), ssmExtra.Labels[repo]...), nil
}

// PutSourceManagerLabel creates a label, or changes the one named old.
func PutSourceManagerLabel(repo, old string, l SourceManagerLabel) error {
	l.Name, l.Description, l.Color = strings.TrimSpace(l.Name), strings.TrimSpace(l.Description), strings.ToLower(strings.TrimSpace(l.Color))
	if l.Name == "" || len([]rune(l.Name)) > 50 || strings.Contains(l.Name, ",") {
		return apierror.InvalidArgument("label name %q: want 1 to 50 characters, no comma", l.Name)
	}
	if !ssmColorRE.MatchString(l.Color) {
		return apierror.InvalidArgument("label color %q: want #rrggbb", l.Color)
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	if err := ssmRepoExistsLocked(repo); err != nil {
		return err
	}
	ls := ssmExtra.Labels[repo]
	at := -1
	for i, x := range ls {
		if x.Name == l.Name && x.Name != old {
			return apierror.AlreadyExists("label %q already exists", l.Name)
		}
		if old != "" && x.Name == old {
			at = i
		}
	}
	if old != "" && at < 0 {
		return apierror.NotFound("label %q not found", old)
	}
	if at < 0 {
		ls = append(ls, l)
	} else {
		ls[at] = l
		ssmRenameInIssuesLocked(repo, func(m *SourceManagerIssueMeta) { m.Labels = replaceString(m.Labels, old, l.Name) })
	}
	sort.Slice(ls, func(i, j int) bool { return strings.ToLower(ls[i].Name) < strings.ToLower(ls[j].Name) })
	ssmExtra.Labels[repo] = ls
	saveSSMLocked()
	return nil
}

// DeleteSourceManagerLabel deletes a label, and takes it off every issue.
func DeleteSourceManagerLabel(repo, name string) error {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	ls, kept := ssmExtra.Labels[repo], []SourceManagerLabel{}
	for _, x := range ls {
		if x.Name != name {
			kept = append(kept, x)
		}
	}
	if len(kept) == len(ls) {
		return apierror.NotFound("label %q not found", name)
	}
	ssmExtra.Labels[repo] = kept
	ssmRenameInIssuesLocked(repo, func(m *SourceManagerIssueMeta) { m.Labels = replaceString(m.Labels, name, "") })
	saveSSMLocked()
	return nil
}

// SourceManagerMilestones are a repository's milestones: open first, by due
// date, then title.
func SourceManagerMilestones(repo string) ([]SourceManagerMilestone, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	if err := ssmRepoExistsLocked(repo); err != nil {
		return nil, err
	}
	return append([]SourceManagerMilestone(nil), ssmExtra.Milestones[repo]...), nil
}

// PutSourceManagerMilestone creates a milestone, or changes the one titled
// old.
func PutSourceManagerMilestone(repo, old string, ms SourceManagerMilestone) error {
	ms.Title, ms.Description, ms.Due = strings.TrimSpace(ms.Title), strings.TrimSpace(ms.Description), strings.TrimSpace(ms.Due)
	if ms.Title == "" || len([]rune(ms.Title)) > 50 {
		return apierror.InvalidArgument("milestone title %q: want 1 to 50 characters", ms.Title)
	}
	if ms.Due != "" {
		if _, err := time.Parse("2006-01-02", ms.Due); err != nil {
			return apierror.InvalidArgument("milestone due date %q: want YYYY-MM-DD", ms.Due)
		}
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	if err := ssmRepoExistsLocked(repo); err != nil {
		return err
	}
	list := ssmExtra.Milestones[repo]
	at := -1
	for i, x := range list {
		if x.Title == ms.Title && x.Title != old {
			return apierror.AlreadyExists("milestone %q already exists", ms.Title)
		}
		if old != "" && x.Title == old {
			at = i
		}
	}
	if old != "" && at < 0 {
		return apierror.NotFound("milestone %q not found", old)
	}
	if at < 0 {
		list = append(list, ms)
	} else {
		list[at] = ms
		ssmRenameInIssuesLocked(repo, func(m *SourceManagerIssueMeta) {
			if m.Milestone == old {
				m.Milestone = ms.Title
			}
		})
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.Closed != b.Closed {
			return !a.Closed
		}
		if (a.Due == "") != (b.Due == "") {
			return a.Due != ""
		}
		if a.Due != b.Due {
			return a.Due < b.Due
		}
		return a.Title < b.Title
	})
	ssmExtra.Milestones[repo] = list
	saveSSMLocked()
	return nil
}

// DeleteSourceManagerMilestone deletes a milestone, and takes it off every
// issue.
func DeleteSourceManagerMilestone(repo, title string) error {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	list, kept := ssmExtra.Milestones[repo], []SourceManagerMilestone{}
	for _, x := range list {
		if x.Title != title {
			kept = append(kept, x)
		}
	}
	if len(kept) == len(list) {
		return apierror.NotFound("milestone %q not found", title)
	}
	ssmExtra.Milestones[repo] = kept
	ssmRenameInIssuesLocked(repo, func(m *SourceManagerIssueMeta) {
		if m.Milestone == title {
			m.Milestone = ""
		}
	})
	saveSSMLocked()
	return nil
}

func ssmRenameInIssuesLocked(repo string, f func(*SourceManagerIssueMeta)) {
	for k, m := range ssmExtra.IssueMeta {
		if strings.HasPrefix(k, repo+"/") {
			f(&m)
			ssmExtra.IssueMeta[k] = m
		}
	}
}

func replaceString(in []string, old, now string) []string {
	var out []string
	for _, s := range in {
		if s == old {
			s = now
		}
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// SourceManagerIssueMetaOf is what an issue holds beside the API; issues
// are a repository's issues' names.
func SourceManagerIssueMetaOf(issues ...string) map[string]SourceManagerIssueMeta {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	out := map[string]SourceManagerIssueMeta{}
	for _, n := range issues {
		if m, ok := ssmExtra.IssueMeta[n]; ok {
			out[n] = m
		}
	}
	return out
}

// SetSourceManagerIssueMeta sets an issue's or a pull request's labels,
// milestone and assignees (and a pull request's reviewers and due date),
// each label and milestone one of the repository's, each person an email.
func SetSourceManagerIssueMeta(issue string, m SourceManagerIssueMeta) error {
	repo := ssmRepoOf(issue)
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	pull := ssmState.pulls[issue] != nil
	if ssmState.issues[issue] == nil && !pull {
		return apierror.NotFound("issue or pull request %q not found", issue)
	}
	if !pull && (len(m.Reviewers) > 0 || m.Due != "") {
		return apierror.InvalidArgument("an issue has no reviewers or due date")
	}
	if m.Due = strings.TrimSpace(m.Due); m.Due != "" {
		if _, err := time.Parse("2006-01-02", m.Due); err != nil {
			return apierror.InvalidArgument("due date %q: want YYYY-MM-DD", m.Due)
		}
	}
	have := map[string]bool{}
	for _, l := range ssmExtra.Labels[repo] {
		have[l.Name] = true
	}
	var labels []string
	for _, l := range m.Labels {
		if l = strings.TrimSpace(l); l == "" || containsString(labels, l) {
			continue
		}
		if !have[l] {
			return apierror.InvalidArgument("label %q is not one of the repository's labels", l)
		}
		labels = append(labels, l)
	}
	sort.Strings(labels)
	if m.Milestone = strings.TrimSpace(m.Milestone); m.Milestone != "" {
		found := false
		for _, x := range ssmExtra.Milestones[repo] {
			found = found || x.Title == m.Milestone
		}
		if !found {
			return apierror.InvalidArgument("milestone %q is not one of the repository's milestones", m.Milestone)
		}
	}
	people := func(what string, in []string) ([]string, error) {
		var who []string
		for _, a := range in {
			if a = strings.ToLower(strings.TrimSpace(a)); a == "" || containsString(who, a) {
				continue
			}
			if !strings.Contains(a, "@") && a != ssmOwnerAccount {
				return nil, apierror.InvalidArgument("%s %q: want an email", what, a)
			}
			who = append(who, a)
		}
		sort.Strings(who)
		return who, nil
	}
	who, err := people("assignee", m.Assignees)
	if err != nil {
		return err
	}
	reviewers, err := people("reviewer", m.Reviewers)
	if err != nil {
		return err
	}
	m.Labels, m.Assignees, m.Reviewers = labels, who, reviewers
	if len(m.Labels) == 0 && m.Milestone == "" && len(m.Assignees) == 0 && len(m.Reviewers) == 0 && m.Due == "" {
		delete(ssmExtra.IssueMeta, issue)
	} else {
		ssmExtra.IssueMeta[issue] = m
	}
	saveSSMLocked()
	return nil
}

func containsString(in []string, s string) bool {
	for _, x := range in {
		if x == s {
			return true
		}
	}
	return false
}

// SourceManagerConsoleAccount is the account the console's requests are
// made as: the instance's own, which "Assigned to me" names.
func SourceManagerConsoleAccount() string {
	ssmCaller.mu.Lock()
	defer ssmCaller.mu.Unlock()
	if ssmCaller.owner != "" {
		return strings.ToLower(ssmCaller.owner)
	}
	return ssmOwnerAccount
}

func ssmPendingKey(pull, account string) string { return pull + "|" + account }

// SourceManagerPendingComments are an account's pending review comments
// on a pull request, in the order they were added.
func SourceManagerPendingComments(pull, account string) []SourceManagerPendingComment {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	return append([]SourceManagerPendingComment(nil), ssmExtra.Pending[ssmPendingKey(pull, account)]...)
}

// AddSourceManagerPendingComment keeps a code comment for the account's
// review of an open pull request: "Start review", then "Add comment".
func AddSourceManagerPendingComment(pull, account, path string, line int64, body string) error {
	if strings.TrimSpace(body) == "" || path == "" || line == 0 {
		return apierror.InvalidArgument("a pending comment needs its file, a line other than 0 and a body")
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	pr := ssmState.pulls[pull]
	if pr == nil {
		return apierror.NotFound("pull request %q not found", pull)
	}
	if pr.GetState() != ssmpb.PullRequest_OPEN {
		return apierror.FailedPrecondition("pull request %q is not open", pull)
	}
	k := ssmPendingKey(pull, account)
	list := ssmExtra.Pending[k]
	next := 1
	for _, c := range list {
		if n, _ := strconv.Atoi(c.ID); n >= next {
			next = n + 1
		}
	}
	if len(list) >= 100 {
		return apierror.FailedPrecondition("a review takes at most 100 code comments")
	}
	ssmExtra.Pending[k] = append(list, SourceManagerPendingComment{ID: strconv.Itoa(next), Path: path, Line: line, Body: body})
	saveSSMLocked()
	return nil
}

// DeleteSourceManagerPendingComments drops pending comments: those whose
// IDs are given, or all of them when none is.
func DeleteSourceManagerPendingComments(pull, account string, ids ...string) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	k := ssmPendingKey(pull, account)
	if len(ids) == 0 {
		delete(ssmExtra.Pending, k)
	} else {
		var kept []SourceManagerPendingComment
		for _, c := range ssmExtra.Pending[k] {
			if !containsString(ids, c.ID) {
				kept = append(kept, c)
			}
		}
		ssmExtra.Pending[k] = kept
		if len(kept) == 0 {
			delete(ssmExtra.Pending, k)
		}
	}
	saveSSMLocked()
}

// SourceManagerHookServiceAccountAuth reports whether a webhook's
// deliveries carry the repository service account's ID token.
func SourceManagerHookServiceAccountAuth(hook string) bool {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	return ssmExtra.HookAuth[hook]
}

// SetSourceManagerHookServiceAccountAuth turns a webhook's service account
// authentication on or off; on needs the repository's service account.
func SetSourceManagerHookServiceAccountAuth(hook string, on bool) error {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	if ssmState.hooks[hook] == nil {
		return apierror.NotFound("hook %q not found", hook)
	}
	if on {
		r := ssmState.repos[ssmRepoOf(hook)]
		if r.GetServiceAccount() == "" {
			return apierror.FailedPrecondition("service account authentication needs the repository's service account: the repository %s has none", lastSegmentOf(ssmRepoOf(hook)))
		}
		ssmExtra.HookAuth[hook] = true
	} else {
		delete(ssmExtra.HookAuth, hook)
	}
	saveSSMLocked()
	return nil
}

func lastSegmentOf(name string) string { return name[strings.LastIndex(name, "/")+1:] }

// ssmTokens mints the ID tokens of deliveries to hooks with service
// account authentication; nil when the instance mints none.
var ssmTokens struct {
	mu sync.Mutex
	t  IDTokenMinter
}

// ssmHookAuthorization is the Authorization header of a delivery to a hook
// with service account authentication: an ID token of the repository's
// service account whose audience is the target URL without its query, or
// "" for a hook without it.
func ssmHookAuthorization(hook, target string) (string, error) {
	ssmState.mu.Lock()
	on := ssmExtra.HookAuth[hook]
	sa := ssmState.repos[ssmRepoOf(hook)].GetServiceAccount()
	ssmState.mu.Unlock()
	if !on {
		return "", nil
	}
	ssmTokens.mu.Lock()
	t := ssmTokens.t
	ssmTokens.mu.Unlock()
	if t == nil {
		return "", fmt.Errorf("not delivered: this instance mints no ID tokens, so the service account authentication of %s cannot be sent", lastSegmentOf(hook))
	}
	tok, err := t.ServiceAccountIDToken(sa, strings.SplitN(target, "?", 2)[0])
	if err != nil {
		return "", fmt.Errorf("not delivered: the ID token of %s: %w", sa, err)
	}
	return "Bearer " + tok, nil
}

// WithTokens gives webhook deliveries with service account
// authentication their ID tokens.
func (m *SecureSourceManagerServer) WithTokens(t IDTokenMinter) *SecureSourceManagerServer {
	ssmTokens.mu.Lock()
	ssmTokens.t = t
	ssmTokens.mu.Unlock()
	return m
}

// SourceManagerIssueMetaAll is what each of a repository's issues (kind
// "issues") or pull requests ("pullRequests") holds beside the API, by
// name.
func SourceManagerIssueMetaAll(repo, kind string) map[string]SourceManagerIssueMeta {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	out := map[string]SourceManagerIssueMeta{}
	for k, m := range ssmExtra.IssueMeta {
		if strings.HasPrefix(k, repo+"/"+kind+"/") {
			out[k] = m
		}
	}
	return out
}

// SourceManagerAuthors are the accounts that opened issues or pull
// requests (or wrote comments), by name; "" for one made before #2071
// recorded issues' authors.
func SourceManagerAuthors(names ...string) map[string]string {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	out := map[string]string{}
	for _, n := range names {
		if a := ssmState.authors[n]; a != "" {
			out[n] = a
		}
	}
	return out
}
