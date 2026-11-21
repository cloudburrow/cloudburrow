package main

// A Secure Source Manager pull request's reviews and comments, and a
// repository's webhooks (#1927), as Google's web interface has them
// (https://docs.cloud.google.com/secure-source-manager/docs/work-with-issues-pull-requests,
// https://docs.cloud.google.com/secure-source-manager/docs/set-up-webhooks,
// https://docs.cloud.google.com/secure-source-manager/docs/webhooks-overview,
// read 2026-11-19):
//   - the Conversation tab is the pull request's timeline: its
//     description, then every pull request comment (ListPullRequestComments)
//     in order: reviews (Approved, Changes requested, Commented, with the
//     commit they were made on), general comments, and code comments on a
//     file's line, each reply under its conversation's first comment, with
//     Resolve conversation and Unresolve conversation;
//   - Comment adds a general comment, Add line comment a code comment on a
//     changed file's line (negative for the old side, as the API counts
//     it), and Review sends the review: its summary with Approve, Comment
//     or Request changes, and an optional line comment with it, in one
//     BatchCreatePullRequestComments;
//   - Files changed shows each file's lines and the code comments on them
//     (consolesourcemanagercode.go, #2071);
//   - Settings › Webhooks lists the repository's hooks (Hook ID, Target URL,
//     Trigger on, Branch filter, Active); Add webhook, Edit and Delete; a
//     hook's page has Test delivery and its Recent deliveries.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	securesourcemanager "cloud.google.com/go/securesourcemanager/apiv1"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// ssmHooksSegment is the path segment of a repository's webhooks.
const ssmHooksSegment = "hooks"

// prComments are a pull request's comments, through
// ListPullRequestComments, in the order they were made.
func (p sourceManagerProvider) prComments(ctx context.Context, pr string) ([]*ssmpb.PullRequestComment, error) {
	var out []*ssmpb.PullRequestComment
	err := p.withClient(ctx, func(c *securesourcemanager.Client) error {
		it := c.ListPullRequestComments(ctx, &ssmpb.ListPullRequestCommentsRequest{Parent: pr})
		for {
			cm, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				return err
			}
			out = append(out, cm)
		}
	})
	sort.SliceStable(out, func(i, j int) bool { return ssmNumber(out[i].GetName()) < ssmNumber(out[j].GetName()) })
	return out, err
}

// ssmReviewWords are a review's action as Google's web interface says it.
var ssmReviewWords = map[ssmpb.PullRequestComment_Review_ActionType]string{
	ssmpb.PullRequestComment_Review_APPROVED:         "Approved",
	ssmpb.PullRequestComment_Review_CHANGE_REQUESTED: "Changes requested",
	ssmpb.PullRequestComment_Review_COMMENT:          "Commented",
}

// ssmPosition is a code comment's file and line: L for the new side, and
// the old side's line, which the API counts negative, as old L.
func ssmPosition(pos *ssmpb.PullRequestComment_Position) string {
	if pos == nil {
		return ""
	}
	if pos.GetLine() < 0 {
		return fmt.Sprintf("%s:old %d", pos.GetPath(), -pos.GetLine())
	}
	return fmt.Sprintf("%s:%d", pos.GetPath(), pos.GetLine())
}

// conversationSection is the Conversation tab: the description, then each
// comment, a code comment's replies right under the comment that started
// its conversation.
func (p sourceManagerProvider) conversationSection(pr *ssmpb.PullRequest, cms []*ssmpb.PullRequestComment, err error, files []string) console.Section {
	body := pr.GetBody()
	if strings.TrimSpace(body) == "" {
		body = "No description provided."
	}
	sec := console.Section{ID: "conversation", Label: "Conversation", Listing: console.Listing{
		Columns: []string{"Comment", "Type", "File", "Commit", "Created"}, NameColumn: "ID", Noun: "comments", AlwaysStatus: true},
		Actions: prCommentActions(pr, files)}
	sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: "description",
		Fields: map[string]string{"Comment": body, "Type": "Description", "Created": stampAge(pr.GetCreateTime())}})
	if err != nil {
		sec.Unavailable = consoleErr(err)
	}
	// A code comment's replies follow their root, in order.
	replies := map[string][]*ssmpb.PullRequestComment{}
	var roots []*ssmpb.PullRequestComment
	for _, cm := range cms {
		if root := cm.GetCode().GetEffectiveRootComment(); root != "" && root != cm.GetName() {
			replies[root] = append(replies[root], cm)
			continue
		}
		roots = append(roots, cm)
	}
	var ordered []*ssmpb.PullRequestComment
	for _, cm := range roots {
		ordered = append(ordered, cm)
		ordered = append(ordered, replies[cm.GetName()]...)
		delete(replies, cm.GetName())
	}
	for _, rest := range replies { // replies whose root is gone
		ordered = append(ordered, rest...)
	}
	for _, cm := range ordered {
		sec.Listing.Items = append(sec.Listing.Items, prCommentRow(cm))
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return sec
}

