package console

import (
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/metrics"
)

// Request charts for /monitoring (#292): rate, error rate and latency for
// the services CloudBurrow serves itself, from periodic snapshots of the
// same counters /metrics exposes. The series is in memory and bounded, like
// the node series beside it.

// NotMeasured labels a service whose calls are not counted, rather than
// charting it at zero, which would read as "nobody called it".
const NotMeasured = "not measured (direct port-forward to upstream emulator)"

// RequestMetricsSource is the counters' registry, as the console reads it.
type RequestMetricsSource interface {
	Snapshot() []metrics.Point
	Unmeasured() []string
}

type requestSample struct {
	at     time.Time
	points []metrics.Point
}

type requestSeries struct {
	mu      sync.Mutex
	src     RequestMetricsSource
	samples []requestSample
	limit   int
}

// SetRequestMetrics attaches the counters the request charts are drawn from.
func (s *Server) SetRequestMetrics(src RequestMetricsSource) {
	s.reqMetrics = &requestSeries{src: src, limit: SeriesLimit}
}

// SampleRequests takes one snapshot. The sampler calls it on its own tick.
func (s *Server) SampleRequests(now time.Time) {
	rs := s.reqMetrics
	if rs == nil {
		return
	}
	snap := rs.src.Snapshot()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.samples = append(rs.samples, requestSample{now.UTC(), snap})
	if len(rs.samples) > rs.limit {
		rs.samples = rs.samples[len(rs.samples)-rs.limit:]
	}
}

// RequestPoint is one service over one sampling interval.
type RequestPoint struct {
	At time.Time `json:"at"`
	// Rate is calls per second over the interval.
	Rate float64 `json:"rate"`
	// ErrorRate is the fraction of those calls that failed; nil with no calls.
	ErrorRate *float64 `json:"error_rate"`
	// P50MS and P95MS are latency quantiles estimated from the histogram,
	// as Prometheus's histogram_quantile does; nil with no calls.
	P50MS *float64 `json:"p50_ms"`
	P95MS *float64 `json:"p95_ms"`
}

type serviceTotals struct {
	count, errors uint64
	buckets       []uint64
}

// failed reports whether a canonical code is a failure: any gRPC code but OK,
// or an HTTP status of 400 or above.
func failed(code string) bool {
	if code == "OK" {
		return false
	}
	if len(code) == 3 && code[0] >= '0' && code[0] <= '9' {
		return code[0] >= '4'
	}
	return true
}

func totals(points []metrics.Point) map[string]*serviceTotals {
	out := map[string]*serviceTotals{}
	for _, p := range points {
		t := out[p.Service]
		if t == nil {
			t = &serviceTotals{buckets: make([]uint64, len(metrics.Buckets))}
			out[p.Service] = t
		}
		t.count += p.Count
		if failed(p.Code) {
			t.errors += p.Count
		}
		for i, b := range p.Buckets {
			t.buckets[i] += b
		}
	}
	return out
}

// quantile interpolates within the bucket holding the q-th call, from
// cumulative bucket counts over total calls. A quantile past the last finite
// bound reports that bound, since nothing finer is known.
func quantile(q float64, cumulative []uint64, total uint64) float64 {
	rank := q * float64(total)
	lower, prev := 0.0, uint64(0)
	for i, c := range cumulative {
		upper := metrics.Buckets[i]
		if float64(c) >= rank {
			in := c - prev
			if in == 0 {
				return upper
			}
			return lower + (upper-lower)*(rank-float64(prev))/float64(in)
		}
		lower, prev = upper, c
	}
	return metrics.Buckets[len(metrics.Buckets)-1]
}

func (s *Server) handleRequestMetrics(w http.ResponseWriter, _ *http.Request) {
	body := map[string]any{"services": []any{}, "unmeasured": []string{}}
	rs := s.reqMetrics
	if rs == nil {
		body["unavailable"] = "request metrics are not collected by this instance"
		writeJSON(w, http.StatusOK, body)
		return
	}
	rs.mu.Lock()
	samples := append([]requestSample(nil), rs.samples...)
	rs.mu.Unlock()

	series := map[string][]RequestPoint{}
	for i := 1; i < len(samples); i++ {
		prev, cur := totals(samples[i-1].points), totals(samples[i].points)
		secs := samples[i].at.Sub(samples[i-1].at).Seconds()
		if secs <= 0 {
			continue
		}
		for svc, c := range cur {
			p := prev[svc]
			if p == nil {
				p = &serviceTotals{buckets: make([]uint64, len(metrics.Buckets))}
			}
			n := c.count - p.count
			pt := RequestPoint{At: samples[i].at, Rate: float64(n) / secs}
			if n > 0 {
				er := float64(c.errors-p.errors) / float64(n)
				delta := make([]uint64, len(c.buckets))
				for j := range delta {
					delta[j] = c.buckets[j] - p.buckets[j]
				}
				p50, p95 := quantile(0.5, delta, n)*1000, quantile(0.95, delta, n)*1000
				pt.ErrorRate, pt.P50MS, pt.P95MS = &er, &p50, &p95
			}
			series[svc] = append(series[svc], pt)
		}
	}
	names := make([]string, 0, len(series))
	for svc := range series {
		names = append(names, svc)
	}
	sort.Strings(names)
	var out []map[string]any
	for _, svc := range names {
		out = append(out, map[string]any{"service": svc, "points": series[svc]})
	}
	if out != nil {
		body["services"] = out
	}
	body["unmeasured"] = rs.src.Unmeasured()
	body["unmeasured_label"] = NotMeasured
	writeJSON(w, http.StatusOK, body)
}
