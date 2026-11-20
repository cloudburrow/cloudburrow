//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/logging"
	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	dashboard "cloud.google.com/go/monitoring/dashboard/apiv1"
	"cloud.google.com/go/monitoring/dashboard/apiv1/dashboardpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func monitoringOpts(h *Harness) []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(h.Endpoint(EnvMonitoring)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

// webhook is a local receiver of Monitoring's notifications.
type webhook struct {
	mu     sync.Mutex
	bodies []map[string]any
	auth   []string
	srv    *httptest.Server
}

func newWebhook(t *testing.T) *webhook {
	w := &webhook{}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		w.mu.Lock()
		w.bodies = append(w.bodies, m)
		w.auth = append(w.auth, r.Header.Get("Authorization"))
		w.mu.Unlock()
		_, _ = io.WriteString(rw, "ok healthy")
	}))
	t.Cleanup(w.srv.Close)
	return w
}

// wait returns the first incident body in state within d.
func (w *webhook) wait(t *testing.T, state string, d time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		for _, b := range w.bodies {
			if in, ok := b["incident"].(map[string]any); ok && in["state"] == state {
				w.mu.Unlock()
				return in
			}
		}
		w.mu.Unlock()
		time.Sleep(time.Second)
	}
	t.Fatalf("no %s incident reached the webhook in %s", state, d)
	return nil
}

func writeGauge(t *testing.T, h *Harness, c *monitoring.MetricClient, metric string, v float64) {
	t.Helper()
	err := c.CreateTimeSeries(h.Context(), &monitoringpb.CreateTimeSeriesRequest{Name: "projects/" + h.Project(), TimeSeries: []*monitoringpb.TimeSeries{{
		Metric: &metricpb.Metric{Type: metric}, Resource: &mrpb.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": h.Project()}},
		Points: []*monitoringpb.Point{{Interval: &monitoringpb.TimeInterval{EndTime: timestamppb.Now()},
			Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: v}}}}}}})
	if err != nil {
		t.Fatalf("CreateTimeSeries: %v", err)
	}
}

