package vertexai

// Model Monitoring alert delivery (#2214): a run's alerts go to the
// notification spec's Cloud Monitoring notification channels (a Pub/Sub
// channel publishes to this instance's Pub/Sub, a local webhook is posted
// to, an email channel mails the local mail sink, other types are
// recorded), to its email addresses (mailed to the local mail sink,
// internal/mailsink, which the console's Mail page shows and which relays
// nothing), and, when enable_logging (v1) or enable_cloud_logging (v2) is
// set, to Cloud Logging as model_monitoring_anomaly entries.
//
// Documented behaviour ("Monitor feature skew and drift",
// https://docs.cloud.google.com/vertex-ai/docs/model-monitoring/using-model-monitoring;
// the ModelMonitoringNotificationSpec and ModelMonitoringAlertConfig
// protos; read 2026-11-24):
//   - notification channels are named projects/<project>/notificationChannels/<channelId>;
//   - an anomaly email "includes the following": the time the job ran, the
//     feature name, and "The alerting threshold as well as the recorded
//     statistical distance measure";
//   - the console "supports PagerDuty, Slack, and Pub/Sub notification
//     channels";
//   - v1 Cloud Logging: "Anomalies are written to Cloud Logging with the
//     log name: model_monitoring_anomaly" (logName
//     projects/P/logs/aiplatform.googleapis.com%2Fmodel_monitoring_anomaly),
//     severity WARNING, resource aiplatform.googleapis.com/ModelDeploymentMonitoringJob
//     with labels model_deployment_monitoring_job, location and
//     resource_container, and a jsonPayload of
//     google.cloud.aiplatform.logging.ModelMonitoringAnomaliesLogEntry:
//     anomalyObjective, featureAnomalies (featureDisplayName, deviation,
//     threshold), totalAnomaliesCount, startTime, endTime,
//     modelDeploymentMonitoringJob and deployedModelId; "At each monitoring
//     interval, an anomaly log is written to Cloud Logging if the
//     distribution of at least one feature exceeds the threshold";
//   - v2's enable_cloud_logging puts the anomalies in a jsonPayload
//     "encoded from proto google.cloud.aiplatform.logging.ModelMonitoringAnomaliesLogEntry".
//
// UNVERIFIED (Google does not document them; re-read 2026-11-24): the email's subject and
// layout and its sender; the channel payload (here {"modelMonitoringJob", "alerts"} with
// each alert as its JSON, or v1's job and anomalies); resource_container's
// value (here projects/<project as the job names it>); v2's log name,
// resource type and payload fields (here v1's log name, the resource
// aiplatform.googleapis.com/ModelMonitor with labels model_monitor,
// location and resource_container, and v1's payload with
// modelMonitoringJob in place of modelDeploymentMonitoringJob); one entry
// per deployed model and objective (v1) or per objective (v2).

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	betapb "cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// AlertNotifier delivers Model Monitoring's alerts: Cloud Monitoring's
// server, on an instance that serves it.
type AlertNotifier interface {
	// NotifyChannels delivers payload to the notification channels names;
	// an email channel is sent subject and text.
	NotifyChannels(ctx context.Context, kind string, names []string, payload []byte, subject, text string) []AlertDelivery
	// SendEmail mails addresses in project through the local mail sink.
	SendEmail(ctx context.Context, project, kind, from string, to []string, subject, body string)
}

// AlertsSender is the From address of Model Monitoring's email. Google's
// sender is not documented; the local one names no Google domain.
const AlertsSender = "model-monitoring-noreply@cloudburrow.localhost"

// AnomalyLogger writes one entry to this instance's Cloud Logging.
type AnomalyLogger func(ctx context.Context, e *loggingpb.LogEntry) error

// SetAnomalyLogger writes Model Monitoring's anomaly log entries through l.
func (s *Server) SetAnomalyLogger(l AnomalyLogger) {
	s.mu.Lock()
	s.anomalyLogger = l
	s.mu.Unlock()
}

