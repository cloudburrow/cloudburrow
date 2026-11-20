package main

// Cloud Monitoring's Alerting, Uptime checks, Dashboards, Groups, Services
// and Snoozes pages (#1964), over the Monitoring API the CLI process
// serves, called in process:
//
//   - Alerting (/monitoring/alerting): alert policies (Create policy with a
//     metric-threshold, metric-absence or log-match condition and its
//     notification channels; Enable, Disable, Snooze, Delete), Incidents
//     (/monitoring/alerting/incidents: Acknowledge, Close) and
//     Notification channels (/monitoring/alerting/channels: Create channel
//     of a webhook, Pub/Sub, email or SMS type; Send verification code,
//     Verify, Enable, Disable, Delete; the channel's recent deliveries);
//   - Uptime checks (/monitoring/uptime): Create uptime check, Edit, Delete,
//     and each check's recent results and latency chart;
//   - Dashboards (/monitoring/dashboards): Create dashboard from its JSON,
//     Edit JSON, Delete, and a dashboard's page drawing each XY chart
//     widget's time series;
//   - Groups, Services (SLOs) and Snoozes, each with create and delete.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"cloud.google.com/go/monitoring/dashboard/apiv1/dashboardpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

// monitoringAdmin is what the configuration pages share: the server.
type monitoringAdmin struct {
	api func() *monitoring.Server
}

// monitoringServerOf is the instance's Monitoring server, or nil before it
// starts.
func monitoringServerOf(svc *monitoringService) func() *monitoring.Server {
	return func() *monitoring.Server {
		if svc == nil {
			return nil
		}
		return svc.API()
	}
}

func (a monitoringAdmin) server() (*monitoring.Server, error) {
	if a.api == nil {
		return nil, errors.New(monitoringNotStarted)
	}
	s := a.api()
	if s == nil {
		return nil, errors.New(monitoringNotStarted)
	}
	return s, nil
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format(time.RFC3339)
}

func fmtTS(t *timestamppb.Timestamp) string {
	if t == nil {
		return "—"
	}
	return fmtTime(t.AsTime())
}

// seconds reads a number of seconds from a form value, def when empty.
func seconds(field, v string, def int64) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Duration(def) * time.Second, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s %q is not a number of seconds", field, v)
	}
	return time.Duration(n) * time.Second, nil
}

// ---- Alerting: policies ----

type monitoringAlertingProvider struct{ monitoringAdmin }

func (monitoringAlertingProvider) ID() string    { return "monitoring-alerting" }
func (monitoringAlertingProvider) Title() string { return "Alerting" }

func (p monitoringAlertingProvider) policies(ctx context.Context, s *monitoring.Server, project string) ([]*monitoringpb.AlertPolicy, error) {
	var out []*monitoringpb.AlertPolicy
	req := &monitoringpb.ListAlertPoliciesRequest{Name: "projects/" + project, PageSize: 1000}
	for range 100 {
		resp, err := s.ListAlertPolicies(ctx, req)
		if err != nil {
			return nil, err
		}
		out = append(out, resp.GetAlertPolicies()...)
		if req.PageToken = resp.GetNextPageToken(); req.PageToken == "" {
			break
		}
	}
	return out, nil
}

func conditionSummary(c *monitoringpb.AlertPolicy_Condition) string {
	switch {
	case c.GetConditionThreshold() != nil:
		t := c.GetConditionThreshold()
		return fmt.Sprintf("%s: %s %s %g for %s", c.GetDisplayName(), t.GetFilter(), t.GetComparison(), t.GetThresholdValue(), t.GetDuration().AsDuration())
	case c.GetConditionAbsent() != nil:
		a := c.GetConditionAbsent()
		return fmt.Sprintf("%s: %s absent for %s", c.GetDisplayName(), a.GetFilter(), a.GetDuration().AsDuration())
	case c.GetConditionMatchedLog() != nil:
		return fmt.Sprintf("%s: log entries matching %s", c.GetDisplayName(), c.GetConditionMatchedLog().GetFilter())
	}
	return c.GetDisplayName()
}

func enabledText(b *wrapperspb.BoolValue) string {
	if b == nil || b.GetValue() {
		return "Enabled"
	}
	return "Disabled"
}

func (p monitoringAlertingProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Display name", "Enabled", "Severity", "Conditions", "Notification channels", "Open incidents"},
		NameColumn: "Policy", Noun: "alert policies", RowsOpenable: true,
		Note: "Policies are evaluated every 15 seconds over this instance's time series and Cloud Logging; notifications reach only local webhooks and this instance's Pub/Sub, never email or SMS."}
	s, err := p.server()
	if err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		out.Prompt = "Alerting lists one project's alert policies. Choose one in the toolbar."
		return out, nil
	}
	ps, err := p.policies(ctx, s, project)
	if err != nil {
		return console.Listing{}, err
	}
	open := map[string]int{}
	for _, in := range s.Incidents(project) {
		if in.State == monitoring.IncidentOpen {
			open[in.Policy]++
		}
	}
	for _, pol := range ps {
		var conds, chans []string
		for _, c := range pol.GetConditions() {
			conds = append(conds, c.GetDisplayName())
		}
		for _, c := range pol.GetNotificationChannels() {
			chans = append(chans, lastSegment(c))
		}
		out.Items = append(out.Items, console.Resource{Name: lastSegment(pol.GetName()), Status: strings.ToLower(enabledText(pol.GetEnabled())),
			Fields: map[string]string{"Display name": pol.GetDisplayName(), "Enabled": enabledText(pol.GetEnabled()),
				"Severity": orDash(strings.TrimPrefix(pol.GetSeverity().String(), "SEVERITY_UNSPECIFIED")), "Conditions": orDash(strings.Join(conds, "; ")),
				"Notification channels": orDash(strings.Join(chans, ", ")), "Open incidents": strconv.Itoa(open[pol.GetName()])}})
	}
	out.Total = len(out.Items)
	return out, nil
}

var policyComparisons = []string{"COMPARISON_GT", "COMPARISON_GE", "COMPARISON_LT", "COMPARISON_LE", "COMPARISON_EQ", "COMPARISON_NE"}

func (monitoringAlertingProvider) CreateForm() (string, []console.Field) {
	return "Create policy", []console.Field{
		{Name: "displayName", Label: "Alert policy name", Type: "text", Required: true, Section: "Policy"},
		{Name: "severity", Label: "Severity", Type: "select", Options: []string{"SEVERITY_UNSPECIFIED", "CRITICAL", "ERROR", "WARNING"},
			Default: "SEVERITY_UNSPECIFIED", Section: "Policy"},
		{Name: "documentation", Label: "Documentation", Type: "textarea", Section: "Policy", Help: "Markdown sent with each notification."},
		{Name: "conditionType", Label: "Condition type", Type: "select", Options: []string{"threshold", "absence", "log-match"}, Default: "threshold",
			Section: "Condition", Help: "Metric threshold, metric absence, or a match on log entries."},
		{Name: "conditionName", Label: "Condition name", Type: "text", Section: "Condition", Default: "Condition"},
		{Name: "metric", Label: "Metric type", Type: "text", Section: "Condition", Help: "Threshold and absence: such as custom.googleapis.com/shop/sales."},
		{Name: "resource", Label: "Resource type", Type: "text", Section: "Condition", Help: "Such as global; empty for any."},
		{Name: "filter", Label: "Filter", Type: "text", Section: "Condition", Help: `Extra terms, such as metric.labels.env = "prod".`},
		{Name: "aligner", Label: "Aligner", Type: "select", Options: explorerAligners, Default: "ALIGN_NONE", Section: "Condition"},
		{Name: "alignmentPeriod", Label: "Alignment period (seconds)", Type: "number", Default: "60", Section: "Condition"},
		{Name: "reducer", Label: "Reducer", Type: "select", Options: explorerReducers, Default: "REDUCE_NONE", Section: "Condition"},
		{Name: "comparison", Label: "Comparison", Type: "select", Options: policyComparisons, Default: "COMPARISON_GT", Section: "Condition"},
		{Name: "threshold", Label: "Threshold", Type: "number", Default: "0", Section: "Condition"},
		{Name: "duration", Label: "Duration (seconds)", Type: "number", Default: "0", Section: "Condition",
			Help: "How long the condition must hold: whole minutes; at least 120 for absence."},
		{Name: "logFilter", Label: "Log filter", Type: "text", Section: "Condition", Help: `Log match: a Logging filter, such as severity>=ERROR.`},
		{Name: "rateLimit", Label: "Notification rate limit (seconds)", Type: "number", Default: "300", Section: "Condition",
			Help: "Log match: at most one notification per period."},
		{Name: "channels", Label: "Notification channels", Type: "text", Section: "Notifications",
			Help: "Channel IDs from Notification channels, comma separated."},
	}
}

