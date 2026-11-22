//go:build browser

package browser

import (
	"context"
	"net/http"
	"strings"
	"testing"

	cloudquotas "cloud.google.com/go/cloudquotas/apiv1"
	qpb "cloud.google.com/go/cloudquotas/apiv1/cloudquotaspb"
	"google.golang.org/api/option"
)

// TestQuotasAtTheOrganizationThroughThePage (#2102): with the organization
// chosen in the toolbar, IAM & Admin > Quotas & System Limits lists the
// organization's quotas (Workforce identity pools per organization at
// Google's 100, no project quota); Edit quotas submits an organization
// preference the official cloudquotas client reads back granted.
func TestQuotasAtTheOrganizationThroughThePage(t *testing.T) {
	needService(t, "serviceusage")
	rm := endpoint(t, envResourceManager)
	c, err := cloudquotas.NewRESTClient(context.Background(), option.WithEndpoint("http://"+rm), option.WithoutAuthentication(), option.WithHTTPClient(&http.Client{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	const org = "organizations/100000000001"
	const quota = "WORKFORCE-POOL-COUNT-per-organization"
	project := uniqueProject(t)
	p := open(t)
	p.navigate("/iam-admin/quotas?project=" + project + "&organizationId=100000000001")
	row := `[...document.querySelectorAll("#quotas-table tbody tr")].find((r) => r.textContent.includes("Workforce identity pools per organization"))`
	p.waitFor(row + ` !== undefined`)
	var text string
	p.eval(`document.querySelector("#quotas-table").textContent`, &text)
	if strings.Contains(text, "Service Account Count") {
		t.Errorf("the organization's page lists a project quota: %q", text)
	}
	p.eval(row+`.textContent`, &text)
	if !strings.Contains(text, "iam.googleapis.com") || !strings.Contains(text, "100") {
		t.Errorf("workforce pool row = %q", text)
	}
	t.Cleanup(func() {
		it := c.ListQuotaPreferences(context.Background(), &qpb.ListQuotaPreferencesRequest{Parent: org + "/locations/global", Filter: "quotaId=" + quota})
		for {
			q, err := it.Next()
			if err != nil {
				return
			}
			q.QuotaConfig.PreferredValue = 100
			_, _ = c.UpdateQuotaPreference(context.Background(), &qpb.UpdateQuotaPreferenceRequest{QuotaPreference: q,
				IgnoreSafetyChecks: []qpb.QuotaSafetyCheck{qpb.QuotaSafetyCheck_QUOTA_DECREASE_PERCENTAGE_TOO_HIGH}})
		}
	})
	p.clickExpr(row + `.querySelector("button")`)
	p.waitFor(`document.querySelector("#quota-edit") !== null`)
	p.setField("#quota-edit #f-value", "105")
	p.setField("#quota-edit #f-justification", "browser org")
	p.clickExpr(`document.querySelector('#quota-edit button[type="submit"]')`)
	p.waitFor(`document.querySelector("#quota-edit") === null && ` + row + `.textContent.includes("(default 100)")`)
	var pref *qpb.QuotaPreference
	it := c.ListQuotaPreferences(context.Background(), &qpb.ListQuotaPreferencesRequest{Parent: org + "/locations/global", Filter: "quotaId=" + quota})
	for {
		q, err := it.Next()
		if err != nil {
			break
		}
		pref = q
	}
	if pref == nil || pref.GetQuotaConfig().GetGrantedValue().GetValue() != 105 || pref.GetJustification() != "browser org" ||
		!strings.HasPrefix(pref.GetName(), org+"/locations/global/quotaPreferences/") {
		t.Fatalf("the organization preference the page made = %v", pref)
	}
}
