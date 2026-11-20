package workflows

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/egress"
	"github.com/cloudburrow/cloudburrow/internal/egress/egresstest"
)

// TestHTTPStepsSendNothingToGoogle (#2015): http.post to a served
// googleapis.com API reaches this instance's endpoint of it; an unserved
// API, another Google host, and a local URL redirecting to Google are
// refused; the loopback-only sink beneath the executions' client counts no
// attempt off the machine.
func TestHTTPStepsSendNothingToGoogle(t *testing.T) {
	var rec egresstest.Recorder
	api := httptest.NewServer(rec.Handler())
	defer api.Close()
	redirect := httptest.NewServer(http.RedirectHandler("https://storage.googleapis.com/b/x", http.StatusFound))
	defer redirect.Close()
	sink := egresstest.New(t)
	h := &HTTPCaller{Client: NewHTTPClient(), Local: func(a string) string {
		if a == "pubsub" {
			return api.URL
		}
		return ""
	}}
	egress.SetNext(h.Client, sink.Transport())
	post := func(u string) error {
		_, err := h.httpStep(context.Background(), "post", map[string]any{"url": u, "body": map[string]any{"a": 1}})
		return err
	}
	if err := post("https://pubsub.googleapis.com/v1/projects/p/topics/t:publish"); err != nil {
		t.Fatalf("served API: %v", err)
	}
	if got := rec.Requests(); len(got) != 1 || !strings.HasPrefix(got[0], "POST /v1/projects/p/topics/t:publish host=127.0.0.1:") {
		t.Errorf("local delivery: %v", got)
	}
	for _, u := range []string{"https://translate.googleapis.com/v3/x", "https://x-uc.a.run.app/", redirect.URL} {
		if err := post(u); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s: %v, want a local refusal", u, err)
		}
	}
	sink.RequireNone(t)
}