// policyFromForm is the policy a Create policy form describes.
func policyFromForm(project string, values map[string]string) (*monitoringpb.AlertPolicy, error) {
	name := strings.TrimSpace(values["displayName"])
	if name == "" {
		return nil, errors.New("an alert policy needs a name")
	}
	cname := strings.TrimSpace(values["conditionName"])
	if cname == "" {
		cname = "Condition"
	}
	pol := &monitoringpb.AlertPolicy{DisplayName: name, Combiner: monitoringpb.AlertPolicy_OR, Enabled: wrapperspb.Bool(true)}
	if sev, ok := monitoringpb.AlertPolicy_Severity_value[values["severity"]]; ok {
		pol.Severity = monitoringpb.AlertPolicy_Severity(sev)
	}
	if d := strings.TrimSpace(values["documentation"]); d != "" {
		pol.Documentation = &monitoringpb.AlertPolicy_Documentation{Content: d, MimeType: "text/markdown"}
	}
	cond := &monitoringpb.AlertPolicy_Condition{DisplayName: cname}
	dur, err := seconds("duration", values["duration"], 0)
	if err != nil {
		return nil, err
	}
	kind := values["conditionType"]
	if kind == "" {
		kind = "threshold"
	}
	switch kind {
	case "threshold", "absence":
		filter, err := explorerFilter(values)
		if err != nil {
			return nil, err
		}
		var aggs []*monitoringpb.Aggregation
		if al := values["aligner"]; al != "" && al != "ALIGN_NONE" {
			period, err := seconds("alignment period", values["alignmentPeriod"], 60)
			if err != nil {
				return nil, err
			}
			a, ok := monitoringpb.Aggregation_Aligner_value[al]
			if !ok {
				return nil, fmt.Errorf("aligner %q is not an aligner", al)
			}
			agg := &monitoringpb.Aggregation{AlignmentPeriod: durationpb.New(period), PerSeriesAligner: monitoringpb.Aggregation_Aligner(a)}
			if r, ok := monitoringpb.Aggregation_Reducer_value[values["reducer"]]; ok {
				agg.CrossSeriesReducer = monitoringpb.Aggregation_Reducer(r)
			}
			aggs = append(aggs, agg)
		}
		if kind == "absence" {
			cond.Condition = &monitoringpb.AlertPolicy_Condition_ConditionAbsent{ConditionAbsent: &monitoringpb.AlertPolicy_Condition_MetricAbsence{
				Filter: filter, Aggregations: aggs, Duration: durationpb.New(dur)}}
			break
		}
		cmp, ok := monitoringpb.ComparisonType_value[values["comparison"]]
		if !ok {
			return nil, fmt.Errorf("comparison %q is not one of %s", values["comparison"], strings.Join(policyComparisons, ", "))
		}
		threshold := 0.0
		if v := strings.TrimSpace(values["threshold"]); v != "" {
			if threshold, err = strconv.ParseFloat(v, 64); err != nil || math.IsNaN(threshold) {
				return nil, fmt.Errorf("threshold %q is not a number", v)
			}
		}
		cond.Condition = &monitoringpb.AlertPolicy_Condition_ConditionThreshold{ConditionThreshold: &monitoringpb.AlertPolicy_Condition_MetricThreshold{
			Filter: filter, Aggregations: aggs, Comparison: monitoringpb.ComparisonType(cmp), ThresholdValue: threshold, Duration: durationpb.New(dur)}}
	case "log-match":
		lf := strings.TrimSpace(values["logFilter"])
		if lf == "" {
			return nil, errors.New("a log match condition needs a log filter")
		}
		rate, err := seconds("notification rate limit", values["rateLimit"], 300)
		if err != nil {
			return nil, err
		}
		cond.Condition = &monitoringpb.AlertPolicy_Condition_ConditionMatchedLog{ConditionMatchedLog: &monitoringpb.AlertPolicy_Condition_LogMatch{Filter: lf}}
		pol.AlertStrategy = &monitoringpb.AlertPolicy_AlertStrategy{NotificationRateLimit: &monitoringpb.AlertPolicy_AlertStrategy_NotificationRateLimit{Period: durationpb.New(rate)}}
	default:
		return nil, fmt.Errorf("condition type %q is not threshold, absence or log-match", kind)
	}
	pol.Conditions = []*monitoringpb.AlertPolicy_Condition{cond}
	for _, c := range splitList(values["channels"]) {
		if !strings.HasPrefix(c, "projects/") {
			c = "projects/" + project + "/notificationChannels/" + c
		}
		pol.NotificationChannels = append(pol.NotificationChannels, c)
	}
	return pol, nil
}

func (p monitoringAlertingProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s, err := p.server()
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", errors.New("choose a project before creating a policy")
	}
	pol, err := policyFromForm(project, values)
	if err != nil {
		return "", err
	}
	got, err := s.CreateAlertPolicy(ctx, &monitoringpb.CreateAlertPolicyRequest{Name: "projects/" + project, AlertPolicy: pol})
	if err != nil {
		return "", err
	}
	return lastSegment(got.GetName()), nil
}

func (p monitoringAlertingProvider) Delete(ctx context.Context, project, name string) error {
	s, err := p.server()
	if err != nil {
		return err
	}
	_, err = s.DeleteAlertPolicy(ctx, &monitoringpb.DeleteAlertPolicyRequest{Name: "projects/" + project + "/alertPolicies/" + name})
	return err
}

func (monitoringAlertingProvider) DeleteLabel() string { return "Delete policy" }

func (p monitoringAlertingProvider) Actions(r console.Resource) []console.Action {
	if r.Fields["Enabled"] == "Disabled" {
		return []console.Action{{ID: "enable", Label: "Enable"}}
	}
	return []console.Action{{ID: "disable", Label: "Disable"}}
}

func (p monitoringAlertingProvider) Act(ctx context.Context, project, name, action string) error {
	return p.ActAt(ctx, project, []string{name}, action, nil)
}

func (p monitoringAlertingProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	s, err := p.server()
	if err != nil || len(path) != 1 {
		return nil
	}
	pol, err := s.GetAlertPolicy(ctx, &monitoringpb.GetAlertPolicyRequest{Name: "projects/" + project + "/alertPolicies/" + path[0]})
	if err != nil {
		return nil
	}
	acts := []console.Action{{ID: "disable", Label: "Disable"}}
	if enabledText(pol.GetEnabled()) == "Disabled" {
		acts = []console.Action{{ID: "enable", Label: "Enable"}}
	}
	return append(acts, console.Action{ID: "snooze", Label: "Snooze", Fields: []console.Field{
		{Name: "displayName", Label: "Snooze name", Type: "text", Required: true, Default: "Snooze " + pol.GetDisplayName()},
		{Name: "minutes", Label: "Duration (minutes)", Type: "number", Required: true, Default: "60"},
	}})
}

func (p monitoringAlertingProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	s, err := p.server()
	if err != nil {
		return err
	}
	if len(path) != 1 {
		return fmt.Errorf("no action at %v", path)
	}
	name := "projects/" + project + "/alertPolicies/" + path[0]
	switch action {
	case "enable", "disable":
		_, err = s.UpdateAlertPolicy(ctx, &monitoringpb.UpdateAlertPolicyRequest{
			AlertPolicy: &monitoringpb.AlertPolicy{Name: name, Enabled: wrapperspb.Bool(action == "enable")},
			UpdateMask:  &fieldmaskpb.FieldMask{Paths: []string{"enabled"}}})
		return err
	case "snooze":
		mins, err := strconv.Atoi(strings.TrimSpace(values["minutes"]))
		if err != nil || mins <= 0 {
			return fmt.Errorf("duration %q is not a number of minutes", values["minutes"])
		}
		now := time.Now()
		_, err = s.CreateSnooze(ctx, &monitoringpb.CreateSnoozeRequest{Parent: "projects/" + project, Snooze: &monitoringpb.Snooze{
			DisplayName: strings.TrimSpace(values["displayName"]), Criteria: &monitoringpb.Snooze_Criteria{Policies: []string{name}},
			Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(now), EndTime: timestamppb.New(now.Add(time.Duration(mins) * time.Minute))}}})
		return err
	}
	return fmt.Errorf("unknown action %q on an alert policy", action)
}

func incidentListing(ins []monitoring.Incident) console.Listing {
	out := console.Listing{Columns: []string{"State", "Policy", "Condition", "Resource", "Started", "Ended", "Summary"},
		NameColumn: "Incident", Noun: "incidents", AlwaysStatus: true}
	for _, in := range ins {
		state := in.State
		if in.Acknowledged && in.State == monitoring.IncidentOpen {
			state = "acknowledged"
		}
		out.Items = append(out.Items, console.Resource{Name: in.ID, Status: state, Link: "/monitoring/alerting/incidents/" + in.ID,
			Fields: map[string]string{"State": state, "Policy": in.PolicyName, "Condition": in.ConditionName,
				"Resource": orDash(strings.TrimSpace(in.ResourceType + " " + labelsText(in.ResourceLabels))),
				"Started":  fmtTime(in.Started), "Ended": fmtTime(in.Ended), "Summary": in.Summary}})
	}
	out.Total = len(out.Items)
	return out
}

