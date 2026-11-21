package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/cloudquotas"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestConsoleQuotasAtTheOrganization (#2102): with the organization chosen
// in the toolbar, Quotas & System Limits lists the organization's quotas
// and Edit quotas sets an organization preference, which IAM's
// CreateWorkforcePool then enforces.
func TestConsoleQuotasAtTheOrganization(t *testing.T) {
	if resourcemanager.WorkforcePoolQuotaID != cloudquotas.WorkforcePoolQuotaID {
		t.Fatalf("quota IDs differ: %s, %s", resourcemanager.WorkforcePoolQuotaID, cloudquotas.WorkforcePoolQuotaID)
	}
	st := store.NewMemory()
	reg := resourcemanager.New(st)
	q := cloudquotas.NewServer(st, nil, nil)
	q.Hierarchy = quotasHierarchy{reg: reg}
	p := serviceUsageProvider{quotas: q}
	ctx := console.WithScopeValue(context.Background(), scopeOrgParam, resourcemanager.OrganizationNumber)
	out, err := p.quotasCall(ctx, "p1", "quotas", json.RawMessage(`{"service":"iam.googleapis.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	rows := m["quotas"].([]consoleQuota)
	if m["scope"] != resourcemanager.OrganizationName || len(rows) != 5 {
		t.Fatalf("organization quotas = %v rows, scope %v", len(rows), m["scope"])
	}
	found := false
	for _, r := range rows {
		if r.QuotaID == cloudquotas.WorkforcePoolQuotaID && r.Value == 100 && r.Enforced && r.Adjustable {
			found = true
		}
		if strings.Contains(r.QuotaID, "per-project") {
			t.Errorf("project quota %s at the organization", r.QuotaID)
		}
	}
	if !found {
		t.Fatalf("no workforce pool quota in %v", rows)
	}
	set, err := p.quotasCall(ctx, "p1", "quota-set", json.RawMessage(`{"service":"iam.googleapis.com","quotaId":"`+cloudquotas.WorkforcePoolQuotaID+`","value":"150"}`))
	if err != nil {
		t.Fatal(err)
	}
	if name := set.(map[string]any)["name"].(string); !strings.HasPrefix(name, resourcemanager.OrganizationName+"/locations/global/quotaPreferences/") {
		t.Errorf("preference %s", name)
	}
	if v, ok := q.Value(resourcemanager.OrganizationName, "iam.googleapis.com", cloudquotas.WorkforcePoolQuotaID, nil); !ok || v != 150 {
		t.Errorf("granted = %d, %v", v, ok)
	}
	// The project's own page is unchanged.
	proj, err := p.quotasCall(context.Background(), "p1", "quotas", json.RawMessage(`{"service":"iam.googleapis.com"}`))
	if err != nil || len(proj.(map[string]any)["quotas"].([]consoleQuota)) != 3 {
		t.Errorf("project quotas = %v, %v", proj, err)
	}
	// A folder lists none: no served API documents a folder quota.
	f, err := reg.CreateFolder(resourcemanager.OrganizationName, "Quota folder")
	if err != nil {
		t.Fatal(err)
	}
	fctx := console.WithScopeValue(context.Background(), scopeFolderParam, f.ID())
	if got, err := p.quotasCall(fctx, "p1", "quotas", nil); err != nil || len(got.(map[string]any)["quotas"].([]consoleQuota)) != 0 {
		t.Errorf("folder quotas = %v, %v", got, err)
	}
}