func (s *Server) anomalyLog() AnomalyLogger {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.anomalyLogger
}

// AnomalyLogID is the log the anomalies are written to.
const AnomalyLogID = "aiplatform.googleapis.com%2Fmodel_monitoring_anomaly"

const anomalyEntryType = "type.googleapis.com/google.cloud.aiplatform.logging.ModelMonitoringAnomaliesLogEntry"

// featureAnomaly is one featureAnomalies element.
type featureAnomaly struct {
	Feature              string
	Deviation, Threshold float64
}

// anomalyEntry is one model_monitoring_anomaly entry.
func anomalyEntry(project, resourceType string, labels map[string]string, payload map[string]any, at time.Time) *loggingpb.LogEntry {
	pb, err := structpb.NewStruct(payload)
	if err != nil {
		return nil
	}
	return &loggingpb.LogEntry{LogName: "projects/" + project + "/logs/" + AnomalyLogID, Severity: ltype.LogSeverity_WARNING,
		Resource:  &monitoredres.MonitoredResource{Type: resourceType, Labels: labels},
		Timestamp: timestamppb.New(at), Payload: &loggingpb.LogEntry_JsonPayload{JsonPayload: pb}}
}

func featureAnomalyList(fs []featureAnomaly) []any {
	sort.Slice(fs, func(i, j int) bool { return fs[i].Feature < fs[j].Feature })
	out := make([]any, 0, len(fs))
	for _, f := range fs {
		out = append(out, map[string]any{"featureDisplayName": f.Feature, "deviation": f.Deviation, "threshold": f.Threshold})
	}
	return out
}

// writeAnomalies writes entries through the logger, if there is one.
func (s *Server) writeAnomalies(ctx context.Context, entries []*loggingpb.LogEntry) {
	l := s.anomalyLog()
	if l == nil {
		return
	}
	for _, e := range entries {
		if e != nil {
			_ = l(ctx, e)
		}
	}
}

// AlertDelivery is one channel's delivery, as Cloud Monitoring recorded it.
type AlertDelivery struct {
	Channel, State, Detail string
}

// SetAlertNotifier delivers Model Monitoring's alerts through n.
func (s *Server) SetAlertNotifier(n AlertNotifier) {
	s.mu.Lock()
	s.alertNotifier = n
	s.mu.Unlock()
}

func (s *Server) notifier() AlertNotifier {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.alertNotifier
}

var channelNameRE = regexp.MustCompile(`^projects/[^/]+/notificationChannels/[^/]+$`)

// checkChannels validates notification channel names at field.
func checkChannels(field string, names []string) error {
	for i, n := range names {
		if !channelNameRE.MatchString(n) {
			return apierror.InvalidArgument("%s[%d] %q is not projects/<project>/notificationChannels/<channelId>", field, i, n)
		}
	}
	return nil
}

// checkNotificationSpec validates a v2 notification spec at field.
func checkNotificationSpec(field string, ns *betapb.ModelMonitoringNotificationSpec) error {
	var names []string
	for _, c := range ns.GetNotificationChannelConfigs() {
		names = append(names, c.GetNotificationChannel())
	}
	if err := checkChannels(field+".notification_channel_configs.notification_channel", names); err != nil {
		return err
	}
	return checkEmails(field+".email_config.user_emails", ns.GetEmailConfig().GetUserEmails())
}

var emailRE = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func checkEmails(field string, to []string) error {
	for i, e := range to {
		if !emailRE.MatchString(e) {
			return apierror.InvalidArgument("%s[%d] %q is not an email address", field, i, e)
		}
	}
	return nil
}

