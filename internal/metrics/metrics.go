// Package metrics counts the API calls CloudBurrow serves itself (#292), and
// exposes them in the Prometheus text format.
//
// It counts the services that register a call observer (Measure), which are
// the ones whose calls pass through CloudBurrow. An enabled service that never
// registers one, such as Pub/Sub or an opt-in emulator reached over a raw
// port-forward to an upstream process, is reported as unmeasured rather than
// as zero, which would read as "nobody called it".
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Buckets are the latency histogram's upper bounds, in seconds.
var Buckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type key struct{ service, method, code string }

type series struct {
	count   uint64
	sum     float64
	buckets []uint64 // cumulative per bound; len(Buckets)
}

// Registry holds the counters. The zero value is not usable; use New.
type Registry struct {
	mu       sync.Mutex
	series   map[key]*series
	services []string
	measured map[string]bool
}

// New returns an empty registry. services names the enabled services, in the
// order they are reported; each is unmeasured until Measure is called for it.
func New(services ...string) *Registry {
	return &Registry{series: map[key]*series{}, services: append([]string(nil), services...), measured: map[string]bool{}}
}

// Measure records that a call observer counting into this registry exists
// for service, so it is no longer reported as unmeasured. The unmeasured set
// is derived from these registrations, not kept by hand, so a service wired
// with an observer can never be both counted and reported as unseen.
func (r *Registry) Measure(service string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.measured[service] = true
}

// Observe counts one call. method is the short RPC name, such as CreateTask.
func (r *Registry) Observe(service, method, code string, d time.Duration) {
	if r == nil {
		return
	}
	if i := strings.LastIndex(method, "/"); i >= 0 {
		method = method[i+1:]
	}
	secs := d.Seconds()
	r.mu.Lock()
	defer r.mu.Unlock()
	k := key{service, method, code}
	s := r.series[k]
	if s == nil {
		s = &series{buckets: make([]uint64, len(Buckets))}
		r.series[k] = s
	}
	s.count++
	s.sum += secs
	for i, b := range Buckets {
		if secs <= b {
			s.buckets[i]++
		}
	}
}

// Point is one series at one moment, for charts.
type Point struct {
	Service, Method, Code string
	Count                 uint64
	Sum                   float64
	Buckets               []uint64
}

// Snapshot copies every series.
func (r *Registry) Snapshot() []Point {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Point, 0, len(r.series))
	for k, s := range r.series {
		out = append(out, Point{k.service, k.method, k.code, s.count, s.sum, append([]uint64(nil), s.buckets...)})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		if a.Method != b.Method {
			return a.Method < b.Method
		}
		return a.Code < b.Code
	})
	return out
}

// Unmeasured names the enabled services with no call observer, whose calls
// are not seen.
func (r *Registry) Unmeasured() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, s := range r.services {
		if !r.measured[s] {
			out = append(out, s)
		}
	}
	return out
}

func label(v string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(v)
}

// Write renders the registry in the Prometheus text exposition format.
func (r *Registry) Write(w io.Writer) error {
	points := r.Snapshot()
	var b strings.Builder
	b.WriteString("# HELP cloudburrow_requests_total API calls served by CloudBurrow itself, by service, method and canonical code.\n")
	b.WriteString("# TYPE cloudburrow_requests_total counter\n")
	for _, p := range points {
		fmt.Fprintf(&b, "cloudburrow_requests_total{service=\"%s\",method=\"%s\",code=\"%s\"} %d\n",
			label(p.Service), label(p.Method), label(p.Code), p.Count)
	}
	b.WriteString("# HELP cloudburrow_request_duration_seconds How long served API calls took.\n")
	b.WriteString("# TYPE cloudburrow_request_duration_seconds histogram\n")
	for _, p := range points {
		ls := fmt.Sprintf("service=\"%s\",method=\"%s\",code=\"%s\"", label(p.Service), label(p.Method), label(p.Code))
		for i, bound := range Buckets {
			fmt.Fprintf(&b, "cloudburrow_request_duration_seconds_bucket{%s,le=\"%g\"} %d\n", ls, bound, p.Buckets[i])
		}
		fmt.Fprintf(&b, "cloudburrow_request_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n", ls, p.Count)
		fmt.Fprintf(&b, "cloudburrow_request_duration_seconds_sum{%s} %g\n", ls, p.Sum)
		fmt.Fprintf(&b, "cloudburrow_request_duration_seconds_count{%s} %d\n", ls, p.Count)
	}
	b.WriteString("# HELP cloudburrow_service_measured Whether an enabled service's calls are counted: 0 for one CloudBurrow has no call observer on, such as one reached by direct port-forward to an upstream emulator.\n")
	b.WriteString("# TYPE cloudburrow_service_measured gauge\n")
	for _, s := range r.Unmeasured() {
		fmt.Fprintf(&b, "cloudburrow_service_measured{service=\"%s\"} 0\n", label(s))
	}
	_, err := io.WriteString(w, b.String())
	return err
}
