package pubsubfront

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/egress"
	"github.com/cloudburrow/cloudburrow/internal/egress/egresstest"
)

// TestPushesSendNothingToGoogle (#2015): the push relay sends a push for a
// googleapis.com endpoint to the instance's API when the process serves it,
// and refuses it locally in a front that serves nothing (the pod), as it
// refuses every other Google host and a redirect to one; the loopback-only
// sink beneath the relay's own client counts no attempt off the machine.
func TestPushesSendNothingToGoogle(t *testing.T) {
	var rec egresstest.Recorder
	api := httptest.NewServer(rec.Handler())
	defer api.Close()
	redirect := httptest.NewServer(http.RedirectHandler("https://storage.googleapis.com/b/x", http.StatusFound))
	defer redirect.Close()
	sink := egresstest.New(t)
	f := &Front{pushClient: egress.Client(nil), subs: map[string]*subState{}, clock: &OffsetClock{}, relayBase: "http://127.0.0.1:1" + relayPath}
	egress.SetNext(f.pushClient, sink.Transport())
	push := func(endpoint string) int {
		w := httptest.NewRecorder()
		f.relay(w, httptest.NewRequest(http.MethodPost, strings.TrimPrefix(f.relayEndpoint("projects/p/subscriptions/s", endpoint), "http://127.0.0.1:1"), strings.NewReader("{}")))
		return w.Code
	}
	const google = "https://cloudfunctions.googleapis.com/v1/projects/p/locations/l/functions/f:call"
	t.Cleanup(func() { egress.SetLocal(nil) })
	egress.SetLocal(nil)
	for _, e := range []string{google, "https://hello-x-uc.a.run.app/", redirect.URL} {
		if code := push(e); code != http.StatusBadGateway {
			t.Errorf("a push to %s in a front serving nothing: %d, want 502 (refused)", e, code)
		}
	}
	egress.SetLocal(func(a string) string {
		if a == "cloudfunctions" {
			return api.URL
		}
		return ""
	})
	if code := push(google); code != http.StatusOK {
		t.Errorf("a push to a served API: %d", code)
	}
	if got := rec.Requests(); len(got) != 1 || !strings.HasPrefix(got[0], "POST /v1/projects/p/locations/l/functions/f:call host=127.0.0.1:") {
		t.Errorf("local delivery: %v", got)
	}
	sink.RequireNone(t)
}
