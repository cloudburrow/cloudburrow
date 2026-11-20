package monitoring

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Alert evaluation (#1964): every EvaluationInterval each enabled policy's
// conditions are evaluated over the served time series (the same reads
// ListTimeSeries makes) and over the local Cloud Logging's entries, as
// docs.cloud.google.com/monitoring/alerts/concepts-indepth describes:
//
//   - a metric-threshold condition is met by a time series whose aligned
//     values have violated the threshold continuously for the condition's
//     duration (the latest value alone when the duration is 0);
//   - a metric-absence condition by a series that reported in the last 24
//     hours and has had no point for the duration;
//   - a log-match condition by each entry its filter matches received since
//     the last evaluation, at most one notification per rate-limit period;
//   - the trigger is how many series (or what percentage) must meet it.
//
// An incident is opened per series that meets the condition and closed when
// it stops meeting it (absence: when data returns; log match: after
// autoClose), each opening and closing notified to the policy's channels
// unless a snooze covers the policy. Incidents are not a public API at
// Google either: the console's Alerting page shows them.

// EvaluationInterval is how often policies are evaluated.
var EvaluationInterval = 15 * time.Second

// Incident states, as Google's notifications name them.
const (
	IncidentOpen   = "open"
	IncidentClosed = "closed"
)

// Incident is one condition met by one time series.
type Incident struct {
	ID             string            `json:"id"`
	Project        string            `json:"project"`
	Policy         string            `json:"policy"`
	PolicyName     string            `json:"policyName"`
	Condition      string            `json:"condition"`
	ConditionName  string            `json:"conditionName"`
	Key            string            `json:"key"`
	State          string            `json:"state"`
	Acknowledged   bool              `json:"acknowledged,omitempty"`
	Summary        string            `json:"summary"`
	Started        time.Time         `json:"started"`
	Ended          time.Time         `json:"ended,omitempty"`
	MetricType     string            `json:"metricType,omitempty"`
	MetricLabels   map[string]string `json:"metricLabels,omitempty"`
	ResourceType   string            `json:"resourceType,omitempty"`
	ResourceLabels map[string]string `json:"resourceLabels,omitempty"`
	Observed       string            `json:"observed,omitempty"`
	Threshold      string            `json:"threshold,omitempty"`
	Severity       string            `json:"severity,omitempty"`
	// Notified is when each channel was last told the incident is open,
	// for renotifyInterval.
	Notified map[string]time.Time `json:"notified,omitempty"`
}

// MaxIncidents bounds the incidents kept per project; the oldest closed
// ones go first.
const MaxIncidents = 5000

// LogMatcher reads the local Cloud Logging for log-match conditions.
type LogMatcher interface {
	// MatchLogEntries are a project's entries the filter matches that were
	// received in (after, until], with the label extractors applied.
	MatchLogEntries(project, filter string, extractors map[string]string, after, until time.Time) ([]LogMatch, error)
}

// LogMatch is one entry a log-match condition matched.
type LogMatch struct {
	Time     time.Time
	Log      string
	Resource map[string]string
	Type     string
	Labels   map[string]string
	Summary  string
}

// SetLogMatcher sets the Cloud Logging log-match conditions read.
func (s *Server) SetLogMatcher(m LogMatcher) {
	s.mu.Lock()
	s.logs = m
	s.mu.Unlock()
}

// Incidents are a project's incidents, newest first.
func (s *Server) Incidents(project string) []Incident {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Incident
	for i := len(s.incidents) - 1; i >= 0; i-- {
		if s.incidents[i].Project == project {
			out = append(out, *s.incidents[i])
		}
	}
	return out
}

// AcknowledgeIncident marks an open incident acknowledged, and CloseIncident
// closes it, as the console's incident page does.
func (s *Server) AcknowledgeIncident(project, id string) error {
	return s.incidentAction(project, id, func(in *Incident) error {
		if in.State != IncidentOpen {
			return apierror.FailedPrecondition("Incident %s is closed", id)
		}
		in.Acknowledged = true
		return nil
	})
}

func (s *Server) CloseIncident(project, id string) error {
	var closed *Incident
	err := s.incidentAction(project, id, func(in *Incident) error {
		if in.State != IncidentOpen {
			return apierror.FailedPrecondition("Incident %s is already closed", id)
		}
		in.State, in.Ended = IncidentClosed, s.now()
		in.Summary += " (closed by hand)"
		c := *in
		closed = &c
		return nil
	})
	if err == nil && closed != nil {
		s.notifyIncidents(context.Background(), []Incident{*closed})
	}
	return err
}

func (s *Server) incidentAction(project, id string, f func(*Incident) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, in := range s.incidents {
		if in.Project == project && in.ID == id {
			if err := f(in); err != nil {
				return err
			}
			return s.save()
		}
	}
	return apierror.NotFound("Incident %s was not found", id)
}

// StartEvaluating evaluates every EvaluationInterval until ctx ends.
func (s *Server) StartEvaluating(ctx context.Context) {
	go func() {
		t := time.NewTicker(EvaluationInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.Evaluate(ctx)
				s.RunUptimeChecks(ctx)
			}
		}
	}()
}

