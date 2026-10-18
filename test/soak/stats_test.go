//go:build soak

package soak

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/status"
)

// recorder keeps every operation's outcome and latency, bucketed into
// windows of fixed length from the start of the run, so the report can
// compare the first window with the last: a latency that drifts up over
// thirty minutes is the symptom of a leak long before memory runs out.
type recorder struct {
	start  time.Time
	window time.Duration

	mu     sync.Mutex
	groups map[string]*groupStats
}

type groupStats struct {
	ops     int64
	errs    int64
	byCode  map[string]int64
	samples map[string][]string // code -> first few messages
	windows map[int][]time.Duration
}

func newRecorder(window time.Duration) *recorder {
	return &recorder{start: time.Now(), window: window, groups: map[string]*groupStats{}}
}

func (r *recorder) group(name string) *groupStats {
	g := r.groups[name]
	if g == nil {
		g = &groupStats{byCode: map[string]int64{}, samples: map[string][]string{}, windows: map[int][]time.Duration{}}
		r.groups[name] = g
	}
	return g
}

// do times fn and records it under group; an error is classified by its gRPC
// or HTTP code. It returns fn's error so a worker can react to it.
func (r *recorder) do(group string, fn func() error) error {
	t0 := time.Now()
	err := fn()
	r.record(group, t0, time.Since(t0), err)
	return err
}

func (r *recorder) record(group string, at time.Time, d time.Duration, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.group(group)
	g.ops++
	w := int(at.Sub(r.start) / r.window)
	g.windows[w] = append(g.windows[w], d)
	if err != nil {
		g.errs++
		code := errorCode(err)
		g.byCode[code]++
		if len(g.samples[code]) < 3 {
			g.samples[code] = append(g.samples[code], truncate(err.Error(), 300))
		}
	}
}

// fail records a failure found by a check rather than by a call, such as a
// message lost or a task delivered twice.
func (r *recorder) fail(group, code, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.group(group)
	g.errs++
	g.byCode[code]++
	if len(g.samples[code]) < 3 {
		g.samples[code] = append(g.samples[code], truncate(msg, 300))
	}
}

func errorCode(err error) string {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return fmt.Sprintf("HTTP %d", ge.Code)
	}
	if s, ok := status.FromError(err); ok {
		return "gRPC " + s.Code().String()
	}
	if strings.Contains(err.Error(), "context deadline exceeded") {
		return "deadline"
	}
	return "other"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// percentiles returns p50, p95 and p99 of ds.
func percentiles(ds []time.Duration) [3]time.Duration {
	if len(ds) == 0 {
		return [3]time.Duration{}
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(p float64) time.Duration { return s[int(p*float64(len(s)-1))] }
	return [3]time.Duration{at(0.50), at(0.95), at(0.99)}
}

// latencyRow is one window's figures for one group.
type latencyRow struct {
	Window int     `json:"window"`
	Ops    int     `json:"ops"`
	P50ms  float64 `json:"p50_ms"`
	P95ms  float64 `json:"p95_ms"`
	P99ms  float64 `json:"p99_ms"`
}

type groupReport struct {
	Group     string              `json:"group"`
	Ops       int64               `json:"ops"`
	Errors    int64               `json:"errors"`
	ErrorRate float64             `json:"error_rate"`
	ByCode    map[string]int64    `json:"errors_by_code,omitempty"`
	Samples   map[string][]string `json:"error_samples,omitempty"`
	First     latencyRow          `json:"first_window"`
	Last      latencyRow          `json:"last_window"`
	Windows   []latencyRow        `json:"windows"`
	Drift     string              `json:"drift,omitempty"`
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// report summarises every group. The last window is the last complete one:
// a partial window at the end has too few samples to compare.
func (r *recorder) report() []groupReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []groupReport
	for name, g := range r.groups {
		rep := groupReport{Group: name, Ops: g.ops, Errors: g.errs, ByCode: g.byCode, Samples: g.samples}
		if g.ops > 0 {
			rep.ErrorRate = float64(g.errs) / float64(g.ops)
		}
		var ws []int
		for w := range g.windows {
			ws = append(ws, w)
		}
		sort.Ints(ws)
		for _, w := range ws {
			p := percentiles(g.windows[w])
			rep.Windows = append(rep.Windows, latencyRow{w, len(g.windows[w]), ms(p[0]), ms(p[1]), ms(p[2])})
		}
		if n := len(rep.Windows); n > 0 {
			rep.First = rep.Windows[0]
			rep.Last = rep.Windows[n-1]
			if n > 2 {
				rep.Last = rep.Windows[n-2]
			}
			rep.Drift = drift(rep.First, rep.Last)
		}
		out = append(out, rep)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Group < out[j].Group })
	return out
}

// drift names a percentile that grew both by half and by more than 20 ms
// between the first window and the last: below that, the difference is
// scheduling noise on a shared machine.
func drift(first, last latencyRow) string {
	var bad []string
	for _, c := range []struct {
		name     string
		from, to float64
	}{{"p50", first.P50ms, last.P50ms}, {"p95", first.P95ms, last.P95ms}, {"p99", first.P99ms, last.P99ms}} {
		if c.to > c.from*1.5 && c.to-c.from > 20 {
			bad = append(bad, fmt.Sprintf("%s %.1f -> %.1f ms", c.name, c.from, c.to))
		}
	}
	return strings.Join(bad, ", ")
}
