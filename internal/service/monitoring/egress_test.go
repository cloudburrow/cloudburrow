package monitoring

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/egress"
	"github.com/cloudburrow/cloudburrow/internal/egress/egresstest"
)

// TestWebhooksAndUptimeChecksSendNothingToGoogle (#2015): a webhook
// channel or an uptime check on a Google host, the metadata server's
// link-local address included (which IsLocalHost alone let through), is
// not sent, and a local one is; the loopback-only sink beneath their own
// clients counts no attempt off the machine.
func TestWebhooksAndUptimeChecksSendNothingToGoogle(t *testing.T) {
	var rec egresstest.Recorder
	local := httptest.NewServer(rec.Handler())
	defer local.Close()
	sink := egresstest.New(t)
	prev := egress.SetNext(webhookClient, sink.Transport())
	t.Cleanup(func() { egress.SetNext(webhookClient, prev) })
	prevUptime := uptimeTransport
	uptimeTransport = func(bool) http.RoundTripper { return sink.Transport() }
	t.Cleanup(func() { uptimeTransport = prevUptime })

	hook := func(u string) string {
		state, detail := deliver(context.Background(), &monitoringpb.NotificationChannel{Type: "webhook_tokenauth",
			Labels: map[string]string{"url": u}}, []byte("{}"), nil)
		return state + ": " + detail
	}
	for _, u := range []string{"https://monitoring.googleapis.com/v3/x", "http://169.254.169.254/computeMetadata/v1/"} {
		if got := hook(u); strings.HasPrefix(got, DeliverySent) {
			t.Errorf("%s was sent: %s", u, got)
		}
	}
	if got := hook(local.URL + "/hook?token=x"); !strings.HasPrefix(got, DeliverySent) {
		t.Errorf("a local webhook: %s", got)
	}

	check := func(host string, port int32) UptimeResult {
		return runCheck(context.Background(), &monitoringpb.UptimeCheckConfig{Timeout: durationpb.New(5 * time.Second),
			CheckRequestType: &monitoringpb.UptimeCheckConfig_HttpCheck_{HttpCheck: &monitoringpb.UptimeCheckConfig_HttpCheck{Port: port, Path: "/up"}}}, host)
	}
	for _, h := range []string{"storage.googleapis.com", "169.254.169.254"} {
		if r := check(h, 80); r.Passed || r.Status != 0 {
			t.Errorf("an uptime check of %s ran: %+v", h, r)
		}
	}
	host, port := hostPort(t, local.URL)
	if r := check(host, port); !r.Passed {
		t.Errorf("a local uptime check: %+v", r)
	}
	if got := rec.Requests(); len(got) != 2 {
		t.Errorf("local requests: %v, want the webhook and the check", got)
	}
	sink.RequireNone(t)
}
