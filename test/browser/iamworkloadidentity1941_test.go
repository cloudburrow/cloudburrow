//go:build browser

package browser

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	iam "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
)

// TestIAMWorkloadIdentityFederationPage (#1941): IAM & Admin > Workload
// Identity Federation in Google's steps. New workload provider and pool
// makes a pool and its first OIDC provider (issuer, audience, attribute
// mapping and condition); the list shows the pool; its page lists the
// provider; Add provider makes an AWS one; a deleted provider's page and a
// deleted pool's row offer Restore. Each is read back through the official
// google.golang.org/api/iam/v1 client.
func TestIAMWorkloadIdentityFederationPage(t *testing.T) {
	needService(t, "iam-workload-identity")
	ctx := context.Background()
	svc, err := iam.NewService(ctx, option.WithEndpoint("http://"+endpoint(t, envResourceManager)+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	project := uniqueProject(t)
	parent := "projects/" + project + "/locations/global/workloadIdentityPools/"

	p := open(t)
	p.navigate("/iam-admin/workload-identity-pools/create?project=" + project)
	p.waitFor(`document.querySelector("#f-displayName") !== null`)
	var steps []string
	p.eval(`[...document.querySelectorAll(".create-step-title")].map((s) => s.textContent)`, &steps)
	if len(steps) != 3 || steps[0] != "Create an identity pool" || !strings.HasPrefix(steps[1], "Add a provider to pool") || steps[2] != "Configure provider attributes" {
		t.Errorf("the create page's steps are %v", steps)
	}
	p.eval(`(() => { const f = document.querySelector("#f-displayName"); f.value = "CI pool";
		f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	var id string
	p.eval(`document.querySelector("#f-poolId").value`, &id)
	if id != "ci-pool" {
		t.Errorf("the pool ID made from the name is %q; want ci-pool", id)
	}
	p.clickText(".create-step-continue button", "Continue")
	setField(p, "#view", "f-providerType", "OpenID Connect (OIDC)")
	setField(p, "#view", "f-providerId", "github")
	setField(p, "#view", "f-issuerUri", "https://idp.example.test")
	setField(p, "#view", "f-allowedAudiences", "ci-audience")
	p.clickText(".create-step-continue button", "Continue")
	setField(p, "#view", "f-attributeMapping", "google.subject=assertion.sub\nattribute.repo=assertion.repo")
	setField(p, "#view", "f-attributeCondition", "assertion.repo == 'cloudburrow'")
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("ci-pool"))`)

	pool, err := svc.Projects.Locations.WorkloadIdentityPools.Get(parent + "ci-pool").Context(ctx).Do()
	if err != nil || pool.DisplayName != "CI pool" || pool.State != "ACTIVE" {
		t.Fatalf("the new pool = %+v, %v", pool, err)
	}
	prov, err := svc.Projects.Locations.WorkloadIdentityPools.Providers.Get(parent + "ci-pool/providers/github").Context(ctx).Do()
	if err != nil || prov.Oidc == nil || prov.Oidc.IssuerUri != "https://idp.example.test" || prov.AttributeMapping["attribute.repo"] != "assertion.repo" ||
		prov.AttributeCondition != "assertion.repo == 'cloudburrow'" {
		t.Fatalf("the new provider = %+v, %v", prov, err)
	}

	// The pool's page lists the provider; Add provider makes an AWS one.
	p.navigate("/iam-admin/workload-identity-pools/ci-pool?project=" + project)
	p.waitFor(`document.querySelector("#view [role=tab]") !== null`)
	p.clickText("#view [role=tab]", "Providers")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("github"))`)
	p.pageAction("Add provider", map[string]string{"providerType": "AWS", "providerId": "aws-prod", "accountId": "123456789012"}, "")
	aws, err := svc.Projects.Locations.WorkloadIdentityPools.Providers.Get(parent + "ci-pool/providers/aws-prod").Context(ctx).Do()
	if err != nil || aws.Aws == nil || aws.Aws.AccountId != "123456789012" || aws.AttributeMapping["google.subject"] != "assertion.arn" {
		t.Fatalf("the AWS provider = %+v, %v", aws, err)
	}

	// A deleted provider's page offers Restore.
	if _, err := svc.Projects.Locations.WorkloadIdentityPools.Providers.Delete(parent + "ci-pool/providers/github").Context(ctx).Do(); err != nil {
		t.Fatalf("delete the provider through the API: %v", err)
	}
	p.navigate("/iam-admin/workload-identity-pools/ci-pool/github?project=" + project)
	where := p.findAction("Restore")
	if where == "" {
		t.Fatal("a deleted provider's page offers no Restore")
	}
	p.clickText(where, "Restore")
	p.waitFor(`[...document.querySelectorAll("#view")].some((v) => v.textContent.includes("ENABLED"))`)
	if got, err := svc.Projects.Locations.WorkloadIdentityPools.Providers.Get(parent + "ci-pool/providers/github").Context(ctx).Do(); err != nil || got.State != "ACTIVE" {
		t.Errorf("after Restore the provider = %+v, %v", got, err)
	}

	// The pool's Delete leaves it DELETED, with Restore on its row.
	if _, err := svc.Projects.Locations.WorkloadIdentityPools.Delete(parent + "ci-pool").Context(ctx).Do(); err != nil {
		t.Fatalf("delete the pool through the API: %v", err)
	}
	p.navigate("/iam-admin/workload-identity-pools?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("ci-pool") && r.textContent.includes("DELETED"))`)
	p.eval(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes("ci-pool")).querySelector(".overflow-trigger").click()`, nil)
	p.clickText("#view .overflow-item, .overflow-item", "Restore")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("ci-pool") && !r.textContent.includes("DELETED"))`)
	if got, err := svc.Projects.Locations.WorkloadIdentityPools.Get(parent + "ci-pool").Context(ctx).Do(); err != nil || got.State != "ACTIVE" {
		t.Errorf("after Restore the pool = %+v, %v", got, err)
	}
}
