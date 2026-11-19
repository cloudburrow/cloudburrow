package apienable

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type keyFunc func(KeyCall) *Refusal

func (f keyFunc) CheckKey(_ context.Context, c KeyCall) *Refusal { return f(c) }

// A call carrying a key is checked first, by header or key=, and one
// without a key is not asked about.
func TestGateChecksKeys(t *testing.T) {
	var seen []KeyCall
	UseKeys(keyFunc(func(c KeyCall) *Refusal {
		seen = append(seen, c)
		if c.Key == "good" {
			return nil
		}
		return &Refusal{HTTP: 400, Status: "INVALID_ARGUMENT", Message: "API key not valid. Please pass a valid API key.",
			Reason: "API_KEY_INVALID", Metadata: map[string]string{"service": c.Service}}
	}))
	defer UseKeys(nil)
	h := Gate(fixed{}, "pubsub.googleapis.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, tc := range []struct {
		path, header string
		want         int
	}{{"/v1/projects/p/topics?key=bad", "", 400}, {"/v1/projects/p/topics", "bad", 400}, {"/v1/projects/p/topics?key=good", "", 200}, {"/v1/projects/p/topics", "", 200}} {
		r := httptest.NewRequest("GET", tc.path, nil)
		if tc.header != "" {
			r.Header.Set("X-Goog-Api-Key", tc.header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != tc.want {
			t.Errorf("%s %q: %d, want %d", tc.path, tc.header, rec.Code, tc.want)
		}
	}
	if len(seen) != 3 || seen[0].Service != "pubsub.googleapis.com" {
		t.Errorf("checked %+v", seen)
	}
}

// The host's endpoint answers a refusal, or none.
func TestKeyHandlerRoundTrip(t *testing.T) {
	srv := httptest.NewServer(KeyHandler(keyFunc(func(c KeyCall) *Refusal {
		if c.Key == "good" {
			return nil
		}
		return &Refusal{HTTP: 403, Status: "PERMISSION_DENIED", Reason: "API_KEY_SERVICE_BLOCKED"}
	})))
	defer srv.Close()
	hc := srv.Client()
	defer hc.CloseIdleConnections()
	r := &Remote{Base: srv.URL, Client: hc}
	if f := r.CheckKey(context.Background(), KeyCall{Key: "good"}); f != nil {
		t.Errorf("good key refused: %+v", f)
	}
	if f := r.CheckKey(context.Background(), KeyCall{Key: "x"}); f == nil || f.Reason != "API_KEY_SERVICE_BLOCKED" {
		t.Errorf("bad key: %+v", f)
	}
}
