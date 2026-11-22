package main

// The Dataform pages' v1beta1 parts (#2105): a development workspace's
// branches (branch management, its original branch and a shallow clone's
// depth), its commit history, and a workflow configuration's table update
// trigger with its evaluations.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	b1pb "cloud.google.com/go/dataform/apiv1beta1/dataformpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/dataform"
)

// dataformReread copies a v1 message into its v1beta1 twin by wire form.
func dataformReread(src, dst proto.Message) error {
	b, err := proto.Marshal(src)
	if err != nil {
		return err
	}
	return proto.Unmarshal(b, dst)
}

func dataformCreateWorkspace(ctx context.Context, s *dataform.Server, repo string, values map[string]string) error {
	id := strings.TrimSpace(values["workspaceId"])
	w := &b1pb.Workspace{}
	if values["branchManagement"] == "true" {
		w.EnableBranchManagement = proto.Bool(true)
	}
	if b := strings.TrimSpace(values["originalBranch"]); b != "" {
		w.OriginalBranch = proto.String(b)
	}
	if d := strings.TrimSpace(values["depth"]); d != "" {
		n, err := strconv.Atoi(d)
		if err != nil || n < 0 {
			return fmt.Errorf("clone depth must be a whole number")
		}
		w.Depth = int32(n)
	}
	_, err := s.CallBeta(ctx, "CreateWorkspace", &b1pb.CreateWorkspaceRequest{Parent: repo, WorkspaceId: id, Workspace: w})
	return err
}

func dataformBetaWorkspace(ctx context.Context, s *dataform.Server, ws string) *b1pb.Workspace {
	out, err := s.CallBeta(ctx, "GetWorkspace", &b1pb.GetWorkspaceRequest{Name: ws})
	if err != nil {
		return nil
	}
	return out.(*b1pb.Workspace)
}

func dataformBranchList(ctx context.Context, s *dataform.Server, ws string, f b1pb.FetchWorkspaceBranchesRequest_BranchFilter) []*b1pb.BranchMetadata {
	out, err := s.CallBeta(ctx, "FetchWorkspaceBranches", &b1pb.FetchWorkspaceBranchesRequest{Name: ws, Filter: f, PageSize: 1000})
	if err != nil {
		return nil
	}
	return out.(*b1pb.FetchWorkspaceBranchesResponse).GetBranches()
}

