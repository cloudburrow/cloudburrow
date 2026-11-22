package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/dataform"
)

// TestConsoleDataformBranchesAndTriggers (#2105): v1beta1 on Dataform's
// pages. Create development workspace with branch management; Switch
// branch creating one, its Branch summary, Branches and Commit history;
// Delete branch and Fetch; a workflow configuration's table update trigger
// on Create and Edit, its Trigger summary, and Edit clearing it.
func TestConsoleDataformBranchesAndTriggers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	api, err := dataform.NewServer(dataform.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	p := dataformProvider{svc: &dataformService{api: api}}
	repo, err := p.Create(ctx, "cp", map[string]string{"repositoryId": "br", "region": "us-central1"})
	if err != nil {
		t.Fatal(err)
	}
	summary := func(d console.Detail, label string) string {
		for _, s := range d.Summary {
			if s.Label == label {
				return s.Value
			}
		}
		return ""
	}
	names := func(d console.Detail, id string) string {
		s := workflowsSection(d, id)
		if s == nil {
			return ""
		}
		var out []string
		for _, it := range s.Listing.Items {
			out = append(out, it.Name)
		}
		return strings.Join(out, ",")
	}
	if err := p.ActAt(ctx, "cp", []string{repo}, "createworkspace", map[string]string{"workspaceId": "dev", "branchManagement": "true"}); err != nil {
		t.Fatal(err)
	}
	ws := []string{repo, "workspaces", "dev"}
	if err := p.ActAt(ctx, "cp", ws, "createfile", map[string]string{"path": "definitions/a.sqlx", "contents": "select 1 as a\n"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "cp", ws, "commit", map[string]string{"message": "First", "authorName": "A", "authorEmail": "a@example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "cp", ws, "push", nil); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "cp", ws, "checkout", map[string]string{"branch": "feature", "create": "true"}); err != nil {
		t.Fatal(err)
	}
	d, err := p.Detail(ctx, "cp", ws)
	if err != nil || summary(d, "Branch") != "feature" {
		t.Fatalf("after Switch branch: %q, %v", summary(d, "Branch"), err)
	}
	if got := names(d, "branches"); got != "feature,main,origin/main" {
		t.Errorf("Branches = %q", got)
	}
	if got := names(d, "history"); got != "First" {
		t.Errorf("Commit history = %q", got)
	}
	if err := p.ActAt(ctx, "cp", ws, "deletebranch", map[string]string{"branch": "feature"}); err == nil {
		t.Error("Delete branch took the checked-out branch")
	}
	if err := p.ActAt(ctx, "cp", ws, "checkout", map[string]string{"branch": "main"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "cp", ws, "deletebranch", map[string]string{"branch": "feature", "force": "true"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "cp", ws, "syncrefs", nil); err != nil {
		t.Fatal(err)
	}
	if d, _ = p.Detail(ctx, "cp", ws); names(d, "branches") != "main,origin/main" {
		t.Errorf("after Delete branch and Fetch: %q", names(d, "branches"))
	}

	if err := p.ActAt(ctx, "cp", []string{repo}, "createrelease", map[string]string{"releaseId": "prod", "gitCommitish": "main"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "cp", []string{repo}, "createworkflow", map[string]string{"workflowId": "on-update", "releaseConfig": "prod",
		"triggerTables": "sales.orders\ncp.sales.customers", "triggerCondition": "All tables updated", "maxWait": "600"}); err != nil {
		t.Fatal(err)
	}
	wf := []string{repo, "workflows", "on-update"}
	if d, _ = p.Detail(ctx, "cp", wf); !strings.Contains(summary(d, "Trigger"), "all of sales.orders, cp.sales.customers") {
		t.Fatalf("Trigger = %q", summary(d, "Trigger"))
	}
	if err := p.ActAt(ctx, "cp", wf, "editworkflow", map[string]string{"releaseConfig": "prod", "triggerTables": "sales.orders"}); err != nil {
		t.Fatal(err)
	}
	if d, _ = p.Detail(ctx, "cp", wf); !strings.Contains(summary(d, "Trigger"), "any of sales.orders") {
		t.Errorf("after Edit: Trigger = %q", summary(d, "Trigger"))
	}
	if err := p.ActAt(ctx, "cp", wf, "editworkflow", map[string]string{"releaseConfig": "prod", "triggerTables": ""}); err != nil {
		t.Fatal(err)
	}
	if d, _ = p.Detail(ctx, "cp", wf); summary(d, "Trigger") != "None" {
		t.Errorf("after Edit cleared it: Trigger = %q", summary(d, "Trigger"))
	}
	if err := p.ActAt(ctx, "cp", []string{repo}, "createworkflow", map[string]string{"workflowId": "bad", "releaseConfig": "prod", "triggerTables": "nodataset"}); err == nil {
		t.Error("a trigger table without a dataset was taken")
	}
}
