package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/cloudquotas"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"

	pb "cloud.google.com/go/cloudquotas/apiv1/cloudquotaspb"
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

// Edit quotas changes the preference that holds the quota, whoever made it,
// as often as it is used: Cloud Quotas refuses a second preference on one
// quota and dimensions, and main run 45171873407's page got 400 editing an
// organization quota whose preference had another ID.
func TestConsoleQuotaEditUpdatesTheExistingPreference(t *testing.T) {
	st := store.NewMemory()
	reg := resourcemanager.New(st)
	q := cloudquotas.NewServer(st, nil, nil)
	q.Hierarchy = quotasHierarchy{reg: reg}
	p := serviceUsageProvider{quotas: q}
	ctx := console.WithScopeValue(context.Background(), scopeOrgParam, resourcemanager.OrganizationNumber)
	parent := resourcemanager.OrganizationName + "/locations/global"
	if _, err := q.CreateQuotaPreference(ctx, &pb.CreateQuotaPreferenceRequest{Parent: parent, QuotaPreferenceId: "made-by-gcloud",
		QuotaPreference: &pb.QuotaPreference{Service: "iam.googleapis.com", QuotaId: cloudquotas.WorkforcePoolQuotaID,
			QuotaConfig: &pb.QuotaConfig{PreferredValue: 120}}}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []int64{150, 170} {
		set, err := p.quotasCall(ctx, "p1", "quota-set", json.RawMessage(fmt.Sprintf(`{"service":"iam.googleapis.com","quotaId":%q,"value":"%d"}`, cloudquotas.WorkforcePoolQuotaID, v)))
		if err != nil {
			t.Fatalf("edit to %d: %v", v, err)
		}
		if name := set.(map[string]any)["name"].(string); !strings.HasSuffix(name, "/quotaPreferences/made-by-gcloud") {
			t.Errorf("edit to %d changed %s, not the existing preference", v, name)
		}
		if got, ok := q.Value(resourcemanager.OrganizationName, "iam.googleapis.com", cloudquotas.WorkforcePoolQuotaID, nil); !ok || got != v {
			t.Errorf("after the edit to %d the quota is %d, %v", v, got, ok)
		}
	}
	prefs, err := q.PreferencesOf(resourcemanager.OrganizationName)
	if err != nil || len(prefs) != 1 {
		t.Errorf("preferences = %v, %v; want the one", prefs, err)
	}
}
