package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/backupdr"
)

// TestConsoleBackupDRTriggerBackupOffersTheAssociationsPlanRules (#1491,
// #1844): with two plans, an association's Create on-demand backup offers
// the rules of its own plan only, and a rule of another plan is refused
// before triggerBackup is sent.
func TestConsoleBackupDRTriggerBackupOffersTheAssociationsPlanRules(t *testing.T) {
	srv := backupdr.NewServer("127.0.0.1:0", &fakeBackupDRSQL{runs: map[string]bool{}}, "")
	p := backupDRProvider{bdr: srv, sql: http.NotFoundHandler()}
	ctx := context.Background()
	const project = "bdr-trigger"
	if _, err := p.Create(ctx, project, map[string]string{"id": "vault-1", "region": "us-central1", "retentionDays": "1"}); err != nil {
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
	assocs := bdrAssocsProvider{api: p}
	var opts []string
	for _, a := range assocs.DetailActions(ctx, project, []string{"assoc-a"}) {
		if a.ID == "ondemand" {
			opts = a.Fields[1].Options
		}
	}
	if strings.Join(opts, ",") != "daily" {
		t.Errorf("Create on-demand backup offers %q; want plan-a's rule only", opts)
	}
	err := assocs.ActAt(ctx, project, []string{"assoc-a"}, "ondemand", map[string]string{"rule": "weekly"})
	if err == nil || !strings.Contains(err.Error(), "not in backup plan plan-a") {
		t.Errorf("Create on-demand backup with another plan's rule = %v; want it refused naming the plan", err)
	}
	if err := assocs.ActAt(ctx, project, []string{"assoc-a"}, "ondemand", map[string]string{"rule": "daily"}); err != nil {
		t.Errorf("Create on-demand backup of assoc-a under daily: %v", err)
	}
}