// TestMonitoringAlertPoliciesAndChannels (#1964) through
// cloud.google.com/go/monitoring/apiv3/v2:
//   - the channel descriptors are listed and one is got;
//   - a webhook_tokenauth channel to a local receiver, a basic-auth one,
//     and an email one (UNVERIFIED, its verification code recorded, never
//     sent) are created, listed by a type filter, got and updated;
//   - a threshold policy over a custom gauge is created, listed, got and
//     updated with a mask; a point above the threshold opens an incident
//     the webhooks receive in Google's 1.2 schema, with basic auth; a point
//     below closes it;
//   - deleting a channel in use is FAILED_PRECONDITION, with force it is
//     removed from the policy; an MQL condition is UNIMPLEMENTED;
//   - the policy and the channels are deleted.
//
// covers: google.monitoring.v3.NotificationChannelService/ListNotificationChannelDescriptors, google.monitoring.v3.NotificationChannelService/GetNotificationChannelDescriptor, google.monitoring.v3.NotificationChannelService/CreateNotificationChannel, google.monitoring.v3.NotificationChannelService/ListNotificationChannels, google.monitoring.v3.NotificationChannelService/GetNotificationChannel, google.monitoring.v3.NotificationChannelService/UpdateNotificationChannel, google.monitoring.v3.NotificationChannelService/DeleteNotificationChannel, google.monitoring.v3.NotificationChannelService/SendNotificationChannelVerificationCode, google.monitoring.v3.AlertPolicyService/CreateAlertPolicy, google.monitoring.v3.AlertPolicyService/ListAlertPolicies, google.monitoring.v3.AlertPolicyService/GetAlertPolicy, google.monitoring.v3.AlertPolicyService/UpdateAlertPolicy, google.monitoring.v3.AlertPolicyService/DeleteAlertPolicy
func TestMonitoringAlertPoliciesAndChannels(t *testing.T) {
	h := New(t)
	opts := monitoringOpts(h)
	ctx := h.Context()
	mc := monitoringClient(t, h)
	nc, err := monitoring.NewNotificationChannelClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	ac, err := monitoring.NewAlertPolicyClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	parent := "projects/" + h.Project()

	var types []string
	for it := nc.ListNotificationChannelDescriptors(ctx, &monitoringpb.ListNotificationChannelDescriptorsRequest{Name: parent}); ; {
		d, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListNotificationChannelDescriptors: %v", err)
		}
		types = append(types, d.GetType())
	}
	if !strings.Contains(strings.Join(types, ","), "webhook_tokenauth") || !strings.Contains(strings.Join(types, ","), "pubsub") {
		t.Errorf("channel types %v", types)
	}
	if d, err := nc.GetNotificationChannelDescriptor(ctx, &monitoringpb.GetNotificationChannelDescriptorRequest{Name: parent + "/notificationChannelDescriptors/email"}); err != nil || d.GetLabels()[0].GetKey() != "email_address" {
		t.Errorf("GetNotificationChannelDescriptor: %v, %v", d, err)
	}

	hook := newWebhook(t)
	tok, err := nc.CreateNotificationChannel(ctx, &monitoringpb.CreateNotificationChannelRequest{Name: parent, NotificationChannel: &monitoringpb.NotificationChannel{
		Type: "webhook_tokenauth", DisplayName: "local hook", Labels: map[string]string{"url": hook.srv.URL + "/alerts?token=s3cret"}}})
	if err != nil {
		t.Fatalf("CreateNotificationChannel: %v", err)
	}
	defer nc.DeleteNotificationChannel(ctx, &monitoringpb.DeleteNotificationChannelRequest{Name: tok.GetName(), Force: true})
	basic, err := nc.CreateNotificationChannel(ctx, &monitoringpb.CreateNotificationChannelRequest{Name: parent, NotificationChannel: &monitoringpb.NotificationChannel{
		Type: "webhook_basicauth", DisplayName: "basic hook", Labels: map[string]string{"url": hook.srv.URL + "/basic", "username": "u", "password": "pw"}}})
	if err != nil {
		t.Fatalf("CreateNotificationChannel basic: %v", err)
	}
	defer nc.DeleteNotificationChannel(ctx, &monitoringpb.DeleteNotificationChannelRequest{Name: basic.GetName(), Force: true})
	if basic.GetLabels()["password"] == "pw" {
		t.Errorf("the password is returned in clear: %v", basic.GetLabels())
	}
	mail, err := nc.CreateNotificationChannel(ctx, &monitoringpb.CreateNotificationChannelRequest{Name: parent, NotificationChannel: &monitoringpb.NotificationChannel{
		Type: "email", DisplayName: "oncall", Labels: map[string]string{"email_address": "oncall@example.com"}}})
	if err != nil || mail.GetVerificationStatus() != monitoringpb.NotificationChannel_UNVERIFIED {
		t.Fatalf("email channel %v, %v", mail, err)
	}
	if err := nc.SendNotificationChannelVerificationCode(ctx, &monitoringpb.SendNotificationChannelVerificationCodeRequest{Name: mail.GetName()}); err != nil {
		t.Errorf("SendNotificationChannelVerificationCode: %v", err)
	}
	_, err = nc.CreateNotificationChannel(ctx, &monitoringpb.CreateNotificationChannelRequest{Name: parent, NotificationChannel: &monitoringpb.NotificationChannel{Type: "carrier_pigeon"}})
	wantMonitoringCode(t, "an unknown type", err, codes.InvalidArgument, "unknown notification channel type")
	var listed int
	for it := nc.ListNotificationChannels(ctx, &monitoringpb.ListNotificationChannelsRequest{Name: parent, Filter: `type = "email"`}); ; {
		c, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListNotificationChannels: %v", err)
		}
		if c.GetType() != "email" {
			t.Errorf("the type filter listed %s", c.GetType())
		}
		listed++
	}
	if listed == 0 {
		t.Error("ListNotificationChannels by type listed nothing")
	}
	mail.DisplayName = "on call"
	if u, err := nc.UpdateNotificationChannel(ctx, &monitoringpb.UpdateNotificationChannelRequest{NotificationChannel: mail,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); err != nil || u.GetDisplayName() != "on call" {
		t.Errorf("UpdateNotificationChannel: %v, %v", u, err)
	}
	if g, err := nc.GetNotificationChannel(ctx, &monitoringpb.GetNotificationChannelRequest{Name: mail.GetName()}); err != nil || g.GetDisplayName() != "on call" {
		t.Errorf("GetNotificationChannel: %v, %v", g, err)
	}

	metric := fmt.Sprintf("custom.googleapis.com/compat/queue_depth_%d", time.Now().UnixNano()%1e6)
	writeGauge(t, h, mc, metric, 1)
	cond := func(threshold float64) *monitoringpb.AlertPolicy_Condition {
		return &monitoringpb.AlertPolicy_Condition{DisplayName: "queue depth high", Condition: &monitoringpb.AlertPolicy_Condition_ConditionThreshold{
			ConditionThreshold: &monitoringpb.AlertPolicy_Condition_MetricThreshold{Filter: `metric.type = "` + metric + `" AND resource.type = "global"`,
				Comparison: monitoringpb.ComparisonType_COMPARISON_GT, ThresholdValue: threshold, Duration: durationpb.New(0)}}}
	}
	p, err := ac.CreateAlertPolicy(ctx, &monitoringpb.CreateAlertPolicyRequest{Name: parent, AlertPolicy: &monitoringpb.AlertPolicy{
		DisplayName: "Queue too deep", Combiner: monitoringpb.AlertPolicy_OR, Conditions: []*monitoringpb.AlertPolicy_Condition{cond(1000)},
		NotificationChannels: []string{tok.GetName(), basic.GetName(), mail.GetName()}, UserLabels: map[string]string{"team": "payments"},
		Documentation: &monitoringpb.AlertPolicy_Documentation{Content: "Drain the queue."}}})
	if err != nil {
		t.Fatalf("CreateAlertPolicy: %v", err)
	}
	defer ac.DeleteAlertPolicy(ctx, &monitoringpb.DeleteAlertPolicyRequest{Name: p.GetName()})
	if !p.GetEnabled().GetValue() || !strings.HasPrefix(p.GetConditions()[0].GetName(), p.GetName()+"/conditions/") || p.GetDocumentation().GetMimeType() != "text/markdown" {
		t.Errorf("created policy %v", p)
	}
	found := false
	for it := ac.ListAlertPolicies(ctx, &monitoringpb.ListAlertPoliciesRequest{Name: parent, Filter: `user_labels.team = "payments"`}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListAlertPolicies: %v", err)
		}
		found = found || x.GetName() == p.GetName()
	}
	if !found {
		t.Error("ListAlertPolicies by a user label did not list the policy")
	}
	// Lower the threshold with a mask; the point then violates it.
	p.Conditions = []*monitoringpb.AlertPolicy_Condition{cond(50)}
	p.Conditions[0].Name = ""
	if p, err = ac.UpdateAlertPolicy(ctx, &monitoringpb.UpdateAlertPolicyRequest{AlertPolicy: p, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"conditions"}}}); err != nil {
		t.Fatalf("UpdateAlertPolicy: %v", err)
	}
	if g, err := ac.GetAlertPolicy(ctx, &monitoringpb.GetAlertPolicyRequest{Name: p.GetName()}); err != nil || g.GetConditions()[0].GetConditionThreshold().GetThresholdValue() != 50 {
		t.Fatalf("GetAlertPolicy: %v, %v", g, err)
	}
	time.Sleep(6 * time.Second) // at most one point per 5 seconds per series
	writeGauge(t, h, mc, metric, 99)
	in := hook.wait(t, "open", 90*time.Second)
	if in["policy_name"] != "Queue too deep" || in["observed_value"] != "99" || in["threshold_value"] != "50" || in["scoping_project_id"] != h.Project() {
		t.Errorf("the open incident %v", in)
	}
	hook.mu.Lock()
	sawBasic := false
	for _, a := range hook.auth {
		sawBasic = sawBasic || strings.HasPrefix(a, "Basic ")
	}
	hook.mu.Unlock()
	if !sawBasic {
		t.Error("the basic-auth webhook was not sent its credentials")
	}
	time.Sleep(6 * time.Second) // the next point of the series, at most one per 5 seconds
	writeGauge(t, h, mc, metric, 2)
	hook.wait(t, "closed", 90*time.Second)

	err = nc.DeleteNotificationChannel(ctx, &monitoringpb.DeleteNotificationChannelRequest{Name: mail.GetName()})
	wantMonitoringCode(t, "deleting a channel in use", err, codes.FailedPrecondition, "referenced")
	if err := nc.DeleteNotificationChannel(ctx, &monitoringpb.DeleteNotificationChannelRequest{Name: mail.GetName(), Force: true}); err != nil {
		t.Errorf("DeleteNotificationChannel force: %v", err)
	}
	if g, _ := ac.GetAlertPolicy(ctx, &monitoringpb.GetAlertPolicyRequest{Name: p.GetName()}); len(g.GetNotificationChannels()) != 2 {
		t.Errorf("after the forced delete the policy names %v", g.GetNotificationChannels())
	}
	_, err = ac.CreateAlertPolicy(ctx, &monitoringpb.CreateAlertPolicyRequest{Name: parent, AlertPolicy: &monitoringpb.AlertPolicy{DisplayName: "mql",
		Conditions: []*monitoringpb.AlertPolicy_Condition{{DisplayName: "q", Condition: &monitoringpb.AlertPolicy_Condition_ConditionMonitoringQueryLanguage{
			ConditionMonitoringQueryLanguage: &monitoringpb.AlertPolicy_Condition_MonitoringQueryLanguageCondition{Query: "fetch global"}}}}}})
	wantMonitoringCode(t, "an MQL condition", err, codes.Unimplemented, "Monitoring Query Language")
	if err := ac.DeleteAlertPolicy(ctx, &monitoringpb.DeleteAlertPolicyRequest{Name: p.GetName()}); err != nil {
		t.Errorf("DeleteAlertPolicy: %v", err)
	}
	_, err = ac.GetAlertPolicy(ctx, &monitoringpb.GetAlertPolicyRequest{Name: p.GetName()})
	wantMonitoringCode(t, "a deleted policy", err, codes.NotFound, "not found")
}

