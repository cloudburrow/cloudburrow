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
