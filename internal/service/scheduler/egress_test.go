package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/egress"
	"github.com/cloudburrow/cloudburrow/internal/egress/egresstest"
)

// TestHTTPJobsSendNothingToGoogle (#2015): a job aimed at a served
// googleapis.com API reaches this instance's endpoint of it; one aimed at an
// unserved API, at another Google host, or at a local URL that redirects to
// Google is refused; and the loopback-only sink beneath the runner's own
// client counts no connection attempt off the machine.
func TestHTTPJobsSendNothingToGoogle(t *testing.T) {
	var rec egresstest.Recorder
	api := httptest.NewServer(rec.Handler())
	defer api.Close()
	redirect := httptest.NewServer(http.RedirectHandler("https://storage.googleapis.com/b/x", http.StatusFound))
	defer redirect.Close()
	sink := egresstest.New(t)
	r := NewRunner(nil, &http.Client{}, nil, nil, time.Second).WithLocalAPIs(func(a string) string {
		if a == "workflowexecutions" {
			return api.URL
		}
		return ""
	})
	egress.SetNext(r.client, sink.Transport())
	job := func(uri string) Job {
		return Job{Name: "projects/p/locations/l/jobs/j", AttemptDeadline: 10 * time.Second,
			HTTP: &HTTPTarget{URI: uri, Method: http.MethodPost, Body: []byte("{}")}}
	}
	if _, err := r.attempt(context.Background(), job("https://workflowexecutions.googleapis.com/v1/projects/p/locations/l/workflows/w/executions"), time.Now(), 1); err != nil {
		t.Fatalf("served API: %v", err)
	}
	if got := rec.Requests(); len(got) != 1 || !strings.HasPrefix(got[0], "POST /v1/projects/p/locations/l/workflows/w/executions host=127.0.0.1:") {
		t.Errorf("local delivery: %v", got)
	}
	for _, uri := range []string{"https://translate.googleapis.com/v3/x", "https://hello-x-uc.a.run.app/", redirect.URL} {
		if _, err := r.attempt(context.Background(), job(uri), time.Now(), 1); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s: %v, want a local refusal", uri, err)
		}
	}
	sink.RequireNone(t)
}
