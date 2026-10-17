package scheduler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// With --appengine-url, an App Engine job's attempt goes to the stand-in
// server, with the routing's Host and the headers AppEngineHttpTarget
// documents; a non-2xx answer fails it (#1506).
func TestAppEngineJobDelivered(t *testing.T) {
	got := make(chan *http.Request, 2)
	bodies := make(chan string, 2)
	code := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- r
		bodies <- string(b)
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	r := NewRunner(NewStore(store.NewMemory()), nil, nil, nil, time.Second).WithAppEngine(srv.URL + "/")
	j := Job{Name: "projects/p1/locations/us-central1/jobs/nightly", AttemptDeadline: time.Minute,
		AppEngine: &AppEngineTarget{Method: "PUT", RelativeURI: "/cron?x=1", Body: []byte("b"),
			Headers: map[string]string{"X-A": "1", "X-AppEngine-QueueName": "forged", "User-Agent": "mine"},
			Routing: AppEngineRouting{Service: "worker", Version: "v1"}}}
	when := time.Date(2026, 10, 17, 1, 0, 0, 0, time.UTC)
	if c, err := r.attempt(context.Background(), j, when, 1); err != nil || c != 200 {
		t.Fatalf("attempt = %d, %v", c, err)
	}
	req := <-got
	if req.Method != "PUT" || req.URL.RequestURI() != "/cron?x=1" || req.Host != "v1.worker.p1.appspot.com" || <-bodies != "b" {
		t.Errorf("request = %s %s Host %s", req.Method, req.URL, req.Host)
	}
	for k, want := range map[string]string{"X-A": "1", "X-AppEngine-QueueName": "", "User-Agent": "mine " + AppEngineUserAgent,
		"X-CloudScheduler": "true", "X-CloudScheduler-JobName": "nightly", "X-CloudScheduler-ScheduleTime": "2026-10-17T01:00:00Z",
		"Content-Type": "application/octet-stream"} {
		if v := req.Header.Get(k); v != want {
			t.Errorf("%s = %q, want %q", k, v, want)
		}
	}
	code = http.StatusInternalServerError
	if c, err := r.attempt(context.Background(), j, when, 2); err == nil || c != 500 {
		t.Errorf("a 500 = %d, %v; want a failed attempt", c, err)
	}
}
