package main

import (
	"context"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// The APIs & Services page lists the instance's APIs for a project, and its
// Disable and Enable change what the Service Usage API reports (#1352).
func TestConsoleServiceUsageEnableDisable(t *testing.T) {
	cfg := config.Default()
	cfg.Services = []config.Service{config.ServiceStorage}
	srv := newServiceUsageServer(cfg, nil, store.NewMemory())
	p := serviceUsageProvider{api: srv}
	ctx := context.Background()
	state := func() string {
		t.Helper()
		l, err := p.List(ctx, "su-console")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range l.Items {
			if r.Name == "storage.googleapis.com" {
				return r.Status + "/" + r.Actions[0].ID
			}
		}
		t.Fatalf("storage.googleapis.com is not listed: %+v", l.Items)
		return ""
	}
	if got := state(); got != "ENABLED/disable" {
		t.Fatalf("storage = %s, want ENABLED with Disable", got)
	}
	if err := p.Act(ctx, "su-console", "storage.googleapis.com", "disable"); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != "DISABLED/enable" {
		t.Fatalf("after Disable storage = %s, want DISABLED with Enable", got)
	}
	if err := p.Act(ctx, "su-console", "storage.googleapis.com", "enable"); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != "ENABLED/disable" {
		t.Fatalf("after Enable storage = %s", got)
	}
	if err := p.Act(ctx, "su-console", "spanner.googleapis.com", "enable"); err == nil {
		t.Error("enabling Spanner on an instance without it succeeded")
	}
}

// The Service Usage operations page lists a project's operations, done,
// and its Delete forgets one, as operations.delete does (#1530).
func TestConsoleServiceUsageOperations(t *testing.T) {
	cfg := config.Default()
	cfg.Services = []config.Service{config.ServiceStorage}
	srv := newServiceUsageServer(cfg, nil, store.NewMemory())
	p := serviceUsageOperationsProvider{api: srv}
	ctx := context.Background()
	if err := srv.SetState("su-ops-console", "storage.googleapis.com", false); err != nil {
		t.Fatal(err)
	}
	if err := srv.SetState("another-project", "storage.googleapis.com", false); err != nil {
		t.Fatal(err)
	}
	l, err := p.List(ctx, "su-ops-console")
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("list = %+v, %v; want this project's one operation", l.Items, err)
	}
	r := l.Items[0]
	if r.Status != "DONE" || r.Fields["Services"] != "storage.googleapis.com" || r.Fields["Result"] != "DisableServiceResponse" || r.Actions[0].ID != "delete" {
		t.Fatalf("row = %+v", r)
	}
	if err := p.Act(ctx, "su-ops-console", r.Name, "delete"); err != nil {
		t.Fatal(err)
	}
	if l, _ := p.List(ctx, "su-ops-console"); len(l.Items) != 0 {
		t.Errorf("after Delete: %+v", l.Items)
	}
	if len(srv.Operations()) != 1 {
		t.Errorf("Delete removed another project's operation: %d left", len(srv.Operations()))
	}
}
