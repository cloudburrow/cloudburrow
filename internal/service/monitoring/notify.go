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
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/egress"
)

// Notification delivery (#1964) stays on the machine: a webhook channel's
// URL must be local (loopback, a private address, localhost, a cluster
// service name or host.docker.internal), a Pub/Sub channel publishes to
// the instance's own Pub/Sub, and an email channel's mail goes to the
// instance's local mail sink (internal/mailsink, #2214), which keeps it
// for the console's Mail page and relays nothing. Every other channel
// type is recorded, never sent.

// Publisher publishes one message to a topic of the local Pub/Sub.
type Publisher func(ctx context.Context, topic string, data []byte, attrs map[string]string) error

// Mailer hands one plain-text email to the local mail sink. source names
// the service that sent it.
type Mailer func(ctx context.Context, source, from string, to []string, subject, text string) error

// AlertsSender is the From address of Cloud Monitoring's email. Google's
// is alerting-noreply@google.com; the local one names no Google domain.
const AlertsSender = "alerting-noreply@cloudburrow.localhost"

// SetMailer sets how email channels and other services' emails are sent.
func (s *Server) SetMailer(m Mailer) {
	s.mu.Lock()
	s.mailer = m
	s.mu.Unlock()
}

// mail is one email notification's subject and text.
type mail struct{ subject, text string }

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
	s.mu.Lock()
	mailer := s.mailer
	m := s.incidentMail(&in, p)
	s.mu.Unlock()
	for _, c := range channels {
		d := Delivery{Channel: c.GetName(), Type: c.GetType(), Kind: "incident", Incident: in.ID}
		if !c.GetEnabled().GetValue() && c.Enabled != nil {
			d.State, d.Detail = DeliveryRecorded, "not sent: the channel is disabled"
		} else {
			d.State, d.Detail = deliverTo(ctx, c, payload, publish, mailer, m)
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
	return deliverTo(ctx, c, payload, publish, nil, mail{})
}

// deliverTo delivers payload to c, or for an email channel m, through
// mailer.
func deliverTo(ctx context.Context, c *monitoringpb.NotificationChannel, payload []byte, publish Publisher, mailer Mailer, m mail) (state, detail string) {
	t := channelTypeOf(c.GetType())
	switch {
	case t != nil && t.local == "email":
		to := c.GetLabels()["email_address"]
		if mailer == nil || m.subject == "" {
			return DeliveryRecorded, "not sent: this instance has no local mail sink. To: " + to
		}
		if err := mailer(ctx, "monitoring", AlertsSender, []string{to}, m.subject, m.text); err != nil {
			return DeliveryFailed, "the local mail sink refused the email to " + to + ": " + err.Error()
		}
		return DeliverySent, "sent to " + to + " through the local mail sink (the console's Mail page)"
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
	publish, mailer := s.publish, s.mailer
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
	d.State, d.Detail = deliverTo(ctx, c, payload, publish, mailer, mail{subject: "Test notification",
		text: "This is a test notification from Cloud Monitoring for the notification channel " + c.GetDisplayName() + " (" + c.GetName() + ").\n"})
	s.mu.Lock()
	s.recordDelivery(d)
	s.mu.Unlock()
	return d, nil
}

// NotifyChannels delivers another service's alert (Agent Platform Model
// Monitoring's, #2214) to the named notification channels as an
// incident's notification is delivered: a Pub/Sub channel publishes to
// the instance's Pub/Sub, a local webhook is posted to, and every other
// type is recorded. Each delivery is recorded with the channel's, of kind
// kind; a channel that does not exist is a failed delivery.
func (s *Server) NotifyChannels(ctx context.Context, kind string, names []string, payload []byte, m ...string) []Delivery {
	var em mail
	if len(m) == 2 {
		em = mail{subject: m[0], text: m[1]}
	}
	s.mu.Lock()
	publish, mailer := s.publish, s.mailer
	var chans []*monitoringpb.NotificationChannel
	var out []Delivery
	for _, n := range names {
		c, _ := s.get(kindChannels, n).(*monitoringpb.NotificationChannel)
		if c == nil {
			d := Delivery{Channel: n, Kind: kind, State: DeliveryFailed, Detail: "the notification channel does not exist"}
			s.recordDelivery(d)
			out = append(out, d)
			continue
		}
		chans = append(chans, proto.Clone(c).(*monitoringpb.NotificationChannel))
	}
	s.mu.Unlock()
	for _, c := range chans {
		d := Delivery{Channel: c.GetName(), Type: c.GetType(), Kind: kind}
		if c.GetEnabled() != nil && !c.GetEnabled().GetValue() {
			d.State, d.Detail = DeliveryRecorded, "not sent: the channel is disabled"
		} else {
			d.State, d.Detail = deliverTo(ctx, c, payload, publish, mailer, em)
		}
		s.mu.Lock()
		s.recordDelivery(d)
		s.mu.Unlock()
		out = append(out, d)
	}
	return out
}

// SendEmail sends another service's email (Model Monitoring's, #2214)
// to addresses in project, of kind kind, from from, through the local
// mail sink, and records the delivery with the project's. Without a sink
// the email is recorded, never sent.
func (s *Server) SendEmail(ctx context.Context, project, kind, from string, to []string, subject, body string) Delivery {
	s.mu.Lock()
	mailer := s.mailer
	s.mu.Unlock()
	d := Delivery{Channel: "projects/" + project, Type: "email", Kind: kind}
	head := "To: " + strings.Join(to, ", ") + "\nSubject: " + subject
	switch {
	case mailer == nil:
		d.State, d.Detail = DeliveryRecorded, "not sent: this instance has no local mail sink. "+head+"\n\n"+body
	default:
		if err := mailer(ctx, kind, from, to, subject, body); err != nil {
			d.State, d.Detail = DeliveryFailed, "the local mail sink refused it: "+err.Error()+". "+head
		} else {
			d.State, d.Detail = DeliverySent, "sent through the local mail sink (the console's Mail page). "+head
		}
	}
	s.mu.Lock()
	s.recordDelivery(d)
	s.mu.Unlock()
	return d
}

// incidentMail is an incident's email; the caller holds mu.
//
// UNVERIFIED (re-read 2026-11-24,
// https://docs.cloud.google.com/monitoring/support/notification-options:
// Google documents the email's content, not its layout): the subject and
// the text's order.
func (s *Server) incidentMail(in *Incident, p *monitoringpb.AlertPolicy) mail {
	state := "opened"
	if in.State == IncidentClosed {
		state = "closed"
	}
	subj := fmt.Sprintf("[ALERT - %s] %s: incident %s", severityName(p.GetSeverity()), p.GetDisplayName(), state)
	if d := p.GetDocumentation().GetSubject(); d != "" {
		subj = d
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Incident %s was %s for the policy %s, condition %s.\n\n", in.ID, state, p.GetDisplayName(), in.ConditionName)
	if in.Summary != "" {
		fmt.Fprintf(&b, "%s\n\n", in.Summary)
	}
	fmt.Fprintf(&b, "Project: %s\nResource: %s\nMetric: %s\nStarted: %s\n", in.Project, in.ResourceType, in.MetricType, in.Started.UTC().Format(time.RFC3339))
	if !in.Ended.IsZero() {
		fmt.Fprintf(&b, "Ended: %s\n", in.Ended.UTC().Format(time.RFC3339))
	}
	if c := p.GetDocumentation().GetContent(); c != "" {
		fmt.Fprintf(&b, "\nDocumentation:\n%s\n", c)
	}
	fmt.Fprintf(&b, "\nView incident: %s/monitoring/alerting/incidents/%s?project=%s\n", s.consoleURL, in.ID, url.QueryEscape(in.Project))
	return mail{subject: subj, text: b.String()}
}
