package bigqueryfront

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// shortServiceCalls makes serviceCall's attempts short for a test, and
// returns the lines it logs.
func shortServiceCalls(t *testing.T) func() []string {
	t.Helper()
	timeout, backoff := serviceAttemptTimeout, serviceBackoff
	serviceAttemptTimeout, serviceBackoff = 200*time.Millisecond, time.Millisecond
	var mu sync.Mutex
	var lines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	was := frontLogf.Swap(&logf)
	t.Cleanup(func() {
		serviceAttemptTimeout, serviceBackoff = timeout, backoff
		frontLogf.Store(was)
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

// stalls answers nothing until the client gives up: the request's
// context ends when its connection closes, which the server sees once the
// body is read.
func stalls(r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	<-r.Context().Done()
}

// TestAncestryRetriesAStalledCall (#1628): a getAncestry Resource Manager
// does not answer is sent again after serviceAttemptTimeout, not held
// until the query's own deadline, and the stall is logged.
func TestAncestryRetriesAStalledCall(t *testing.T) {
	logged := shortServiceCalls(t)
	var calls atomic.Int32
	rm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			stalls(r)
			return
		}
		_, _ = w.Write([]byte(`{"ancestor":[{"resourceId":{"type":"project","id":"p-1"}},{"resourceId":{"type":"organization","id":"1"}}]}`))
	}))
	defer rm.Close()
	start := time.Now()
	chain, st, msg := (front{resourceManager: rm.URL}).ancestry(context.Background(), "p-1")
	if st != 0 || len(chain) != 2 || chain[1].ResourceID.Type != "organization" {
		t.Fatalf("ancestry = %v, %d %s", chain, st, msg)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %s", took)
	}
	if calls.Load() != 2 {
		t.Errorf("%d calls, want 2", calls.Load())
	}
	lines := strings.Join(logged(), "\n")
	if !strings.Contains(lines, "projects.getAncestry of p-1: attempt 1 of 4 failed") || !strings.Contains(lines, "(2 attempts), answered 200") {
		t.Errorf("logged %q", lines)
	}
}

// TestAncestryGivesUpBounded (#1628): a Resource Manager that never
// answers costs the query serviceAttempts attempts, then 500, logged; a
// 503 is sent again, a 404 or a 500 is not.
func TestAncestryGivesUpBounded(t *testing.T) {
	logged := shortServiceCalls(t)
	var calls atomic.Int32
	rm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); stalls(r) }))
	defer rm.Close()
	start := time.Now()
	_, st, msg := (front{resourceManager: rm.URL}).ancestry(context.Background(), "p-1")
	if st != http.StatusInternalServerError || !strings.Contains(msg, "Resource Manager") {
		t.Errorf("a stalled Resource Manager: %d %s", st, msg)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("took %s", took)
	}
	if calls.Load() != serviceAttempts {
		t.Errorf("%d calls, want %d", calls.Load(), serviceAttempts)
	}
	if lines := strings.Join(logged(), "\n"); !strings.Contains(lines, "projects.getAncestry of p-1 failed after") {
		t.Errorf("logged %q", lines)
	}

	for _, c := range []struct {
		codes []int
		want  int // status ancestry gives
		calls int32
	}{
		{[]int{503, 502, 200}, 0, 3},
		{[]int{404}, 0, 1},
		{[]int{500}, http.StatusInternalServerError, 1},
		{[]int{503, 503, 503, 503}, http.StatusInternalServerError, 4},
	} {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			code := c.codes[min(int(n.Add(1))-1, len(c.codes)-1)]
			w.WriteHeader(code)
			if code == http.StatusOK {
				_, _ = w.Write([]byte(`{"ancestor":[]}`))
			}
		}))
		_, st, _ := (front{resourceManager: srv.URL}).ancestry(context.Background(), "p-1")
		srv.Close()
		if st != c.want || n.Load() != c.calls {
			t.Errorf("%v: status %d after %d calls, want %d after %d", c.codes, st, n.Load(), c.want, c.calls)
		}
	}
}

// TestServiceCallStopsWithItsContext: an attempt ends with the caller's
// context, and no other is sent after it.
func TestServiceCallStopsWithItsContext(t *testing.T) {
	shortServiceCalls(t)
	serviceAttemptTimeout = 10 * time.Second
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); stalls(r) }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := serviceCall(ctx, "test", http.MethodGet, srv.URL, "", nil); err == nil {
		t.Error("no error")
	}
	if took := time.Since(start); took > 2*time.Second || calls.Load() != 1 {
		t.Errorf("took %s and %d calls", took, calls.Load())
	}
}