// Evaluate evaluates every enabled policy once.
func (s *Server) Evaluate(ctx context.Context) {
	now := s.now()
	s.mu.Lock()
	var policies []*monitoringpb.AlertPolicy
	for _, m := range s.under(kindPolicies, "") {
		p := m.(*monitoringpb.AlertPolicy)
		if p.Enabled == nil || p.GetEnabled().GetValue() {
			policies = append(policies, clone(p))
		}
	}
	since := s.lastEval
	if since.IsZero() {
		since = now.Add(-EvaluationInterval)
	}
	s.lastEval = now
	logs := s.logs
	s.mu.Unlock()

	var changed []Incident
	for _, p := range policies {
		project := projectOfName(p.GetName())
		met := map[string][]violation{} // by condition name
		all := true
		for _, c := range p.GetConditions() {
			vs, ok := s.evaluateCondition(ctx, project, p, c, since, now, logs)
			if !ok {
				all = false
				continue
			}
			if len(vs) == 0 {
				all = false
			}
			met[c.GetName()] = vs
		}
		if p.GetCombiner() == monitoringpb.AlertPolicy_AND && !all {
			for k := range met {
				met[k] = nil
			}
		}
		if p.GetCombiner() == monitoringpb.AlertPolicy_AND_WITH_MATCHING_RESOURCE && len(p.GetConditions()) > 1 {
			common := map[string]int{}
			for _, vs := range met {
				seen := map[string]bool{}
				for _, v := range vs {
					if !seen[v.resourceKey] {
						common[v.resourceKey]++
						seen[v.resourceKey] = true
					}
				}
			}
			for k, vs := range met {
				var keep []violation
				for _, v := range vs {
					if common[v.resourceKey] == len(p.GetConditions()) {
						keep = append(keep, v)
					}
				}
				met[k] = keep
			}
		}
		changed = append(changed, s.applyViolations(p, met, now)...)
	}
	s.mu.Lock()
	_ = s.save()
	s.mu.Unlock()
	s.notifyIncidents(ctx, changed)
	s.renotify(ctx, now)
}