func (p monitoringAlertingProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	s, err := p.server()
	if err != nil {
		return console.Detail{}, err
	}
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	pol, err := s.GetAlertPolicy(ctx, &monitoringpb.GetAlertPolicyRequest{Name: "projects/" + project + "/alertPolicies/" + path[0]})
	if err != nil {
		return console.Detail{}, err
	}
	var chans []string
	for _, c := range pol.GetNotificationChannels() {
		chans = append(chans, lastSegment(c))
	}
	d := console.Detail{Title: pol.GetDisplayName(), Summary: []console.Property{
		{Label: "Name", Value: pol.GetName()}, {Label: "Enabled", Value: enabledText(pol.GetEnabled())},
		{Label: "Severity", Value: pol.GetSeverity().String()}, {Label: "Combiner", Value: pol.GetCombiner().String()},
		{Label: "Notification channels", Value: orDash(strings.Join(chans, ", "))},
		{Label: "Created", Value: fmtTS(pol.GetCreationRecord().GetMutateTime())},
		{Label: "Last changed", Value: fmtTS(pol.GetMutationRecord().GetMutateTime())},
	}}
	conds := console.Listing{Columns: []string{"Condition"}, NameColumn: "Name", Noun: "conditions"}
	for _, c := range pol.GetConditions() {
		conds.Items = append(conds.Items, console.Resource{Name: c.GetDisplayName(), Fields: map[string]string{"Condition": conditionSummary(c)}})
	}
	conds.Total = len(conds.Items)
	var mine []monitoring.Incident
	for _, in := range s.Incidents(project) {
		if in.Policy == pol.GetName() {
			mine = append(mine, in)
		}
	}
	snz := console.Listing{Columns: []string{"Display name", "Start", "End"}, NameColumn: "Snooze", Noun: "snoozes"}
	if resp, err := s.ListSnoozes(ctx, &monitoringpb.ListSnoozesRequest{Parent: "projects/" + project, PageSize: 1000}); err == nil {
		for _, sn := range resp.GetSnoozes() {
			for _, pn := range sn.GetCriteria().GetPolicies() {
				if pn == pol.GetName() {
					snz.Items = append(snz.Items, console.Resource{Name: lastSegment(sn.GetName()), Fields: map[string]string{
						"Display name": sn.GetDisplayName(), "Start": fmtTS(sn.GetInterval().GetStartTime()), "End": fmtTS(sn.GetInterval().GetEndTime())}})
				}
			}
		}
	}
	snz.Total = len(snz.Items)
	b, _ := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(pol)
	d.Sections = []console.Section{
		{ID: "conditions", Label: "Conditions", Listing: conds},
		{ID: "incidents", Label: "Incidents", Listing: incidentListing(mine)},
		{ID: "snoozes", Label: "Snoozes", Listing: snz},
		{ID: "json", Label: "JSON", Kind: console.KindText, Text: string(b)},
	}
	return d, nil
}

// ---- Alerting: incidents ----

type monitoringIncidentsProvider struct{ monitoringAdmin }

func (monitoringIncidentsProvider) ID() string    { return "monitoring-incidents" }
func (monitoringIncidentsProvider) Title() string { return "Incidents" }

func (p monitoringIncidentsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := p.server()
	if err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		return console.Listing{Prompt: "Incidents are one project's. Choose one in the toolbar."}, nil
	}
	out := incidentListing(s.Incidents(project))
	out.RowsOpenable = true
	for i := range out.Items {
		out.Items[i].Link = ""
	}
	out.Note = "An incident opens for each time series that meets a policy's condition, and closes when it stops meeting it."
	return out, nil
}

func (p monitoringIncidentsProvider) Actions(r console.Resource) []console.Action {
	switch r.Status {
	case monitoring.IncidentOpen:
		return []console.Action{{ID: "acknowledge", Label: "Acknowledge"}, {ID: "close", Label: "Close incident"}}
	case "acknowledged":
		return []console.Action{{ID: "close", Label: "Close incident"}}
	}
	return nil
}

func (p monitoringIncidentsProvider) Act(ctx context.Context, project, name, action string) error {
	s, err := p.server()
	if err != nil {
		return err
	}
	switch action {
	case "acknowledge":
		return s.AcknowledgeIncident(project, name)
	case "close":
		return s.CloseIncident(project, name)
	}
	return fmt.Errorf("unknown action %q on an incident", action)
}

func (p monitoringIncidentsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	s, err := p.server()
	if err != nil {
		return console.Detail{}, err
	}
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	for _, in := range s.Incidents(project) {
		if in.ID != path[0] {
			continue
		}
		state := in.State
		if in.Acknowledged && state == monitoring.IncidentOpen {
			state = "acknowledged"
		}
		d := console.Detail{Title: "Incident " + in.ID, Summary: []console.Property{
			{Label: "State", Value: state}, {Label: "Policy", Value: in.PolicyName, Link: "/monitoring/alerting/" + lastSegment(in.Policy)},
			{Label: "Condition", Value: in.ConditionName}, {Label: "Severity", Value: orDash(in.Severity)},
			{Label: "Metric", Value: orDash(strings.TrimSpace(in.MetricType + " " + labelsText(in.MetricLabels)))},
			{Label: "Resource", Value: orDash(strings.TrimSpace(in.ResourceType + " " + labelsText(in.ResourceLabels)))},
			{Label: "Observed value", Value: orDash(in.Observed)}, {Label: "Threshold", Value: orDash(in.Threshold)},
			{Label: "Started", Value: fmtTime(in.Started)}, {Label: "Ended", Value: fmtTime(in.Ended)},
			{Label: "Summary", Value: in.Summary},
		}}
		var ds []monitoring.Delivery
		for _, dl := range s.Deliveries(project) {
			if dl.Incident == in.ID {
				ds = append(ds, dl)
			}
		}
		d.Sections = []console.Section{{ID: "notifications", Label: "Notifications", Listing: deliveryListing(ds)}}
		return d, nil
	}
	return console.Detail{}, fmt.Errorf("incident %s was not found", path[0])
}

func deliveryListing(ds []monitoring.Delivery) console.Listing {
	out := console.Listing{Columns: []string{"Time", "Channel", "Type", "Kind", "Incident", "Detail"}, NameColumn: "State", Noun: "deliveries"}
	for _, d := range ds {
		out.Items = append(out.Items, console.Resource{Name: d.State, Status: d.State, Fields: map[string]string{
			"Time": fmtTime(d.Time), "Channel": lastSegment(d.Channel), "Type": d.Type, "Kind": d.Kind, "Incident": orDash(d.Incident), "Detail": orDash(d.Detail)}})
	}
	out.Total = len(out.Items)
	return out
}

// ---- Alerting: notification channels ----

type monitoringChannelsProvider struct{ monitoringAdmin }

func (monitoringChannelsProvider) ID() string    { return "monitoring-channels" }
func (monitoringChannelsProvider) Title() string { return "Notification channels" }

func channelTarget(c *monitoringpb.NotificationChannel) string {
	for _, k := range []string{"url", "topic", "email_address", "number", "channel_name", "space"} {
		if v := c.GetLabels()[k]; v != "" {
			return v
		}
	}
	return "—"
}

func verificationText(v monitoringpb.NotificationChannel_VerificationStatus) string {
	switch v {
	case monitoringpb.NotificationChannel_VERIFIED:
		return "Verified"
	case monitoringpb.NotificationChannel_UNVERIFIED:
		return "Unverified"
	}
	return "Not needed"
}

func (p monitoringChannelsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Display name", "Type", "Target", "Enabled", "Verification"}, NameColumn: "Channel",
		Noun: "notification channels", RowsOpenable: true,
		Note: "Webhooks are posted only to local URLs and Pub/Sub channels publish to this instance's Pub/Sub; email, SMS, Slack, PagerDuty and Google Chat notifications are recorded, never sent."}
	s, err := p.server()
	if err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		out.Prompt = "Notification channels are one project's. Choose one in the toolbar."
		return out, nil
	}
	resp, err := s.ListNotificationChannels(ctx, &monitoringpb.ListNotificationChannelsRequest{Name: "projects/" + project, PageSize: 1000})
	if err != nil {
		return console.Listing{}, err
	}
	for _, c := range resp.GetNotificationChannels() {
		out.Items = append(out.Items, console.Resource{Name: lastSegment(c.GetName()), Fields: map[string]string{
			"Display name": orDash(c.GetDisplayName()), "Type": c.GetType(), "Target": channelTarget(c),
			"Enabled": enabledText(c.GetEnabled()), "Verification": verificationText(c.GetVerificationStatus())}})
	}
	out.Total = len(out.Items)
	return out, nil
}

var channelFormTypes = []string{"webhook_tokenauth", "webhook_basicauth", "pubsub", "email", "sms", "slack", "pagerduty", "google_chat"}

func (monitoringChannelsProvider) CreateForm() (string, []console.Field) {
	return "Create channel", []console.Field{
		{Name: "type", Label: "Channel type", Type: "select", Required: true, Options: channelFormTypes, Default: "webhook_tokenauth"},
		{Name: "displayName", Label: "Display name", Type: "text", Required: true},
		{Name: "description", Label: "Description", Type: "text"},
		{Name: "url", Label: "Endpoint URL", Type: "text", Help: "Webhooks: a local URL, such as http://localhost:8080/alerts."},
		{Name: "username", Label: "Username", Type: "text", Help: "Webhook with basic auth."},
		{Name: "password", Label: "Password", Type: "password", Help: "Webhook with basic auth."},
		{Name: "topic", Label: "Topic", Type: "text", Help: "Pub/Sub: projects/PROJECT/topics/TOPIC on this instance."},
		{Name: "email_address", Label: "Email address", Type: "text", Help: "Recorded only: no email is sent."},
		{Name: "number", Label: "Phone number", Type: "text", Help: "SMS, recorded only: no message is sent."},
		{Name: "channel_name", Label: "Slack channel", Type: "text"},
		{Name: "auth_token", Label: "Slack token", Type: "password"},
		{Name: "service_key", Label: "PagerDuty service key", Type: "password"},
		{Name: "space", Label: "Google Chat space", Type: "text"},
	}
}