// prCommentRow is one comment of the Conversation tab.
func prCommentRow(cm *ssmpb.PullRequestComment) console.Resource {
	row := console.Resource{Name: lastSegment(cm.GetName()), Fields: map[string]string{"Created": stampAge(cm.GetCreateTime())},
		Actions: prCommentRowActions(cm)}
	switch {
	case cm.GetReview() != nil:
		rv := cm.GetReview()
		row.Status = ssmReviewWords[rv.GetActionType()]
		row.Fields["Type"], row.Fields["Comment"] = "Review", orDash(rv.GetBody())
		row.Fields["Commit"] = orDash(shortSHA(rv.GetEffectiveCommitSha()))
	case cm.GetCode() != nil:
		cd := cm.GetCode()
		row.Status = "Unresolved"
		if cd.GetResolved() {
			row.Status = "Resolved"
		}
		row.Fields["Type"], row.Fields["Comment"] = "Code comment", cd.GetBody()
		if root := cd.GetEffectiveRootComment(); root != "" && root != cm.GetName() {
			row.Fields["Type"] = "Reply to " + lastSegment(root)
		}
		row.Fields["File"] = orDash(ssmPosition(cd.GetPosition()))
		row.Fields["Commit"] = orDash(shortSHA(cd.GetEffectiveCommitSha()))
	default:
		row.Fields["Type"], row.Fields["Comment"] = "Comment", cm.GetComment().GetBody()
	}
	return row
}

// prCommentBody is the body of whichever kind a comment is.
func prCommentBody(cm *ssmpb.PullRequestComment) string {
	switch {
	case cm.GetReview() != nil:
		return cm.GetReview().GetBody()
	case cm.GetCode() != nil:
		return cm.GetCode().GetBody()
	}
	return cm.GetComment().GetBody()
}

// prCommentRowActions are a comment's Edit and Delete; a code comment's
// Reply, and Resolve conversation or Unresolve conversation.
func prCommentRowActions(cm *ssmpb.PullRequestComment) []console.Action {
	acts := []console.Action{}
	if cd := cm.GetCode(); cd != nil {
		acts = append(acts, console.Action{ID: "reply-comment", Label: "Reply", Submit: "Add comment", Fields: []console.Field{
			{Name: "body", Label: "Comment", Type: "textarea", Required: true, Help: "Comments support styling with markdown."}}})
		if cd.GetResolved() {
			acts = append(acts, console.Action{ID: "unresolve-comment", Label: "Unresolve conversation"})
		} else {
			acts = append(acts, console.Action{ID: "resolve-comment", Label: "Resolve conversation"})
		}
	}
	return append(acts,
		console.Action{ID: "edit-pr-comment", Label: "Edit", Submit: "Update comment", Fields: []console.Field{
			{Name: "body", Label: "Comment", Type: "textarea", Required: cm.GetReview() == nil, Default: prCommentBody(cm)}}},
		console.Action{ID: "delete-pr-comment", Label: "Delete", Destructive: true, Confirm: "the comment"})
}

