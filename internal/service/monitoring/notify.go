package monitoring

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/egress"
)

// Notification delivery (#1964) stays on the machine: a webhook channel's
// URL must be local (loopback, a private address, localhost, a cluster
// service name or host.docker.internal), and a Pub/Sub channel publishes
// to the instance's own Pub/Sub. Every other channel type is recorded,
// never sent.

// Publisher publishes one message to a topic of the local Pub/Sub.
type Publisher func(ctx context.Context, topic string, data []byte, attrs map[string]string) error

// Delivery states.
const (
	DeliverySent     = "sent"
	DeliveryFailed   = "failed"
	DeliveryRecorded = "recorded"
)

// Delivery is one notification CloudBurrow delivered or recorded.
type Delivery struct {
	Time     time.Time `json:"time"`
	Channel  string    `json:"channel"`
	Type     string    `json:"type"`
	Kind     string    `json:"kind"` // "incident", "verification"
	Incident string    `json:"incident,omitempty"`
	State    string    `json:"state"`
	Detail   string    `json:"detail,omitempty"`
}

// MaxDeliveries bounds the deliveries kept.
const MaxDeliveries = 1000

// SetPublisher sets how Pub/Sub channels publish.
func (s *Server) SetPublisher(p Publisher) {
	s.mu.Lock()
	s.publish = p
	s.mu.Unlock()
}

// SetConsoleURL sets the console's base URL, for an incident's url.
func (s *Server) SetConsoleURL(u string) {
	s.mu.Lock()
	s.consoleURL = strings.TrimSuffix(u, "/")
	s.mu.Unlock()
}

// recordDelivery keeps d; the caller holds mu.
func (s *Server) recordDelivery(d Delivery) {
	if d.Time.IsZero() {
		d.Time = s.now()
	}
	s.deliveries = append(s.deliveries, d)
	if len(s.deliveries) > MaxDeliveries {
		s.deliveries = s.deliveries[len(s.deliveries)-MaxDeliveries:]
	}
}

// Deliveries are the recorded deliveries of a project's channels, newest
// first.
func (s *Server) Deliveries(project string) []Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Delivery
	for i := len(s.deliveries) - 1; i >= 0; i-- {
		if projectOfName(s.deliveries[i].Channel) == project {
			out = append(out, s.deliveries[i])
		}
	}
	return out
}

