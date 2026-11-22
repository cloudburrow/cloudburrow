//go:build browser

package browser

import (
	"context"
	"strings"
	"testing"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/durationpb"

	monitoringsvc "github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

// TestMonitoringSummaryCriteriaLogsAndSnoozesThroughThePage (#2146, the
// rest): Alerting's Summary pane with the number of open alerts and See
// all alerts; Create group's Name and Region criteria matching a bucket
// made with the official Cloud Storage client, the group's page listing it
// by name and region; Create uptime check's Log check failures, read back
// with the official client; Create snooze's Alert policies menu, the
// policy picked by name. The changed screens pass the WCAG audit.
func TestMonitoringSummaryCriteriaLogsAndSnoozesThroughThePage(t *testing.T) {
	needService(t, "monitoring-alerting")
	opts := monitoringOpts(t)
	sc, _ := storageSDK(t)
	ctx := context.Background()
	project := uniqueProject(t)
	ac, err := monitoring.NewAlertPolicyClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	gc, err := monitoring.NewGroupClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer gc.Close()
	uc, err := monitoring.NewUptimeCheckClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	sn, err := monitoring.NewSnoozeClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer sn.Close()

	bucketName := project + "-grp-eu"
	b := sc.Bucket(bucketName)
	if err := b.Create(ctx, project, &storage.BucketAttrs{Location: "EUROPE-WEST2", Labels: map[string]string{"team": "web"}}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = b.Delete(context.Background()) })
	pol, err := ac.CreateAlertPolicy(ctx, &monitoringpb.CreateAlertPolicyRequest{Name: "projects/" + project, AlertPolicy: &monitoringpb.AlertPolicy{
		DisplayName: "Picker policy", Combiner: monitoringpb.AlertPolicy_OR,
		Conditions: []*monitoringpb.AlertPolicy_Condition{{DisplayName: "absent", Condition: &monitoringpb.AlertPolicy_Condition_ConditionAbsent{
			ConditionAbsent: &monitoringpb.AlertPolicy_Condition_MetricAbsence{Filter: `metric.type = "custom.googleapis.com/browser/picker" AND resource.type = "global"`,
				Duration: durationpb.New(time.Hour)}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	pid := pol.GetName()[strings.LastIndex(pol.GetName(), "/")+1:]

	p := open(t)
	// The Summary pane.
	p.navigate("/monitoring/alerting?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view .listing-summary h2")].some((h) => h.textContent.trim() === "Summary")`)
	var summary string
	p.eval(`[...document.querySelector("#view .listing-summary").querySelectorAll("dt, dd")].map((e) => e.textContent.trim()).join("|")`, &summary)
	if summary != "Open alerts|0|Alerts|See all alerts" {
		t.Errorf("the Summary pane reads %q", summary)
	}

	// Create group: Name contains the bucket's, Region equals its.
	p.navigate("/monitoring/groups/create?project=" + project)
	p.waitFor(`document.querySelector("#f-displayName") !== null && document.querySelector("#f-criteria") !== null`)
	var types string
	p.eval(`(() => { const b = document.querySelector("form.create-page .group-add"); b.click(); b.click(); return true; })()`, nil)
	p.waitFor(`document.querySelectorAll("form.create-page .group-row").length === 2`)
	p.eval(`[...document.querySelector("#view .group-row select").options].map((o) => o.textContent).join("|")`, &types)
	if types != "Name|Tag|Region|Resource Type|Cloud Project" {
		t.Errorf("the criteria types are %s", types)
	}
	var ok bool
	p.eval(`(() => {
		const rows = [...document.querySelectorAll("#view .group-row")];
		const set = (c, v) => { c.value = v; c.dispatchEvent(new Event("input", { bubbles: true })); c.dispatchEvent(new Event("change", { bubbles: true })); };
		const fill = (r, type, op, value) => { const s = r.querySelectorAll("select"); set(s[0], type); set(s[1], op); set(r.querySelector("input[type=text]"), value); };
		fill(rows[0], "name", "contains", "grp-eu");
		fill(rows[1], "region", "equals", "europe-west2");
		return true;
	})()`, &ok)
	p.submitForm(map[string]string{"displayName": "EU buckets"}, "displayName")
	var gname string
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("EU buckets"))`)
	it := gc.ListGroups(ctx, &monitoringpb.ListGroupsRequest{Name: "projects/" + project})
	for {
		g, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if g.GetDisplayName() == "EU buckets" {
			gname = g.GetName()
			if want := `resource.metadata.name = has_substring("grp-eu") AND resource.metadata.region = "europe-west2"`; g.GetFilter() != want {
				t.Errorf("the group's filter = %q, want %q", g.GetFilter(), want)
			}
		}
	}
	if gname == "" {
		t.Fatal("Create group made no group")
	}
	m, err := gc.ListGroupMembers(ctx, &monitoringpb.ListGroupMembersRequest{Name: gname}).Next()
	if err != nil || m.GetType() != "gcs_bucket" || m.GetLabels()["bucket_name"] != bucketName {
		t.Errorf("the group's member = %v, %v", m, err)
	}
	gid := gname[strings.LastIndex(gname, "/")+1:]
	p.navigate("/monitoring/groups/" + gid + "?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("` + bucketName + `") && r.textContent.includes("europe-west2"))`)

	// Create uptime check: Log check failures, on by default.
	p.navigate("/monitoring/uptime/create?project=" + project)
	p.waitFor(`document.querySelector("#f-logCheckFailures") !== null`)
	var logged bool
	p.eval(`document.querySelector("#f-logCheckFailures").checked`, &logged)
	if !logged {
		t.Error("Log check failures is not on by default")
	}
	p.submitForm(map[string]string{"protocol": "TCP", "host": "127.0.0.1", "port": "1", "createAlert": "false", "displayName": "Logged check"}, "displayName")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Logged check"))`)
	var found bool
	uit := uc.ListUptimeCheckConfigs(ctx, &monitoringpb.ListUptimeCheckConfigsRequest{Parent: "projects/" + project})
	for {
		c, err := uit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if c.GetDisplayName() == "Logged check" {
			found = true
			if !monitoringsvc.LogCheckFailures(c) {
				t.Error("the check made does not log its failures")
			}
		}
	}
	if !found {
		t.Fatal("Create uptime check made no check")
	}

	// Create snooze: the Alert policies menu.
	p.navigate("/monitoring/snoozes/create?project=" + project)
	p.waitFor(`document.querySelector("#f-policies.multiselect-button") !== null`)
	p.eval(`document.querySelector("#f-policies").click(), true`, nil)
	p.waitFor(`document.querySelector("#f-policies").getAttribute("aria-expanded") === "true" && document.activeElement === document.querySelector("#f-policies-filter")`)
	p.waitFor(`[...document.querySelectorAll("#view .multiselect-options label")].some((l) => l.textContent.includes("Picker policy"))`)
	p.eval(`[...document.querySelectorAll("#view .multiselect-options label")].find((l) => l.textContent.includes("Picker policy")).querySelector("input").click(), true`, nil)
	p.waitFor(`document.querySelector("#f-policies").textContent.trim() === "1 selected" && [...document.querySelectorAll("#view .multiselect-chips li")].some((c) => c.textContent.includes("Picker policy"))`)
	// Keyboard: Escape closes the menu and gives focus back to its button.
	p.eval(`document.querySelector("#f-policies-filter").dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true })), true`, nil)
	p.waitFor(`document.querySelector("#f-policies").getAttribute("aria-expanded") === "false" && document.activeElement === document.querySelector("#f-policies")`)
	p.submitForm(map[string]string{"displayName": "Quiet hour", "minutes": "60"}, "displayName")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Quiet hour"))`)
	sit := sn.ListSnoozes(ctx, &monitoringpb.ListSnoozesRequest{Parent: "projects/" + project})
	snoozed := false
	for {
		s, err := sit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if s.GetDisplayName() == "Quiet hour" {
			snoozed = len(s.GetCriteria().GetPolicies()) == 1 && strings.HasSuffix(s.GetCriteria().GetPolicies()[0], "/alertPolicies/"+pid)
		}
	}
	if !snoozed {
		t.Error("Create snooze did not snooze the picked policy")
	}

	auditScreens(t, []a11yPage{
		screen("Alerting summary", "/monitoring/alerting", project),
		screen("Create monitoring group", "/monitoring/groups/create", project),
		screen("Monitoring group", "/monitoring/groups/"+gid, project),
		screen("Create uptime check", "/monitoring/uptime/create", project),
		screen("Create snooze", "/monitoring/snoozes/create", project),
	})
}
