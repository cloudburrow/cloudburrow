package monitoring

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	promparser "github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/util/annotations"
	"google.golang.org/genproto/googleapis/api/distribution"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// PromQL over Cloud Monitoring's time series (#1966), as
// docs.cloud.google.com/monitoring/promql/promql-mapping maps them (read
// 2026-11-20):
//
//   - a metric is named by its type, quoted inside the braces
//     ({"kubernetes.io/container/cpu/limit_cores"}), or by its legacy name:
//     the first "/" becomes ":" and every other character outside
//     [a-zA-Z0-9_:] becomes "_" (kubernetes_io:container_cpu_limit_cores);
//     prometheus.googleapis.com/NAME/KIND is NAME;
//   - a series' labels are its metric and resource labels plus
//     monitored_resource (the resource type); where a metric label and a
//     resource label share a key, the key is the resource label's and
//     metric_KEY the metric label's;
//   - a distribution is a histogram: NAME_count, NAME_sum and NAME_bucket
//     (with le).
//
// Queries run on the Prometheus engine (github.com/prometheus/prometheus,
// Apache-2.0) over a storage.Queryable that reads the same series
// ListTimeSeries reads.

// PromLookback is the engine's lookback delta, Prometheus' default.
const PromLookback = 5 * time.Minute

var (
	promEngineOnce sync.Once
	promEngine     *promql.Engine
)

func engine() *promql.Engine {
	promEngineOnce.Do(func() {
		promEngine = promql.NewEngine(promql.EngineOpts{
			MaxSamples: 50_000_000, Timeout: 2 * time.Minute, LookbackDelta: PromLookback,
			EnableAtModifier: true, EnableNegativeOffset: true,
			NoStepSubqueryIntervalFn: func(int64) int64 { return 60_000 },
		})
	})
	return promEngine
}

var promParser = promparser.NewParser(promparser.Options{})

// parsePromQL parses a PromQL expression.
func parsePromQL(q string) (promparser.Expr, error) { return promParser.ParseExpr(q) }

