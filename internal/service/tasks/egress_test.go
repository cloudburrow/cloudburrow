package tasks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/egress"
	"github.com/cloudburrow/cloudburrow/internal/egress/egresstest"
	"github.com/cloudburrow/cloudburrow/internal/sched"
)

// TestHTTPTargetsSendNothingToGoogle (#2015): a task aimed at a served
// googleapis.com API reaches this instance's endpoint of it; one aimed at an
// unserved API, another Google host, or a local URL redirecting to Google
// is a failed attempt refused locally; the loopback-only sink beneath the
// dispatcher's own client counts no connection attempt off the machine.
func TestHTTPTargetsSendNothingToGoogle(t *testing.T) {
	var rec egresstest.Recorder
	api := httptest.NewServer(rec.Handler())
	defer api.Close()
	redirect := httptest.NewServer(http.RedirectHandler("https://storage.googleapis.com/b/x", http.StatusFound))
	defer redirect.Close()
	egress.SetLocal(func(a string) string {
		if a == "pubsub" {
			return api.URL
		}
		return ""
	})
	t.Cleanup(func() { egress.SetLocal(nil) })
	sink := egresstest.New(t)
	s := newStore(t)
	mustQueue(t, s, queueA)
	d := NewDispatcher(s, nil, sched.NewFakeClock(time.Now()))
	egress.SetNext(d.client, sink.Transport())

	dispatch := func(id, url string) error {
		task := Task{Name: TaskName(queueA, id), HTTPRequest: &HTTPRequest{URL: url, Method: http.MethodPost, Body: []byte("{}")},
			DispatchDeadline: 10 * time.Second}
		if _, err := s.CreateTask(task); err != nil {
			t.Fatal(err)
		}
		return d.Dispatch(context.Background(), task)
	}
	if err := dispatch("served", "https://pubsub.googleapis.com/v1/projects/p/topics/t:publish"); err != nil {
		t.Fatalf("served API: %v", err)
	}
	if got := rec.Requests(); len(got) != 1 || !strings.HasPrefix(got[0], "POST /v1/projects/p/topics/t:publish host=127.0.0.1:") {
		t.Errorf("local delivery: %v", got)
	}
	for i, url := range []string{"https://translate.googleapis.com/v3/x", "https://us-central1-p.cloudfunctions.net/f", redirect.URL} {
		if err := dispatch("refused"+string(rune('a'+i)), url); err == nil {
			t.Errorf("%s: the attempt succeeded", url)
		}
	}
	if len(rec.Requests()) != 1 {
		t.Errorf("a refused task reached the local API: %v", rec.Requests())
	}
	sink.RequireNone(t)
}