func (p monitoringChannelsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s, err := p.server()
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", errors.New("choose a project before creating a channel")
	}
	t := values["type"]
	keys := map[string][]string{"webhook_tokenauth": {"url"}, "webhook_basicauth": {"url", "username", "password"}, "pubsub": {"topic"},
		"email": {"email_address"}, "sms": {"number"}, "slack": {"channel_name", "auth_token"}, "pagerduty": {"service_key"}, "google_chat": {"space"}}[t]
	if keys == nil {
		return "", fmt.Errorf("channel type %q is not one of %s", t, strings.Join(channelFormTypes, ", "))
	}
	labels := map[string]string{}
	for _, k := range keys {
		if v := strings.TrimSpace(values[k]); v != "" {
			labels[k] = v
		}
	}
	c, err := s.CreateNotificationChannel(ctx, &monitoringpb.CreateNotificationChannelRequest{Name: "projects/" + project,
		NotificationChannel: &monitoringpb.NotificationChannel{Type: t, DisplayName: strings.TrimSpace(values["displayName"]),
			Description: strings.TrimSpace(values["description"]), Labels: labels, Enabled: wrapperspb.Bool(true)}})
	if err != nil {
		return "", err
	}
	return lastSegment(c.GetName()), nil
}

// Delete removes a channel and takes it off the policies that name it, as
// Google's console offers.
func (p monitoringChannelsProvider) Delete(ctx context.Context, project, name string) error {
	s, err := p.server()
	if err != nil {
		return err
	}
	_, err = s.DeleteNotificationChannel(ctx, &monitoringpb.DeleteNotificationChannelRequest{Name: "projects/" + project + "/notificationChannels/" + name, Force: true})
	return err
}

func (monitoringChannelsProvider) DeleteLabel() string { return "Delete channel" }

func (p monitoringChannelsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	s, err := p.server()
	if err != nil || len(path) != 1 {
		return nil
	}
	c, err := s.GetNotificationChannel(ctx, &monitoringpb.GetNotificationChannelRequest{Name: "projects/" + project + "/notificationChannels/" + path[0]})
	if err != nil {
		return nil
	}
	acts := []console.Action{{ID: "disable", Label: "Disable"}}
	if enabledText(c.GetEnabled()) == "Disabled" {
		acts = []console.Action{{ID: "enable", Label: "Enable"}}
	}
	switch c.GetVerificationStatus() {
	case monitoringpb.NotificationChannel_UNVERIFIED:
		acts = append(acts, console.Action{ID: "send-code", Label: "Send verification code"},
			console.Action{ID: "verify", Label: "Verify", Fields: []console.Field{{Name: "code", Label: "Verification code", Type: "text", Required: true,
				Help: "The code is in this channel's Deliveries: CloudBurrow sends no email or SMS."}}})
	case monitoringpb.NotificationChannel_VERIFIED:
		acts = append(acts, console.Action{ID: "get-code", Label: "Get verification code"})
	}
	return acts
}

func (p monitoringChannelsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	_, err := p.ActAtResult(ctx, project, path, action, values)
	return err
}

func (p monitoringChannelsProvider) ActAtResult(ctx context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	s, err := p.server()
	if err != nil {
		return nil, err
	}
	if len(path) != 1 {
		return nil, fmt.Errorf("no action at %v", path)
	}
	name := "projects/" + project + "/notificationChannels/" + path[0]
	switch action {
	case "enable", "disable":
		_, err = s.UpdateNotificationChannel(ctx, &monitoringpb.UpdateNotificationChannelRequest{
			NotificationChannel: &monitoringpb.NotificationChannel{Name: name, Enabled: wrapperspb.Bool(action == "enable")},
			UpdateMask:          &fieldmaskpb.FieldMask{Paths: []string{"enabled"}}})
		return nil, err
	case "send-code":
		_, err = s.SendNotificationChannelVerificationCode(ctx, &monitoringpb.SendNotificationChannelVerificationCodeRequest{Name: name})
		return nil, err
	case "verify":
		_, err = s.VerifyNotificationChannel(ctx, &monitoringpb.VerifyNotificationChannelRequest{Name: name, Code: strings.TrimSpace(values["code"])})
		return nil, err
	case "get-code":
		resp, err := s.GetNotificationChannelVerificationCode(ctx, &monitoringpb.GetNotificationChannelVerificationCodeRequest{Name: name})
		if err != nil {
			return nil, err
		}
		return &console.Listing{Columns: []string{"Expires"}, NameColumn: "Code", Noun: "codes", Total: 1,
			Items: []console.Resource{{Name: resp.GetCode(), Fields: map[string]string{"Expires": fmtTS(resp.GetExpireTime())}}}}, nil
	}
	return nil, fmt.Errorf("unknown action %q on a notification channel", action)
}

func (p monitoringChannelsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	s, err := p.server()
	if err != nil {
		return console.Detail{}, err
	}
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	c, err := s.GetNotificationChannel(ctx, &monitoringpb.GetNotificationChannelRequest{Name: "projects/" + project + "/notificationChannels/" + path[0]})
	if err != nil {
		return console.Detail{}, err
	}
	d := console.Detail{Title: orDash(c.GetDisplayName()), Summary: []console.Property{
		{Label: "Name", Value: c.GetName()}, {Label: "Type", Value: c.GetType()}, {Label: "Target", Value: channelTarget(c)},
		{Label: "Labels", Value: orDash(labelsText(c.GetLabels()))}, {Label: "Enabled", Value: enabledText(c.GetEnabled())},
		{Label: "Verification", Value: verificationText(c.GetVerificationStatus())},
		{Label: "Created", Value: fmtTS(c.GetCreationRecord().GetMutateTime())},
	}}
	var ds []monitoring.Delivery
	for _, dl := range s.Deliveries(project) {
		if dl.Channel == c.GetName() {
			ds = append(ds, dl)
		}
	}
	d.Sections = []console.Section{{ID: "deliveries", Label: "Deliveries", Listing: deliveryListing(ds)}}
	return d, nil
}

// ---- Uptime checks ----

type monitoringUptimeProvider struct{ monitoringAdmin }

func (monitoringUptimeProvider) ID() string    { return "monitoring-uptime" }
func (monitoringUptimeProvider) Title() string { return "Uptime checks" }

func uptimeProtocol(c *monitoringpb.UptimeCheckConfig) string {
	switch {
	case c.GetTcpCheck() != nil:
		return "TCP"
	case c.GetHttpCheck().GetUseSsl():
		return "HTTPS"
	}
	return "HTTP"
}

func uptimeTarget(c *monitoringpb.UptimeCheckConfig) string {
	host := c.GetMonitoredResource().GetLabels()["host"]
	if g := c.GetResourceGroup(); g != nil {
		host = "group " + g.GetGroupId()
	}
	if t := c.GetTcpCheck(); t != nil {
		return fmt.Sprintf("%s:%d", host, t.GetPort())
	}
	return fmt.Sprintf("%s:%d%s", host, c.GetHttpCheck().GetPort(), c.GetHttpCheck().GetPath())
}

func (p monitoringUptimeProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Display name", "Protocol", "Target", "Period", "Timeout", "Last result", "Latency"},
		NameColumn: "Uptime check", Noun: "uptime checks", RowsOpenable: true, AlwaysStatus: true,
		Note: "Checks run from one local checker against hosts on this machine only; any other host fails without being contacted."}
	s, err := p.server()
	if err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		out.Prompt = "Uptime checks are one project's. Choose one in the toolbar."
		return out, nil
	}
	resp, err := s.ListUptimeCheckConfigs(ctx, &monitoringpb.ListUptimeCheckConfigsRequest{Parent: "projects/" + project, PageSize: 1000})
	if err != nil {
		return console.Listing{}, err
	}
	for _, c := range resp.GetUptimeCheckConfigs() {
		last, latency, status := "Not run yet", "—", "pending"
		if rs := s.UptimeResults(c.GetName()); len(rs) > 0 {
			latency = fmt.Sprintf("%.1f ms", float64(rs[0].Latency.Microseconds())/1000)
			if rs[0].Passed {
				last, status = "Passed", "passed"
			} else {
				last, status = "Failed: "+rs[0].Error, "failed"
			}
		}
		out.Items = append(out.Items, console.Resource{Name: lastSegment(c.GetName()), Status: status, Fields: map[string]string{
			"Display name": c.GetDisplayName(), "Protocol": uptimeProtocol(c), "Target": uptimeTarget(c),
			"Period": c.GetPeriod().AsDuration().String(), "Timeout": c.GetTimeout().AsDuration().String(), "Last result": last, "Latency": latency}})
	}
	out.Total = len(out.Items)
	return out, nil
}