// renotify repeats an open incident's notification to each channel whose
// notificationChannelStrategy sets a renotifyInterval, once that interval
// has passed since the channel was last told
// (docs.cloud.google.com/monitoring/alerts/policies-in-api, read
// 2026-11-20). A snoozed policy is not repeated.
func (s *Server) renotify(ctx context.Context, now time.Time) {
	type send struct {
		in       Incident
		p        *monitoringpb.AlertPolicy
		channels []*monitoringpb.NotificationChannel
	}
	var sends []send
	s.mu.Lock()
	for _, in := range s.incidents {
		if in.State != IncidentOpen {
			continue
		}
		p, _ := s.get(kindPolicies, in.Policy).(*monitoringpb.AlertPolicy)
		if p == nil || p.Enabled != nil && !p.GetEnabled().GetValue() || s.snoozedLocked(p.GetName(), now) {
			continue
		}
		var due []*monitoringpb.NotificationChannel
		for _, st := range p.GetAlertStrategy().GetNotificationChannelStrategy() {
			if st.GetRenotifyInterval() == nil {
				continue
			}
			every := st.GetRenotifyInterval().AsDuration()
			for _, n := range st.GetNotificationChannelNames() {
				last := in.Started
				if t, ok := in.Notified[n]; ok && t.After(last) {
					last = t
				}
				if now.Sub(last) < every {
					continue
				}
				if c, ok := s.get(kindChannels, n).(*monitoringpb.NotificationChannel); ok {
					due = append(due, clone(c))
					if in.Notified == nil {
						in.Notified = map[string]time.Time{}
					}
					in.Notified[n] = now
				}
			}
		}
		if len(due) > 0 {
			sends = append(sends, send{*in, clone(p), due})
		}
	}
	if len(sends) > 0 {
		_ = s.save()
	}
	publish := s.publish
	s.mu.Unlock()
	for _, x := range sends {
		s.notify(ctx, x.in, x.p, x.channels, publish)
	}
}

// violation is one series (or log entry) meeting a condition.
type violation struct {
	key, resourceKey string
	metricType       string
	metricLabels     map[string]string
	resourceType     string
	resourceLabels   map[string]string
	observed         string
	threshold        string
	summary          string
	// log is a log-match: it opens an incident each time, never "stops".
	log bool
}

// evaluateCondition is the series meeting c; ok is false when it could not
// be evaluated (no data, an error), which leaves its incidents as they are.
func (s *Server) evaluateCondition(ctx context.Context, project string, p *monitoringpb.AlertPolicy, c *monitoringpb.AlertPolicy_Condition, since, now time.Time, logs LogMatcher) ([]violation, bool) {
	switch {
	case c.GetConditionThreshold() != nil:
		return s.evalThreshold(ctx, project, c.GetConditionThreshold(), now)
	case c.GetConditionAbsent() != nil:
		return s.evalAbsent(ctx, project, c.GetConditionAbsent(), now)
	case c.GetConditionMonitoringQueryLanguage() != nil:
		return s.evalMQL(ctx, project, c.GetConditionMonitoringQueryLanguage(), now)
	case c.GetConditionPrometheusQueryLanguage() != nil:
		return s.evalPromQL(ctx, project, c.GetName(), c.GetConditionPrometheusQueryLanguage(), now)
	case c.GetConditionMatchedLog() != nil:
		if logs == nil {
			return nil, false
		}
		l := c.GetConditionMatchedLog()
		ms, err := logs.MatchLogEntries(project, l.GetFilter(), l.GetLabelExtractors(), since, now)
		if err != nil {
			return nil, false
		}
		var out []violation
		for _, m := range ms {
			out = append(out, violation{key: "log|" + m.Log + "|" + m.Time.String(), log: true, resourceType: m.Type, resourceLabels: m.Resource,
				metricLabels: m.Labels, summary: "Log match condition fired: " + m.Summary})
		}
		return out, true
	}
	return nil, false
}

// conditionRead reads a condition's series with its aggregations.
func (s *Server) conditionRead(ctx context.Context, project, filter string, aggs []*monitoringpb.Aggregation, start, end time.Time) ([]*monitoringpb.TimeSeries, error) {
	req := &monitoringpb.ListTimeSeriesRequest{Name: "projects/" + project, Filter: filter,
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(end)}, PageSize: 100000}
	if len(aggs) > 0 {
		req.Aggregation = aggs[0]
	}
	if len(aggs) > 1 {
		req.SecondaryAggregation = &monitoringpb.Aggregation{CrossSeriesReducer: aggs[1].GetCrossSeriesReducer(), GroupByFields: aggs[1].GetGroupByFields()}
	}
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

func periodOf(aggs []*monitoringpb.Aggregation) time.Duration {
	if len(aggs) > 0 && aggs[0].GetAlignmentPeriod() != nil {
		return aggs[0].GetAlignmentPeriod().AsDuration()
	}
	return 0
}

