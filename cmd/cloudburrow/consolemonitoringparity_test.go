package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"cloud.google.com/go/monitoring/dashboard/apiv1/dashboardpb"

	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

// The 2026-11-22 round (#2146): Create policy's Google steps (trigger,
// missing data, group by, subject line, closure notifications, autoclose,
// user labels) and Copy; Create uptime check's request options and its
// alert policy; Create group's criteria; Copy dashboard; Create snooze's
// start time and End now; Send test notification; incidents as Alerts.
func TestConsoleMonitoringGoogleRound(t *testing.T) {
	ctx := context.Background()
	admin, _, srv := monitoringAdminFixture(t)
	const project = "mon-round"
	policies := monitoringAlertingProvider{admin}

	steps := map[string]bool{}
	_, fields := policies.CreateForm()
	for _, f := range fields {
		steps[f.Section] = true
	}
	if !steps[monStepSeries] || !steps[monStepTrigger] || !steps[monStepNotify] || policies.CreateTitle() != "Create alerting policy" {
		t.Errorf("Create policy's steps = %v, title %q", steps, policies.CreateTitle())
	}
	id, err := policies.Create(ctx, project, map[string]string{"displayName": "Queue", "conditionType": "threshold",
		"metric": "custom.googleapis.com/q", "resource": "global", "filter": `metric.labels.env = "prod"`,
		"aligner": "ALIGN_MEAN", "alignmentPeriod": "300", "reducer": "REDUCE_SUM", "groupBy": "metric.labels.env",
		"trigger": "percent", "triggerPercent": "25", "comparison": "COMPARISON_LT", "threshold": "3", "duration": "120",
		"missingData": "EVALUATION_MISSING_DATA_ACTIVE", "subject": "Queue low", "notifyOnClose": "false", "autoClose": "3600",
		"userLabels": `{"team":"ops"}`, "severity": "ERROR"})
	if err != nil {
		t.Fatal(err)
	}
	pol, err := srv.GetAlertPolicy(ctx, &monitoringpb.GetAlertPolicyRequest{Name: "projects/" + project + "/alertPolicies/" + id})
	if err != nil {
		t.Fatal(err)
	}
	th := pol.GetConditions()[0].GetConditionThreshold()
	if th.GetTrigger().GetPercent() != 25 || th.GetEvaluationMissingData() != monitoringpb.AlertPolicy_Condition_EVALUATION_MISSING_DATA_ACTIVE ||
		strings.Join(th.GetAggregations()[0].GetGroupByFields(), ",") != "metric.labels.env" || pol.GetDocumentation().GetSubject() != "Queue low" ||
		len(pol.GetAlertStrategy().GetNotificationPrompts()) != 1 || pol.GetAlertStrategy().GetAutoClose().AsDuration() != time.Hour ||
		pol.GetUserLabels()["team"] != "ops" {
		t.Fatalf("the policy Create policy made = %v", pol)
	}
	if _, err := policies.Create(ctx, project, map[string]string{"displayName": "Bad", "metric": "custom.googleapis.com/q", "trigger": "percent", "triggerPercent": "150"}); err == nil {
		t.Error("a trigger of 150 percent was accepted")
	}
	// Copy opens Create policy holding the policy's settings.
	var copyHref string
	for _, a := range policies.DetailActions(ctx, project, []string{id}) {
		if a.ID == "copy" {
			copyHref = a.Href
		}
	}
	if copyHref != "/monitoring/alerting/create?from="+id {
		t.Errorf("the policy's Copy opens %q", copyHref)
	}
	cf, err := policies.CreateFormFrom(ctx, project, id)
	if err != nil {
		t.Fatal(err)
	}
	vals := map[string]string{}
	for _, f := range cf {
		vals[f.Name] = f.Default
	}
	if vals["displayName"] != "Copy of Queue" || vals["metric"] != "custom.googleapis.com/q" || vals["filter"] != `metric.labels.env = "prod"` ||
		vals["trigger"] != "percent" || vals["triggerPercent"] != "25" || vals["comparison"] != "COMPARISON_LT" || vals["autoClose"] != "3600" ||
		vals["notifyOnClose"] != "false" || vals["subject"] != "Queue low" || vals["alignmentPeriod"] != "300" {
		t.Fatalf("Copy's form = %v", vals)
	}
	cid, err := policies.Create(ctx, project, vals)
	if err != nil {
		t.Fatal(err)
	}
	cp, _ := srv.GetAlertPolicy(ctx, &monitoringpb.GetAlertPolicyRequest{Name: "projects/" + project + "/alertPolicies/" + cid})
	if cp.GetConditions()[0].GetConditionThreshold().GetFilter() != th.GetFilter() || cp.GetDisplayName() != "Copy of Queue" {
		t.Errorf("the copy = %v", cp)
	}

	// Uptime check with Google's request options and Create an alert.
	var mu sync.Mutex
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte("ok"))
	}))
	defer site.Close()
	port := site.URL[strings.LastIndex(site.URL, ":")+1:]
	uptime := monitoringUptimeProvider{admin}
	uid, err := uptime.Create(ctx, project, map[string]string{"displayName": "Site", "protocol": "HTTP", "host": "127.0.0.1", "port": port,
		"path": "/", "period": "60", "timeout": "5", "requestMethod": "POST", "contentType": "URL_ENCODED", "body": "a=1",
		"headers": `{"X-Probe":"yes"}`, "authUser": "u", "authPassword": "p", "contentMatching": "true", "matcher": "NOT_CONTAINS_STRING",
		"content": "down", "acceptedCodes": "STATUS_CLASS_ANY", "regions": "USA_OREGON, EUROPE, ASIA_PACIFIC",
		"createAlert": "true", "alertDuration": "300", "userLabels": `{"tier":"web"}`})
	if err != nil {
		t.Fatal(err)
	}
	uc, err := srv.GetUptimeCheckConfig(ctx, &monitoringpb.GetUptimeCheckConfigRequest{Name: "projects/" + project + "/uptimeCheckConfigs/" + uid})
	if err != nil {
		t.Fatal(err)
	}
	h := uc.GetHttpCheck()
	if h.GetRequestMethod() != monitoringpb.UptimeCheckConfig_HttpCheck_POST || string(h.GetBody()) != "a=1" || h.GetHeaders()["X-Probe"] != "yes" ||
		h.GetAuthInfo().GetUsername() != "u" || len(uc.GetSelectedRegions()) != 3 || uc.GetUserLabels()["tier"] != "web" ||
		uc.GetContentMatchers()[0].GetMatcher() != monitoringpb.UptimeCheckConfig_ContentMatcher_NOT_CONTAINS_STRING || len(h.GetAcceptedResponseStatusCodes()) != 1 {
		t.Fatalf("the uptime check = %v", uc)
	}
	pl, err := policies.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range pl.Items {
		if it.Fields["Display name"] == "Site uptime failure" {
			found = true
		}
	}
	if !found {
		t.Errorf("Create an alert made no policy: %v", pl.Items)
	}

	// Groups: criteria make the filter.
	groups := monitoringGroupsProvider{admin}
	gid, err := groups.Create(ctx, project, map[string]string{"displayName": "Global", "combine": "OR",
		"criteria": `[{"type":"resource_type","operator":"equals","value":"global"},{"type":"project","operator":"starts_with","value":"mon-"}]`})
	if err != nil {
		t.Fatal(err)
	}
	g, _ := srv.GetGroup(ctx, &monitoringpb.GetGroupRequest{Name: "projects/" + project + "/groups/" + gid})
	if g.GetFilter() != `resource.type = "global" OR resource.labels.project_id = starts_with("mon-")` {
		t.Errorf("the group's filter = %q", g.GetFilter())
	}
	if _, err := groups.Create(ctx, project, map[string]string{"displayName": "Empty"}); err == nil {
		t.Error("a group with no criteria and no filter was created")
	}

	// Copy dashboard.
	dashboards := monitoringDashboardsProvider{monitoringAdmin: admin}
	did, err := dashboards.Create(ctx, project, map[string]string{"displayName": "Shop",
		"json": `{"gridLayout":{"widgets":[{"title":"Sales","text":{"content":"hi"}}]}}`})
	if err != nil {
		t.Fatal(err)
	}
	if err := dashboards.ActAt(ctx, project, []string{did}, "copy", map[string]string{"displayName": "Shop copy"}); err != nil {
		t.Fatal(err)
	}
	dl, _ := srv.ListDashboards(ctx, &dashboardpb.ListDashboardsRequest{Parent: "projects/" + project})
	copied := false
	for _, d := range dl.GetDashboards() {
		if d.GetDisplayName() == "Shop copy" && len(dashboardWidgets(d)) == 1 && lastSegment(d.GetName()) != did {
			copied = true
		}
	}
	if !copied {
		t.Errorf("Copy dashboard made %v", dl.GetDashboards())
	}

	// Create snooze starting later: Cancel snooze ends it.
	snoozes := monitoringSnoozesProvider{admin}
	start := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	sid, err := snoozes.Create(ctx, project, map[string]string{"displayName": "Later", "policies": id, "start": start, "minutes": "30"})
	if err != nil {
		t.Fatal(err)
	}
	sl, _ := snoozes.List(ctx, project)
	row, _ := monRow(sl, sid)
	if row.Status != "upcoming" || len(snoozes.Actions(row)) != 1 || snoozes.Actions(row)[0].Label != "Cancel snooze" {
		t.Errorf("the upcoming snooze = %v, actions %v", row, snoozes.Actions(row))
	}

	// Send test notification posts a test incident to a local webhook.
	var bodies []map[string]any
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
	}))
	defer hook.Close()
	channels := monitoringChannelsProvider{admin}
	ch, err := channels.Create(ctx, project, map[string]string{"type": "webhook_tokenauth", "displayName": "Hook", "url": hook.URL + "/t"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := channels.ActAtResult(ctx, project, []string{ch}, "test", nil)
	if err != nil || res == nil || res.Items[0].Name != monitoring.DeliverySent {
		t.Fatalf("Send test notification = %+v, %v", res, err)
	}
	mu.Lock()
	n := len(bodies)
	mu.Unlock()
	if n != 1 {
		t.Errorf("the webhook got %d test notifications", n)
	}
	// Services: Google's Overview columns.
	services := monitoringServicesProvider{admin}
	if _, err := services.Create(ctx, project, map[string]string{"id": "checkout", "displayName": "Checkout"}); err != nil {
		t.Fatal(err)
	}
	svl, err := services.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := monRow(svl, "checkout"); r.Fields["Type"] != "Custom" || r.Fields["SLOs out of error budget"] != "0" || r.Fields["SLOs with firing alerts"] != "0" {
		t.Errorf("the Services row = %v", r.Fields)
	}
	if (monitoringIncidentsProvider{admin}).Title() != "Alerts" {
		t.Error("incidents are not listed as Alerts")
	}
}
