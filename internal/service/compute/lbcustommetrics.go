package compute

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	orcav3 "github.com/cncf/xds/go/xds/data/orca/v3"
	"google.golang.org/protobuf/proto"
)

// The CUSTOM_METRICS balancing mode and WEIGHTED_ROUND_ROBIN (#2160), as
// https://docs.cloud.google.com/load-balancing/docs/https/applb-custom-metrics
// and the Compute Engine reference's Backend.customMetrics and
// BackendService.customMetrics describe them (read 2026-11-22):
//
//   - a backend of balancing mode CUSTOM_METRICS names up to two metrics
//     (three counting dry-run ones), each with a maxUtilization in
//     [0.0, 1.0]; the backend's fullness is the highest of its metrics'
//     current value over maxUtilization, and the load balancer sends to
//     the backends below full in proportion to what is left, keeping them
//     about equally full, and to all of them once every one is full;
//   - a backend service's customMetrics (with localityLbPolicy
//     WEIGHTED_ROUND_ROBIN) weigh the endpoints within a backend by the
//     utilization they report, as gRPC's weighted round robin does:
//     rps_fractional over utilization (application_utilization, else
//     cpu_utilization, else the highest named metric of the service's);
//   - a dry-run metric is reported but not used;
//   - the endpoints report the metrics in an Open Request Cost Aggregation
//     (ORCA) load report on each response: the endpoint-load-metrics
//     header (TEXT key=value pairs, JSON, or BIN base64 of an
//     xds.data.orca.v3.OrcaLoadReport), endpoint-load-metrics-json, or
//     endpoint-load-metrics-bin (also a gRPC trailer). orca.cpu_utilization,
//     orca.mem_utilization, orca.application_utilization, orca.eps and
//     orca.rps_fractional name the report's fields; any other name is one
//     of its named_metrics.

// customMetricNameRE is BackendCustomMetric.name's pattern.
var customMetricNameRE = regexp.MustCompile(`^[a-z]([-_.a-z0-9]*[a-z0-9])?$`)

// orcaReserved are the report's own fields, by metric name.
var orcaReserved = map[string]bool{"orca.cpu_utilization": true, "orca.mem_utilization": true, "orca.application_utilization": true,
	"orca.eps": true, "orca.rps_fractional": true}

// checkCustomMetrics validates a customMetrics list: backend lists have a
// maxUtilization, the service's do not.
func checkCustomMetrics(field string, l []any, backend bool) ([]any, error) {
	var out []any
	used, total := 0, 0
	seen := map[string]bool{}
	for i, e := range l {
		m := objOf(e)
		f := field + "[" + itoa(i) + "]"
		name := str(m, "name")
		if name == "" {
			return nil, required(f + ".name")
		}
		if len(name) > 64 || !customMetricNameRE.MatchString(name) {
			return nil, invalid("Invalid value for field '%s.name': '%s'. Must be a match of regex '[a-z]([-_.a-z0-9]*[a-z0-9])?' and at most 64 characters.", f, name)
		}
		if strings.HasPrefix(name, "orca.") && !orcaReserved[name] && !strings.HasPrefix(name, "orca.named_metrics.") {
			return nil, invalid("Invalid value for field '%s.name': '%s'. The reserved metrics are orca.cpu_utilization, orca.mem_utilization, orca.application_utilization, orca.eps and orca.rps_fractional.", f, name)
		}
		if seen[name] {
			return nil, invalid("Invalid value for field '%s.name': '%s'. Duplicate custom metric.", f, name)
		}
		seen[name] = true
		nm := obj{"name": name}
		dry := m["dryRun"] == true
		if _, ok := m["dryRun"]; ok {
			nm["dryRun"] = dry
		}
		if backend {
			if v, ok := num(m, "maxUtilization"); ok {
				if v < 0 || v > 1 {
					return nil, invalid("Invalid value for field '%s.maxUtilization': '%v'. Must be between 0.0 and 1.0.", f, v)
				}
				nm["maxUtilization"] = v
			} else if !dry {
				return nil, required(f + ".maxUtilization")
			}
		} else if _, ok := m["maxUtilization"]; ok {
			return nil, invalid("Invalid value for field '%s.maxUtilization': '%v'. A backend service's custom metrics have no maxUtilization; set it on the backends.", f, m["maxUtilization"])
		}
		total++
		if !dry {
			used++
		}
		out = append(out, nm)
	}
	if used > 2 {
		return nil, invalid("Invalid value for field '%s': ''. At most 2 custom metrics can be used for load balancing (3 counting dry-run metrics).", field)
	}
	if total > 3 {
		return nil, invalid("Invalid value for field '%s': ''. At most 3 custom metrics, counting dry-run metrics.", field)
	}
	return out, nil
}