// dataformBranchSections are a workspace's current branch, its Branches
// section (with branch management) and its Commit history.
func dataformBranchSections(ctx context.Context, s *dataform.Server, ws, repo string) (string, []console.Section, console.Section) {
	branch := "—"
	if out, err := s.CallBeta(ctx, "FetchCurrentWorkspaceBranch", &b1pb.FetchCurrentWorkspaceBranchRequest{Name: ws}); err == nil {
		branch = out.(*b1pb.FetchCurrentWorkspaceBranchResponse).GetBranchName()
	}
	w := dataformBetaWorkspace(ctx, s, ws)
	if w.GetShallow() {
		branch += " (shallow clone)"
	}
	var sections []console.Section
	if w.GetEnableBranchManagement() {
		l := console.Listing{Columns: []string{"Last commit", "Author", "Committed"}, NameColumn: "Branch", Noun: "branches"}
		for _, b := range dataformBranchList(ctx, s, ws, b1pb.FetchWorkspaceBranchesRequest_ALL) {
			c := b.GetLastCommit()
			l.Items = append(l.Items, console.Resource{Name: b.GetBranchName(), Fields: map[string]string{
				"Last commit": firstLine(c.GetCommitMessage()), "Author": c.GetAuthor().GetName(), "Committed": kmsTime(c.GetCommitTime())}})
		}
		l.Total = len(l.Items)
		l.Note = "Branches of the workspace and of its remote (origin/…). Switch branch checks one out; Fetch updates the remote's."
		sections = append(sections, console.Section{ID: "branches", Label: "Branches", Listing: l})
	}
	h := console.Listing{Columns: []string{"Commit", "Author", "Committed"}, NameColumn: "Message", Noun: "commits"}
	if out, err := s.CallBeta(ctx, "FetchWorkspaceHistory", &b1pb.FetchWorkspaceHistoryRequest{Name: ws, PageSize: 100}); err == nil {
		resp := out.(*b1pb.FetchWorkspaceHistoryResponse)
		for _, c := range resp.GetCommits() {
			sha := c.GetCommitSha()
			if len(sha) > 7 {
				sha = sha[:7]
			}
			h.Items = append(h.Items, console.Resource{Name: firstLine(c.GetCommitMessage()), Fields: map[string]string{
				"Commit": sha, "Author": c.GetAuthor().GetName(), "Committed": kmsTime(c.GetCommitTime())}})
		}
		if resp.GetReachesShallowBoundary() {
			h.Note = "The history ends at the shallow clone's depth; Fetch with a deeper history to read more."
		}
	} else {
		h.Unavailable = consoleErr(err)
	}
	h.Total = len(h.Items)
	return branch, sections, console.Section{ID: "history", Label: "Commit history", Listing: h}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// dataformBranchActions are Switch branch, Delete branch and Fetch, for a
// workspace with branch management.
func dataformBranchActions(ctx context.Context, s *dataform.Server, ws string) []console.Action {
	w := dataformBetaWorkspace(ctx, s, ws)
	if !w.GetEnableBranchManagement() {
		return nil
	}
	var local, all []string
	for _, b := range dataformBranchList(ctx, s, ws, b1pb.FetchWorkspaceBranchesRequest_ALL) {
		all = append(all, b.GetBranchName())
		if !strings.HasPrefix(b.GetBranchName(), "origin/") {
			local = append(local, b.GetBranchName())
		}
	}
	return []console.Action{
		{ID: "checkout", Label: "Switch branch", Submit: "Switch", Fields: []console.Field{
			{Name: "branch", Label: "Branch", Type: "text", Required: true, Help: "A branch of the workspace or its remote, or a new branch's name."},
			{Name: "create", Label: "Create the branch if it does not exist", Type: "checkbox"},
			{Name: "source", Label: "Create from", Type: "select", Options: append([]string{""}, all...),
				Help: "The branch a new branch starts from; empty is the current branch."}}},
		{ID: "deletebranch", Label: "Delete branch", Destructive: true, Submit: "Delete", Fields: []console.Field{
			{Name: "branch", Label: "Branch", Type: "select", Required: true, Options: local},
			{Name: "force", Label: "Delete even if its commits are not merged or pushed", Type: "checkbox"}}},
		{ID: "syncrefs", Label: "Fetch", Submit: "Fetch", Fields: []console.Field{
			{Name: "remoteBranch", Label: "Remote branch", Type: "text", Help: "Empty fetches every branch of the remote."},
			{Name: "deepen", Label: "Deepen history by", Type: "text", Pattern: `^[0-9]*$`, Help: "Commits to add to a shallow clone's history."}}},
	}
}

func dataformBranchAction(ctx context.Context, s *dataform.Server, ws, action string, values map[string]string) error {
	switch action {
	case "checkout":
		_, err := s.CallBeta(ctx, "CheckoutWorkspaceBranch", &b1pb.CheckoutWorkspaceBranchRequest{Name: ws, Branch: strings.TrimSpace(values["branch"]),
			CreateIfNotExists: values["create"] == "true", SourceBranch: strings.TrimSpace(values["source"])})
		return err
	case "deletebranch":
		_, err := s.CallBeta(ctx, "DeleteBranch", &b1pb.DeleteBranchRequest{Name: ws, Branch: strings.TrimSpace(values["branch"]), Force: values["force"] == "true"})
		return err
	case "syncrefs":
		req := &b1pb.SyncWorkspaceRefsRequest{Name: ws, RemoteBranchName: strings.TrimSpace(values["remoteBranch"])}
		if d := strings.TrimSpace(values["deepen"]); d != "" {
			n, err := strconv.Atoi(d)
			if err != nil || n < 0 {
				return fmt.Errorf("deepen must be a whole number")
			}
			req.Deepen = int32(n)
		}
		_, err := s.CallBeta(ctx, "SyncWorkspaceRefs", req)
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

func dataformBetaWorkflow(ctx context.Context, s *dataform.Server, name string) *b1pb.WorkflowConfig {
	if name == "" {
		return nil
	}
	out, err := s.CallBeta(ctx, "GetWorkflowConfig", &b1pb.GetWorkflowConfigRequest{Name: name})
	if err != nil {
		return nil
	}
	return out.(*b1pb.WorkflowConfig)
}

func dataformTriggerTarget(t *b1pb.Target) string {
	parts := []string{}
	for _, p := range []string{t.GetDatabase(), t.GetSchema(), t.GetName()} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ".")
}

func dataformSeconds(d *durationpb.Duration) string {
	if d == nil {
		return ""
	}
	return strconv.FormatInt(int64(d.AsDuration()/time.Second), 10)
}

// dataformTriggerFields are the workflow configuration form's Trigger
// section.
func dataformTriggerFields(ctx context.Context, s *dataform.Server, name string) []console.Field {
	tc := dataformBetaWorkflow(ctx, s, name).GetWorkflowTriggerConfig()
	var tables []string
	for _, t := range tc.GetWorkflowTriggers() {
		tables = append(tables, dataformTriggerTarget(t.GetTableUpdateTrigger().GetTable()))
	}
	cond := "Any table updated"
	if tc.GetCondition() == b1pb.WorkflowTriggerConfig_ALL {
		cond = "All tables updated"
	}
	return []console.Field{
		{Name: "triggerTables", Label: "Tables that trigger the workflow", Type: "textarea", Default: strings.Join(tables, "\n"), Section: "Table update trigger",
			Help: "One BigQuery table per line, as dataset.table or project.dataset.table; an update to them runs the workflow. Empty: no trigger."},
		{Name: "triggerCondition", Label: "Condition", Type: "select", Options: []string{"Any table updated", "All tables updated"}, Default: cond, Section: "Table update trigger"},
		{Name: "minInterval", Label: "Minimum time between executions (seconds)", Type: "text", Pattern: `^[0-9]*$`, Default: dataformSeconds(tc.GetMinExecutionDuration()), Section: "Table update trigger",
			Help: "Empty: the workflow runs whenever the condition is met and no execution of it is running."},
		{Name: "maxWait", Label: "Maximum wait for all tables (seconds)", Type: "text", Pattern: `^[0-9]*$`, Default: dataformSeconds(tc.GetMaxWaitDuration()), Section: "Table update trigger",
			Help: "With All tables updated: how long a partly met condition waits before the workflow runs anyway. Empty waits until every table is updated."},
	}
}

// dataformTrigger is the form's trigger config; nil when it names no table.
func dataformTrigger(values map[string]string) (*b1pb.WorkflowTriggerConfig, error) {
	tc := &b1pb.WorkflowTriggerConfig{Condition: b1pb.WorkflowTriggerConfig_ANY}
	if values["triggerCondition"] == "All tables updated" {
		tc.Condition = b1pb.WorkflowTriggerConfig_ALL
	}
	for _, line := range strings.FieldsFunc(values["triggerTables"], func(r rune) bool { return r == '\n' || r == ',' }) {
		line = strings.Trim(strings.TrimSpace(line), "`")
		if line == "" {
			continue
		}
		p := strings.Split(line, ".")
		t := &b1pb.Target{}
		switch len(p) {
		case 2:
			t.Schema, t.Name = p[0], p[1]
		case 3:
			t.Database, t.Schema, t.Name = p[0], p[1], p[2]
		default:
			return nil, fmt.Errorf("%q is not dataset.table or project.dataset.table", line)
		}
		tc.WorkflowTriggers = append(tc.WorkflowTriggers, &b1pb.WorkflowTrigger{Trigger: &b1pb.WorkflowTrigger_TableUpdateTrigger{
			TableUpdateTrigger: &b1pb.TableUpdateTrigger{Table: t}}})
	}
	if len(tc.WorkflowTriggers) == 0 {
		return nil, nil
	}
	for field, dst := range map[string]**durationpb.Duration{"minInterval": &tc.MinExecutionDuration, "maxWait": &tc.MaxWaitDuration} {
		if v := strings.TrimSpace(values[field]); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("durations are whole numbers of seconds")
			}
			*dst = durationpb.New(time.Duration(n) * time.Second)
		}
	}
	return tc, nil
}

