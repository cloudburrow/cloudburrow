package main

// Prometheus readings into Cloud Monitoring's prometheus.googleapis.com
// series (#2100), as Managed Service for Prometheus writes them
// (internal/service/monitoring/prombuiltin.go names the kinds).

import (
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/relabel"

	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

// promStarts keeps each cumulative series' start: when it was first read,
// or when its value last went down (a restart of what it counts).
type promStarts struct {
	mu   sync.Mutex
	seen map[string]promStart
}

type promStart struct {
	start time.Time
	last  float64
}

func newPromStarts() *promStarts { return &promStarts{seen: map[string]promStart{}} }

// start is key's start for a reading of v at now.
func (p *promStarts) start(key string, v float64, now time.Time) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.seen[key]
	if !ok || v < s.last {
		s.start = now.Add(-time.Millisecond)
	}
	s.last = v
	p.seen[key] = s
	return s.start
}

// forget drops the starts of every series whose key has prefix.
func (p *promStarts) forget(prefix string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.seen {
		if strings.HasPrefix(k, prefix) {
			delete(p.seen, k)
		}
	}
}

// promScrape is what is read of one target: its exposition text, the
// prometheus_target labels its series go on, which metric names are kept
// (nil keeps every one), and the relabelling and extra labels each series
// gets.
type promScrape struct {
	resource map[string]string
	keep     map[string]bool
	relabel  []*relabel.Config
	extra    map[string]string
}

// promResourceKeys are prometheus_target's labels: a series' own label of
// one of these names is renamed exported_NAME, as Prometheus does with
// honor_labels off, except namespace, which places the series, as the
// packages' series of pods and containers are placed.
var promResourceKeys = map[string]bool{"project_id": true, "location": true, "cluster": true, "namespace": true, "job": true, "instance": true}

func promSeriesLabels(m *dto.Metric, sc promScrape, name string) (map[string]string, map[string]string, bool) {
	ls := map[string]string{}
	res := map[string]string{}
	for k, v := range sc.resource {
		res[k] = v
	}
	for k, v := range sc.extra {
		ls[k] = v
	}
	for _, lp := range m.GetLabel() {
		k, v := lp.GetName(), lp.GetValue()
		switch {
		case k == "namespace":
			res["namespace"] = v
		case promResourceKeys[k]:
			ls["exported_"+k] = v
		default:
			ls[k] = v
		}
	}
	if len(sc.relabel) > 0 {
		b := labels.NewBuilder(labels.FromMap(ls))
		b.Set(labels.MetricName, name)
		if !relabel.ProcessBuilder(b, sc.relabel...) {
			return nil, nil, false
		}
		out := map[string]string{}
		b.Labels().Range(func(l labels.Label) {
			if l.Name != labels.MetricName {
				out[l.Name] = l.Value
			}
		})
		ls = out
	}
	return ls, res, true
}

// promReadings are the points of an exposition text r.
func promReadings(r io.Reader, sc promScrape, starts *promStarts, now time.Time) ([]monitoring.PromPoint, error) {
	p := expfmt.NewTextParser(model.UTF8Validation)
	mfs, err := p.TextToMetricFamilies(r)
	if err != nil && len(mfs) == 0 {
		return nil, err
	}
	names := make([]string, 0, len(mfs))
	for n := range mfs {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []monitoring.PromPoint
	targetKey := sortedLabels(sc.resource)
	for _, name := range names {
		if sc.keep != nil && !sc.keep[name] {
			continue
		}
		mf := mfs[name]
		for _, m := range mf.GetMetric() {
			ls, res, ok := promSeriesLabels(m, sc, name)
			if !ok {
				continue
			}
			at := now
			if ms := m.GetTimestampMs(); ms > 0 {
				at = time.UnixMilli(ms)
			}
			point := func(n, kind string, v float64, cumulative bool) {
				pt := monitoring.PromPoint{Type: monitoring.PromMetricType(n, kind), Labels: ls, Resource: res, End: at, Value: v}
				if cumulative {
					pt.Start = starts.start(targetKey+"\x00"+pt.Type+sortedLabels(ls), v, at)
				}
				out = append(out, pt)
			}
			switch mf.GetType() {
			case dto.MetricType_GAUGE:
				point(name, "gauge", m.GetGauge().GetValue(), false)
			case dto.MetricType_COUNTER:
				point(name, "counter", m.GetCounter().GetValue(), true)
			case dto.MetricType_UNTYPED:
				point(name, "unknown", m.GetUntyped().GetValue(), false)
				point(name, "unknown:counter", m.GetUntyped().GetValue(), true)
			case dto.MetricType_SUMMARY:
				s := m.GetSummary()
				point(name+"_sum", "summary:counter", s.GetSampleSum(), true)
				point(name+"_count", "summary", float64(s.GetSampleCount()), true)
				for _, q := range s.GetQuantile() {
					ql := map[string]string{"quantile": formatFloat(q.GetQuantile())}
					for k, v := range ls {
						ql[k] = v
					}
					out = append(out, monitoring.PromPoint{Type: monitoring.PromMetricType(name, "summary"), Labels: ql, Resource: res, End: at, Value: q.GetValue()})
				}
			case dto.MetricType_HISTOGRAM:
				h := m.GetHistogram()
				var bounds []float64
				var counts []int64
				var prev uint64
				for _, b := range h.GetBucket() {
					if math.IsInf(b.GetUpperBound(), 1) {
						continue
					}
					bounds = append(bounds, b.GetUpperBound())
					c := b.GetCumulativeCount()
					counts = append(counts, int64(c-min(prev, c)))
					prev = c
				}
				total := h.GetSampleCount()
				counts = append(counts, int64(total-min(prev, total)))
				pt := monitoring.PromPoint{Type: monitoring.PromMetricType(name, "histogram"), Labels: ls, Resource: res, End: at,
					Histogram: &monitoring.PromHistogram{Bounds: bounds, Counts: counts, Count: int64(total), Sum: h.GetSampleSum()}}
				pt.Start = starts.start(targetKey+"\x00"+pt.Type+sortedLabels(ls), float64(total), at)
				out = append(out, pt)
			}
		}
	}
	return out, nil
}

func formatFloat(f float64) string { return model.SampleValue(f).String() }

// sortedLabels is a label set's key.
func sortedLabels(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString("\x00" + k + "=" + m[k])
	}
	return b.String()
}