// notifyMonitorAlerts delivers a v2 run's alerts as its notification
// spec says.
func (s *Server) notifyMonitorAlerts(ctx context.Context, job string, ns *betapb.ModelMonitoringNotificationSpec, alerts []*mmAlert) {
	if ns == nil || len(alerts) == 0 {
		return
	}
	if ns.GetEnableCloudLogging() {
		s.writeAnomalies(ctx, monitorAnomalyEntries(job, alerts))
	}
	n := s.notifier()
	if n == nil {
		return
	}
	var raw []json.RawMessage
	var lines []string
	for _, a := range alerts {
		raw = append(raw, monJSON(a.Alert))
		ta := a.Alert.GetAnomaly().GetTabularAnomaly()
		lines = append(lines, fmt.Sprintf("- %s (%s): %s", a.Alert.GetStatsName(), a.Alert.GetObjectiveType(), ta.GetSummary()))
	}
	project := modelProject(job)
	subject := "Model Monitoring alert: " + lastSegment(job)
	text := fmt.Sprintf("The model monitoring job %s ran at %s and raised %d alerts:\n%s\n",
		job, time.Now().UTC().Format(time.RFC3339), len(alerts), strings.Join(lines, "\n"))
	if len(ns.GetNotificationChannelConfigs()) > 0 {
		var names []string
		for _, c := range ns.GetNotificationChannelConfigs() {
			names = append(names, c.GetNotificationChannel())
		}
		payload, _ := json.Marshal(map[string]any{"modelMonitoringJob": job, "alerts": raw})
		n.NotifyChannels(ctx, "model-monitoring", names, payload, subject, text)
	}
	if to := ns.GetEmailConfig().GetUserEmails(); len(to) > 0 {
		n.SendEmail(ctx, project, "model-monitoring", AlertsSender, to, subject, text)
	}
}

// notifyMDMAnomalies delivers a v1 run's anomalies as the job's alert
// config says.
func (s *Server) notifyMDMAnomalies(ctx context.Context, j *aiplatformpb.ModelDeploymentMonitoringJob, at time.Time, stats []*mdmStat) {
	ac := j.GetModelMonitoringAlertConfig()
	if ac == nil {
		return
	}
	if ac.GetEnableLogging() {
		s.writeAnomalies(ctx, mdmAnomalyEntries(j, at, stats))
	}
	n := s.notifier()
	if n == nil {
		return
	}
	var anomalies []map[string]any
	var lines []string
	for _, x := range stats {
		if x.Threshold > 0 && x.Prediction.GetScore() > x.Threshold {
			anomalies = append(anomalies, map[string]any{"deployedModelId": x.DeployedModel, "objective": x.Objective.String(),
				"featureDisplayName": x.Feature, "deviation": x.Prediction.GetScore(), "threshold": x.Threshold})
			lines = append(lines, fmt.Sprintf("- %s, deployed model %s, %s: the distance %g is above the alerting threshold %g",
				x.Feature, x.DeployedModel, x.Objective, x.Prediction.GetScore(), x.Threshold))
		}
	}
	if len(anomalies) == 0 {
		return
	}
	sort.Strings(lines)
	subject := "Model Monitoring anomalies: " + j.GetDisplayName()
	text := fmt.Sprintf("The model monitoring job %s (%s) ran at %s and detected %d anomalies:\n%s\n",
		j.GetDisplayName(), j.GetName(), at.UTC().Format(time.RFC3339), len(anomalies), strings.Join(lines, "\n"))
	if len(ac.GetNotificationChannels()) > 0 {
		payload, _ := json.Marshal(map[string]any{"modelDeploymentMonitoringJob": j.GetName(), "endpoint": j.GetEndpoint(),
			"runTime": at.UTC().Format(time.RFC3339), "anomalies": anomalies})
		n.NotifyChannels(ctx, "model-monitoring", ac.GetNotificationChannels(), payload, subject, text)
	}
	if to := ac.GetEmailAlertConfig().GetUserEmails(); len(to) > 0 {
		n.SendEmail(ctx, modelProject(j.GetName()), "model-monitoring", AlertsSender, to, subject, text)
	}
}

// locationOf is a resource name's location.
func locationOf(name string) string {
	parts := strings.Split(name, "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "locations" {
			return parts[i+1]
		}
	}
	return ""
}