// staleAfter is how old a series' newest point may be and still be
// evaluated, without an alignment period.
const staleAfter = 5 * time.Minute

func (s *Server) evalThreshold(ctx context.Context, project string, t *monitoringpb.AlertPolicy_Condition_MetricThreshold, now time.Time) ([]violation, bool) {
	dur := t.GetDuration().AsDuration()
	recent := max(periodOf(t.GetAggregations()), staleAfter)
	series, err := s.conditionRead(ctx, project, t.GetFilter(), t.GetAggregations(), now.Add(-dur-recent), now)
	if err != nil {
		return nil, false
	}
	var denom map[string]*monitoringpb.TimeSeries
	if t.GetDenominatorFilter() != "" {
		ds, err := s.conditionRead(ctx, project, t.GetDenominatorFilter(), t.GetDenominatorAggregations(), now.Add(-dur-recent), now)
		if err != nil {
			return nil, false
		}
		denom = map[string]*monitoringpb.TimeSeries{}
		for _, d := range ds {
			denom[labelsKey(d)] = d
		}
	}
	if len(series) == 0 {
		switch t.GetEvaluationMissingData() {
		case monitoringpb.AlertPolicy_Condition_EVALUATION_MISSING_DATA_ACTIVE:
			return []violation{{key: "missing", resourceKey: "missing", summary: "No data arrived for the condition, which treats missing data as violating"}}, true
		case monitoringpb.AlertPolicy_Condition_EVALUATION_MISSING_DATA_NO_OP:
			return nil, false
		}
		return nil, true
	}
	var out []violation
	for _, ts := range series {
		pts := ts.GetPoints() // newest first
		if denom != nil {
			pts = ratioPoints(pts, denom[labelsKey(ts)])
		}
		if len(pts) == 0 {
			continue
		}
		latest := pts[0]
		if now.Sub(endOf(latest)) > recent {
			continue
		}
		if !compare(t.GetComparison(), valueOf(latest.GetValue()), t.GetThresholdValue()) {
			continue
		}
		since := endOf(latest)
		for _, p := range pts[1:] {
			if !compare(t.GetComparison(), valueOf(p.GetValue()), t.GetThresholdValue()) {
				break
			}
			since = endOf(p)
		}
		if dur > 0 && now.Sub(since) < dur {
			continue
		}
		out = append(out, seriesViolation(ts, fmt.Sprintf("%g", valueOf(latest.GetValue())), fmt.Sprintf("%g", t.GetThresholdValue()),
			fmt.Sprintf("%s is %s %g (currently %g)", ts.GetMetric().GetType(), comparisonWords(t.GetComparison()), t.GetThresholdValue(), valueOf(latest.GetValue()))))
	}
	return trigger(t.GetTrigger(), out, len(series)), true
}

func ratioPoints(num []*monitoringpb.Point, den *monitoringpb.TimeSeries) []*monitoringpb.Point {
	if den == nil {
		return nil
	}
	by := map[int64]float64{}
	for _, p := range den.GetPoints() {
		by[endOf(p).Unix()] = valueOf(p.GetValue())
	}
	var out []*monitoringpb.Point
	for _, p := range num {
		d, ok := by[endOf(p).Unix()]
		if !ok || d == 0 {
			continue
		}
		out = append(out, &monitoringpb.Point{Interval: p.GetInterval(), Value: typed(tDouble, valueOf(p.GetValue())/d)})
	}
	return out
}

func (s *Server) evalAbsent(ctx context.Context, project string, a *monitoringpb.AlertPolicy_Condition_MetricAbsence, now time.Time) ([]violation, bool) {
	dur := a.GetDuration().AsDuration()
	series, err := s.conditionRead(ctx, project, a.GetFilter(), a.GetAggregations(), now.Add(-dur-24*time.Hour), now)
	if err != nil {
		return nil, false
	}
	var out []violation
	for _, ts := range series {
		if len(ts.GetPoints()) == 0 {
			continue
		}
		last := endOf(ts.GetPoints()[0])
		if now.Sub(last) < dur {
			continue
		}
		out = append(out, seriesViolation(ts, "", "", fmt.Sprintf("%s has had no data for %s (last point %s)", ts.GetMetric().GetType(), durationpb.New(dur).AsDuration(), last.UTC().Format(time.RFC3339))))
	}
	return trigger(a.GetTrigger(), out, len(series)), true
}