// dataformTriggerSection is a workflow configuration's trigger, in a
// word, and its Trigger evaluations.
func dataformTriggerSection(ctx context.Context, s *dataform.Server, name string) (string, console.Section) {
	tc := dataformBetaWorkflow(ctx, s, name).GetWorkflowTriggerConfig()
	l := console.Listing{Columns: []string{"Evaluated", "Result"}, NameColumn: "Evaluation", Noun: "trigger evaluations"}
	summary := "None"
	if tc != nil {
		var tables []string
		for _, t := range tc.GetWorkflowTriggers() {
			u := t.GetTableUpdateTrigger()
			tables = append(tables, dataformTriggerTarget(u.GetTable()))
		}
		word := "any of"
		if tc.GetCondition() == b1pb.WorkflowTriggerConfig_ALL {
			word = "all of"
		}
		summary = "When " + word + " " + strings.Join(tables, ", ") + " is updated"
		for i, r := range tc.GetRecentTriggerEvaluationRecords() {
			res := "Evaluated"
			if st := r.GetStatus(); st.GetCode() != 0 {
				res = "Failed: " + st.GetMessage()
			}
			l.Items = append(l.Items, console.Resource{Name: strconv.Itoa(i + 1), Status: res, Fields: map[string]string{
				"Evaluated": kmsTime(r.GetEvaluationTime()), "Result": res}})
		}
		if t := tc.GetLastSuccessfulEvaluationTime(); t != nil {
			l.Note = "Last successful evaluation: " + kmsTime(t) + "."
		}
	} else {
		l.Note = "This configuration has no table update trigger; Edit adds one."
	}
	l.Total = len(l.Items)
	return summary, console.Section{ID: "triggers", Label: "Trigger evaluations", Listing: l}
}
