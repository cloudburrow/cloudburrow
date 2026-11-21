package main

// Logs Storage's usage summary and Create usage alert, and the Log
// Router's Volume (#2021), from docs.cloud.google.com/logging/docs/buckets
// and docs.cloud.google.com/logging/docs/export/configure_export_v2 (read
// 2026-11-20):
//
//   - the Logs Storage page shows Current month ingestion, Previous month
//     ingestion, Projected ingestion by EOM and Current month billable
//     storage, none of which counts _Required. Here they are Cloud
//     Logging's own counts of the bytes written into the project's log
//     buckets (internal/service/logging/volume.go), the ones Cloud
//     Monitoring serves as logging.googleapis.com/billing/bytes_ingested;
//     billable storage is the bytes of the stored entries received more
//     than 30 days ago;
//   - Create usage alert opens a dialog and creates a Cloud Monitoring
//     alerting policy as Google's console builds it: one threshold
//     condition on Global's "Monthly log bytes ingested"
//     (logging.googleapis.com/billing/monthly_bytes_ingested), no filter,
//     across-time-series sum, a 60-minute rolling window of max, Any time
//     series violates, Above threshold, the threshold the user sets, and a
//     retest window of at least 30 minutes;
//   - the Log Router's Volume is "the total volume of logs routed to the
//     log sink", here the bytes of logging.googleapis.com/exports/byte_count
//     over the past 30 days, and the row's View sink log volume and error
//     rates opens Metrics Explorer on that metric for the sink.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	usageAlertAction = "usage-alert"
	sinkVolumeAction = "sink-volume"
	// sinkVolumeWindow is the Volume column's period.
	sinkVolumeWindow = 30 * 24 * time.Hour
	monthlyIngested  = "logging.googleapis.com/billing/monthly_bytes_ingested"
	exportByteCount  = "logging.googleapis.com/exports/byte_count"
)

const noMonitoring = "Not available: this instance does not serve Cloud Monitoring, which holds the metric."

// sinkVolume is a sink's Volume: the bytes it routed in the past 30 days.
func (s loggingAdminScreen) sinkVolume(name string) string {
	if s.svc == nil || s.svc.api == nil {
		return "—"
	}
	parent := name[:strings.Index(name, "/sinks/")]
	now := s.svc.api.Now()
	var n int64
	for _, v := range s.svc.api.SinkVolumes(parent, now.Add(-sinkVolumeWindow), now.Add(time.Minute)) {
		if v.Sink == name {
			n += v.Bytes
		}
	}
	if n == 0 {
		return "0 B"
	}
	return formatBytes(n)
}

// sinkVolumeAction is the row's View sink log volume and error rates:
// Metrics Explorer on the sink's exported bytes. Monitoring's metrics are
// per project, so a folder's or organization's sink has none here.
func (s loggingAdminScreen) sinkVolumeLink(name string) console.Action {
	a := console.Action{ID: sinkVolumeAction, Label: "View sink log volume and error rates"}
	project, id, ok := strings.Cut(strings.TrimPrefix(name, "projects/"), "/sinks/")
	switch {
	case s.monitoring == nil:
		a.Unavailable = noMonitoring
	case !strings.HasPrefix(name, "projects/") || !ok:
		a.Unavailable = "Not available: Cloud Monitoring keeps a sink's metrics in a project, and this sink belongs to a folder, organization or billing account."
	default:
		q := url.Values{"metric": {exportByteCount}, "resource": {"logging_sink"}, "project": {project},
			"filter": {fmt.Sprintf("resource.labels.name = %q", id)}}
		a.Href = "/monitoring/metrics-explorer?" + q.Encode()
	}
	return a
}

// monthStart is the first instant of t's month, UTC.
func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// usageSummary is the Logs Storage page's statistics for a project.
func (s loggingAdminScreen) usageSummary(project string) []console.PropertyGroup {
	if s.svc == nil || s.svc.api == nil || project == "" {
		return nil
	}
	api := s.svc.api
	now := api.Now()
	cur := monthStart(now)
	prev := monthStart(cur.Add(-time.Nanosecond))
	sum := func(from, to time.Time) int64 {
		var n int64
		for _, v := range api.Ingested(project, from, to) {
			n += v.Bytes
		}
		return n
	}
	current := sum(cur, now.Add(time.Minute))
	previous := sum(prev, cur)
	projected := current
	if elapsed := now.Sub(cur); elapsed > time.Minute {
		projected = int64(float64(current) * float64(cur.AddDate(0, 1, 0).Sub(cur)) / float64(elapsed))
	}
	var billable int64
	for _, e := range api.ProjectEntries(project, time.Time{}, now) {
		if r := e.GetReceiveTimestamp(); r != nil && now.Sub(r.AsTime()) > 30*24*time.Hour {
			billable += int64(proto.Size(e))
		}
	}
	word := func(n int64) string {
		if n <= 0 {
			return "0 B"
		}
		return formatBytes(n)
	}
	return []console.PropertyGroup{{Heading: "Usage", Properties: []console.Property{
		{Label: "Current month ingestion", Value: word(current)},
		{Label: "Previous month ingestion", Value: word(previous)},
		{Label: "Projected ingestion by EOM", Value: word(projected)},
		{Label: "Current month billable storage", Value: word(billable)},
	}}}
}

