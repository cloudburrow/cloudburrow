package monitoring

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
)

type sentMail struct {
	source, from, subject, text string
	to                          []string
}

func TestEmailChannelsSendToTheMailSink(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestServer(t, filepath.Join(t.TempDir(), "m.json"))
	var mu sync.Mutex
	var sent []sentMail
	s.SetMailer(func(_ context.Context, source, from string, to []string, subject, text string) error {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, sentMail{source, from, subject, text, to})
		return nil
	})
	ch, err := s.CreateNotificationChannel(ctx, &monitoringpb.CreateNotificationChannelRequest{Name: "projects/p1",
		NotificationChannel: &monitoringpb.NotificationChannel{Type: "email", DisplayName: "ops", Labels: map[string]string{"email_address": "ops@example.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendNotificationChannelVerificationCode(ctx, &monitoringpb.SendNotificationChannelVerificationCodeRequest{Name: ch.GetName()}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	code := s.codes[ch.GetName()].code
	s.mu.Unlock()
	if len(sent) != 1 || sent[0].to[0] != "ops@example.com" || !strings.Contains(sent[0].text, code) || sent[0].from != AlertsSender {
		t.Fatalf("verification mail = %+v (code %s)", sent, code)
	}
	if d, err := s.SendTestNotification(ctx, ch.GetName()); err != nil || d.State != DeliverySent {
		t.Fatalf("test notification = %+v, %v", d, err)
	}
	if len(sent) != 2 || sent[1].subject != "Test notification" {
		t.Fatalf("test mail = %+v", sent[1])
	}
	ds := s.NotifyChannels(ctx, "model-monitoring", []string{ch.GetName()}, []byte("{}"), "Model Monitoring alert", "x drifted")
	if len(ds) != 1 || ds[0].State != DeliverySent || len(sent) != 3 || sent[2].text != "x drifted" {
		t.Fatalf("NotifyChannels = %+v, mail %+v", ds, sent)
	}
	if d := s.SendEmail(ctx, "p1", "model-monitoring", "mm@cloudburrow.localhost", []string{"a@example.com"}, "s", "b"); d.State != DeliverySent ||
		len(sent) != 4 || sent[3].source != "model-monitoring" {
		t.Fatalf("SendEmail = %+v", d)
	}
	// The incident email names the policy and the incident.
	in := &Incident{ID: "i1", Project: "p1", State: "open", ConditionName: "depth > 10", Summary: "depth is 12"}
	m := s.incidentMail(in, &monitoringpb.AlertPolicy{DisplayName: "Queue too deep"})
	if !strings.Contains(m.subject, "Queue too deep") || !strings.Contains(m.text, "depth is 12") || !strings.Contains(m.text, "/monitoring/alerting/incidents/i1") {
		t.Fatalf("incident mail = %+v", m)
	}
}