func trigger(t *monitoringpb.AlertPolicy_Condition_Trigger, vs []violation, total int) []violation {
	switch {
	case t.GetPercent() > 0:
		if total == 0 || float64(len(vs))*100/float64(total) < t.GetPercent() {
			return nil
		}
	case t.GetCount() > 1:
		if len(vs) < int(t.GetCount()) {
			return nil
		}
	}
	return vs
}

func seriesViolation(ts *monitoringpb.TimeSeries, observed, threshold, summary string) violation {
	return violation{key: seriesKey(ts), resourceKey: resourceKey(ts), metricType: ts.GetMetric().GetType(), metricLabels: ts.GetMetric().GetLabels(),
		resourceType: ts.GetResource().GetType(), resourceLabels: ts.GetResource().GetLabels(), observed: observed, threshold: threshold, summary: summary}
}

func resourceKey(ts *monitoringpb.TimeSeries) string {
	var b strings.Builder
	b.WriteString(ts.GetResource().GetType())
	writeLabels(&b, ts.GetResource().GetLabels())
	return b.String()
}

func labelsKey(ts *monitoringpb.TimeSeries) string {
	var b strings.Builder
	writeLabels(&b, ts.GetMetric().GetLabels())
	b.WriteString("|")
	b.WriteString(resourceKey(ts))
	return b.String()
}

func valueOf(v *monitoringpb.TypedValue) float64 {
	if d := v.GetDistributionValue(); d != nil {
		return d.GetMean()
	}
	if _, ok := v.GetValue().(*monitoringpb.TypedValue_BoolValue); ok {
		if v.GetBoolValue() {
			return 1
		}
		return 0
	}
	return asFloat(v)
}

func compare(c monitoringpb.ComparisonType, v, threshold float64) bool {
	if math.IsNaN(v) {
		return false
	}
	switch c {
	case monitoringpb.ComparisonType_COMPARISON_GT:
		return v > threshold
	case monitoringpb.ComparisonType_COMPARISON_GE:
		return v >= threshold
	case monitoringpb.ComparisonType_COMPARISON_LT:
		return v < threshold
	case monitoringpb.ComparisonType_COMPARISON_LE:
		return v <= threshold
	case monitoringpb.ComparisonType_COMPARISON_EQ:
		return v == threshold
	case monitoringpb.ComparisonType_COMPARISON_NE:
		return v != threshold
	}
	return false
}

func comparisonWords(c monitoringpb.ComparisonType) string {
	return map[monitoringpb.ComparisonType]string{
		monitoringpb.ComparisonType_COMPARISON_GT: "above", monitoringpb.ComparisonType_COMPARISON_GE: "at or above",
		monitoringpb.ComparisonType_COMPARISON_LT: "below", monitoringpb.ComparisonType_COMPARISON_LE: "at or below",
		monitoringpb.ComparisonType_COMPARISON_EQ: "equal to", monitoringpb.ComparisonType_COMPARISON_NE: "not equal to",
	}[c]
}