// lineCommentFields are a code comment's file, line and body, in section
// sec ("" for none); required makes them required.
func lineCommentFields(files []string, sec string, required bool) []console.Field {
	def := ""
	if len(files) > 0 {
		def = files[0]
	}
	file := console.Field{Name: "path", Label: "File", Type: "select", Options: files, Default: def, Required: required, Section: sec}
	if len(files) == 0 {
		file.Type = "text"
	}
	return []console.Field{file,
		{Name: "line", Label: "Line", Type: "number", Required: required, Section: sec,
			Help: "The line in the changed file; a negative number is a line on the old side of the diff."},
		{Name: "lineBody", Label: "Line comment", Type: "textarea", Required: required, Section: sec,
			Help: "Comments support styling with markdown."}}
}

// reviewAction is Review: the summary, Approve, Comment or Request
// changes, and an optional line comment sent with it.
func reviewAction(files []string) console.Action {
	f := []console.Field{
		{Name: "body", Label: "Leave a comment", Type: "textarea", Help: "Sum up your review, or leave it blank."},
		{Name: "decision", Label: "Review", Type: "radio", Required: true, Default: "COMMENT",
			Options:      []string{"APPROVED", "COMMENT", "CHANGE_REQUESTED"},
			OptionLabels: map[string]string{"APPROVED": "Approve", "COMMENT": "Comment", "CHANGE_REQUESTED": "Request changes"},
			Help:         "You can't approve or request changes to your own pull request."},
	}
	return console.Action{ID: "review", Label: "Review", Submit: "Submit review",
		Fields: append(f, lineCommentFields(files, "Line comment (optional)", false)...)}
}

// prCommentActions are the pull request's Comment, Add line comment and
// Review; a merged one takes comments but no review.
func prCommentActions(pr *ssmpb.PullRequest, files []string) []console.Action {
	acts := []console.Action{{ID: "add-pr-comment", Label: "Comment", Submit: "Comment", Fields: []console.Field{
		{Name: "body", Label: "Comment", Type: "textarea", Required: true, Help: "Comments support styling with markdown."}}},
		{ID: "add-line-comment", Label: "Add line comment", Submit: "Add comment", Fields: lineCommentFields(files, "", true)}}
	if pr.GetState() == ssmpb.PullRequest_OPEN {
		acts = append(acts, reviewAction(files))
	}
	return acts
}

// fileDiffs are a pull request's changed files.
func (p sourceManagerProvider) fileDiffs(ctx context.Context, name string) ([]*ssmpb.FileDiff, error) {
	var out []*ssmpb.FileDiff
	err := p.withClient(ctx, func(c *securesourcemanager.Client) error {
		it := c.ListPullRequestFileDiffs(ctx, &ssmpb.ListPullRequestFileDiffsRequest{Name: name})
		for {
			fd, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				return err
			}
			out = append(out, fd)
		}
	})
	return out, err
}

func diffNames(diffs []*ssmpb.FileDiff) []string {
	out := make([]string, 0, len(diffs))
	for _, d := range diffs {
		out = append(out, d.GetName())
	}
	return out
}

// codeComment reads Add line comment's (or Review's) file, line and body;
// nil when none was given and none is required.
func codeComment(v map[string]string, bodyKey string, required bool) (*ssmpb.PullRequestComment, error) {
	body, path, line := v[bodyKey], strings.TrimSpace(v["path"]), strings.TrimSpace(v["line"])
	if strings.TrimSpace(body) == "" && !required {
		return nil, nil
	}
	if strings.TrimSpace(body) == "" || path == "" || line == "" {
		return nil, fmt.Errorf("a line comment needs its file, line and comment")
	}
	n, err := strconv.ParseInt(line, 10, 64)
	if err != nil || n == 0 {
		return nil, fmt.Errorf("line: want a line number, negative for the old side, got %q", line)
	}
	return &ssmpb.PullRequestComment{CommentDetail: &ssmpb.PullRequestComment_Code_{Code: &ssmpb.PullRequestComment_Code{
		Body: body, Position: &ssmpb.PullRequestComment_Position{Path: path, Line: n}}}}, nil
}

