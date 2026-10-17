package main

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/backupdr"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
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

// TestConsoleBackupDRFindProtectedUnderFolders (#1536): Find protected
// resources takes a folder or the organization, and answers every project
// under it (folders and organizations resourceBackupConfigs.fetch), each
// row naming its project.
func TestConsoleBackupDRFindProtectedUnderFolders(t *testing.T) {
	reg := resourcemanager.New(store.NewMemory())
	top, err := reg.CreateFolder(resourcemanager.OrganizationName, "Top")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := reg.CreateFolder(top.Name, "Sub")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []resourcemanager.Project{{ProjectID: "bdr-in-top", Parent: top.Name}, {ProjectID: "bdr-in-sub", Parent: sub.Name}} {
		if _, err := reg.Create(p); err != nil {
			t.Fatal(err)
		}
	}
	srv := backupdr.NewServer("127.0.0.1:0", &fakeBackupDRSQL{runs: map[string]bool{}}, "")
	srv.UseHierarchy(backupDRHierarchy{reg: reg})
	p := backupDRProvider{bdr: srv}
	ctx := context.Background()
	projects := func(l *console.Listing) string {
		var out []string
		for _, it := range l.Items {
			out = append(out, it.Fields["Project"]+"/"+it.Name)
		}
		sort.Strings(out)
		return strings.Join(out, " ")
	}
	for scope, want := range map[string]string{
		top.Name: "bdr-in-sub/pg bdr-in-top/pg", sub.Name: "bdr-in-sub/pg",
		resourcemanager.OrganizationName: "bdr-in-sub/pg bdr-in-top/pg", "": "bdr-console/pg"} {
		l, err := p.ActAtResult(ctx, "bdr-console", nil, "protected", map[string]string{"location": "us-central1", "scope": scope})
		if err != nil || projects(l) != want {
			t.Errorf("scope %q: %v %v, want %s", scope, err, l, want)
		}
	}
	if _, err := p.ActAtResult(ctx, "bdr-console", nil, "protected", map[string]string{"location": "us-central1", "scope": "projects/x"}); err == nil {
		t.Error("a project as the scope was accepted")
	}
	if _, err := p.ActAtResult(ctx, "bdr-console", nil, "protected", map[string]string{"location": "us-central1", "scope": "folders/404"}); err == nil {
		t.Error("a folder that does not exist was accepted")
	}
}
