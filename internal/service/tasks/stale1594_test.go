package tasks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/sched"
)

// A pass that read its due tasks while an attempt was out does not start
// the task again once that attempt has ended (#1594): a 2xx deleted it, a
// retry rescheduled it. The soak delivered 340 of 33,141 tasks twice.
func TestStaleDueListDoesNotDispatchAgain(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
	}{{"succeeded", http.StatusOK}, {"retried", http.StatusInternalServerError}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			mustQueue(t, s, queueA)
			var hits int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt64(&hits, 1)
				w.WriteHeader(tc.code)
			}))
			defer srv.Close()
			if _, err := s.CreateTask(Task{Name: TaskName(queueA, "once"), HTTPRequest: &HTTPRequest{URL: srv.URL}}); err != nil {
				t.Fatal(err)
			}
			clock := sched.NewFakeClock(time.Now().UTC().Add(time.Second))
			w := NewWorker(s, NewDispatcher(s, srv.Client(), clock), clock, time.Second)
			ctx := context.Background()

			now := clock.Now()
			stale, err := s.DueTasks(now)
			if err != nil || len(stale) != 1 {
				t.Fatalf("due = %v, %v; want the task", stale, err)
			}
			w.dispatchDue(ctx) // the attempt, which ends before the stale pass
			w.startAll(ctx, now, stale)
			w.running.Wait()
			if got := atomic.LoadInt64(&hits); got != 1 {
				t.Errorf("the target was called %d times, want 1", got)
			}
			if n := len(w.inflight); n != 0 {
				t.Errorf("%d tasks left marked in flight", n)
			}
		})
	}
}
