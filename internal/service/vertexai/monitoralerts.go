package vertexai

// Model Monitoring alert delivery (#2214): a run's alerts go to the
// notification spec's Cloud Monitoring notification channels (a Pub/Sub
// channel publishes to this instance's Pub/Sub, a local webhook is posted
// to, other types are recorded) and to its email addresses (recorded with
// Cloud Monitoring's deliveries: CloudBurrow sends no email off the
// machine).
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
//     channels".
//
// UNVERIFIED (Google does not document them; re-read 2026-11-24): the email's subject and
// layout; the channel payload (here {"modelMonitoringJob", "alerts"} with
// each alert as its JSON, or v1's job and anomalies).

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

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// AlertNotifier delivers Model Monitoring's alerts: Cloud Monitoring's
// server, on an instance that serves it.
type AlertNotifier interface {
	// NotifyChannels delivers payload to the notification channels names.
	NotifyChannels(ctx context.Context, kind string, names []string, payload []byte) []AlertDelivery
	// RecordEmail records an email to addresses in project.
	RecordEmail(project, kind string, to []string, subject, body string)
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
	n := s.notifier()
	if n == nil || ns == nil || len(alerts) == 0 {
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
	if len(ns.GetNotificationChannelConfigs()) > 0 {
		var names []string
		for _, c := range ns.GetNotificationChannelConfigs() {
			names = append(names, c.GetNotificationChannel())
		}
		payload, _ := json.Marshal(map[string]any{"modelMonitoringJob": job, "alerts": raw})
		n.NotifyChannels(ctx, "model-monitoring", names, payload)
	}
	if to := ns.GetEmailConfig().GetUserEmails(); len(to) > 0 {
		n.RecordEmail(project, "model-monitoring", to, "Model Monitoring alert: "+lastSegment(job),
			fmt.Sprintf("The model monitoring job %s ran at %s and raised %d alerts:\n%s\n",
				job, time.Now().UTC().Format(time.RFC3339), len(alerts), strings.Join(lines, "\n")))
	}
}

// notifyMDMAnomalies delivers a v1 run's anomalies as the job's alert
// config says.
func (s *Server) notifyMDMAnomalies(ctx context.Context, j *aiplatformpb.ModelDeploymentMonitoringJob, at time.Time, stats []*mdmStat) {
	n := s.notifier()
	ac := j.GetModelMonitoringAlertConfig()
	if n == nil || ac == nil {
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
	if len(ac.GetNotificationChannels()) > 0 {
		payload, _ := json.Marshal(map[string]any{"modelDeploymentMonitoringJob": j.GetName(), "endpoint": j.GetEndpoint(),
			"runTime": at.UTC().Format(time.RFC3339), "anomalies": anomalies})
		n.NotifyChannels(ctx, "model-monitoring", ac.GetNotificationChannels(), payload)
	}
	if to := ac.GetEmailAlertConfig().GetUserEmails(); len(to) > 0 {
		n.RecordEmail(modelProject(j.GetName()), "model-monitoring", to, "Model Monitoring anomalies: "+j.GetDisplayName(),
			fmt.Sprintf("The model monitoring job %s (%s) ran at %s and detected %d anomalies:\n%s\n",
				j.GetDisplayName(), j.GetName(), at.UTC().Format(time.RFC3339), len(anomalies), strings.Join(lines, "\n")))
	}
}