func (monitoringUptimeProvider) CreateForm() (string, []console.Field) {
	return "Create uptime check", []console.Field{
		{Name: "displayName", Label: "Title", Type: "text", Required: true},
		{Name: "protocol", Label: "Protocol", Type: "select", Options: []string{"HTTP", "HTTPS", "TCP"}, Default: "HTTP"},
		{Name: "host", Label: "Hostname", Type: "text", Required: true, Help: "A host on this machine, such as localhost or 127.0.0.1."},
		{Name: "port", Label: "Port", Type: "number", Help: "Empty for 80 (HTTP) or 443 (HTTPS); required for TCP."},
		{Name: "path", Label: "Path", Type: "text", Default: "/"},
		{Name: "period", Label: "Check frequency (seconds)", Type: "select", Options: []string{"60", "300", "600", "900"}, Default: "60"},
		{Name: "timeout", Label: "Response timeout (seconds)", Type: "number", Default: "10"},
		{Name: "content", Label: "Response content contains", Type: "text", Help: "Optional: the check fails without it."},
	}
}

func uptimeFromForm(project string, values map[string]string, c *monitoringpb.UptimeCheckConfig) error {
	c.DisplayName = strings.TrimSpace(values["displayName"])
	period, err := seconds("check frequency", values["period"], 60)
	if err != nil {
		return err
	}
	timeout, err := seconds("timeout", values["timeout"], 10)
	if err != nil {
		return err
	}
	c.Period, c.Timeout = durationpb.New(period), durationpb.New(timeout)
	port := 0
	if v := strings.TrimSpace(values["port"]); v != "" {
		if port, err = strconv.Atoi(v); err != nil || port <= 0 || port > 65535 {
			return fmt.Errorf("port %q is not a port", v)
		}
	}
	switch values["protocol"] {
	case "TCP":
		c.CheckRequestType = &monitoringpb.UptimeCheckConfig_TcpCheck_{TcpCheck: &monitoringpb.UptimeCheckConfig_TcpCheck{Port: int32(port)}}
	case "", "HTTP", "HTTPS":
		c.CheckRequestType = &monitoringpb.UptimeCheckConfig_HttpCheck_{HttpCheck: &monitoringpb.UptimeCheckConfig_HttpCheck{
			Port: int32(port), Path: strings.TrimSpace(values["path"]), UseSsl: values["protocol"] == "HTTPS"}}
	default:
		return fmt.Errorf("protocol %q is not HTTP, HTTPS or TCP", values["protocol"])
	}
	c.ContentMatchers = nil
	if v := values["content"]; v != "" {
		c.ContentMatchers = []*monitoringpb.UptimeCheckConfig_ContentMatcher{{Content: v}}
	}
	return nil
}

func (p monitoringUptimeProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s, err := p.server()
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", errors.New("choose a project before creating an uptime check")
	}
	c := &monitoringpb.UptimeCheckConfig{Resource: &monitoringpb.UptimeCheckConfig_MonitoredResource{MonitoredResource: &monitoredres.MonitoredResource{
		Type: "uptime_url", Labels: map[string]string{"host": strings.TrimSpace(values["host"]), "project_id": project}}}}
	if err := uptimeFromForm(project, values, c); err != nil {
		return "", err
	}
	got, err := s.CreateUptimeCheckConfig(ctx, &monitoringpb.CreateUptimeCheckConfigRequest{Parent: "projects/" + project, UptimeCheckConfig: c})
	if err != nil {
		return "", err
	}
	return lastSegment(got.GetName()), nil
}

func (p monitoringUptimeProvider) Delete(ctx context.Context, project, name string) error {
	s, err := p.server()
	if err != nil {
		return err
	}
	_, err = s.DeleteUptimeCheckConfig(ctx, &monitoringpb.DeleteUptimeCheckConfigRequest{Name: "projects/" + project + "/uptimeCheckConfigs/" + name})
	return err
}

func (monitoringUptimeProvider) DeleteLabel() string { return "Delete uptime check" }

func (p monitoringUptimeProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	s, err := p.server()
	if err != nil {
		return console.Detail{}, err
	}
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	c, err := s.GetUptimeCheckConfig(ctx, &monitoringpb.GetUptimeCheckConfigRequest{Name: "projects/" + project + "/uptimeCheckConfigs/" + path[0]})
	if err != nil {
		return console.Detail{}, err
	}
	content := ""
	if len(c.GetContentMatchers()) > 0 {
		content = c.GetContentMatchers()[0].GetContent()
	}
	port := ""
	if c.GetTcpCheck() != nil {
		port = strconv.Itoa(int(c.GetTcpCheck().GetPort()))
	} else {
		port = strconv.Itoa(int(c.GetHttpCheck().GetPort()))
	}
	d := console.Detail{Title: c.GetDisplayName(), Summary: []console.Property{
		{Label: "Name", Value: c.GetName()}, {Label: "Protocol", Value: uptimeProtocol(c)}, {Label: "Target", Value: uptimeTarget(c)},
		{Label: "Check frequency", Value: c.GetPeriod().AsDuration().String()}, {Label: "Timeout", Value: c.GetTimeout().AsDuration().String()},
		{Label: "Content contains", Value: orDash(content)},
	}, Edit: &console.EditForm{Label: "Edit", Title: "Edit uptime check", Submit: "Save",
		Note: "The hostname cannot change: create another check for another host.",
		Fields: []console.Field{
			{Name: "displayName", Label: "Title", Type: "text", Required: true, Default: c.GetDisplayName()},
			{Name: "protocol", Label: "Protocol", Type: "select", Options: []string{uptimeProtocol(c)}, Default: uptimeProtocol(c), Immutable: true},
			{Name: "port", Label: "Port", Type: "number", Default: port},
			{Name: "path", Label: "Path", Type: "text", Default: c.GetHttpCheck().GetPath()},
			{Name: "period", Label: "Check frequency (seconds)", Type: "select", Options: []string{"60", "300", "600", "900"},
				Default: strconv.Itoa(int(c.GetPeriod().AsDuration().Seconds()))},
			{Name: "timeout", Label: "Response timeout (seconds)", Type: "number", Default: strconv.Itoa(int(c.GetTimeout().AsDuration().Seconds()))},
			{Name: "content", Label: "Response content contains", Type: "text", Default: content},
		}}}
	rs := s.UptimeResults(c.GetName())
	results := console.Listing{Columns: []string{"Time", "Latency", "Status", "Error"}, NameColumn: "Result", Noun: "results", AlwaysStatus: true}
	series := console.ChartSeries{Label: "Request latency", Unit: "ms"}
	for i, r := range rs {
		state := "failed"
		if r.Passed {
			state = "passed"
		}
		status := "—"
		if r.Status > 0 {
			status = strconv.Itoa(r.Status)
		}
		if i < 200 {
			results.Items = append(results.Items, console.Resource{Name: state, Status: state, Fields: map[string]string{
				"Time": fmtTime(r.Time), "Latency": fmt.Sprintf("%.1f ms", float64(r.Latency.Microseconds())/1000), "Status": status, "Error": orDash(r.Error)}})
		}
	}
	for i := len(rs) - 1; i >= 0 && i >= len(rs)-500; i-- {
		v := float64(rs[i].Latency.Microseconds()) / 1000
		series.Points = append(series.Points, console.ChartPoint{At: rs[i].Time.UTC().Format(time.RFC3339Nano), Value: &v})
	}
	results.Total = len(rs)
	d.Sections = []console.Section{
		{ID: "results", Label: "Results", Listing: results},
		{ID: "latency", Label: "Latency", Kind: console.KindChart, Series: []console.ChartSeries{series}},
	}
	return d, nil
}

func (p monitoringUptimeProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	s, err := p.server()
	if err != nil {
		return err
	}
	if len(path) != 1 {
		return fmt.Errorf("no uptime check at %v", path)
	}
	c, err := s.GetUptimeCheckConfig(ctx, &monitoringpb.GetUptimeCheckConfigRequest{Name: "projects/" + project + "/uptimeCheckConfigs/" + path[0]})
	if err != nil {
		return err
	}
	values["protocol"] = uptimeProtocol(c)
	if err := uptimeFromForm(project, values, c); err != nil {
		return err
	}
	_, err = s.UpdateUptimeCheckConfig(ctx, &monitoringpb.UpdateUptimeCheckConfigRequest{UptimeCheckConfig: c})
	return err
}

// ---- Dashboards ----

type monitoringDashboardsProvider struct {
	monitoringAdmin
	metrics monitoringScreen
}

func (monitoringDashboardsProvider) ID() string    { return "monitoring-dashboards" }
func (monitoringDashboardsProvider) Title() string { return "Dashboards" }

// dashboardWidgets are a dashboard's widgets in layout order.
func dashboardWidgets(d *dashboardpb.Dashboard) []*dashboardpb.Widget {
	var out []*dashboardpb.Widget
	switch l := d.GetLayout().(type) {
	case *dashboardpb.Dashboard_GridLayout:
		out = l.GridLayout.GetWidgets()
	case *dashboardpb.Dashboard_MosaicLayout:
		for _, t := range l.MosaicLayout.GetTiles() {
			out = append(out, t.GetWidget())
		}
	case *dashboardpb.Dashboard_RowLayout:
		for _, r := range l.RowLayout.GetRows() {
			out = append(out, r.GetWidgets()...)
		}
	case *dashboardpb.Dashboard_ColumnLayout:
		for _, c := range l.ColumnLayout.GetColumns() {
			out = append(out, c.GetWidgets()...)
		}
	}
	return out
}

func layoutName(d *dashboardpb.Dashboard) string {
	switch d.GetLayout().(type) {
	case *dashboardpb.Dashboard_GridLayout:
		return "Grid"
	case *dashboardpb.Dashboard_MosaicLayout:
		return "Mosaic"
	case *dashboardpb.Dashboard_RowLayout:
		return "Rows"
	case *dashboardpb.Dashboard_ColumnLayout:
		return "Columns"
	}
	return "—"
}