// prCommentAct performs a pull request page's Comment, Add line comment
// and Review.
func prCommentAct(ctx context.Context, c *securesourcemanager.Client, name, action string, v map[string]string) (bool, error) {
	var cm *ssmpb.PullRequestComment
	switch action {
	case "add-pr-comment":
		cm = &ssmpb.PullRequestComment{CommentDetail: &ssmpb.PullRequestComment_Comment_{Comment: &ssmpb.PullRequestComment_Comment{Body: v["body"]}}}
	case "add-line-comment":
		var err error
		if cm, err = codeComment(v, "lineBody", true); err != nil {
			return true, err
		}
	case "review":
		at, ok := ssmpb.PullRequestComment_Review_ActionType_value[v["decision"]]
		if !ok || at == 0 {
			return true, fmt.Errorf("review: choose Approve, Comment or Request changes")
		}
		reqs := []*ssmpb.CreatePullRequestCommentRequest{{Parent: name, PullRequestComment: &ssmpb.PullRequestComment{
			CommentDetail: &ssmpb.PullRequestComment_Review_{Review: &ssmpb.PullRequestComment_Review{
				ActionType: ssmpb.PullRequestComment_Review_ActionType(at), Body: v["body"]}}}}}
		code, err := codeComment(v, "lineBody", false)
		if err != nil {
			return true, err
		}
		// The comments the plus icon's Start review kept Pending go
		// with the review (#2071).
		reqs = append(reqs, pendingRequests(name)...)
		if code != nil {
			reqs = append(reqs, &ssmpb.CreatePullRequestCommentRequest{Parent: name, PullRequestComment: code})
		}
		op, err := c.BatchCreatePullRequestComments(ctx, &ssmpb.BatchCreatePullRequestCommentsRequest{Parent: name, Requests: reqs})
		if err != nil {
			return true, err
		}
		if _, err = op.Wait(ctx); err == nil {
			runadapter.DeleteSourceManagerPendingComments(name, runadapter.SourceManagerConsoleAccount())
		}
		return true, err
	default:
		return false, nil
	}
	op, err := c.CreatePullRequestComment(ctx, &ssmpb.CreatePullRequestCommentRequest{Parent: name, PullRequestComment: cm})
	if err != nil {
		return true, err
	}
	_, err = op.Wait(ctx)
	return true, err
}