// ListActions implements console.ListActor: Create usage alert, beside
// Create log bucket, on a project's page.
func (p loggingBucketsProvider) ListActions(ctx context.Context, project string) []console.Action {
	parent, err := bucketParent(ctx, project)
	if err != nil || !strings.HasPrefix(parent, "projects/") {
		return nil
	}
	a := console.Action{ID: usageAlertAction, Label: "Create usage alert", Submit: "Create policy", Fields: []console.Field{
		{Name: "displayName", Label: "Alert policy name", Type: "text", Required: true, Default: "Logs ingestion usage"},
		{Name: "threshold", Label: "Threshold (GiB)", Type: "number", Required: true, Default: "50",
			Help: "An incident opens when the month's log bytes ingested into this project's log buckets are above it."},
		{Name: "retest", Label: "Retest window (minutes)", Type: "number", Default: "30", Help: "At least 30 minutes."},
		{Name: "channels", Label: "Notification channels", Type: "text", Help: "Channel IDs from Notification channels, comma separated; empty for none."},
	}}
	if p.monitoring == nil {
		a.Unavailable = "Not available: this instance does not serve Cloud Monitoring, where the alerting policy is created."
		a.Fields = nil
	}
	return []console.Action{a}
}

// usagePolicy is the alerting policy Create usage alert makes.
func usagePolicy(project string, values map[string]string) (*monitoringpb.AlertPolicy, error) {
	name := strings.TrimSpace(values["displayName"])
	if name == "" {
		return nil, errors.New("an alert policy needs a name")
	}
	gib, err := strconv.ParseFloat(strings.TrimSpace(values["threshold"]), 64)
	if err != nil || gib <= 0 {
		return nil, fmt.Errorf("threshold %q is not a positive number of GiB", values["threshold"])
	}
	minutes := 30
	if v := strings.TrimSpace(values["retest"]); v != "" {
		if minutes, err = strconv.Atoi(v); err != nil {
			return nil, fmt.Errorf("retest window %q is not a whole number of minutes", v)
		}
	}
	if minutes < 30 {
		return nil, fmt.Errorf("the retest window is at least 30 minutes, not %d", minutes)
	}
	cond := &monitoringpb.AlertPolicy_Condition{DisplayName: "Monthly log bytes ingested",
		Condition: &monitoringpb.AlertPolicy_Condition_ConditionThreshold{ConditionThreshold: &monitoringpb.AlertPolicy_Condition_MetricThreshold{
			Filter: fmt.Sprintf(`resource.type = "global" AND metric.type = %q`, monthlyIngested),
			Aggregations: []*monitoringpb.Aggregation{{AlignmentPeriod: durationpb.New(time.Hour),
				PerSeriesAligner: monitoringpb.Aggregation_ALIGN_MAX, CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM}},
			Comparison: monitoringpb.ComparisonType_COMPARISON_GT, ThresholdValue: gib * (1 << 30),
			Duration: durationpb.New(time.Duration(minutes) * time.Minute),
			Trigger:  &monitoringpb.AlertPolicy_Condition_Trigger{Type: &monitoringpb.AlertPolicy_Condition_Trigger_Count{Count: 1}}}}}
	pol := &monitoringpb.AlertPolicy{DisplayName: name, Combiner: monitoringpb.AlertPolicy_OR, Enabled: wrapperspb.Bool(true),
		Conditions: []*monitoringpb.AlertPolicy_Condition{cond},
		Documentation: &monitoringpb.AlertPolicy_Documentation{MimeType: "text/markdown",
			Content: fmt.Sprintf("The log bytes ingested into project %s's log buckets this month are above %g GiB.", project, gib)}}
	for _, c := range splitList(values["channels"]) {
		if !strings.HasPrefix(c, "projects/") {
			c = "projects/" + project + "/notificationChannels/" + c
		}
		pol.NotificationChannels = append(pol.NotificationChannels, c)
	}
	return pol, nil
}

// createUsageAlert creates the policy in the project's Cloud Monitoring.
func (p loggingBucketsProvider) createUsageAlert(ctx context.Context, project string, values map[string]string) error {
	if p.monitoring == nil || p.monitoring() == nil {
		return errors.New(noMonitoring)
	}
	parent, err := bucketParent(ctx, project)
	if err != nil || !strings.HasPrefix(parent, "projects/") {
		return errors.New("a usage alert is a project's: choose a project")
	}
	pol, err := usagePolicy(strings.TrimPrefix(parent, "projects/"), values)
	if err != nil {
		return err
	}
	_, err = p.monitoring().CreateAlertPolicy(ctx, &monitoringpb.CreateAlertPolicyRequest{Name: parent, AlertPolicy: pol})
	return err
}

var _ console.ListActor = loggingBucketsProvider{}