func widgetKind(w *dashboardpb.Widget) string {
	switch w.GetContent().(type) {
	case *dashboardpb.Widget_XyChart:
		return "Line chart"
	case *dashboardpb.Widget_Scorecard:
		return "Scorecard"
	case *dashboardpb.Widget_Text:
		return "Text"
	case *dashboardpb.Widget_Blank:
		return "Blank"
	case *dashboardpb.Widget_AlertChart:
		return "Alert chart"
	case *dashboardpb.Widget_TimeSeriesTable:
		return "Table"
	case *dashboardpb.Widget_CollapsibleGroup:
		return "Group"
	case *dashboardpb.Widget_LogsPanel:
		return "Logs panel"
	case *dashboardpb.Widget_IncidentList:
		return "Incident list"
	case *dashboardpb.Widget_PieChart:
		return "Pie chart"
	case *dashboardpb.Widget_SectionHeader:
		return "Section header"
	}
	return "Widget"
}

func (p monitoringDashboardsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Display name", "Layout", "Widgets", "Labels"}, NameColumn: "Dashboard", Noun: "dashboards", RowsOpenable: true}
	s, err := p.server()
	if err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		out.Prompt = "Dashboards are one project's. Choose one in the toolbar."
		return out, nil
	}
	resp, err := s.ListDashboards(ctx, &dashboardpb.ListDashboardsRequest{Parent: "projects/" + project, PageSize: 1000})
	if err != nil {
		return console.Listing{}, err
	}
	for _, d := range resp.GetDashboards() {
		out.Items = append(out.Items, console.Resource{Name: lastSegment(d.GetName()), Fields: map[string]string{
			"Display name": d.GetDisplayName(), "Layout": layoutName(d), "Widgets": strconv.Itoa(len(dashboardWidgets(d))),
			"Labels": orDash(labelsText(d.GetLabels()))}})
	}
	out.Total = len(out.Items)
	return out, nil
}

func (monitoringDashboardsProvider) CreateForm() (string, []console.Field) {
	return "Create dashboard", []console.Field{
		{Name: "displayName", Label: "Dashboard name", Type: "text", Required: true},
		{Name: "json", Label: "Dashboard JSON", Type: "textarea",
			Help: `Optional: the dashboard as the API's JSON, such as {"gridLayout": {"widgets": [{"title": "Sales", "xyChart": {"dataSets": [{"timeSeriesQuery": {"timeSeriesFilter": {"filter": "metric.type=\"custom.googleapis.com/shop/sales\""}}}]}}]}}; the name above wins.`},
	}
}

func dashboardFromJSON(values map[string]string) (*dashboardpb.Dashboard, error) {
	d := &dashboardpb.Dashboard{}
	if j := strings.TrimSpace(values["json"]); j != "" {
		if err := protojson.Unmarshal([]byte(j), d); err != nil {
			return nil, fmt.Errorf("dashboard JSON: %v", err)
		}
	}
	if n := strings.TrimSpace(values["displayName"]); n != "" {
		d.DisplayName = n
	}
	if d.GetLayout() == nil {
		d.Layout = &dashboardpb.Dashboard_GridLayout{GridLayout: &dashboardpb.GridLayout{}}
	}
	return d, nil
}

func (p monitoringDashboardsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s, err := p.server()
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", errors.New("choose a project before creating a dashboard")
	}
	d, err := dashboardFromJSON(values)
	if err != nil {
		return "", err
	}
	d.Name = ""
	got, err := s.CreateDashboard(ctx, &dashboardpb.CreateDashboardRequest{Parent: "projects/" + project, Dashboard: d})
	if err != nil {
		return "", err
	}
	return lastSegment(got.GetName()), nil
}

func (p monitoringDashboardsProvider) Delete(ctx context.Context, project, name string) error {
	s, err := p.server()
	if err != nil {
		return err
	}
	_, err = s.DeleteDashboard(ctx, &dashboardpb.DeleteDashboardRequest{Name: "projects/" + project + "/dashboards/" + name})
	return err
}

func (monitoringDashboardsProvider) DeleteLabel() string { return "Delete dashboard" }

// widgetSeries runs an XY chart's data sets over the last hour, one chart
// line per series.
func (p monitoringDashboardsProvider) widgetSeries(ctx context.Context, project string, w *dashboardpb.Widget) ([]console.ChartSeries, string) {
	a, err := p.metrics.server()
	if err != nil {
		return nil, err.Error()
	}
	var out []console.ChartSeries
	var notes []string
	now := time.Now()
	for _, ds := range w.GetXyChart().GetDataSets() {
		q := ds.GetTimeSeriesQuery()
		f := q.GetTimeSeriesFilter()
		if f == nil {
			notes = append(notes, "only timeSeriesFilter queries are drawn")
			continue
		}
		req := &monitoringpb.ListTimeSeriesRequest{Name: "projects/" + project, Filter: f.GetFilter(), PageSize: 100,
			Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(now.Add(-time.Hour)), EndTime: timestamppb.New(now)}}
		if ag := f.GetAggregation(); ag != nil {
			req.Aggregation = &monitoringpb.Aggregation{AlignmentPeriod: ag.GetAlignmentPeriod(),
				PerSeriesAligner:   monitoringpb.Aggregation_Aligner(ag.GetPerSeriesAligner()),
				CrossSeriesReducer: monitoringpb.Aggregation_Reducer(ag.GetCrossSeriesReducer()), GroupByFields: ag.GetGroupByFields()}
		}
		resp, err := a.ListTimeSeries(ctx, req)
		if err != nil {
			notes = append(notes, err.Error())
			continue
		}
		for _, ts := range resp.GetTimeSeries() {
			label := ts.GetMetric().GetType()
			if l := labelsText(ts.GetMetric().GetLabels()); l != "" {
				label += " " + l
			}
			cs := console.ChartSeries{Label: label, Unit: resp.GetUnit()}
			pp := ts.GetPoints()
			for i := len(pp) - 1; i >= 0; i-- {
				n, _ := pointValue(pp[i].GetValue())
				pt := console.ChartPoint{At: pp[i].GetInterval().GetEndTime().AsTime().UTC().Format(time.RFC3339Nano)}
				if !math.IsNaN(n) && !math.IsInf(n, 0) {
					v := n
					pt.Value = &v
				}
				cs.Points = append(cs.Points, pt)
			}
			out = append(out, cs)
		}
	}
	return out, strings.Join(notes, "; ")
}

func (p monitoringDashboardsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	s, err := p.server()
	if err != nil {
		return console.Detail{}, err
	}
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	d, err := s.GetDashboard(ctx, &dashboardpb.GetDashboardRequest{Name: "projects/" + project + "/dashboards/" + path[0]})
	if err != nil {
		return console.Detail{}, err
	}
	b, _ := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(d)
	out := console.Detail{Title: d.GetDisplayName(), Summary: []console.Property{
		{Label: "Name", Value: d.GetName()}, {Label: "Layout", Value: layoutName(d)}, {Label: "Etag", Value: d.GetEtag()},
		{Label: "Labels", Value: orDash(labelsText(d.GetLabels()))},
	}, Edit: &console.EditForm{Label: "Edit JSON", Title: "Edit dashboard", Submit: "Save",
		Note:   "The dashboard is replaced by this JSON; a change made elsewhere since this page was opened is refused (its etag).",
		Fields: []console.Field{{Name: "json", Label: "Dashboard JSON", Type: "textarea", Required: true, Default: string(b)}}}}
	widgets := console.Listing{Columns: []string{"Type", "Detail"}, NameColumn: "Widget", Noun: "widgets"}
	for i, w := range dashboardWidgets(d) {
		title := w.GetTitle()
		if title == "" {
			title = fmt.Sprintf("Widget %d", i+1)
		}
		detail := "—"
		switch {
		case w.GetXyChart() != nil:
			series, note := p.widgetSeries(ctx, project, w)
			out.Sections = append(out.Sections, console.Section{ID: fmt.Sprintf("chart-%d", i+1), Label: title, Group: "Charts",
				Kind: console.KindChart, Series: series, Note: note})
			detail = fmt.Sprintf("%d time series over the last hour", len(series))
		case w.GetText() != nil:
			detail = w.GetText().GetContent()
		case w.GetScorecard() != nil:
			detail = w.GetScorecard().GetTimeSeriesQuery().GetTimeSeriesFilter().GetFilter()
		}
		widgets.Items = append(widgets.Items, console.Resource{Name: title, Fields: map[string]string{"Type": widgetKind(w), "Detail": orDash(detail)}})
	}
	widgets.Total = len(widgets.Items)
	out.Sections = append([]console.Section{{ID: "widgets", Label: "Widgets", Listing: widgets}}, out.Sections...)
	out.Sections = append(out.Sections, console.Section{ID: "json", Label: "JSON", Kind: console.KindText, Text: string(b)})
	return out, nil
}

func (p monitoringDashboardsProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	s, err := p.server()
	if err != nil {
		return err
	}
	if len(path) != 1 {
		return fmt.Errorf("no dashboard at %v", path)
	}
	d, err := dashboardFromJSON(map[string]string{"json": values["json"]})
	if err != nil {
		return err
	}
	d.Name = "projects/" + project + "/dashboards/" + path[0]
	_, err = s.UpdateDashboard(ctx, &dashboardpb.UpdateDashboardRequest{Dashboard: d})
	return err
}