// TestMonitoringLogMatchAlert (#1964): a log-match policy over the local
// Cloud Logging notifies a webhook of an entry its filter matches, with its
// label extractors applied.
//
// covers: google.monitoring.v3.AlertPolicyService/CreateAlertPolicy
func TestMonitoringLogMatchAlert(t *testing.T) {
	h := New(t)
	opts := monitoringOpts(h)
	ctx := h.Context()
	logAddr := h.Endpoint(EnvLogging)
	nc, err := monitoring.NewNotificationChannelClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	ac, err := monitoring.NewAlertPolicyClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	parent := "projects/" + h.Project()
	hook := newWebhook(t)
	ch, err := nc.CreateNotificationChannel(ctx, &monitoringpb.CreateNotificationChannelRequest{Name: parent, NotificationChannel: &monitoringpb.NotificationChannel{
		Type: "webhook_tokenauth", Labels: map[string]string{"url": hook.srv.URL + "/logs"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer nc.DeleteNotificationChannel(ctx, &monitoringpb.DeleteNotificationChannelRequest{Name: ch.GetName(), Force: true})
	logID := fmt.Sprintf("payments-%d", time.Now().UnixNano()%1e6)
	p, err := ac.CreateAlertPolicy(ctx, &monitoringpb.CreateAlertPolicyRequest{Name: parent, AlertPolicy: &monitoringpb.AlertPolicy{
		DisplayName: "Payment failures", Combiner: monitoringpb.AlertPolicy_OR, NotificationChannels: []string{ch.GetName()},
		AlertStrategy: &monitoringpb.AlertPolicy_AlertStrategy{NotificationRateLimit: &monitoringpb.AlertPolicy_AlertStrategy_NotificationRateLimit{Period: durationpb.New(5 * time.Minute)}},
		Conditions: []*monitoringpb.AlertPolicy_Condition{{DisplayName: "payment failed", Condition: &monitoringpb.AlertPolicy_Condition_ConditionMatchedLog{
			ConditionMatchedLog: &monitoringpb.AlertPolicy_Condition_LogMatch{Filter: fmt.Sprintf(`logName="%s/logs/%s" AND severity>=ERROR`, parent, logID),
				LabelExtractors: map[string]string{"card": "EXTRACT(jsonPayload.card)"}}}}}}})
	if err != nil {
		t.Fatalf("CreateAlertPolicy log match: %v", err)
	}
	defer ac.DeleteAlertPolicy(ctx, &monitoringpb.DeleteAlertPolicyRequest{Name: p.GetName()})
	lc, err := logging.NewClient(ctx, parent, option.WithEndpoint(logAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer lc.Close()
	lg := lc.Logger(logID, logging.CommonResource(&mrpb.MonitoredResource{Type: "global"}))
	if err := lg.LogSync(ctx, logging.Entry{Severity: logging.Info, Payload: map[string]any{"card": "visa"}}); err != nil {
		t.Fatal(err)
	}
	if err := lg.LogSync(ctx, logging.Entry{Severity: logging.Error, Payload: map[string]any{"card": "amex", "msg": "declined"}}); err != nil {
		t.Fatal(err)
	}
	in := hook.wait(t, "open", 90*time.Second)
	if in["condition_name"] != "payment failed" || !strings.Contains(fmt.Sprint(in["summary"]), "declined") {
		t.Errorf("the log-match incident %v", in)
	}
	if labels, _ := in["metric"].(map[string]any)["labels"].(map[string]any); labels["card"] != "amex" {
		t.Errorf("the extracted labels %v", in["metric"])
	}
}

// TestMonitoringUptimeGroupsServicesSnoozes (#1964): an HTTP uptime check
// of a local server runs and writes check_passed; the checker IPs are
// listed; groups nest and list their members; a service and an SLO are
// created, listed, updated and deleted; a snooze is created, listed, got
// and ended.
//
// covers: google.monitoring.v3.UptimeCheckService/CreateUptimeCheckConfig, google.monitoring.v3.UptimeCheckService/GetUptimeCheckConfig, google.monitoring.v3.UptimeCheckService/ListUptimeCheckConfigs, google.monitoring.v3.UptimeCheckService/UpdateUptimeCheckConfig, google.monitoring.v3.UptimeCheckService/DeleteUptimeCheckConfig, google.monitoring.v3.UptimeCheckService/ListUptimeCheckIps, google.monitoring.v3.GroupService/CreateGroup, google.monitoring.v3.GroupService/GetGroup, google.monitoring.v3.GroupService/ListGroups, google.monitoring.v3.GroupService/UpdateGroup, google.monitoring.v3.GroupService/DeleteGroup, google.monitoring.v3.GroupService/ListGroupMembers, google.monitoring.v3.ServiceMonitoringService/CreateService, google.monitoring.v3.ServiceMonitoringService/GetService, google.monitoring.v3.ServiceMonitoringService/ListServices, google.monitoring.v3.ServiceMonitoringService/UpdateService, google.monitoring.v3.ServiceMonitoringService/DeleteService, google.monitoring.v3.ServiceMonitoringService/CreateServiceLevelObjective, google.monitoring.v3.ServiceMonitoringService/GetServiceLevelObjective, google.monitoring.v3.ServiceMonitoringService/ListServiceLevelObjectives, google.monitoring.v3.ServiceMonitoringService/UpdateServiceLevelObjective, google.monitoring.v3.ServiceMonitoringService/DeleteServiceLevelObjective, google.monitoring.v3.SnoozeService/CreateSnooze, google.monitoring.v3.SnoozeService/ListSnoozes, google.monitoring.v3.SnoozeService/GetSnooze, google.monitoring.v3.SnoozeService/UpdateSnooze
func TestMonitoringUptimeGroupsServicesSnoozes(t *testing.T) {
	h := New(t)
	opts := monitoringOpts(h)
	ctx := h.Context()
	mc := monitoringClient(t, h)
	parent := "projects/" + h.Project()
	uc, err := monitoring.NewUptimeCheckClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	hook := newWebhook(t)
	u, _ := url.Parse(hook.srv.URL)
	port, _ := strconv.Atoi(u.Port())
	check, err := uc.CreateUptimeCheckConfig(ctx, &monitoringpb.CreateUptimeCheckConfigRequest{Parent: parent, UptimeCheckConfig: &monitoringpb.UptimeCheckConfig{
		DisplayName: "Health page", Period: durationpb.New(time.Minute), Timeout: durationpb.New(5 * time.Second),
		Resource: &monitoringpb.UptimeCheckConfig_MonitoredResource{MonitoredResource: &mrpb.MonitoredResource{Type: "uptime_url",
			Labels: map[string]string{"host": u.Hostname(), "project_id": h.Project()}}},
		CheckRequestType: &monitoringpb.UptimeCheckConfig_HttpCheck_{HttpCheck: &monitoringpb.UptimeCheckConfig_HttpCheck{Port: int32(port), Path: "/healthz"}},
		ContentMatchers:  []*monitoringpb.UptimeCheckConfig_ContentMatcher{{Content: "healthy"}}}})
	if err != nil {
		t.Fatalf("CreateUptimeCheckConfig: %v", err)
	}
	defer uc.DeleteUptimeCheckConfig(ctx, &monitoringpb.DeleteUptimeCheckConfigRequest{Name: check.GetName()})
	if !strings.HasPrefix(check.GetName(), parent+"/uptimeCheckConfigs/health-page-") {
		t.Errorf("uptime check name %s", check.GetName())
	}
	_, err = uc.CreateUptimeCheckConfig(ctx, &monitoringpb.CreateUptimeCheckConfigRequest{Parent: parent, UptimeCheckConfig: &monitoringpb.UptimeCheckConfig{
		DisplayName: "bad period", Period: durationpb.New(2 * time.Minute), Timeout: durationpb.New(time.Second),
		Resource:         &monitoringpb.UptimeCheckConfig_MonitoredResource{MonitoredResource: &mrpb.MonitoredResource{Type: "uptime_url", Labels: map[string]string{"host": "localhost"}}},
		CheckRequestType: &monitoringpb.UptimeCheckConfig_TcpCheck_{TcpCheck: &monitoringpb.UptimeCheckConfig_TcpCheck{Port: 80}}}})
	wantMonitoringCode(t, "a 2-minute period", err, codes.InvalidArgument, "60s, 300s, 600s or 900s")
	check.DisplayName = "Health page (v2)"
	if c, err := uc.UpdateUptimeCheckConfig(ctx, &monitoringpb.UpdateUptimeCheckConfigRequest{UptimeCheckConfig: check,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); err != nil || c.GetDisplayName() != "Health page (v2)" {
		t.Errorf("UpdateUptimeCheckConfig: %v, %v", c, err)
	}
	if g, err := uc.GetUptimeCheckConfig(ctx, &monitoringpb.GetUptimeCheckConfigRequest{Name: check.GetName()}); err != nil || g.GetHttpCheck().GetPath() != "/healthz" {
		t.Errorf("GetUptimeCheckConfig: %v, %v", g, err)
	}
	if c, err := uc.ListUptimeCheckConfigs(ctx, &monitoringpb.ListUptimeCheckConfigsRequest{Parent: parent}).Next(); err != nil || c.GetName() == "" {
		t.Errorf("ListUptimeCheckConfigs: %v", err)
	}
	if ip, err := uc.ListUptimeCheckIps(ctx, &monitoringpb.ListUptimeCheckIpsRequest{}).Next(); err != nil || ip.GetIpAddress() == "" {
		t.Errorf("ListUptimeCheckIps: %v, %v", ip, err)
	}
	checkID := check.GetName()[strings.LastIndex(check.GetName(), "/")+1:]
	passed := false
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline) && !passed; time.Sleep(2 * time.Second) {
		resp := listAll(t, mc.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: parent,
			Filter:   `metric.type = "monitoring.googleapis.com/uptime_check/check_passed" AND metric.labels.check_id = "` + checkID + `"`,
			Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(time.Now().Add(-10 * time.Minute)), EndTime: timestamppb.Now()}}))
		passed = len(resp) == 1 && len(resp[0].GetPoints()) > 0 && resp[0].GetPoints()[0].GetValue().GetBoolValue()
	}
	if !passed {
		t.Error("no passing check_passed point of the uptime check in 90s")
	}

	gc, err := monitoring.NewGroupClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer gc.Close()
	metric := fmt.Sprintf("custom.googleapis.com/compat/group_probe_%d", time.Now().UnixNano()%1e6)
	err = mc.CreateTimeSeries(ctx, &monitoringpb.CreateTimeSeriesRequest{Name: parent, TimeSeries: []*monitoringpb.TimeSeries{{
		Metric: &metricpb.Metric{Type: metric}, Resource: &mrpb.MonitoredResource{Type: "generic_node",
			Labels: map[string]string{"project_id": h.Project(), "location": "us-central1", "namespace": "web", "node_id": "web-1"}},
		Points: []*monitoringpb.Point{{Interval: &monitoringpb.TimeInterval{EndTime: timestamppb.Now()},
			Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: 1}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	top, err := gc.CreateGroup(ctx, &monitoringpb.CreateGroupRequest{Name: parent, Group: &monitoringpb.Group{DisplayName: "nodes", Filter: `resource.type = "generic_node"`}})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	child, err := gc.CreateGroup(ctx, &monitoringpb.CreateGroupRequest{Name: parent, Group: &monitoringpb.Group{DisplayName: "web",
		ParentName: top.GetName(), Filter: `resource.labels.namespace = "web"`}})
	if err != nil {
		t.Fatalf("CreateGroup child: %v", err)
	}
	if g, err := gc.ListGroups(ctx, &monitoringpb.ListGroupsRequest{Name: parent, Filter: &monitoringpb.ListGroupsRequest_ChildrenOfGroup{ChildrenOfGroup: top.GetName()}}).Next(); err != nil || g.GetName() != child.GetName() {
		t.Errorf("ListGroups children: %v, %v", g, err)
	}
	m, err := gc.ListGroupMembers(ctx, &monitoringpb.ListGroupMembersRequest{Name: child.GetName()}).Next()
	if err != nil || m.GetLabels()["node_id"] != "web-1" {
		t.Errorf("ListGroupMembers: %v, %v", m, err)
	}
	child.DisplayName = "web tier"
	if g, err := gc.UpdateGroup(ctx, &monitoringpb.UpdateGroupRequest{Group: child}); err != nil || g.GetDisplayName() != "web tier" {
		t.Errorf("UpdateGroup: %v, %v", g, err)
	}
	if g, err := gc.GetGroup(ctx, &monitoringpb.GetGroupRequest{Name: child.GetName()}); err != nil || g.GetParentName() != top.GetName() {
		t.Errorf("GetGroup: %v, %v", g, err)
	}
	err = gc.DeleteGroup(ctx, &monitoringpb.DeleteGroupRequest{Name: top.GetName()})
	wantMonitoringCode(t, "deleting a parent group", err, codes.FailedPrecondition, "recursive")
	if err := gc.DeleteGroup(ctx, &monitoringpb.DeleteGroupRequest{Name: top.GetName(), Recursive: true}); err != nil {
		t.Errorf("DeleteGroup recursive: %v", err)
	}

	sc, err := monitoring.NewServiceMonitoringClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	svcID := fmt.Sprintf("checkout-%d", time.Now().UnixNano()%1e6)
	svc, err := sc.CreateService(ctx, &monitoringpb.CreateServiceRequest{Parent: parent, ServiceId: svcID, Service: &monitoringpb.Service{
		DisplayName: "Checkout", Identifier: &monitoringpb.Service_Custom_{Custom: &monitoringpb.Service_Custom{}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	slo, err := sc.CreateServiceLevelObjective(ctx, &monitoringpb.CreateServiceLevelObjectiveRequest{Parent: svc.GetName(), ServiceLevelObjectiveId: "availability",
		ServiceLevelObjective: &monitoringpb.ServiceLevelObjective{DisplayName: "99.9% good", Goal: 0.999,
			Period: &monitoringpb.ServiceLevelObjective_RollingPeriod{RollingPeriod: durationpb.New(28 * 24 * time.Hour)},
			ServiceLevelIndicator: &monitoringpb.ServiceLevelIndicator{Type: &monitoringpb.ServiceLevelIndicator_RequestBased{RequestBased: &monitoringpb.RequestBasedSli{
				Method: &monitoringpb.RequestBasedSli_GoodTotalRatio{GoodTotalRatio: &monitoringpb.TimeSeriesRatio{
					GoodServiceFilter:  `metric.type = "serviceruntime.googleapis.com/api/request_count" AND metric.labels.response_code_class = "2xx"`,
					TotalServiceFilter: `metric.type = "serviceruntime.googleapis.com/api/request_count"`}}}}}}})
	if err != nil {
		t.Fatalf("CreateServiceLevelObjective: %v", err)
	}
	_, err = sc.CreateServiceLevelObjective(ctx, &monitoringpb.CreateServiceLevelObjectiveRequest{Parent: svc.GetName(),
		ServiceLevelObjective: &monitoringpb.ServiceLevelObjective{Goal: 1.5, Period: &monitoringpb.ServiceLevelObjective_RollingPeriod{RollingPeriod: durationpb.New(24 * time.Hour)}}})
	wantMonitoringCode(t, "a goal of 1.5", err, codes.InvalidArgument, "goal")
	if s, err := sc.ListServices(ctx, &monitoringpb.ListServicesRequest{Parent: parent}).Next(); err != nil || s.GetName() == "" {
		t.Errorf("ListServices: %v", err)
	}
	if o, err := sc.ListServiceLevelObjectives(ctx, &monitoringpb.ListServiceLevelObjectivesRequest{Parent: svc.GetName()}).Next(); err != nil || o.GetName() != slo.GetName() {
		t.Errorf("ListServiceLevelObjectives: %v, %v", o, err)
	}
	slo.Goal = 0.99
	if o, err := sc.UpdateServiceLevelObjective(ctx, &monitoringpb.UpdateServiceLevelObjectiveRequest{ServiceLevelObjective: slo,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"goal"}}}); err != nil || o.GetGoal() != 0.99 {
		t.Errorf("UpdateServiceLevelObjective: %v, %v", o, err)
	}
	if o, err := sc.GetServiceLevelObjective(ctx, &monitoringpb.GetServiceLevelObjectiveRequest{Name: slo.GetName()}); err != nil || o.GetGoal() != 0.99 {
		t.Errorf("GetServiceLevelObjective: %v, %v", o, err)
	}
	svc.DisplayName = "Checkout API"
	if s, err := sc.UpdateService(ctx, &monitoringpb.UpdateServiceRequest{Service: svc, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); err != nil || s.GetDisplayName() != "Checkout API" {
		t.Errorf("UpdateService: %v, %v", s, err)
	}
	if s, err := sc.GetService(ctx, &monitoringpb.GetServiceRequest{Name: svc.GetName()}); err != nil || s.GetCustom() == nil {
		t.Errorf("GetService: %v, %v", s, err)
	}
	if err := sc.DeleteServiceLevelObjective(ctx, &monitoringpb.DeleteServiceLevelObjectiveRequest{Name: slo.GetName()}); err != nil {
		t.Errorf("DeleteServiceLevelObjective: %v", err)
	}
	if err := sc.DeleteService(ctx, &monitoringpb.DeleteServiceRequest{Name: svc.GetName()}); err != nil {
		t.Errorf("DeleteService: %v", err)
	}

	ac, err := monitoring.NewAlertPolicyClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	p, err := ac.CreateAlertPolicy(ctx, &monitoringpb.CreateAlertPolicyRequest{Name: parent, AlertPolicy: &monitoringpb.AlertPolicy{DisplayName: "uptime failing",
		Conditions: []*monitoringpb.AlertPolicy_Condition{{DisplayName: "check failed", Condition: &monitoringpb.AlertPolicy_Condition_ConditionThreshold{
			ConditionThreshold: &monitoringpb.AlertPolicy_Condition_MetricThreshold{Filter: `metric.type = "monitoring.googleapis.com/uptime_check/check_passed" AND resource.type = "uptime_url"`,
				Comparison: monitoringpb.ComparisonType_COMPARISON_LT, ThresholdValue: 1, Duration: durationpb.New(time.Minute)}}}}}})
	if err != nil {
		t.Fatalf("CreateAlertPolicy: %v", err)
	}
	defer ac.DeleteAlertPolicy(ctx, &monitoringpb.DeleteAlertPolicyRequest{Name: p.GetName()})
	snc, err := monitoring.NewSnoozeClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer snc.Close()
	sn, err := snc.CreateSnooze(ctx, &monitoringpb.CreateSnoozeRequest{Parent: parent, Snooze: &monitoringpb.Snooze{DisplayName: "maintenance",
		Criteria: &monitoringpb.Snooze_Criteria{Policies: []string{p.GetName()}},
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.Now(), EndTime: timestamppb.New(time.Now().Add(time.Hour))}}})
	if err != nil {
		t.Fatalf("CreateSnooze: %v", err)
	}
	if s, err := snc.ListSnoozes(ctx, &monitoringpb.ListSnoozesRequest{Parent: parent}).Next(); err != nil || s.GetName() == "" {
		t.Errorf("ListSnoozes: %v", err)
	}
	sn.Interval.EndTime = timestamppb.New(time.Now().Add(time.Minute))
	if s, err := snc.UpdateSnooze(ctx, &monitoringpb.UpdateSnoozeRequest{Snooze: sn, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"interval.end_time"}}}); err != nil ||
		!s.GetInterval().GetEndTime().AsTime().Equal(sn.GetInterval().GetEndTime().AsTime()) {
		t.Errorf("UpdateSnooze: %v, %v", s, err)
	}
	if s, err := snc.GetSnooze(ctx, &monitoringpb.GetSnoozeRequest{Name: sn.GetName()}); err != nil || s.GetDisplayName() != "maintenance" {
		t.Errorf("GetSnooze: %v, %v", s, err)
	}
	if err := uc.DeleteUptimeCheckConfig(ctx, &monitoringpb.DeleteUptimeCheckConfigRequest{Name: check.GetName()}); err != nil {
		t.Errorf("DeleteUptimeCheckConfig: %v", err)
	}
}

// TestMonitoringDashboards (#1964) through
// cloud.google.com/go/monitoring/dashboard/apiv1: a mosaic dashboard is
// created, got, listed, updated with its etag (a stale etag is ABORTED)
// and deleted.
//
// covers: google.monitoring.dashboard.v1.DashboardsService/CreateDashboard, google.monitoring.dashboard.v1.DashboardsService/GetDashboard, google.monitoring.dashboard.v1.DashboardsService/ListDashboards, google.monitoring.dashboard.v1.DashboardsService/UpdateDashboard, google.monitoring.dashboard.v1.DashboardsService/DeleteDashboard
func TestMonitoringDashboards(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	dc, err := dashboard.NewDashboardsClient(ctx, monitoringOpts(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()
	parent := "projects/" + h.Project()
	d, err := dc.CreateDashboard(ctx, &dashboardpb.CreateDashboardRequest{Parent: parent, Dashboard: &dashboardpb.Dashboard{DisplayName: "Checkout",
		Layout: &dashboardpb.Dashboard_MosaicLayout{MosaicLayout: &dashboardpb.MosaicLayout{Columns: 48, Tiles: []*dashboardpb.MosaicLayout_Tile{{
			Width: 24, Height: 16, Widget: &dashboardpb.Widget{Title: "Requests", Content: &dashboardpb.Widget_XyChart{XyChart: &dashboardpb.XyChart{
				DataSets: []*dashboardpb.XyChart_DataSet{{TimeSeriesQuery: &dashboardpb.TimeSeriesQuery{Source: &dashboardpb.TimeSeriesQuery_TimeSeriesFilter{
					TimeSeriesFilter: &dashboardpb.TimeSeriesFilter{Filter: `metric.type = "serviceruntime.googleapis.com/api/request_count"`}}}}}}}}}}}}}})
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}
	defer dc.DeleteDashboard(ctx, &dashboardpb.DeleteDashboardRequest{Name: d.GetName()})
	if d.GetEtag() == "" || !strings.HasPrefix(d.GetName(), parent+"/dashboards/") {
		t.Errorf("created %v", d)
	}
	g, err := dc.GetDashboard(ctx, &dashboardpb.GetDashboardRequest{Name: d.GetName()})
	if err != nil || g.GetMosaicLayout().GetTiles()[0].GetWidget().GetTitle() != "Requests" {
		t.Fatalf("GetDashboard: %v, %v", g, err)
	}
	if l, err := dc.ListDashboards(ctx, &dashboardpb.ListDashboardsRequest{Parent: parent}).Next(); err != nil || l.GetName() == "" {
		t.Errorf("ListDashboards: %v", err)
	}
	stale := g.GetEtag()
	g.DisplayName = "Checkout (prod)"
	u, err := dc.UpdateDashboard(ctx, &dashboardpb.UpdateDashboardRequest{Dashboard: g})
	if err != nil || u.GetEtag() == stale || u.GetDisplayName() != "Checkout (prod)" {
		t.Fatalf("UpdateDashboard: %v, %v", u, err)
	}
	g.Etag = stale
	_, err = dc.UpdateDashboard(ctx, &dashboardpb.UpdateDashboardRequest{Dashboard: g})
	wantMonitoringCode(t, "a stale etag", err, codes.Aborted, "etag")
	if err := dc.DeleteDashboard(ctx, &dashboardpb.DeleteDashboardRequest{Name: d.GetName()}); err != nil {
		t.Errorf("DeleteDashboard: %v", err)
	}
	_, err = dc.GetDashboard(ctx, &dashboardpb.GetDashboardRequest{Name: d.GetName()})
	wantMonitoringCode(t, "a deleted dashboard", err, codes.NotFound, "not found")
}

// TestTerraformMonitoringAlerting (#1964): with egress to Google blocked,
// google_monitoring_notification_channel, _alert_policy,
// _uptime_check_config, _group and _dashboard apply over the JSON API,
// plan clean, update in place and destroy.
//
// covers: google.monitoring.v3.NotificationChannelService/CreateNotificationChannel, google.monitoring.v3.NotificationChannelService/GetNotificationChannel, google.monitoring.v3.NotificationChannelService/DeleteNotificationChannel, google.monitoring.v3.AlertPolicyService/CreateAlertPolicy, google.monitoring.v3.AlertPolicyService/GetAlertPolicy, google.monitoring.v3.AlertPolicyService/UpdateAlertPolicy, google.monitoring.v3.AlertPolicyService/DeleteAlertPolicy, google.monitoring.v3.UptimeCheckService/CreateUptimeCheckConfig, google.monitoring.v3.UptimeCheckService/GetUptimeCheckConfig, google.monitoring.v3.UptimeCheckService/DeleteUptimeCheckConfig, google.monitoring.v3.GroupService/CreateGroup, google.monitoring.v3.GroupService/GetGroup, google.monitoring.v3.GroupService/DeleteGroup, google.monitoring.dashboard.v1.DashboardsService/CreateDashboard, google.monitoring.dashboard.v1.DashboardsService/GetDashboard, google.monitoring.dashboard.v1.DashboardsService/DeleteDashboard
func TestTerraformMonitoringAlerting(t *testing.T) {
	h := New(t)
	_ = monitoringClient(t, h) // skips without CLOUDBURROW_TEST_MONITORING
	m := newTFModule(t)
	conf := func(threshold string) string {
		return fmt.Sprintf(`
resource "google_monitoring_notification_channel" "hook" {
  project      = %[1]q
  display_name = "Local hook"
  type         = "webhook_tokenauth"
  labels = {
    url = "http://127.0.0.1:9/alerts"
  }
}

resource "google_monitoring_alert_policy" "p" {
  project      = %[1]q
  display_name = "High request count"
  combiner     = "OR"
  conditions {
    display_name = "requests"
    condition_threshold {
      filter          = "metric.type = \"serviceruntime.googleapis.com/api/request_count\" AND resource.type = \"consumed_api\""
      comparison      = "COMPARISON_GT"
      threshold_value = %[2]s
      duration        = "60s"
      aggregations {
        alignment_period   = "60s"
        per_series_aligner = "ALIGN_RATE"
      }
    }
  }
  notification_channels = [google_monitoring_notification_channel.hook.name]
  user_labels = {
    team = "platform"
  }
}

resource "google_monitoring_uptime_check_config" "u" {
  project      = %[1]q
  display_name = "Console"
  timeout      = "10s"
  period       = "300s"
  http_check {
    path = "/"
    port = 8080
  }
  monitored_resource {
    type = "uptime_url"
    labels = {
      project_id = %[1]q
      host       = "localhost"
    }
  }
}

resource "google_monitoring_group" "g" {
  project      = %[1]q
  display_name = "Nodes"
  filter       = "resource.type = \"generic_node\""
}

resource "google_monitoring_dashboard" "d" {
  project        = %[1]q
  dashboard_json = jsonencode({
    displayName = "Requests"
    gridLayout = {
      widgets = [{
        title = "Requests"
        xyChart = {
          dataSets = [{
            timeSeriesQuery = {
              timeSeriesFilter = {
                filter = "metric.type=\"serviceruntime.googleapis.com/api/request_count\""
              }
            }
          }]
        }
      }]
    }
  })
}
`, m.project, threshold)
	}
	m.write(conf("10"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	m.planClean()
	m.write(conf("20"))
	m.apply()
	m.planClean()
	ac, err := monitoring.NewAlertPolicyClient(h.Context(), monitoringOpts(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	p, err := ac.ListAlertPolicies(h.Context(), &monitoringpb.ListAlertPoliciesRequest{Name: "projects/" + m.project, Filter: `user_labels.team = "platform"`}).Next()
	if err != nil || p.GetConditions()[0].GetConditionThreshold().GetThresholdValue() != 20 {
		t.Errorf("the policy Terraform updated: %v, %v", p, err)
	}
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
}

// TestGcloudMonitoring (#1964): gcloud monitoring dashboards (GA), and
// gcloud alpha and beta monitoring channels and policies, through the
// configuration `cloudburrow gcloud-setup` writes with egress to Google
// blocked.
//
// covers: google.monitoring.dashboard.v1.DashboardsService/CreateDashboard, google.monitoring.dashboard.v1.DashboardsService/ListDashboards, google.monitoring.dashboard.v1.DashboardsService/GetDashboard, google.monitoring.dashboard.v1.DashboardsService/DeleteDashboard, google.monitoring.v3.NotificationChannelService/CreateNotificationChannel, google.monitoring.v3.NotificationChannelService/ListNotificationChannels, google.monitoring.v3.NotificationChannelService/GetNotificationChannel, google.monitoring.v3.NotificationChannelService/DeleteNotificationChannel, google.monitoring.v3.AlertPolicyService/CreateAlertPolicy, google.monitoring.v3.AlertPolicyService/ListAlertPolicies, google.monitoring.v3.AlertPolicyService/GetAlertPolicy, google.monitoring.v3.AlertPolicyService/DeleteAlertPolicy
func TestGcloudMonitoring(t *testing.T) {
	h := New(t)
	_ = h.Endpoint(EnvMonitoring)
	g := newGcloudSession(t, h)
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		out, err := g.run(nil, append(args, "--project", g.project, "--quiet")...)
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	// nameIn is the resource name a create printed, beside its status line.
	nameIn := func(out string) string {
		for _, l := range strings.Split(out, "\n") {
			if l = strings.TrimSpace(l); strings.HasPrefix(l, "projects/") {
				return l
			}
		}
		return ""
	}
	dash := dir + "/dash.json"
	if err := os.WriteFile(dash, []byte(`{"displayName": "gcloud dash", "gridLayout": {"widgets": [{"title": "Note", "text": {"content": "hello"}}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	name := nameIn(run("monitoring", "dashboards", "create", "--config-from-file", dash, "--format", "value(name)"))
	if !strings.Contains(name, "/dashboards/") {
		t.Fatalf("dashboards create gave %q", name)
	}
	if out := run("monitoring", "dashboards", "list", "--format", "value(displayName)"); !strings.Contains(out, "gcloud dash") {
		t.Errorf("dashboards list: %s", out)
	}
	if out := run("monitoring", "dashboards", "describe", name, "--format", "value(gridLayout.widgets[0].title)"); !strings.Contains(out, "Note") {
		t.Errorf("dashboards describe: %s", out)
	}
	run("monitoring", "dashboards", "delete", name)

	hook := newWebhook(t)
	ch := nameIn(run("beta", "monitoring", "channels", "create", "--display-name", "gcloud hook", "--type", "webhook_tokenauth",
		"--channel-labels", "url="+hook.srv.URL+"/gcloud", "--format", "value(name)"))
	if !strings.Contains(ch, "/notificationChannels/") {
		t.Fatalf("channels create gave %q", ch)
	}
	if out := run("alpha", "monitoring", "channels", "list", "--format", "value(displayName)"); !strings.Contains(out, "gcloud hook") {
		t.Errorf("channels list: %s", out)
	}
	if out := run("beta", "monitoring", "channels", "describe", ch, "--format", "value(type)"); !strings.Contains(out, "webhook_tokenauth") {
		t.Errorf("channels describe: %s", out)
	}
	policy := dir + "/policy.json"
	if err := os.WriteFile(policy, []byte(`{"displayName": "gcloud policy", "combiner": "OR", "conditions": [{"displayName": "c",
  "conditionThreshold": {"filter": "metric.type = \"serviceruntime.googleapis.com/api/request_count\"", "comparison": "COMPARISON_GT",
  "thresholdValue": 1000, "duration": "0s"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pn := nameIn(run("alpha", "monitoring", "policies", "create", "--policy-from-file", policy, "--notification-channels", ch, "--format", "value(name)"))
	if !strings.Contains(pn, "/alertPolicies/") {
		t.Fatalf("policies create gave %q", pn)
	}
	if out := run("beta", "monitoring", "policies", "list", "--format", "value(displayName)"); !strings.Contains(out, "gcloud policy") {
		t.Errorf("policies list: %s", out)
	}
	if out := run("alpha", "monitoring", "policies", "describe", pn, "--format", "value(notificationChannels)"); !strings.Contains(out, ch) {
		t.Errorf("policies describe: %s", out)
	}
	run("alpha", "monitoring", "policies", "delete", pn)
	run("beta", "monitoring", "channels", "delete", ch)

}