// prCommentRowAct performs a comment row's Reply, Resolve conversation,
// Unresolve conversation, Edit and Delete.
func prCommentRowAct(ctx context.Context, c *securesourcemanager.Client, pr, name, action string, v map[string]string) error {
	switch action {
	case "reply-comment":
		op, err := c.CreatePullRequestComment(ctx, &ssmpb.CreatePullRequestCommentRequest{Parent: pr, PullRequestComment: &ssmpb.PullRequestComment{
			CommentDetail: &ssmpb.PullRequestComment_Code_{Code: &ssmpb.PullRequestComment_Code{Body: v["body"], Reply: name}}}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	case "resolve-comment":
		op, err := c.ResolvePullRequestComments(ctx, &ssmpb.ResolvePullRequestCommentsRequest{Parent: pr, Names: []string{name}, AutoFill: true})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	case "unresolve-comment":
		op, err := c.UnresolvePullRequestComments(ctx, &ssmpb.UnresolvePullRequestCommentsRequest{Parent: pr, Names: []string{name}, AutoFill: true})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	case "edit-pr-comment":
		cur, err := c.GetPullRequestComment(ctx, &ssmpb.GetPullRequestCommentRequest{Name: name})
		if err != nil {
			return err
		}
		next := &ssmpb.PullRequestComment{Name: name}
		switch {
		case cur.GetReview() != nil:
			next.CommentDetail = &ssmpb.PullRequestComment_Review_{Review: &ssmpb.PullRequestComment_Review{
				ActionType: cur.GetReview().GetActionType(), Body: v["body"]}}
		case cur.GetCode() != nil:
			next.CommentDetail = &ssmpb.PullRequestComment_Code_{Code: &ssmpb.PullRequestComment_Code{Body: v["body"]}}
		default:
			next.CommentDetail = &ssmpb.PullRequestComment_Comment_{Comment: &ssmpb.PullRequestComment_Comment{Body: v["body"]}}
		}
		op, err := c.UpdatePullRequestComment(ctx, &ssmpb.UpdatePullRequestCommentRequest{PullRequestComment: next,
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"body"}}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	case "delete-pr-comment":
		op, err := c.DeletePullRequestComment(ctx, &ssmpb.DeletePullRequestCommentRequest{Name: name})
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	}
	return fmt.Errorf("unknown action %q", action)
}

// hooks are the repository's webhooks, through ListHooks.
func (p sourceManagerProvider) hooks(ctx context.Context, repo string) ([]*ssmpb.Hook, error) {
	var out []*ssmpb.Hook
	err := p.withClient(ctx, func(c *securesourcemanager.Client) error {
		it := c.ListHooks(ctx, &ssmpb.ListHooksRequest{Parent: repo})
		for {
			h, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				return err
			}
			out = append(out, h)
		}
	})
	return out, err
}

func (p sourceManagerProvider) hook(ctx context.Context, name string) (*ssmpb.Hook, error) {
	var out *ssmpb.Hook
	err := p.withClient(ctx, func(c *securesourcemanager.Client) error {
		var e error
		out, e = c.GetHook(ctx, &ssmpb.GetHookRequest{Name: name})
		return e
	})
	return out, err
}

// ssmHookEventWords are a hook's events as Trigger on names them.
var ssmHookEventWords = map[ssmpb.Hook_HookEventType]string{
	ssmpb.Hook_PUSH:                 "Push",
	ssmpb.Hook_PULL_REQUEST:         "Pull request state changed",
	ssmpb.Hook_PULL_REQUEST_COMMENT: "Pull request comment",
}

// hookEventFields are Trigger on's checkboxes, by event.
var hookEventFields = []struct {
	name  string
	event ssmpb.Hook_HookEventType
}{{"onPush", ssmpb.Hook_PUSH}, {"onPullRequest", ssmpb.Hook_PULL_REQUEST}, {"onPullRequestComment", ssmpb.Hook_PULL_REQUEST_COMMENT}}

func hookEvents(h *ssmpb.Hook) string {
	words := make([]string, 0, len(h.GetEvents()))
	for _, e := range h.GetEvents() {
		words = append(words, ssmHookEventWords[e])
	}
	return strings.Join(words, ", ")
}

// hookFields are Add webhook's fields, as Google's Set up webhooks page
// names them, prefilled from h; the sensitive query string is never shown
// back, and an edit that leaves it empty keeps it.
func hookFields(h *ssmpb.Hook, edit bool) []console.Field {
	has := map[ssmpb.Hook_HookEventType]bool{}
	for _, e := range h.GetEvents() {
		has[e] = true
	}
	if !edit {
		has[ssmpb.Hook_PUSH] = true
	}
	sqHelp := "Appended to the target URL as TARGET_URL?SENSITIVE_QUERY_STRING: the key= and secret= part of a webhook URL."
	if edit {
		sqHelp += " Leave it empty to keep the one set."
	}
	sec := "Trigger on"
	f := []console.Field{
		{Name: "targetUri", Label: "Target URL", Type: "text", Required: true, Default: h.GetTargetUri(), Pattern: "^https?://.+"},
		{Name: "sensitiveQueryString", Label: "Sensitive query string", Type: "password", Help: sqHelp},
		// Enable service account authentication (#2071): each delivery
		// carries an ID token of the repository's service account.
		{Name: "serviceAccountAuth", Label: "Enable service account authentication", Type: "checkbox",
			Default: strconv.FormatBool(runadapter.SourceManagerHookServiceAccountAuth(h.GetName())),
			Help:    "Each request carries an ID token of the repository's service account, for the target URL. Use it or the sensitive query string."},
	}
	for _, ef := range hookEventFields {
		f = append(f, console.Field{Name: ef.name, Label: ssmHookEventWords[ef.event], Type: "checkbox", Section: sec, Default: strconv.FormatBool(has[ef.event])})
	}
	return append(f,
		console.Field{Name: "branchFilter", Label: "Branch filter", Type: "text", Section: sec, Default: h.GetPushOption().GetBranchFilter(), ShowWhen: "onPush",
			Help: "A glob pattern of the branches whose pushes are reported, such as {main,dev}; empty or * for every branch."},
		console.Field{Name: "active", Label: "Active", Type: "checkbox", Default: strconv.FormatBool(!h.GetDisabled()),
			Help: "The webhook sends requests on trigger events when selected."})
}

func addHookAction() console.Action {
	return console.Action{ID: "add-hook", Label: "Add webhook", Submit: "Add webhook", Fields: append([]console.Field{
		{Name: "id", Label: "Hook ID", Type: "text", Required: true, Pattern: "^[a-z]([\\-a-z0-9]{0,61}[a-z0-9])?$",
			Help: "Lower case letters, numbers and dashes, starting with a letter. It can't be changed after the webhook is created."}},
		hookFields(&ssmpb.Hook{}, false)...)}
}

func hookRowActions(h *ssmpb.Hook) []console.Action {
	return []console.Action{
		{ID: "edit-hook", Label: "Edit", Submit: "Update webhook", Fields: hookFields(h, true)},
		{ID: "test-hook", Label: "Test delivery"},
		{ID: "delete-hook", Label: "Delete", Destructive: true, Confirm: "the webhook; its target gets no more events"},
	}
}

// hookFrom reads the webhook fields.
func hookFrom(v map[string]string) (*ssmpb.Hook, error) {
	h := &ssmpb.Hook{TargetUri: strings.TrimSpace(v["targetUri"]), SensitiveQueryString: strings.TrimSpace(v["sensitiveQueryString"]),
		Disabled: v["active"] != "true"}
	for _, ef := range hookEventFields {
		if v[ef.name] == "true" {
			h.Events = append(h.Events, ef.event)
		}
	}
	if len(h.Events) == 0 {
		return nil, fmt.Errorf("trigger on: choose at least one event")
	}
	if v["serviceAccountAuth"] == "true" && h.SensitiveQueryString != "" {
		return nil, fmt.Errorf("authentication: use the sensitive query string or service account authentication, not both")
	}
	if bf := strings.TrimSpace(v["branchFilter"]); bf != "" && v["onPush"] == "true" {
		h.PushOption = &ssmpb.Hook_PushOption{BranchFilter: bf}
	}
	return h, nil
}

// webhooksSection is Settings › Webhooks.
func (p sourceManagerProvider) webhooksSection(ctx context.Context, r *ssmpb.Repository) console.Section {
	sec := console.Section{ID: "webhooks", Label: "Webhooks", Listing: console.Listing{
		Columns: []string{"Target URL", "Trigger on", "Branch filter"}, NameColumn: "Hook ID", Noun: "webhooks", AlwaysStatus: true},
		Actions: []console.Action{addHookAction()}}
	hs, err := p.hooks(ctx, r.GetName())
	if err != nil {
		sec.Unavailable = consoleErr(err)
	}
	for _, h := range hs {
		st := "Active"
		if h.GetDisabled() {
			st = "Inactive"
		}
		at := []string{lastSegment(r.GetInstance()), lastSegment(r.GetName()), ssmHooksSegment, lastSegment(h.GetName())}
		bf := h.GetPushOption().GetBranchFilter()
		if bf == "" {
			bf = "*"
		}
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: lastSegment(h.GetName()), Status: st, Opens: at, ActsOn: at,
			Fields:  map[string]string{"Target URL": h.GetTargetUri(), "Trigger on": hookEvents(h), "Branch filter": bf},
			Actions: hookRowActions(h)})
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return sec
}