// ---- Groups ----

type monitoringGroupsProvider struct{ monitoringAdmin }

func (monitoringGroupsProvider) ID() string    { return "monitoring-groups" }
func (monitoringGroupsProvider) Title() string { return "Groups" }

func (p monitoringGroupsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Display name", "Parent", "Filter", "Cluster"}, NameColumn: "Group", Noun: "groups", RowsOpenable: true}
	s, err := p.server()
	if err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		out.Prompt = "Groups are one project's. Choose one in the toolbar."
		return out, nil
	}
	resp, err := s.ListGroups(ctx, &monitoringpb.ListGroupsRequest{Name: "projects/" + project, PageSize: 1000})
	if err != nil {
		return console.Listing{}, err
	}
	for _, g := range resp.GetGroup() {
		parent := "—"
		if g.GetParentName() != "" {
			parent = lastSegment(g.GetParentName())
		}
		out.Items = append(out.Items, console.Resource{Name: lastSegment(g.GetName()), Fields: map[string]string{
			"Display name": g.GetDisplayName(), "Parent": parent, "Filter": g.GetFilter(), "Cluster": strconv.FormatBool(g.GetIsCluster())}})
	}
	out.Total = len(out.Items)
	return out, nil
}

func (monitoringGroupsProvider) CreateForm() (string, []console.Field) {
	return "Create group", []console.Field{
		{Name: "displayName", Label: "Name", Type: "text", Required: true},
		{Name: "filter", Label: "Filter", Type: "text", Required: true, Help: `Such as resource.type = "global".`},
		{Name: "parent", Label: "Parent group", Type: "text", Help: "Optional: a group ID."},
		{Name: "isCluster", Label: "Treat as a cluster", Type: "checkbox"},
	}
}

func (p monitoringGroupsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s, err := p.server()
	if err != nil {
		return "", err
	}
	g := &monitoringpb.Group{DisplayName: strings.TrimSpace(values["displayName"]), Filter: strings.TrimSpace(values["filter"]),
		IsCluster: values["isCluster"] == "true"}
	if v := strings.TrimSpace(values["parent"]); v != "" {
		g.ParentName = "projects/" + project + "/groups/" + lastSegment(v)
	}
	got, err := s.CreateGroup(ctx, &monitoringpb.CreateGroupRequest{Name: "projects/" + project, Group: g})
	if err != nil {
		return "", err
	}
	return lastSegment(got.GetName()), nil
}

func (p monitoringGroupsProvider) Delete(ctx context.Context, project, name string) error {
	s, err := p.server()
	if err != nil {
		return err
	}
	_, err = s.DeleteGroup(ctx, &monitoringpb.DeleteGroupRequest{Name: "projects/" + project + "/groups/" + name, Recursive: true})
	return err
}

func (p monitoringGroupsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	s, err := p.server()
	if err != nil {
		return console.Detail{}, err
	}
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	name := "projects/" + project + "/groups/" + path[0]
	g, err := s.GetGroup(ctx, &monitoringpb.GetGroupRequest{Name: name})
	if err != nil {
		return console.Detail{}, err
	}
	d := console.Detail{Title: g.GetDisplayName(), Summary: []console.Property{{Label: "Name", Value: g.GetName()},
		{Label: "Filter", Value: g.GetFilter()}, {Label: "Parent", Value: orDash(g.GetParentName())}},
		Edit: &console.EditForm{Label: "Edit", Title: "Edit group", Submit: "Save", Fields: []console.Field{
			{Name: "displayName", Label: "Name", Type: "text", Required: true, Default: g.GetDisplayName()},
			{Name: "filter", Label: "Filter", Type: "text", Required: true, Default: g.GetFilter()}}}}
	members := console.Listing{Columns: []string{"Labels"}, NameColumn: "Resource type", Noun: "members"}
	if resp, err := s.ListGroupMembers(ctx, &monitoringpb.ListGroupMembersRequest{Name: name, PageSize: 1000}); err == nil {
		for _, m := range resp.GetMembers() {
			members.Items = append(members.Items, console.Resource{Name: m.GetType(), Fields: map[string]string{"Labels": labelsText(m.GetLabels())}})
		}
	} else {
		members.Unavailable = err.Error()
	}
	members.Total = len(members.Items)
	children := console.Listing{Columns: []string{"Display name"}, NameColumn: "Group", Noun: "subgroups"}
	if resp, err := s.ListGroups(ctx, &monitoringpb.ListGroupsRequest{Name: "projects/" + project, Filter: &monitoringpb.ListGroupsRequest_ChildrenOfGroup{ChildrenOfGroup: name}}); err == nil {
		for _, c := range resp.GetGroup() {
			children.Items = append(children.Items, console.Resource{Name: lastSegment(c.GetName()), Fields: map[string]string{"Display name": c.GetDisplayName()}})
		}
	}
	children.Total = len(children.Items)
	d.Sections = []console.Section{{ID: "members", Label: "Members", Listing: members}, {ID: "subgroups", Label: "Subgroups", Listing: children}}
	return d, nil
}

func (p monitoringGroupsProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	s, err := p.server()
	if err != nil {
		return err
	}
	if len(path) != 1 {
		return fmt.Errorf("no group at %v", path)
	}
	g, err := s.GetGroup(ctx, &monitoringpb.GetGroupRequest{Name: "projects/" + project + "/groups/" + path[0]})
	if err != nil {
		return err
	}
	g.DisplayName, g.Filter = strings.TrimSpace(values["displayName"]), strings.TrimSpace(values["filter"])
	_, err = s.UpdateGroup(ctx, &monitoringpb.UpdateGroupRequest{Group: g})
	return err
}

// ---- Services and SLOs ----

type monitoringServicesProvider struct{ monitoringAdmin }

func (monitoringServicesProvider) ID() string    { return "monitoring-services" }
func (monitoringServicesProvider) Title() string { return "Services" }

func (p monitoringServicesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Display name", "SLOs"}, NameColumn: "Service", Noun: "services", RowsOpenable: true,
		Note: "Service-level objectives are stored; their compliance and error budget are not computed."}
	s, err := p.server()
	if err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		out.Prompt = "Services are one project's. Choose one in the toolbar."
		return out, nil
	}
	resp, err := s.ListServices(ctx, &monitoringpb.ListServicesRequest{Parent: "projects/" + project, PageSize: 1000})
	if err != nil {
		return console.Listing{}, err
	}
	for _, sv := range resp.GetServices() {
		n := 0
		if r, err := s.ListServiceLevelObjectives(ctx, &monitoringpb.ListServiceLevelObjectivesRequest{Parent: sv.GetName(), PageSize: 1000}); err == nil {
			n = len(r.GetServiceLevelObjectives())
		}
		out.Items = append(out.Items, console.Resource{Name: lastSegment(sv.GetName()), Fields: map[string]string{
			"Display name": orDash(sv.GetDisplayName()), "SLOs": strconv.Itoa(n)}})
	}
	out.Total = len(out.Items)
	return out, nil
}

func (monitoringServicesProvider) CreateForm() (string, []console.Field) {
	return "Define service", []console.Field{
		{Name: "id", Label: "Service ID", Type: "text", Required: true, Pattern: `^[a-zA-Z0-9\-_:.]+$`},
		{Name: "displayName", Label: "Display name", Type: "text"},
	}
}

func (p monitoringServicesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s, err := p.server()
	if err != nil {
		return "", err
	}
	sv, err := s.CreateService(ctx, &monitoringpb.CreateServiceRequest{Parent: "projects/" + project, ServiceId: strings.TrimSpace(values["id"]),
		Service: &monitoringpb.Service{DisplayName: strings.TrimSpace(values["displayName"]),
			Identifier: &monitoringpb.Service_Custom_{Custom: &monitoringpb.Service_Custom{}}}})
	if err != nil {
		return "", err
	}
	return lastSegment(sv.GetName()), nil
}

func (p monitoringServicesProvider) Delete(ctx context.Context, project, name string) error {
	s, err := p.server()
	if err != nil {
		return err
	}
	_, err = s.DeleteService(ctx, &monitoringpb.DeleteServiceRequest{Name: "projects/" + project + "/services/" + name})
	return err
}