// IsLocalHost is whether a host is on this machine or its local network:
// localhost, a loopback or private address, a cluster service name or
// host.docker.internal. Notifications and uptime checks reach nothing else.
func IsLocalHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || h == "host.docker.internal" ||
		strings.HasSuffix(h, ".svc") || strings.HasSuffix(h, ".svc.cluster.local") {
		return true
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

// incidentPayload is Google's webhook and Pub/Sub notification, schema
// version 1.2 (docs.cloud.google.com/monitoring/support/notification-options#schema-webhooks,
// read 2026-11-19).
func (s *Server) incidentPayload(in *Incident, p *monitoringpb.AlertPolicy) []byte {
	var cond json.RawMessage
	for _, c := range p.GetConditions() {
		if c.GetName() == in.Condition {
			cond, _ = protojson.Marshal(c)
		}
	}
	var ended any
	if !in.Ended.IsZero() {
		ended = in.Ended.Unix()
	}
	doc := map[string]any{}
	if d := p.GetDocumentation(); d != nil {
		doc = map[string]any{"content": d.GetContent(), "mime_type": d.GetMimeType(), "subject": d.GetSubject()}
	}
	body := map[string]any{
		"version": "1.2",
		"incident": map[string]any{
			"incident_id":                in.ID,
			"scoping_project_id":         in.Project,
			"url":                        s.consoleURL + "/monitoring/alerting/incidents/" + in.ID + "?project=" + url.QueryEscape(in.Project),
			"started_at":                 in.Started.Unix(),
			"ended_at":                   ended,
			"state":                      in.State,
			"summary":                    in.Summary,
			"observed_value":             in.Observed,
			"threshold_value":            in.Threshold,
			"resource":                   map[string]any{"type": in.ResourceType, "labels": nonNil(in.ResourceLabels)},
			"resource_type_display_name": in.ResourceType,
			"resource_name":              in.Project + " " + in.ResourceType,
			"metric":                     map[string]any{"type": in.MetricType, "displayName": in.MetricType, "labels": nonNil(in.MetricLabels)},
			"metadata":                   map[string]any{"system_labels": map[string]string{}, "user_labels": nonNil(p.GetUserLabels())},
			"policy_name":                p.GetDisplayName(),
			"policy_user_labels":         nonNil(p.GetUserLabels()),
			"documentation":              doc,
			"condition":                  cond,
			"condition_name":             in.ConditionName,
			"severity":                   severityName(p.GetSeverity()),
		},
	}
	b, _ := json.Marshal(body)
	return b
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func severityName(v monitoringpb.AlertPolicy_Severity) string {
	if v == monitoringpb.AlertPolicy_SEVERITY_UNSPECIFIED {
		return "No severity"
	}
	return strings.Title(strings.ToLower(v.String())) //nolint:staticcheck // ASCII enum names
}

// notify sends an incident's opening or closing to the policy's channels.
// It runs without mu held and records what happened.
func (s *Server) notify(ctx context.Context, in Incident, p *monitoringpb.AlertPolicy, channels []*monitoringpb.NotificationChannel, publish Publisher) {
	payload := s.incidentPayload(&in, p)
	for _, c := range channels {
		d := Delivery{Channel: c.GetName(), Type: c.GetType(), Kind: "incident", Incident: in.ID}
		if !c.GetEnabled().GetValue() && c.Enabled != nil {
			d.State, d.Detail = DeliveryRecorded, "not sent: the channel is disabled"
		} else {
			d.State, d.Detail = deliver(ctx, c, payload, publish)
		}
		s.mu.Lock()
		s.recordDelivery(d)
		s.mu.Unlock()
	}
}

// webhookClient sends through the local-only guard (#2015) as well.
var webhookClient = egress.Client(&http.Client{Timeout: 10 * time.Second,
	// A redirect could leave the machine.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})

func deliver(ctx context.Context, c *monitoringpb.NotificationChannel, payload []byte, publish Publisher) (state, detail string) {
	t := channelTypeOf(c.GetType())
	switch {
	case t == nil || t.local == "":
		return DeliveryRecorded, fmt.Sprintf("not sent: CloudBurrow never sends %s notifications off the machine", c.GetType())
	case t.local == "pubsub":
		if publish == nil {
			return DeliveryFailed, "Pub/Sub is not served by this instance"
		}
		if err := publish(ctx, c.GetLabels()["topic"], payload, nil); err != nil {
			return DeliveryFailed, err.Error()
		}
		return DeliverySent, "published to " + c.GetLabels()["topic"]
	}
	u, err := url.Parse(c.GetLabels()["url"])
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return DeliveryFailed, "the webhook URL is not an http or https URL"
	}
	if !IsLocalHost(u.Hostname()) {
		return DeliveryRecorded, "not sent: " + u.Hostname() + " is not a local host, and CloudBurrow delivers only on the machine"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return DeliveryFailed, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Google-Alerts")
	if c.GetType() == "webhook_basicauth" {
		req.SetBasicAuth(c.GetLabels()["username"], c.GetLabels()["password"])
	}
	resp, err := webhookClient.Do(req)
	if err != nil {
		return DeliveryFailed, err.Error()
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return DeliveryFailed, "the webhook answered " + resp.Status
	}
	return DeliverySent, "posted to " + u.Host + u.Path
}

// SendTestNotification is the console's Send test notification (#2146),
// which Google's channel dialogs offer and the API has no method for: a
// test incident's payload delivered to the channel as an incident's would
// be, and recorded with the channel's deliveries.
func (s *Server) SendTestNotification(ctx context.Context, name string) (Delivery, error) {
	s.mu.Lock()
	c, _ := s.get(kindChannels, name).(*monitoringpb.NotificationChannel)
	publish := s.publish
	now := s.now()
	s.mu.Unlock()
	if c == nil {
		return Delivery{}, apierror.NotFound("Requested entity was not found.")
	}
	payload, _ := json.Marshal(map[string]any{"version": "1.2", "incident": map[string]any{
		"incident_id": "test-notification", "state": "open", "started_at": now.Unix(),
		"policy_name": "Test notification", "condition_name": "Test notification",
		"summary": "This is a test notification from Cloud Monitoring."}})
	d := Delivery{Time: now, Channel: c.GetName(), Type: c.GetType(), Kind: "test"}
	d.State, d.Detail = deliver(ctx, c, payload, publish)
	s.mu.Lock()
	s.recordDelivery(d)
	s.mu.Unlock()
	return d, nil
}