// customMetricsScheme is whether a load balancing scheme takes custom
// metrics.
func customMetricsScheme(scheme string) bool {
	return scheme == "EXTERNAL_MANAGED" || scheme == "INTERNAL_MANAGED" || scheme == "INTERNAL_SELF_MANAGED"
}

// orcaReport is the last load report of one endpoint.
type orcaReport struct {
	cpu, mem, app, eps, rps float64
	named                   map[string]float64
	at                      time.Time
}

// value is a metric of the report by its customMetrics name; ok is false
// when the endpoint did not report it.
func (r orcaReport) value(name string) (float64, bool) {
	switch name {
	case "orca.cpu_utilization":
		return r.cpu, r.cpu != 0
	case "orca.mem_utilization":
		return r.mem, r.mem != 0
	case "orca.application_utilization":
		return r.app, r.app != 0
	case "orca.eps":
		return r.eps, true
	case "orca.rps_fractional":
		return r.rps, r.rps != 0
	}
	v, ok := r.named[strings.TrimPrefix(name, "orca.named_metrics.")]
	return v, ok
}

// orcaTTL is how long a report counts: an endpoint that stops reporting
// is weighed as one that never did.
const orcaTTL = 3 * time.Minute

// orcaStore keeps the endpoints' reports.
type orcaStore struct {
	mu sync.Mutex
	by map[string]orcaReport
}

func (o *orcaStore) put(id string, r orcaReport) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.by == nil {
		o.by = map[string]orcaReport{}
	}
	r.at = time.Now()
	o.by[id] = r
}

func (o *orcaStore) get(id string, now time.Time) (orcaReport, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, ok := o.by[id]
	if !ok || now.Sub(r.at) > orcaTTL {
		return orcaReport{}, false
	}
	return r, true
}

// parseORCA reads a load report from response headers (or gRPC
// trailers); ok is false when there is none.
func parseORCA(h http.Header) (orcaReport, bool) {
	if v := h.Get("Endpoint-Load-Metrics-Bin"); v != "" {
		return orcaFromBin(v)
	}
	if v := h.Get("Endpoint-Load-Metrics-Json"); v != "" {
		return orcaFromJSON(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "JSON")))
	}
	v := strings.TrimSpace(h.Get("Endpoint-Load-Metrics"))
	switch {
	case v == "":
		return orcaReport{}, false
	case strings.HasPrefix(v, "BIN "):
		return orcaFromBin(strings.TrimSpace(v[4:]))
	case strings.HasPrefix(v, "JSON "):
		return orcaFromJSON(strings.TrimSpace(v[5:]))
	case strings.HasPrefix(v, "TEXT "):
		v = v[5:]
	}
	r := orcaReport{named: map[string]float64{}}
	any := false
	for _, kv := range strings.Split(v, ",") {
		k, val, found := strings.Cut(strings.TrimSpace(kv), "=")
		if !found {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err != nil || math.IsNaN(f) || f < 0 {
			continue
		}
		any = true
		r.set(strings.TrimSpace(k), f)
	}
	return r, any
}

func (r *orcaReport) set(k string, f float64) {
	switch k {
	case "cpu_utilization":
		r.cpu = f
	case "mem_utilization":
		r.mem = f
	case "application_utilization":
		r.app = f
	case "eps":
		r.eps = f
	case "rps_fractional":
		r.rps = f
	default:
		if n, ok := strings.CutPrefix(k, "named_metrics."); ok {
			r.named[n] = f
		}
	}
}

