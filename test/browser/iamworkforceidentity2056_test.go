//go:build browser

package browser

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	iam "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
)

// TestIAMWorkforceIdentityFederationPage (#2056): IAM & Admin > Workforce
// Identity Federation in Google's steps. Create pool makes a pool (with its
// session duration) and its first OIDC provider (issuer, client ID, the
// web sign-in response type, attribute mapping and condition); the list
// shows the pool; its page lists the provider; Add provider makes another;
// a deleted provider's page and a deleted pool's row offer Restore; the
// list, the create page and the pool's page pass the console's WCAG 2.1
// AA audit. Each is read back through the official
// google.golang.org/api/iam/v1 client.
func TestIAMWorkforceIdentityFederationPage(t *testing.T) {
	needService(t, "iam-workforce-identity")
	ctx := context.Background()
	svc, err := iam.NewService(ctx, option.WithEndpoint("http://"+endpoint(t, envResourceManager)+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	project := uniqueProject(t)
	id := fmt.Sprintf("staff-%d", time.Now().UnixNano()%1e9)
	name := "locations/global/workforcePools/" + id
	pools := svc.Locations.WorkforcePools

	p := open(t)
	p.navigate("/iam-admin/workforce-identity-pools/create?project=" + project)
	p.waitFor(`document.querySelector("#f-displayName") !== null`)
	var steps []string
	p.eval(`[...document.querySelectorAll(".create-step-title")].map((s) => s.textContent)`, &steps)
	if len(steps) != 3 || steps[0] != "Create a workforce pool" || !strings.HasPrefix(steps[1], "Add a provider to pool") || steps[2] != "Configure provider attributes" {
		t.Errorf("the create page's steps are %v", steps)
	}
	setField(p, "#view", "f-displayName", "Staff")
	setField(p, "#view", "f-poolId", id)
	setField(p, "#view", "f-sessionDuration", "7200s")
	p.clickText(".create-step-continue button", "Continue")
	setField(p, "#view", "f-providerType", "OpenID Connect (OIDC)")
	setField(p, "#view", "f-providerId", "staff-idp")
	setField(p, "#view", "f-issuerUri", "https://idp.example.test")
	setField(p, "#view", "f-clientId", "staff-console")
	setField(p, "#view", "f-responseType", "ID token")
	setField(p, "#view", "f-assertionClaims", "Only ID token claims")
	p.clickText(".create-step-continue button", "Continue")
	setField(p, "#view", "f-attributeMapping", "google.subject=assertion.sub\ngoogle.groups=assertion.groups")
	setField(p, "#view", "f-attributeCondition", "'staff' in google.groups")
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("` + id + `"))`)

	pool, err := pools.Get(name).Context(ctx).Do()
	if err != nil || pool.DisplayName != "Staff" || pool.State != "ACTIVE" || pool.SessionDuration != "7200s" || pool.Parent != "organizations/100000000001" {
		t.Fatalf("the new pool = %+v, %v", pool, err)
	}
	t.Cleanup(func() { _, _ = pools.Delete(name).Context(context.Background()).Do() })
	prov, err := pools.Providers.Get(name + "/providers/staff-idp").Context(ctx).Do()
	if err != nil || prov.Oidc == nil || prov.Oidc.ClientId != "staff-console" || prov.Oidc.WebSsoConfig == nil ||
		prov.Oidc.WebSsoConfig.ResponseType != "ID_TOKEN" || prov.AttributeCondition != "'staff' in google.groups" {
		t.Fatalf("the new provider = %+v, %v", prov, err)
	}

	// The pool's page lists the provider; Add provider makes another.
	p.navigate("/iam-admin/workforce-identity-pools/" + id + "?project=" + project)
	p.waitFor(`document.querySelector("#view [role=tab]") !== null`)
	p.clickText("#view [role=tab]", "Providers")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("staff-idp"))`)
	p.pageAction("Add provider", map[string]string{"providerType": "OpenID Connect (OIDC)", "providerId": "second-idp",
		"issuerUri": "https://idp2.example.test", "clientId": "second", "responseType": "ID token", "assertionClaims": "Only ID token claims",
		"attributeMapping": "google.subject=assertion.sub"}, "")
	if got, err := pools.Providers.Get(name + "/providers/second-idp").Context(ctx).Do(); err != nil || got.Oidc == nil || got.Oidc.ClientId != "second" {
		t.Fatalf("the added provider = %+v, %v", got, err)
	}

	// A deleted provider's page offers Restore.
	if _, err := pools.Providers.Delete(name + "/providers/staff-idp").Context(ctx).Do(); err != nil {
		t.Fatalf("delete the provider through the API: %v", err)
	}
	p.navigate("/iam-admin/workforce-identity-pools/" + id + "/staff-idp?project=" + project)
	where := p.findAction("Restore")
	if where == "" {
		t.Fatal("a deleted provider's page offers no Restore")
	}
	p.clickText(where, "Restore")
	p.waitFor(`[...document.querySelectorAll("#view")].some((v) => v.textContent.includes("ENABLED"))`)
	if got, err := pools.Providers.Get(name + "/providers/staff-idp").Context(ctx).Do(); err != nil || got.State != "ACTIVE" {
		t.Errorf("after Restore the provider = %+v, %v", got, err)
	}

	auditScreens(t, []a11yPage{
		screen("Workforce Identity Federation", "/iam-admin/workforce-identity-pools", project),
		screen("create a workforce pool", "/iam-admin/workforce-identity-pools/create", project),
		screen("a workforce pool", "/iam-admin/workforce-identity-pools/"+id, project),
	})

	// The pool's Delete leaves it DELETED, with Restore on its row.
	if _, err := pools.Delete(name).Context(ctx).Do(); err != nil {
		t.Fatalf("delete the pool through the API: %v", err)
	}
	p.navigate("/iam-admin/workforce-identity-pools?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("` + id + `") && r.textContent.includes("DELETED"))`)
	p.eval(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes("`+id+`")).querySelector(".overflow-trigger").click()`, nil)
	p.clickText("#view .overflow-item, .overflow-item", "Restore")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("` + id + `") && !r.textContent.includes("DELETED"))`)
	if got, err := pools.Get(name).Context(ctx).Do(); err != nil || got.State != "ACTIVE" {
		t.Errorf("after Restore the pool = %+v, %v", got, err)
	}
}