// legacyPromName is a metric type's legacy PromQL name.
func legacyPromName(metricType string) string {
	if rest, ok := strings.CutPrefix(metricType, "prometheus.googleapis.com/"); ok {
		if i := strings.LastIndex(rest, "/"); i > 0 {
			return rest[:i]
		}
		return rest
	}
	t := strings.Replace(metricType, "/", ":", 1)
	var b strings.Builder
	for _, r := range t {
		if r == ':' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// promNames are the PromQL names a descriptor answers to, each with the
// part of a distribution it reads ("" for the value).
func promNames(md *metricpb.MetricDescriptor) map[string]string {
	out := map[string]string{}
	bases := []string{md.GetType(), legacyPromName(md.GetType())}
	for _, b := range bases {
		if md.GetValueType() == metricpb.MetricDescriptor_DISTRIBUTION {
			out[b+"_count"], out[b+"_sum"], out[b+"_bucket"] = "count", "sum", "bucket"
			continue
		}
		out[b] = ""
	}
	return out
}

// promQueryable reads a project's series for the engine.
type promQueryable struct {
	s       *Server
	project string
}

func (q promQueryable) Querier(mint, maxt int64) (storage.Querier, error) {
	return &promQuerier{q: q, mint: mint, maxt: maxt}, nil
}

type promQuerier struct {
	q          promQueryable
	mint, maxt int64
}

func (*promQuerier) Close() error { return nil }

// promSeries is one series as the engine sees it.
type promSeries struct {
	lset    labels.Labels
	samples []chunks.Sample
}

func (p promSeries) Labels() labels.Labels { return p.lset }
func (p promSeries) Iterator(chunkenc.Iterator) chunkenc.Iterator {
	return storage.NewListSeriesIterator(samples(p.samples))
}

type samples []chunks.Sample

func (s samples) Get(i int) chunks.Sample { return s[i] }
func (s samples) Len() int                { return len(s) }

type fsample struct {
	t int64
	f float64
}

func (s fsample) T() int64                      { return s.t }
func (s fsample) ST() int64                     { return 0 }
func (s fsample) F() float64                    { return s.f }
func (s fsample) H() *histogram.Histogram       { return nil }
func (s fsample) FH() *histogram.FloatHistogram { return nil }
func (s fsample) Type() chunkenc.ValueType      { return chunkenc.ValFloat }
func (s fsample) Copy() chunks.Sample           { return s }

type listSet struct {
	items []storage.Series
	i     int
	err   error
}

func (l *listSet) Next() bool                        { l.i++; return l.err == nil && l.i <= len(l.items) }
func (l *listSet) At() storage.Series                { return l.items[l.i-1] }
func (l *listSet) Err() error                        { return l.err }
func (l *listSet) Warnings() annotations.Annotations { return nil }

func (pq *promQuerier) Select(ctx context.Context, sortSeries bool, _ *storage.SelectHints, ms ...*labels.Matcher) storage.SeriesSet {
	out, err := pq.q.s.promSelect(ctx, pq.q.project, time.UnixMilli(pq.mint), time.UnixMilli(pq.maxt), ms)
	if err != nil {
		return &listSet{err: err}
	}
	if sortSeries {
		sort.Slice(out, func(i, j int) bool { return labels.Compare(out[i].Labels(), out[j].Labels()) < 0 })
	}
	return &listSet{items: out}
}

func (pq *promQuerier) LabelValues(ctx context.Context, name string, _ *storage.LabelHints, ms ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	set, err := pq.q.s.promSelect(ctx, pq.q.project, time.UnixMilli(pq.mint), time.UnixMilli(pq.maxt), ms)
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	for _, sr := range set {
		if v := sr.Labels().Get(name); v != "" {
			seen[v] = true
		}
	}
	return sortedKeys(seen), nil, nil
}

func (pq *promQuerier) LabelNames(ctx context.Context, _ *storage.LabelHints, ms ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	set, err := pq.q.s.promSelect(ctx, pq.q.project, time.UnixMilli(pq.mint), time.UnixMilli(pq.maxt), ms)
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	for _, sr := range set {
		sr.Labels().Range(func(l labels.Label) { seen[l.Name] = true })
	}
	return sortedKeys(seen), nil, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// errMultipleResources is Google's answer to a selector that matches a
// metric written against several resource types without naming one.
var errMultipleResources = errors.New("metric is configured to be used with more than one monitored resource type; series selector must specify a label matcher on monitored resource name")

// promSelect is every series of the project the matchers select, with its
// points in [start, end].
func (s *Server) promSelect(ctx context.Context, project string, start, end time.Time, ms []*labels.Matcher) ([]storage.Series, error) {
	var nameM *labels.Matcher
	for _, m := range ms {
		if m.Name == labels.MetricName {
			nameM = m
		}
	}
	s.mu.Lock()
	descs := s.allDescriptors(project)
	s.mu.Unlock()
	var out []storage.Series
	for _, md := range descs {
		if md.GetValueType() == metricpb.MetricDescriptor_STRING {
			continue
		}
		names := promNames(md)
		var picked []string
		for n := range names {
			if nameM == nil || nameM.Matches(n) {
				picked = append(picked, n)
			}
		}
		if len(picked) == 0 {
			continue
		}
		sort.Strings(picked)
		// One name per part: an exact matcher picks one, a regex both
		// spellings, which would double every series; keep the legacy one.
		byPart := map[string]string{}
		for _, n := range picked {
			if prev, ok := byPart[names[n]]; !ok || strings.Contains(prev, "/") {
				byPart[names[n]] = n
			}
		}
		ts, err := s.rawSeries(ctx, project, md.GetType(), start, end)
		if err != nil {
			return nil, err
		}
		resources := map[string]bool{}
		before := len(out)
		for part, name := range byPart {
			for _, t := range ts {
				for _, sr := range promFromSeries(name, part, t) {
					if matchAll(ms, sr.lset) {
						out = append(out, sr)
						resources[t.GetResource().GetType()] = true
					}
				}
			}
		}
		if len(resources) > 1 && !hasMatcher(ms, "monitored_resource") {
			_ = before
			return nil, errMultipleResources
		}
	}
	return out, nil
}

func hasMatcher(ms []*labels.Matcher, name string) bool {
	for _, m := range ms {
		if m.Name == name {
			return true
		}
	}
	return false
}

func matchAll(ms []*labels.Matcher, l labels.Labels) bool {
	for _, m := range ms {
		if !m.Matches(l.Get(m.Name)) {
			return false
		}
	}
	return true
}

// rawSeries reads a metric type's series with their points in [start,
// end], newest first.
func (s *Server) rawSeries(ctx context.Context, project, metricType string, start, end time.Time) ([]*monitoringpb.TimeSeries, error) {
	req := &monitoringpb.ListTimeSeriesRequest{Name: "projects/" + project, Filter: fmt.Sprintf("metric.type = %q", metricType),
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(end)}, PageSize: 100000}
	var out []*monitoringpb.TimeSeries
	for {
		resp, err := s.ListTimeSeries(ctx, req)
		if err != nil {
			return nil, err
		}
		out = append(out, resp.GetTimeSeries()...)
		if resp.GetNextPageToken() == "" {
			return out, nil
		}
		req.PageToken = resp.GetNextPageToken()
	}
}

// promLabels are a series' PromQL labels, without __name__.
func promLabels(ts *monitoringpb.TimeSeries) map[string]string {
	m := map[string]string{}
	for k, v := range ts.GetMetric().GetLabels() {
		m[k] = v
	}
	for k, v := range ts.GetResource().GetLabels() {
		if mv, ok := m[k]; ok {
			m["metric_"+k] = mv
		}
		m[k] = v
	}
	m["monitored_resource"] = ts.GetResource().GetType()
	return m
}

// promFromSeries is a series as PromQL series: one, or a distribution's
// count, sum or buckets.
func promFromSeries(name, part string, ts *monitoringpb.TimeSeries) []promSeries {
	base := promLabels(ts)
	base[labels.MetricName] = name
	pts := ts.GetPoints()
	at := func(p *monitoringpb.Point) int64 { return p.GetInterval().GetEndTime().AsTime().UnixMilli() }
	if part == "bucket" {
		byLe := map[string][]chunks.Sample{}
		for i := len(pts) - 1; i >= 0; i-- {
			d := pts[i].GetValue().GetDistributionValue()
			bounds := promBucketBounds(d.GetBucketOptions(), len(d.GetBucketCounts()))
			var cum int64
			for j, c := range d.GetBucketCounts() {
				cum += c
				le := "+Inf"
				if j < len(bounds) {
					le = strconv.FormatFloat(bounds[j], 'g', -1, 64)
				}
				byLe[le] = append(byLe[le], fsample{at(pts[i]), float64(cum)})
			}
			if len(d.GetBucketCounts()) <= len(bounds) {
				byLe["+Inf"] = append(byLe["+Inf"], fsample{at(pts[i]), float64(d.GetCount())})
			}
		}
		var out []promSeries
		for le, ss := range byLe {
			m := map[string]string{"le": le}
			for k, v := range base {
				m[k] = v
			}
			out = append(out, promSeries{lset: labels.FromMap(m), samples: ss})
		}
		return out
	}
	var ss []chunks.Sample
	for i := len(pts) - 1; i >= 0; i-- {
		v := pts[i].GetValue()
		var f float64
		switch part {
		case "count":
			f = float64(v.GetDistributionValue().GetCount())
		case "sum":
			d := v.GetDistributionValue()
			f = d.GetMean() * float64(d.GetCount())
		default:
			f = valueOf(v)
		}
		ss = append(ss, fsample{at(pts[i]), f})
	}
	return []promSeries{{lset: labels.FromMap(base), samples: ss}}
}

// promBucketBounds are a distribution's finite upper bounds: bucket i holds
// values below bounds[i]; the last bucket is the overflow.
func promBucketBounds(o *distribution.Distribution_BucketOptions, n int) []float64 {
	var out []float64
	switch {
	case o.GetExplicitBuckets() != nil:
		out = append(out, o.GetExplicitBuckets().GetBounds()...)
	case o.GetLinearBuckets() != nil:
		l := o.GetLinearBuckets()
		for i := int32(0); i <= l.GetNumFiniteBuckets(); i++ {
			out = append(out, l.GetOffset()+l.GetWidth()*float64(i))
		}
	case o.GetExponentialBuckets() != nil:
		e := o.GetExponentialBuckets()
		for i := int32(0); i <= e.GetNumFiniteBuckets(); i++ {
			out = append(out, e.GetScale()*math.Pow(e.GetGrowthFactor(), float64(i)))
		}
	}
	if len(out) > n-1 && n > 0 {
		out = out[:n-1]
	}
	return out
}

// PromInstant evaluates a PromQL expression at t.
func (s *Server) PromInstant(ctx context.Context, project, q string, t time.Time) (*promql.Result, error) {
	qry, err := engine().NewInstantQuery(ctx, promQueryable{s, project}, nil, q, t)
	if err != nil {
		return nil, err
	}
	defer qry.Close()
	res := qry.Exec(ctx)
	if res.Err != nil {
		return nil, res.Err
	}
	return res, nil
}

// PromRange evaluates a PromQL expression over [start, end] every step.
func (s *Server) PromRange(ctx context.Context, project, q string, start, end time.Time, step time.Duration) (*promql.Result, error) {
	qry, err := engine().NewRangeQuery(ctx, promQueryable{s, project}, nil, q, start, end, step)
	if err != nil {
		return nil, err
	}
	defer qry.Close()
	res := qry.Exec(ctx)
	if res.Err != nil {
		return nil, res.Err
	}
	return res, nil
}

// promMetricNames are the metric names an expression's selectors name
// exactly, for metric existence validation.
func promMetricNames(e promparser.Expr) []string {
	var out []string
	promparser.Inspect(e, func(n promparser.Node, _ []promparser.Node) error {
		if vs, ok := n.(*promparser.VectorSelector); ok {
			for _, m := range vs.LabelMatchers {
				if m.Name == labels.MetricName && m.Type == labels.MatchEqual {
					out = append(out, m.Value)
				}
			}
		}
		return nil
	})
	return out
}

// promNameExists is whether a PromQL metric name names a descriptor of the
// project; the caller holds mu.
func (s *Server) promNameExists(project, name string) bool {
	for _, md := range s.allDescriptors(project) {
		if _, ok := promNames(md)[name]; ok {
			return true
		}
	}
	return false
}

// promDuration reads a Prometheus duration (5m, 1h30m).
func promDuration(v string) (time.Duration, error) {
	d, err := model.ParseDuration(v)
	return time.Duration(d), err
}

// PromQL alert conditions (#1966). Google documents them at
// docs.cloud.google.com/monitoring/promql/promql-in-alerting (read
// 2026-11-20): the expression is evaluated every evaluationInterval (30
// seconds by default); each series of its result is "true"; it fires once
// true for the duration, and its incident closes once it has not been true
// for the larger of 270 seconds and twice the evaluation interval. labels
// are added to, or overwrite, the result's labels.

// DefaultPromEvaluation is a PromQL condition's evaluation interval when
// none is set.
const DefaultPromEvaluation = 30 * time.Second

// promCondState is what a PromQL or MQL condition remembers between
// evaluations.
type promCondState struct {
	last     time.Time
	ok       bool
	pending  map[string]time.Time // first true
	lastTrue map[string]time.Time
	seen     map[string]violation
	now      []violation
}

func (s *Server) condState(name string) *promCondState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.promStates == nil {
		s.promStates = map[string]*promCondState{}
	}
	st := s.promStates[name]
	if st == nil {
		st = &promCondState{pending: map[string]time.Time{}, lastTrue: map[string]time.Time{}, seen: map[string]violation{}}
		s.promStates[name] = st
	}
	return st
}

// heldViolations turns one evaluation's true series into the violations
// that fire: true for dur, and held for hold after they stop being true.
func (st *promCondState) heldViolations(now time.Time, vs []violation, dur, hold time.Duration) []violation {
	trueNow := map[string]bool{}
	for _, v := range vs {
		trueNow[v.key] = true
		if _, ok := st.pending[v.key]; !ok {
			st.pending[v.key] = now
		}
		st.lastTrue[v.key] = now
		st.seen[v.key] = v
	}
	var out []violation
	for k, first := range st.pending {
		if !trueNow[k] {
			if now.Sub(st.lastTrue[k]) >= hold {
				delete(st.pending, k)
				delete(st.lastTrue, k)
				delete(st.seen, k)
				continue
			}
			if now.Sub(first) < dur {
				// Never fired: a gap resets it.
				delete(st.pending, k)
				delete(st.lastTrue, k)
				delete(st.seen, k)
				continue
			}
		}
		if now.Sub(first) >= dur {
			out = append(out, st.seen[k])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

func (s *Server) evalPromQL(ctx context.Context, project, condName string, c *monitoringpb.AlertPolicy_Condition_PrometheusQueryLanguageCondition, now time.Time) ([]violation, bool) {
	every := DefaultPromEvaluation
	if c.GetEvaluationInterval() != nil {
		every = c.GetEvaluationInterval().AsDuration()
	}
	st := s.condState(condName)
	if !st.last.IsZero() && now.Sub(st.last) < every {
		return st.heldViolations(now, st.current(), c.GetDuration().AsDuration(), max(270*time.Second, 2*every)), st.ok
	}
	st.last = now
	res, err := s.PromInstant(ctx, project, c.GetQuery(), now)
	if err != nil {
		st.ok = false
		return nil, false
	}
	st.ok = true
	var vs []violation
	add := func(lset labels.Labels, f float64) {
		b := labels.NewBuilder(lset)
		for k, v := range c.GetLabels() {
			b.Set(k, v)
		}
		l := b.Labels()
		m := l.Map()
		rt := m["monitored_resource"]
		res := map[string]string{}
		for k, v := range m {
			if k != labels.MetricName && k != "monitored_resource" {
				res[k] = v
			}
		}
		vs = append(vs, violation{key: "promql|" + l.String(), resourceKey: rt + "|" + l.String(), metricType: m[labels.MetricName],
			metricLabels: res, resourceType: rt, observed: strconv.FormatFloat(f, 'g', -1, 64),
			summary: fmt.Sprintf("PromQL query %s is true for %s (value %g)", c.GetQuery(), l.String(), f)})
	}
	switch v := res.Value.(type) {
	case promql.Vector:
		for _, smp := range v {
			add(smp.Metric, smp.F)
		}
	case promql.Scalar:
		add(labels.EmptyLabels(), v.V)
	}
	st.now = vs
	return st.heldViolations(now, vs, c.GetDuration().AsDuration(), max(270*time.Second, 2*every)), true
}

// current is the last evaluation's true series.
func (st *promCondState) current() []violation { return st.now }

// checkPromCondition validates a PromQL condition.
func checkPromCondition(field func(string) string, c *monitoringpb.AlertPolicy_Condition_PrometheusQueryLanguageCondition) error {
	if strings.TrimSpace(c.GetQuery()) == "" {
		return apierror.InvalidArgument("Field %s is required", field("conditionPrometheusQueryLanguage.query"))
	}
	if _, err := parsePromQL(c.GetQuery()); err != nil {
		return apierror.InvalidArgument("Field %s had an invalid value of %q: %v", field("conditionPrometheusQueryLanguage.query"), c.GetQuery(), err)
	}
	if d := c.GetDuration(); d != nil && (d.AsDuration() < 0 || d.AsDuration() > 24*time.Hour) {
		return apierror.InvalidArgument("Field %s had an invalid value of %q: the duration must be from 0 to 24 hours", field("conditionPrometheusQueryLanguage.duration"), d.AsDuration())
	}
	if e := c.GetEvaluationInterval(); e != nil {
		if d := e.AsDuration(); d <= 0 || d%(30*time.Second) != 0 {
			return apierror.InvalidArgument("Field %s had an invalid value of %q: must be a positive multiple of 30 seconds or missing", field("conditionPrometheusQueryLanguage.evaluationInterval"), d)
		}
	}
	for k := range c.GetLabels() {
		if !model.LabelName(k).IsValidLegacy() {
			return apierror.InvalidArgument("Field %s had an invalid value: %q is not a valid label name", field("conditionPrometheusQueryLanguage.labels"), k)
		}
	}
	if g := c.GetRuleGroup(); !utf8.ValidString(g) || utf8.RuneCountInString(g) > 2048 {
		return apierror.InvalidArgument("Field %s had an invalid value: a valid UTF-8 string of at most 2048 characters", field("conditionPrometheusQueryLanguage.ruleGroup"))
	}
	if a := c.GetAlertRule(); a != "" && (!model.LabelName(a).IsValidLegacy() || utf8.RuneCountInString(a) > 2048) {
		return apierror.InvalidArgument("Field %s had an invalid value of %q: it must be a valid Prometheus label name of at most 2048 characters", field("conditionPrometheusQueryLanguage.alertRule"), a)
	}
	return nil
}

// checkPromMetrics is metric existence validation; the caller holds mu.
func (s *Server) checkPromMetrics(project string, field string, c *monitoringpb.AlertPolicy_Condition_PrometheusQueryLanguageCondition) error {
	if c.GetDisableMetricValidation() {
		return nil
	}
	e, err := parsePromQL(c.GetQuery())
	if err != nil {
		return nil
	}
	for _, n := range promMetricNames(e) {
		if !s.promNameExists(project, n) {
			return apierror.InvalidArgument("Field %s had an invalid value: the metric %q does not exist in project %s; create it or set disableMetricValidation", field, n, project)
		}
	}
	return nil
}