func orcaFromJSON(v string) (orcaReport, bool) {
	var m map[string]any
	if json.Unmarshal([]byte(v), &m) != nil {
		return orcaReport{}, false
	}
	r := orcaReport{named: map[string]float64{}}
	for k, x := range m {
		if f, ok := x.(float64); ok {
			r.set(k, f)
		}
	}
	if nm, ok := m["named_metrics"].(map[string]any); ok {
		for k, x := range nm {
			if f, ok := x.(float64); ok {
				r.named[k] = f
			}
		}
	}
	return r, true
}

func orcaFromBin(v string) (orcaReport, bool) {
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		if b, err = base64.RawStdEncoding.DecodeString(v); err != nil {
			return orcaReport{}, false
		}
	}
	var lr orcav3.OrcaLoadReport
	if proto.Unmarshal(b, &lr) != nil {
		return orcaReport{}, false
	}
	r := orcaReport{cpu: lr.GetCpuUtilization(), mem: lr.GetMemUtilization(), app: lr.GetApplicationUtilization(), eps: lr.GetEps(),
		rps: lr.GetRpsFractional(), named: map[string]float64{}}
	for k, f := range lr.GetNamedMetrics() {
		r.named[k] = f
	}
	for k, f := range lr.GetUtilization() {
		r.named[k] = f
	}
	return r, true
}

// customFullness is a CUSTOM_METRICS backend's fullness: the highest of
// its metrics' mean reported value over maxUtilization, among its healthy
// endpoints; endpoints that have not reported count as empty.
func (b *lbBalanceState) customFullness(g lbGroup, now time.Time) float64 {
	full := 0.0
	for _, e := range listOf(g.backend["customMetrics"]) {
		m := objOf(e)
		if m["dryRun"] == true {
			continue
		}
		sum, n := 0.0, 0
		for _, ep := range g.healthy {
			if r, ok := b.orca.get(ep.id, now); ok {
				if v, ok := r.value(str(m, "name")); ok {
					sum += v
				}
			}
			n++
		}
		if n == 0 {
			continue
		}
		mu := numOr(m, "maxUtilization", 1)
		f := sum / float64(n)
		if mu <= 0 {
			if f > 0 {
				f = math.Inf(1)
			}
		} else {
			f /= mu
		}
		full = math.Max(full, f)
	}
	return full
}

// wrrWeight is an endpoint's weight under WEIGHTED_ROUND_ROBIN (gRPC's
// weighted round robin over ORCA): rps_fractional over utilization,
// the utilization being application_utilization, else cpu_utilization,
// else the highest of the service's custom metrics; 0 when unknown.
func (b *lbBalanceState) wrrWeight(bs obj, ep lbEndpoint, now time.Time) float64 {
	r, ok := b.orca.get(ep.id, now)
	if !ok {
		return 0
	}
	u := r.app
	if u <= 0 {
		u = r.cpu
	}
	if u <= 0 {
		for _, e := range listOf(bs["customMetrics"]) {
			m := objOf(e)
			if m["dryRun"] == true {
				continue
			}
			if v, ok := r.value(str(m, "name")); ok && v > u {
				u = v
			}
		}
	}
	qps := r.rps
	if qps <= 0 || u <= 0 {
		return 0
	}
	if r.eps > 0 {
		u += r.eps / qps // gRPC's error_utilization_penalty of 1
	}
	return qps / u
}

// recordORCA keeps the load report of a response from ep, reading a gRPC
// response's trailers when its body ends.
func (b *lbBalanceState) recordORCA(ep lbEndpoint, resp *http.Response) {
	if r, ok := parseORCA(resp.Header); ok {
		b.orca.put(ep.id, r)
		return
	}
	if resp.Body == nil || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/grpc") {
		return
	}
	resp.Body = &orcaTrailerReader{ReadCloser: resp.Body, resp: resp, done: func(h http.Header) {
		if r, ok := parseORCA(h); ok {
			b.orca.put(ep.id, r)
		}
	}}
}

type orcaTrailerReader struct {
	io.ReadCloser
	resp *http.Response
	done func(http.Header)
	once sync.Once
}

func (o *orcaTrailerReader) Read(p []byte) (int, error) {
	n, err := o.ReadCloser.Read(p)
	if err == io.EOF {
		o.once.Do(func() { o.done(o.resp.Trailer) })
	}
	return n, err
}