func (p monitoringServicesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	s, err := p.server()
	if err != nil {
		return console.Detail{}, err
	}
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	name := "projects/" + project + "/services/" + path[0]
	sv, err := s.GetService(ctx, &monitoringpb.GetServiceRequest{Name: name})
	if err != nil {
		return console.Detail{}, err
	}
	d := console.Detail{Title: orDash(sv.GetDisplayName()), Summary: []console.Property{{Label: "Name", Value: sv.GetName()}},
		Edit: &console.EditForm{Label: "Edit", Title: "Edit service", Submit: "Save", Fields: []console.Field{
			{Name: "displayName", Label: "Display name", Type: "text", Default: sv.GetDisplayName()}}}}
	slos := console.Listing{Columns: []string{"Display name", "Goal", "Period", "Indicator"}, NameColumn: "SLO", Noun: "service-level objectives"}
	resp, err := s.ListServiceLevelObjectives(ctx, &monitoringpb.ListServiceLevelObjectivesRequest{Parent: name, PageSize: 1000})
	if err != nil {
		return console.Detail{}, err
	}
	for _, o := range resp.GetServiceLevelObjectives() {
		period := o.GetCalendarPeriod().String()
		if o.GetRollingPeriod() != nil {
			period = fmt.Sprintf("rolling %d days", int(o.GetRollingPeriod().AsDuration().Hours()/24))
		}
		ind := "—"
		if r := o.GetServiceLevelIndicator().GetRequestBased().GetGoodTotalRatio(); r != nil {
			ind = "good " + orDash(r.GetGoodServiceFilter()) + " / total " + orDash(r.GetTotalServiceFilter())
		}
		slos.Items = append(slos.Items, console.Resource{Name: lastSegment(o.GetName()), Fields: map[string]string{
			"Display name": orDash(o.GetDisplayName()), "Goal": strconv.FormatFloat(o.GetGoal()*100, 'g', 6, 64) + "%", "Period": period, "Indicator": ind}})
	}
	slos.Total = len(slos.Items)
	d.Sections = []console.Section{{ID: "slos", Label: "Service-level objectives", Listing: slos}}
	return d, nil
}

func (p monitoringServicesProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	s, err := p.server()
	if err != nil {
		return err
	}
	if len(path) != 1 {
		return fmt.Errorf("no service at %v", path)
	}
	_, err = s.UpdateService(ctx, &monitoringpb.UpdateServiceRequest{Service: &monitoringpb.Service{
		Name: "projects/" + project + "/services/" + path[0], DisplayName: strings.TrimSpace(values["displayName"])},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	return err
}

func (p monitoringServicesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	acts := []console.Action{{ID: "create-slo", Label: "Create SLO", Fields: []console.Field{
		{Name: "id", Label: "SLO ID", Type: "text", Pattern: `^[a-zA-Z0-9\-_:.]*$`, Help: "Empty for a generated one."},
		{Name: "displayName", Label: "Display name", Type: "text"},
		{Name: "goal", Label: "Goal (%)", Type: "number", Required: true, Default: "99"},
		{Name: "days", Label: "Rolling period (days)", Type: "number", Required: true, Default: "28"},
		{Name: "good", Label: "Good service filter", Type: "text", Required: true},
		{Name: "total", Label: "Total service filter", Type: "text", Required: true},
	}}}
	s, err := p.server()
	if err != nil {
		return acts
	}
	if resp, err := s.ListServiceLevelObjectives(ctx, &monitoringpb.ListServiceLevelObjectivesRequest{Parent: "projects/" + project + "/services/" + path[0]}); err == nil {
		for _, o := range resp.GetServiceLevelObjectives() {
			acts = append(acts, console.Action{ID: "delete-slo:" + lastSegment(o.GetName()), Label: "Delete SLO " + lastSegment(o.GetName()), Destructive: true})
		}
	}
	return acts
}

func (p monitoringServicesProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	s, err := p.server()
	if err != nil {
		return err
	}
	if len(path) != 1 {
		return fmt.Errorf("no action at %v", path)
	}
	parent := "projects/" + project + "/services/" + path[0]
	if id, ok := strings.CutPrefix(action, "delete-slo:"); ok {
		_, err = s.DeleteServiceLevelObjective(ctx, &monitoringpb.DeleteServiceLevelObjectiveRequest{Name: parent + "/serviceLevelObjectives/" + id})
		return err
	}
	if action != "create-slo" {
		return fmt.Errorf("unknown action %q on a service", action)
	}
	goal, err := strconv.ParseFloat(strings.TrimSpace(values["goal"]), 64)
	if err != nil {
		return fmt.Errorf("goal %q is not a percentage", values["goal"])
	}
	days, err := strconv.Atoi(strings.TrimSpace(values["days"]))
	if err != nil {
		return fmt.Errorf("rolling period %q is not a number of days", values["days"])
	}
	_, err = s.CreateServiceLevelObjective(ctx, &monitoringpb.CreateServiceLevelObjectiveRequest{Parent: parent, ServiceLevelObjectiveId: strings.TrimSpace(values["id"]),
		ServiceLevelObjective: &monitoringpb.ServiceLevelObjective{DisplayName: strings.TrimSpace(values["displayName"]), Goal: goal / 100,
			Period: &monitoringpb.ServiceLevelObjective_RollingPeriod{RollingPeriod: durationpb.New(time.Duration(days) * 24 * time.Hour)},
			ServiceLevelIndicator: &monitoringpb.ServiceLevelIndicator{Type: &monitoringpb.ServiceLevelIndicator_RequestBased{RequestBased: &monitoringpb.RequestBasedSli{
				Method: &monitoringpb.RequestBasedSli_GoodTotalRatio{GoodTotalRatio: &monitoringpb.TimeSeriesRatio{
					GoodServiceFilter: strings.TrimSpace(values["good"]), TotalServiceFilter: strings.TrimSpace(values["total"])}}}}}}})
	return err
}

// ---- Snoozes ----

type monitoringSnoozesProvider struct{ monitoringAdmin }

func (monitoringSnoozesProvider) ID() string    { return "monitoring-snoozes" }
func (monitoringSnoozesProvider) Title() string { return "Snoozes" }

func (p monitoringSnoozesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Display name", "Policies", "Start", "End"}, NameColumn: "Snooze", Noun: "snoozes", AlwaysStatus: true,
		Note: "While a snooze is active its policies open no incidents and send no notifications."}
	s, err := p.server()
	if err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		out.Prompt = "Snoozes are one project's. Choose one in the toolbar."
		return out, nil
	}
	resp, err := s.ListSnoozes(ctx, &monitoringpb.ListSnoozesRequest{Parent: "projects/" + project, PageSize: 1000})
	if err != nil {
		return console.Listing{}, err
	}
	now := time.Now()
	for _, sn := range resp.GetSnoozes() {
		var ps []string
		for _, n := range sn.GetCriteria().GetPolicies() {
			ps = append(ps, lastSegment(n))
		}
		status := "active"
		switch {
		case !sn.GetInterval().GetEndTime().AsTime().After(now):
			status = "ended"
		case sn.GetInterval().GetStartTime() != nil && sn.GetInterval().GetStartTime().AsTime().After(now):
			status = "upcoming"
		}
		out.Items = append(out.Items, console.Resource{Name: lastSegment(sn.GetName()), Status: status, Fields: map[string]string{
			"Display name": sn.GetDisplayName(), "Policies": strings.Join(ps, ", "),
			"Start": fmtTS(sn.GetInterval().GetStartTime()), "End": fmtTS(sn.GetInterval().GetEndTime())}})
	}
	out.Total = len(out.Items)
	return out, nil
}

func (monitoringSnoozesProvider) CreateForm() (string, []console.Field) {
	return "Create snooze", []console.Field{
		{Name: "displayName", Label: "Snooze name", Type: "text", Required: true},
		{Name: "policies", Label: "Alert policies", Type: "text", Required: true, Help: "Policy IDs from Alerting, comma separated."},
		{Name: "minutes", Label: "Duration (minutes)", Type: "number", Required: true, Default: "60"},
	}
}

func (p monitoringSnoozesProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s, err := p.server()
	if err != nil {
		return "", err
	}
	mins, err := strconv.Atoi(strings.TrimSpace(values["minutes"]))
	if err != nil || mins <= 0 {
		return "", fmt.Errorf("duration %q is not a number of minutes", values["minutes"])
	}
	var ps []string
	for _, id := range splitList(values["policies"]) {
		ps = append(ps, "projects/"+project+"/alertPolicies/"+lastSegment(id))
	}
	now := time.Now()
	sn, err := s.CreateSnooze(ctx, &monitoringpb.CreateSnoozeRequest{Parent: "projects/" + project, Snooze: &monitoringpb.Snooze{
		DisplayName: strings.TrimSpace(values["displayName"]), Criteria: &monitoringpb.Snooze_Criteria{Policies: ps},
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(now), EndTime: timestamppb.New(now.Add(time.Duration(mins) * time.Minute))}}})
	if err != nil {
		return "", err
	}
	return lastSegment(sn.GetName()), nil
}

func (p monitoringSnoozesProvider) Actions(r console.Resource) []console.Action {
	if r.Status == "ended" {
		return nil
	}
	return []console.Action{{ID: "end", Label: "End snooze"}}
}

// Act ends a snooze now, which is how Google's API stops one: snoozes are
// never deleted.
func (p monitoringSnoozesProvider) Act(ctx context.Context, project, name, action string) error {
	s, err := p.server()
	if err != nil {
		return err
	}
	if action != "end" {
		return fmt.Errorf("unknown action %q on a snooze", action)
	}
	n := "projects/" + project + "/snoozes/" + name
	sn, err := s.GetSnooze(ctx, &monitoringpb.GetSnoozeRequest{Name: n})
	if err != nil {
		return err
	}
	now := time.Now()
	iv := &monitoringpb.TimeInterval{StartTime: sn.GetInterval().GetStartTime(), EndTime: timestamppb.New(now.Add(time.Second))}
	if iv.StartTime == nil || iv.StartTime.AsTime().After(now) {
		iv.StartTime = timestamppb.New(now)
	}
	_, err = s.UpdateSnooze(ctx, &monitoringpb.UpdateSnoozeRequest{Snooze: &monitoringpb.Snooze{Name: n, Interval: iv},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"interval"}}})
	return err
}