// applyViolations opens and closes p's incidents; it returns the ones
// that changed, to notify.
func (s *Server) applyViolations(p *monitoringpb.AlertPolicy, met map[string][]violation, now time.Time) []Incident {
	s.mu.Lock()
	defer s.mu.Unlock()
	project := projectOfName(p.GetName())
	snoozed := s.snoozedLocked(p.GetName(), now)
	autoClose := 7 * 24 * time.Hour
	if ac := p.GetAlertStrategy().GetAutoClose(); ac != nil {
		autoClose = ac.AsDuration()
	}
	var rate time.Duration
	if r := p.GetAlertStrategy().GetNotificationRateLimit().GetPeriod(); r != nil {
		rate = r.AsDuration()
	}
	var changed []Incident
	for _, c := range p.GetConditions() {
		vs, evaluated := met[c.GetName()]
		open := map[string]*Incident{}
		for _, in := range s.incidents {
			if in.Policy == p.GetName() && in.Condition == c.GetName() && in.State == IncidentOpen {
				open[in.Key] = in
			}
		}
		seen := map[string]bool{}
		for _, v := range vs {
			seen[v.key] = true
			if open[v.key] != nil || snoozed {
				continue
			}
			if v.log && rate > 0 && s.lastLogIncident(p.GetName(), now) > now.Add(-rate).Unix() {
				continue
			}
			in := &Incident{ID: newID(), Project: project, Policy: p.GetName(), PolicyName: p.GetDisplayName(), Condition: c.GetName(),
				ConditionName: c.GetDisplayName(), Key: v.key, State: IncidentOpen, Summary: v.summary, Started: now,
				MetricType: v.metricType, MetricLabels: v.metricLabels, ResourceType: v.resourceType, ResourceLabels: v.resourceLabels,
				Observed: v.observed, Threshold: v.threshold, Severity: severityName(p.GetSeverity())}
			s.incidents = append(s.incidents, in)
			changed = append(changed, *in)
		}
		for k, in := range open {
			closeIt := false
			switch {
			case c.GetConditionMatchedLog() != nil:
				closeIt = now.Sub(in.Started) >= autoClose
			case evaluated && !seen[k]:
				closeIt = true
			case !evaluated && now.Sub(in.Started) >= autoClose:
				closeIt = true
			}
			if closeIt {
				in.State, in.Ended = IncidentClosed, now
				changed = append(changed, *in)
			}
		}
	}
	s.trimIncidents()
	return changed
}

func (s *Server) lastLogIncident(policy string, now time.Time) int64 {
	var last int64
	for _, in := range s.incidents {
		if in.Policy == policy && in.Started.Unix() > last {
			last = in.Started.Unix()
		}
	}
	_ = now
	return last
}

func (s *Server) trimIncidents() {
	if len(s.incidents) <= MaxIncidents {
		return
	}
	sort.SliceStable(s.incidents, func(i, j int) bool {
		a, b := s.incidents[i], s.incidents[j]
		if (a.State == IncidentOpen) != (b.State == IncidentOpen) {
			return b.State == IncidentOpen
		}
		return a.Started.Before(b.Started)
	})
	s.incidents = s.incidents[len(s.incidents)-MaxIncidents:]
	sort.SliceStable(s.incidents, func(i, j int) bool { return s.incidents[i].Started.Before(s.incidents[j].Started) })
}

// notifyIncidents tells each changed incident's policy's channels.
func (s *Server) notifyIncidents(ctx context.Context, changed []Incident) {
	for _, in := range changed {
		s.mu.Lock()
		p, _ := s.get(kindPolicies, in.Policy).(*monitoringpb.AlertPolicy)
		var channels []*monitoringpb.NotificationChannel
		if p != nil {
			p = clone(p)
			for _, n := range p.GetNotificationChannels() {
				if c, ok := s.get(kindChannels, n).(*monitoringpb.NotificationChannel); ok {
					channels = append(channels, clone(c))
				}
			}
		}
		publish := s.publish
		s.mu.Unlock()
		if p == nil || !prompted(p, in.State) {
			continue
		}
		s.notify(ctx, in, p, channels, publish)
	}
}

// prompted is whether a policy notifies an incident's opening or closing:
// both by default, only openings for log-based policies.
func prompted(p *monitoringpb.AlertPolicy, state string) bool {
	prompts := p.GetAlertStrategy().GetNotificationPrompts()
	if len(prompts) == 0 {
		if len(p.GetConditions()) == 1 && p.GetConditions()[0].GetConditionMatchedLog() != nil {
			return state == IncidentOpen
		}
		return true
	}
	for _, pr := range prompts {
		if pr == monitoringpb.AlertPolicy_AlertStrategy_OPENED && state == IncidentOpen ||
			pr == monitoringpb.AlertPolicy_AlertStrategy_CLOSED && state == IncidentClosed {
			return true
		}
	}
	return false
}
