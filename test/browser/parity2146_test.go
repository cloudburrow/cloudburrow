//go:build browser

package browser

import (
	"context"
	"strings"
	"testing"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/api/iterator"
)

// TestMonitoringGoogleRoundThroughThePage (#2146): Alerting's tabs as
// Google's page holds them (Policies, Alerts, Snoozes); Create alerting
// policy in Google's three steps, its trigger, subject line and autoclose
// read back with the official client; the policy's Copy opening Create
// policy with its settings, which makes the copy; Create uptime check's
// four steps; a channel's Send test notification; Create group's criteria.
// The changed screens pass the WCAG audit.
func TestMonitoringGoogleRoundThroughThePage(t *testing.T) {
	needService(t, "monitoring-alerting")
	opts := monitoringOpts(t)
	ctx := context.Background()
	ac, err := monitoring.NewAlertPolicyClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	project := uniqueProject(t)
	policies := func() map[string]*monitoringpb.AlertPolicy {
		out := map[string]*monitoringpb.AlertPolicy{}
		it := ac.ListAlertPolicies(ctx, &monitoringpb.ListAlertPoliciesRequest{Name: "projects/" + project})
		for {
			pol, err := it.Next()
			if err == iterator.Done {
				return out
			}
			if err != nil {
				t.Fatal(err)
			}
			out[pol.GetDisplayName()] = pol
		}
	}

	p := open(t)
	p.navigate("/monitoring/alerting?project=" + project)
	var tabs string
	p.waitFor(`document.querySelectorAll("nav.page-tabs a").length === 3`)
	p.eval(`[...document.querySelectorAll("nav.page-tabs a")].map((a) => a.textContent.trim()).join("|")`, &tabs)
	if tabs != "Policies|Alerts|Snoozes" {
		t.Errorf("Alerting's tabs are %s", tabs)
	}

	p.clickText("#view button", "Create policy")
	p.waitFor(`document.querySelector("#f-trigger") !== null`)
	var steps, title string
	p.eval(`[...document.querySelectorAll("#view .create-step-title")].map((h) => h.textContent.trim()).join("|")`, &steps)
	p.eval(`document.querySelector("#view h1").textContent.trim()`, &title)
	if steps != "Select the time series|Configure trigger|Notifications and name" || title != "Create alerting policy" {
		t.Errorf("Create policy is %q in steps %s", title, steps)
	}
	p.submitForm(map[string]string{"conditionType": "threshold", "metric": "custom.googleapis.com/browser/round", "resource": "global",
		"trigger": "count", "triggerCount": "2", "comparison": "COMPARISON_GT", "threshold": "5", "duration": "60",
		"subject": "Round subject", "autoClose": "86400", "displayName": "Round policy"}, "displayName")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Round policy"))`)
	pol := policies()["Round policy"]
	if pol == nil || pol.GetConditions()[0].GetConditionThreshold().GetTrigger().GetCount() != 2 ||
		pol.GetDocumentation().GetSubject() != "Round subject" || pol.GetAlertStrategy().GetAutoClose().AsDuration().Hours() != 24 ||
		len(pol.GetAlertStrategy().GetNotificationPrompts()) != 2 {
		t.Fatalf("the policy Create alerting policy made = %v", pol)
	}
	id := pol.GetName()[strings.LastIndex(pol.GetName(), "/")+1:]

	// Copy: Create policy holding the policy's settings.
	p.navigate("/monitoring/alerting/" + id + "?project=" + project)
	p.clickText("#view button, #view a", "Copy")
	p.waitFor(`location.pathname === "/monitoring/alerting/create" && document.querySelector("#f-displayName") !== null && document.querySelector("#f-displayName").value === "Copy of Round policy"`)
	var subject string
	p.eval(`document.querySelector("#f-subject").value`, &subject)
	if subject != "Round subject" {
		t.Errorf("the copy's subject line is %q", subject)
	}
	p.submitForm(map[string]string{}, "displayName")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Copy of Round policy"))`)
	if cp := policies()["Copy of Round policy"]; cp == nil || cp.GetConditions()[0].GetConditionThreshold().GetTrigger().GetCount() != 2 {
		t.Errorf("the copy = %v", cp)
	}

	// Create uptime check's steps.
	p.navigate("/monitoring/uptime?project=" + project)
	p.clickText("#view button", "Create uptime check")
	p.waitFor(`document.querySelector("#f-createAlert") !== null`)
	p.eval(`[...document.querySelectorAll("#view .create-step-title")].map((h) => h.textContent.trim()).join("|")`, &steps)
	if steps != "Target|Response validation|Alert & notification|Finalize" {
		t.Errorf("Create uptime check's steps are %s", steps)
	}

	// Create channel's per-type fields and Send test notification.
	p.navigate("/monitoring/alerting/channels?project=" + project)
	p.clickText("#view button", "Create channel")
	p.waitFor(`document.querySelector("#f-type") !== null`)
	p.setField("#f-type", "slack")
	p.waitFor(`document.querySelector("#f-channel_name").closest(".form-field, .field, label, div").hidden === false && document.querySelector("#f-url").closest(".form-field, .field, label, div").hidden`)
	p.submitForm(map[string]string{"type": "email", "displayName": "Round mail", "email_address": "ops@example.com"}, "type")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Round mail"))`)
	p.clickExpr(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes("Round mail")).querySelector("a")`)
	p.clickText("#view button", "Send test notification")
	p.waitFor(`document.querySelector("#view").textContent.includes("recorded") || document.querySelector("#action-result-table") !== null`)

	auditScreens(t, []a11yPage{
		screen("Alerting policies", "/monitoring/alerting", project),
		screen("Alerting alerts", "/monitoring/alerting/incidents", project),
		screen("Create alerting policy", "/monitoring/alerting/create", project),
		screen("Copy alerting policy", "/monitoring/alerting/create?from="+id, project),
		screen("Create uptime check", "/monitoring/uptime/create", project),
		screen("Create monitoring group", "/monitoring/groups/create", project),
		screen("Create snooze", "/monitoring/snoozes/create", project),
	})
}
