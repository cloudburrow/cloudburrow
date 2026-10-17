package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/backupdr"
)

// TestConsoleBackupDRListActions (#1536): the Backup and DR page's own
// actions: Find protected resources with a filter (resourceBackupConfigs.fetch),
// Show, Subscribe to and End trial, and Check Assured Workloads.
func TestConsoleBackupDRListActions(t *testing.T) {
	srv := backupdr.NewServer("127.0.0.1:0", &fakeBackupDRSQL{runs: map[string]bool{}}, "")
	p := backupDRProvider{bdr: srv}
	ctx := context.Background()
	const project = "bdr-console"
	if acts := bdrActionIDs(p.ListActions(ctx, project)); acts != "protected,trial,subscribetrial,endtrial,compliance" {
		t.Fatalf("list actions: %s", acts)
	}
	if _, err := p.Create(ctx, project, map[string]string{"id": "vault-1", "location": "us-central1", "retention": "86400s"}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		action string
		values map[string]string
	}{
		{"createplan", map[string]string{"id": "plan-1", "rule": "daily", "recurrence": "DAILY", "retention": "7", "logdays": "7"}},
		{"associate", map[string]string{"id": "assoc-1", "plan": "plan-1", "instance": "pg"}},
	} {
		if _, err := p.ActAtResult(ctx, project, []string{"vault-1"}, step.action, step.values); err != nil {
			t.Fatalf("%s: %v", step.action, err)
		}
	}
	l, err := p.ActAtResult(ctx, project, nil, "protected", map[string]string{"location": "us-central1", "filter": "vaulted=true", "order": "descending"})
	if err != nil || len(l.Items) != 1 || l.Items[0].Name != "pg" || l.Items[0].Fields["Vaulted"] != "Yes" ||
		!strings.Contains(l.Items[0].Fields["Backup configs"], "BACKUPDR_BACKUP_PLAN assoc-1") {
		t.Fatalf("protected resources: %v %+v", err, l)
	}
	if l, err := p.ActAtResult(ctx, project, nil, "protected", map[string]string{"location": "us-central1", "filter": "vaulted=false"}); err != nil || len(l.Items) != 0 {
		t.Errorf("vaulted=false: %v %+v", err, l)
	}
	if _, err := p.ActAtResult(ctx, project, nil, "protected", map[string]string{"location": "us-central1", "filter": "nope=1"}); err == nil ||
		!strings.Contains(err.Error(), "unknown key") {
		t.Errorf("a bad filter: %v", err)
	}

	if l, err := p.ActAtResult(ctx, project, nil, "trial", map[string]string{"location": "us"}); err != nil || l.Items[0].Status != "ELIGIBLE" {
		t.Fatalf("show trial: %v %+v", err, l)
	}
	if l, err := p.ActAtResult(ctx, project, nil, "subscribetrial", map[string]string{"location": "us"}); err != nil ||
		l.Items[0].Status != "SUBSCRIBED" || l.Items[0].Fields["Ends"] == "" {
		t.Fatalf("subscribe: %v %+v", err, l)
	}
	if l, err := p.ActAtResult(ctx, project, nil, "endtrial", map[string]string{"location": "us", "reason": "MOVE_TO_PAID"}); err != nil ||
		l.Items[0].Status != "UNSUBSCRIBED" || l.Items[0].Fields["End reason"] != "MOVE_TO_PAID" {
		t.Fatalf("end: %v %+v", err, l)
	}
	if _, err := p.ActAtResult(ctx, project, nil, "subscribetrial", map[string]string{"location": "us"}); err == nil {
		t.Error("subscribing again was accepted")
	}
	if l, err := p.ActAtResult(ctx, project, nil, "compliance", map[string]string{"location": "us-central1"}); err != nil ||
		l.Items[0].Fields["Assured Workloads"] != "No" {
		t.Errorf("compliance: %v %+v", err, l)
	}
}