// hookDetail is a webhook's page: its settings, then Recent deliveries.
func (p sourceManagerProvider) hookDetail(ctx context.Context, in *ssmpb.Instance, hpath []string) (console.Detail, error) {
	name := p.repoName(in, hpath[1]) + "/hooks/" + hpath[3]
	h, err := p.hook(ctx, name)
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	st := "Active"
	if h.GetDisabled() {
		st = "Inactive"
	}
	bf := h.GetPushOption().GetBranchFilter()
	if bf == "" {
		bf = "*"
	}
	sq := ""
	if h.GetSensitiveQueryString() != "" {
		sq = "Set"
	}
	d := console.Detail{Title: hpath[3], Trail: []console.Crumb{{Label: hpath[0], Path: hpath[:1]}, {Label: hpath[1], Path: hpath[:2]},
		{Label: "Webhooks", Path: hpath[:3]}, {Label: hpath[3]}},
		Summary: dropDashes([]console.Property{{Label: "Hook ID", Value: hpath[3]}, {Label: "Target URL", Value: h.GetTargetUri()},
			{Label: "Sensitive query string", Value: sq}, {Label: "Trigger on", Value: hookEvents(h)}, {Label: "Branch filter", Value: bf},
			{Label: "Service account authentication", Value: ssmEnabledWord(runadapter.SourceManagerHookServiceAccountAuth(name))},
			{Label: "Active", Value: st}, {Label: "Created", Value: stampAge(h.GetCreateTime())}, {Label: "Updated", Value: stampAge(h.GetUpdateTime())}})}
	del := console.Section{ID: "deliveries", Label: "Recent deliveries", Listing: console.Listing{
		Columns: []string{"Event", "Response", "Delivered"}, NameColumn: "Delivery", Noun: "deliveries", AlwaysStatus: true},
		Note: "Test delivery sends a placeholder push event to the target URL."}
	for _, dv := range runadapter.SourceManagerHookDeliveries(name) {
		resp := fmt.Sprint(dv.Status)
		status := "Succeeded"
		if dv.Err != "" {
			resp, status = dv.Err, "Failed"
		} else if dv.Status < 200 || dv.Status > 299 {
			status = "Failed"
		}
		del.Listing.Items = append(del.Listing.Items, console.Resource{Name: dv.ID, Status: status,
			Fields: map[string]string{"Event": dv.Event, "Response": resp, "Delivered": dv.Time.Format(time.RFC3339)}})
	}
	del.Listing.Total = len(del.Listing.Items)
	d.Sections = []console.Section{del}
	return d, nil
}

