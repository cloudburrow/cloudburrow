package egress_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/egress"
	"github.com/cloudburrow/cloudburrow/internal/egress/egresstest"
)

func served(m map[string]string) egress.LocalAPIs {
	return func(api string) string { return m[api] }
}

func TestResolve(t *testing.T) {
	m := served(map[string]string{"workflowexecutions": "http://127.0.0.1:9", "run": "http://127.0.0.1:8", "storage": "http://127.0.0.1:7"})
	for in, want := range map[string]string{
		"https://workflowexecutions.googleapis.com/v1/x?a=b":  "http://127.0.0.1:9/v1/x?a=b",
		"https://us-central1-run.googleapis.com/v2/projects":  "http://127.0.0.1:8/v2/projects",
		"https://www.googleapis.com/storage/v1/b/x":           "http://127.0.0.1:7/storage/v1/b/x",
		"https://STORAGE.googleapis.com./b":                   "http://127.0.0.1:7/b",
		"http://localhost:8080/hook":                          "http://localhost:8080/hook",
		"https://example.com/hook":                            "https://example.com/hook",
		"http://hello.default.cloudburrow.localhost:9080/x":   "http://hello.default.cloudburrow.localhost:9080/x",
		"https://pubsub.us-east1.rep.googleapis.com/v1/x":     "",
		"https://translate.googleapis.com/v3/x":               "",
		"https://hello-abc-uc.a.run.app/":                     "",
		"https://us-central1-p.cloudfunctions.net/f":          "",
		"https://oauth2.googleapis.com/token":                 "",
		"https://accounts.google.com/o/oauth2/token":          "",
		"http://metadata.google.internal/computeMetadata/v1/": "",
		"http://169.254.169.254/computeMetadata/v1/":          "",
	} {
		got, err := egress.ResolveWith(in, m)
		if want == "" {
			var r *egress.RefusedError
			if !errors.As(err, &r) {
				t.Errorf("%s: %q, %v; want a refusal", in, got, err)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%s: %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := egress.ResolveWith("https://storage.googleapis.com/b", nil); err == nil {
		t.Error("with nothing served, a googleapis.com URL was not refused")
	}
}

// TestTransportSendsNothingToGoogle: through the guard, a served API's
// request reaches the local server, with its Host; an unserved one, another
// Google host, and a redirect to one are refused, and the loopback-only
// sink counts no attempt off the machine.
func TestTransportSendsNothingToGoogle(t *testing.T) {
	var rec egresstest.Recorder
	api := httptest.NewServer(rec.Handler())
	defer api.Close()
	redirect := httptest.NewServer(http.RedirectHandler("https://storage.googleapis.com/b/x", http.StatusFound))
	defer redirect.Close()
	sink := egresstest.New(t)
	c := &http.Client{Transport: &egress.RoundTripper{Next: sink.Transport(),
		Served: served(map[string]string{"pubsub": api.URL})}}

	resp, err := c.Post("https://pubsub.googleapis.com/v1/projects/p/topics/t:publish", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if got := rec.Requests(); len(got) != 1 || got[0] != "POST /v1/projects/p/topics/t:publish host="+strings.TrimPrefix(api.URL, "http://") {
		t.Errorf("local delivery: %v", got)
	}
	for _, u := range []string{"https://translate.googleapis.com/v3/x", "https://x-uc.a.run.app/", redirect.URL} {
		_, err := c.Get(u)
		var r *egress.RefusedError
		if !errors.As(err, &r) {
			t.Errorf("%s: %v, want a refusal", u, err)
		}
	}
	sink.RequireNone(t)
}