// mdmAnomalyEntries are a v1 run's model_monitoring_anomaly entries: one
// for each deployed model and objective with an anomaly.
func mdmAnomalyEntries(j *aiplatformpb.ModelDeploymentMonitoringJob, at time.Time, stats []*mdmStat) []*loggingpb.LogEntry {
	type key struct {
		dm  string
		obj aiplatformpb.ModelDeploymentMonitoringObjectiveType
	}
	groups := map[key][]featureAnomaly{}
	var order []key
	for _, x := range stats {
		if x.Threshold <= 0 || x.Prediction.GetScore() <= x.Threshold {
			continue
		}
		k := key{x.DeployedModel, x.Objective}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], featureAnomaly{x.Feature, x.Prediction.GetScore(), x.Threshold})
	}
	sort.Slice(order, func(a, b int) bool {
		if order[a].dm != order[b].dm {
			return order[a].dm < order[b].dm
		}
		return order[a].obj < order[b].obj
	})
	project := modelProject(j.GetName())
	window := j.GetModelDeploymentMonitoringScheduleConfig().GetMonitorWindow().AsDuration()
	if window <= 0 {
		window = j.GetModelDeploymentMonitoringScheduleConfig().GetMonitorInterval().AsDuration()
	}
	labels := map[string]string{"model_deployment_monitoring_job": lastSegment(j.GetName()), "location": locationOf(j.GetName()),
		"resource_container": "projects/" + project}
	var out []*loggingpb.LogEntry
	for _, k := range order {
		fs := groups[k]
		out = append(out, anomalyEntry(project, "aiplatform.googleapis.com/ModelDeploymentMonitoringJob", labels, map[string]any{
			"@type": anomalyEntryType, "modelDeploymentMonitoringJob": j.GetName(), "deployedModelId": k.dm,
			"anomalyObjective": k.obj.String(), "featureAnomalies": featureAnomalyList(fs), "totalAnomaliesCount": float64(len(fs)),
			"startTime": at.Add(-window).UTC().Format(time.RFC3339Nano), "endTime": at.UTC().Format(time.RFC3339Nano)}, at))
	}
	return out
}

// monitorAnomalyEntries are a v2 run's model_monitoring_anomaly entries:
// one for each objective with an alert.
func monitorAnomalyEntries(job string, alerts []*mmAlert) []*loggingpb.LogEntry {
	groups := map[string][]featureAnomaly{}
	var order []string
	at := time.Now()
	for _, a := range alerts {
		obj := a.Alert.GetObjectiveType()
		if _, ok := groups[obj]; !ok {
			order = append(order, obj)
		}
		f := a.Alert.GetAnomaly().GetTabularAnomaly().GetAnomaly().GetStructValue().GetFields()
		groups[obj] = append(groups[obj], featureAnomaly{a.Alert.GetStatsName(), f["distance"].GetNumberValue(), f["threshold"].GetNumberValue()})
		if t := a.Alert.GetAlertTime(); t != nil {
			at = t.AsTime()
		}
	}
	sort.Strings(order)
	project := modelProject(job)
	monitor := job
	if i := strings.Index(job, "/modelMonitoringJobs/"); i >= 0 {
		monitor = job[:i]
	}
	labels := map[string]string{"model_monitor": lastSegment(monitor), "location": locationOf(job), "resource_container": "projects/" + project}
	var out []*loggingpb.LogEntry
	for _, obj := range order {
		fs := groups[obj]
		out = append(out, anomalyEntry(project, "aiplatform.googleapis.com/ModelMonitor", labels, map[string]any{
			"@type": anomalyEntryType, "modelMonitoringJob": job, "modelMonitor": monitor, "anomalyObjective": obj,
			"featureAnomalies": featureAnomalyList(fs), "totalAnomaliesCount": float64(len(fs)), "endTime": at.UTC().Format(time.RFC3339Nano)}, at))
	}
	return out
}
