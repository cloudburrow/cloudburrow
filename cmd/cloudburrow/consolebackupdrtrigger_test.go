package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/backupdr"
)

// TestConsoleBackupDRTriggerBackupOffersTheAssociationsPlanRules (#1491):
// in a vault with two plans, Trigger backup offers each association with
// the rules of its own plan only, and a rule of another plan is refused
// before triggerBackup is sent.
func TestConsoleBackupDRTriggerBackupOffersTheAssociationsPlanRules(t *testing.T) {
	srv := backupdr.NewServer("127.0.0.1:0", &fakeBackupDRSQL{runs: map[string]bool{}}, "")
	p := backupDRProvider{bdr: srv, sql: http.NotFoundHandler()}
	ctx := context.Background()
	const project = "bdr-trigger"
	if _, err := p.Create(ctx, project, map[string]string{"id": "vault-1", "location": "us-central1", "retention": "86400s"}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		action string
		values map[string]string
	}{
		{"createplan", map[string]string{"id": "plan-a", "rule": "daily", "recurrence": "DAILY", "retention": "7"}},
		{"createplan", map[string]string{"id": "plan-b", "rule": "weekly", "recurrence": "WEEKLY", "retention": "7"}},
		{"associate", map[string]string{"id": "assoc-a", "plan": "plan-a", "instance": "pg"}},
	} {
		if err := p.ActAt(ctx, project, []string{"vault-1"}, step.action, step.values); err != nil {
			t.Fatalf("%s: %v", step.action, err)
		}
	}
	var opts []string
	for _, a := range p.DetailActions(ctx, project, []string{"vault-1"}) {
		if a.ID == "trigger" {
			opts = a.Fields[0].Options
		}
	}
	if strings.Join(opts, ",") != "assoc-a / daily" {
		t.Errorf("Trigger backup offers %q; want assoc-a with plan-a's rule only", opts)
	}
	err := p.ActAt(ctx, project, []string{"vault-1"}, "trigger", map[string]string{"association": "assoc-a", "rule": "weekly"})
	if err == nil || !strings.Contains(err.Error(), "not in backup plan plan-a") {
		t.Errorf("Trigger backup with another plan's rule = %v; want it refused naming the plan", err)
	}
	if err := p.ActAt(ctx, project, []string{"vault-1"}, "trigger", map[string]string{"target": "assoc-a / daily"}); err != nil {
		t.Errorf("Trigger backup of assoc-a / daily: %v", err)
	}
}