// hookActions are a webhook's page's actions.
func (p sourceManagerProvider) hookActions(ctx context.Context, name string) []console.Action {
	h, err := p.hook(ctx, name)
	if err != nil {
		return nil
	}
	acts := hookRowActions(h)
	acts[len(acts)-1].Leaves = true
	return acts
}

// hookAct performs a webhook's Edit, Test delivery and Delete.
func hookAct(ctx context.Context, c *securesourcemanager.Client, name, action string, v map[string]string) error {
	switch action {
	case "edit-hook":
		h, err := hookFrom(v)
		if err != nil {
			return err
		}
		h.Name = name
		paths := []string{"target_uri", "disabled", "events", "push_option"}
		if h.GetSensitiveQueryString() != "" {
			paths = append(paths, "sensitive_query_string")
		}
		if v["serviceAccountAuth"] == "true" {
			// Checked before the change, so a refusal changes nothing.
			if err := ssmCanAuthenticate(ctx, c, name); err != nil {
				return err
			}
		}
		op, err := c.UpdateHook(ctx, &ssmpb.UpdateHookRequest{Hook: h, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
		if err != nil {
			return err
		}
		if _, err = op.Wait(ctx); err != nil {
			return err
		}
		return runadapter.SetSourceManagerHookServiceAccountAuth(name, v["serviceAccountAuth"] == "true")
	case "test-hook":
		return runadapter.SourceManagerTestDelivery(ctx, name)
	case "delete-hook":
		op, err := c.DeleteHook(ctx, &ssmpb.DeleteHookRequest{Name: name})
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	}
	return fmt.Errorf("unknown action %q", action)
}

func ssmEnabledWord(on bool) string {
	if on {
		return "Enabled"
	}
	return "Disabled"
}

// ssmCanAuthenticate refuses service account authentication for a hook
// whose repository has no service account.
func ssmCanAuthenticate(ctx context.Context, c *securesourcemanager.Client, hook string) error {
	r, err := c.GetRepository(ctx, &ssmpb.GetRepositoryRequest{Name: hook[:strings.Index(hook, "/hooks/")]})
	if err != nil {
		return err
	}
	if r.GetServiceAccount() == "" {
		return fmt.Errorf("service account authentication needs the repository's service account: %s has none (Create repository's Service account)", lastSegment(r.GetName()))
	}
	return nil
}
