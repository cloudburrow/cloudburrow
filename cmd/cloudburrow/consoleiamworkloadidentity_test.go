package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// IAM & Admin > Workload Identity Federation (#1941): Create pool with a
// first OIDC provider, the list, the pool's page with its providers, Add
// provider (AWS), Edit pool and provider, Disable and Enable, Delete and
// Restore of a provider and of the pool, over the registry the IAM API
// serves, with the API's refusals.
func TestConsoleIAMWorkloadIdentity(t *testing.T) {
	reg := resourcemanager.New(store.NewMemory())
	if _, err := reg.Create(resourcemanager.Project{ProjectID: "console-wif"}); err != nil {
		t.Fatal(err)
	}
	p := iamWorkloadIdentityProvider{registry: reg}
	ctx := context.Background()
	const project = "console-wif"
	if _, err := p.Create(ctx, "", map[string]string{"poolId": "ci-pool"}); err == nil {
		t.Error("Create without a project was accepted")
	}
	if _, err := p.Create(ctx, project, map[string]string{"poolId": "gcp-pool"}); err == nil {
		t.Error("the reserved gcp- prefix was accepted")
	}
	// A provider the API refuses: the pool is made, as Google's first
	// step makes it, and the error says the provider was not.
	if _, err := p.Create(ctx, project, map[string]string{"poolId": "bad-pool", "providerId": "bad-prov", "providerType": wifTypeOIDC,
		"issuerUri": "https://idp.example.test", "attributeMapping": `{}`}); err == nil || !strings.Contains(err.Error(), "but not its provider") {
		t.Errorf("an OIDC provider without google.subject = %v", err)
	}
	if _, err := reg.WorkloadIdentityPool(project, "bad-pool"); err != nil {
		t.Errorf("the pool of a refused provider: %v", err)
	}
	if err := p.Delete(ctx, project, "bad-pool"); err != nil {
		t.Fatal(err)
	}
	id, err := p.Create(ctx, project, map[string]string{"poolId": "ci-pool", "displayName": "CI", "description": "builds",
		"providerId": "github", "providerDisplayName": "GitHub", "providerType": wifTypeOIDC, "issuerUri": "https://idp.example.test",
		"allowedAudiences": "aud-1\naud-2", "attributeMapping": console.FormatMap(map[string]string{"google.subject": "assertion.sub", "attribute.repo": "assertion.repo"}),
		"attributeCondition": "assertion.repo == 'cloudburrow'"})
	if err != nil || id != "ci-pool" {
		t.Fatalf("Create = %q, %v", id, err)
	}
	l, _ := p.List(ctx, project)
	if len(l.Items) != 2 || l.Items[0].Status != "DELETED" || l.Items[1].Name != "ci-pool" || l.Items[1].Fields["Name"] != "CI" || l.Items[1].Status != "ENABLED" {
		t.Fatalf("List = %+v", l.Items)
	}
	d, _ := p.Detail(ctx, project, []string{"ci-pool"})
	if d.Unavailable != "" || len(d.Sections) != 2 || d.Sections[1].Listing.Total != 1 || d.Edit == nil {
		t.Fatalf("pool page: %q, %d sections", d.Unavailable, len(d.Sections))
	}
	if r := d.Sections[1].Listing.Items[0]; r.Name != "github" || r.Fields["Type"] != wifTypeOIDC || r.Fields["Issuer"] != "https://idp.example.test" {
		t.Errorf("provider row = %+v", r)
	}
	acts := p.DetailActions(ctx, project, []string{"ci-pool"})
	if len(acts) != 2 || acts[0].ID != "add-provider" || acts[1].ID != "disable" {
		t.Errorf("pool actions = %+v", acts)
	}
	if err := p.ActAt(ctx, project, []string{"ci-pool"}, "add-provider", map[string]string{"providerId": "aws-prov", "providerType": wifTypeAWS, "accountId": "123456789012"}); err != nil {
		t.Fatalf("Add provider (AWS): %v", err)
	}
	if aws, err := reg.WorkloadIdentityPoolProvider(project, "ci-pool", "aws-prov"); err != nil || aws.AttributeMapping["google.subject"] != "assertion.arn" {
		t.Errorf("AWS provider = %+v, %v; want Google's default mapping", aws.AttributeMapping, err)
	}
	if err := p.ActAt(ctx, project, []string{"ci-pool"}, "add-provider", map[string]string{"providerId": "aws-two", "providerType": wifTypeAWS, "accountId": "12"}); err == nil {
		t.Error("an AWS account ID of two digits was accepted")
	}
	// The provider's page and Edit provider.
	pd, _ := p.Detail(ctx, project, []string{"ci-pool", "github"})
	if pd.Unavailable != "" || pd.Edit == nil {
		t.Fatalf("provider page: %q", pd.Unavailable)
	}
	if err := p.Edit(ctx, project, []string{"ci-pool", "github"}, map[string]string{"providerDisplayName": "GitHub Actions",
		"issuerUri": "https://idp.example.test", "allowedAudiences": "aud-3",
		"attributeMapping": console.FormatMap(map[string]string{"google.subject": "assertion.sub"}), "attributeCondition": ""}); err != nil {
		t.Fatalf("Edit provider: %v", err)
	}
	v, _ := reg.WorkloadIdentityPoolProvider(project, "ci-pool", "github")
	if v.DisplayName != "GitHub Actions" || len(v.Oidc.AllowedAudiences) != 1 || v.AttributeCondition != "" || len(v.AttributeMapping) != 1 {
		t.Errorf("after Edit provider = %+v", v)
	}
	if err := p.Edit(ctx, project, []string{"ci-pool", "github"}, map[string]string{"issuerUri": "ftp://x",
		"attributeMapping": console.FormatMap(map[string]string{"google.subject": "assertion.sub"})}); err == nil {
		t.Error("an ftp:// issuer was accepted")
	}
	if err := p.Edit(ctx, project, []string{"ci-pool"}, map[string]string{"displayName": "Builds", "description": "d"}); err != nil {
		t.Fatal(err)
	}
	// Provider Disable, Enable, Delete and Restore.
	for _, a := range []string{"disable-provider", "enable-provider", "delete-provider"} {
		if err := p.ActAt(ctx, project, []string{"ci-pool", "github"}, a, nil); err != nil {
			t.Fatalf("%s: %v", a, err)
		}
	}
	if acts := p.DetailActions(ctx, project, []string{"ci-pool", "github"}); len(acts) != 1 || acts[0].ID != "undelete-provider" {
		t.Errorf("a deleted provider's actions = %+v", acts)
	}
	if err := p.ActAt(ctx, project, []string{"ci-pool", "github"}, "undelete-provider", nil); err != nil {
		t.Fatal(err)
	}
	// The pool's Disable, Enable, Delete and Restore.
	if err := p.Act(ctx, project, "ci-pool", "disable"); err != nil {
		t.Fatal(err)
	}
	if l, _ := p.List(ctx, project); l.Items[1].Status != "DISABLED" || l.Items[1].Actions[0].ID != "enable" {
		t.Errorf("disabled pool row = %+v", l.Items[1])
	}
	if err := p.Act(ctx, project, "ci-pool", "enable"); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(ctx, project, "ci-pool"); err != nil {
		t.Fatal(err)
	}
	l, _ = p.List(ctx, project)
	if len(l.Items) != 2 || l.Items[1].Status != "DELETED" || l.Items[1].Actions[0].ID != "undelete" {
		t.Fatalf("a deleted pool's row = %+v", l.Items)
	}
	if dd, _ := p.Detail(ctx, project, []string{"ci-pool"}); dd.Edit != nil {
		t.Error("a deleted pool offers Edit")
	}
	if _, err := p.Create(ctx, project, map[string]string{"poolId": "ci-pool"}); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Errorf("reusing a deleted pool's ID = %v; want ALREADY_EXISTS", err)
	}
	if err := p.Act(ctx, project, "ci-pool", "undelete"); err != nil {
		t.Fatal(err)
	}
	if pool, _ := reg.WorkloadIdentityPool(project, "ci-pool"); pool.State != "ACTIVE" || pool.DisplayName != "Builds" {
		t.Errorf("after Restore the pool is %+v", pool)
	}
}
