package logging

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// Values of log-based metrics (#1939), for Cloud Monitoring's
// logging.googleapis.com/user/* time series: each enabled metric counts,
// or records the extracted value of, the entries its filter matches that
// were received after it was created, as Google's do
// (docs.cloud.google.com/logging/docs/logs-based-metrics: "only for log
// entries that are received after the metric is created"), with its labels'
// extractors applied.

// MetricSample is one entry a log-based metric matched.
type MetricSample struct {
	Entry *loggingpb.LogEntry
	// Labels are the metric's labels' extracted values; a label whose
	// extractor finds nothing is "".
	Labels map[string]string
	// Value is a distribution's extracted value; HasValue is false when the
	// entry held none, and the entry is not counted.
	Value    float64
	HasValue bool
}

// EnabledMetrics are a project's enabled log-based metrics.
func (s *Server) EnabledMetrics(project string) []*loggingpb.LogMetric {
	if s.admin == nil {
		return nil
	}
	ms, err := s.admin.Metrics(project)
	if err != nil {
		return nil
	}
	var out []*loggingpb.LogMetric
	for _, m := range ms {
		if !m.GetDisabled() {
			out = append(out, m)
		}
	}
	return out
}

// ProjectEntries are a project's stored entries with timestamps in
// [from, to].
func (s *Server) ProjectEntries(project string, from, to time.Time) []*loggingpb.LogEntry {
	var out []*loggingpb.LogEntry
	for _, e := range s.store.snapshot() {
		p, _, err := resource.ParseLogName(e.GetLogName())
		t := e.GetTimestamp().AsTime()
		if err == nil && p == project && !t.Before(from) && !t.After(to) {
			out = append(out, e)
		}
	}
	return out
}

// MetricSamples are the entries the enabled metric matched with timestamps
// in [from, to].
func (s *Server) MetricSamples(project, name string, from, to time.Time) ([]MetricSample, error) {
	var m *loggingpb.LogMetric
	for _, x := range s.EnabledMetrics(project) {
		if x.GetName() == name {
			m = x
		}
	}
	if m == nil {
		return nil, nil
	}
	match, err := compile(m.GetFilter())
	if err != nil {
		return nil, err
	}
	var entries []*loggingpb.LogEntry
	if b := m.GetBucketName(); b != "" {
		ref, err := parseBucket("bucket_name", b)
		if err != nil {
			return nil, err
		}
		entries = s.store.bucketEntries(ref)
	} else {
		entries = s.ProjectEntries(project, from, to)
	}
	created := m.GetCreateTime().AsTime()
	labels := map[string]extractor{}
	for k, expr := range m.GetLabelExtractors() {
		x, err := parseExtractor(expr)
		if err != nil {
			return nil, err
		}
		labels[k] = x
	}
	var value *extractor
	if m.GetValueExtractor() != "" {
		x, err := parseExtractor(m.GetValueExtractor())
		if err != nil {
			return nil, err
		}
		value = &x
	}
	var out []MetricSample
	for _, e := range entries {
		t := e.GetTimestamp().AsTime()
		if t.Before(from) || t.After(to) || !match(e) {
			continue
		}
		received := t
		if e.GetReceiveTimestamp() != nil {
			received = e.GetReceiveTimestamp().AsTime()
		}
		if m.GetCreateTime() != nil && received.Before(created) {
			continue
		}
		fields := entryFields(e)
		smp := MetricSample{Entry: e, Labels: map[string]string{}}
		for k, x := range labels {
			v, _ := x.apply(fields)
			smp.Labels[k] = v
		}
		if value != nil {
			v, ok := value.apply(fields)
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); ok && err == nil {
				smp.Value, smp.HasValue = f, true
			}
		}
		out = append(out, smp)
	}
	return out, nil
}

// extractor is a parsed EXTRACT(field) or REGEXP_EXTRACT(field, "regex").
type extractor struct {
	path []string
	re   *regexp.Regexp
}

func parseExtractor(expr string) (extractor, error) {
	m := extractorRe.FindStringSubmatch(expr)
	if m == nil {
		return extractor{}, fmt.Errorf("extractor %q is not EXTRACT(field) or REGEXP_EXTRACT(field, \"regex\")", expr)
	}
	x := extractor{path: fieldPath(m[2])}
	if m[1] == "REGEXP_EXTRACT" {
		re, err := regexp.Compile(strings.ReplaceAll(m[3][1:len(m[3])-1], `\"`, `"`))
		if err != nil {
			return extractor{}, err
		}
		x.re = re
	}
	return x, nil
}

// fieldPath splits a.b."c.d" into its segments.
func fieldPath(f string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for _, r := range f {
		switch {
		case r == '"':
			quoted = !quoted
		case r == '.' && !quoted:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(out, cur.String())
}

// entryFields is an entry in its JSON form, which field paths name.
func entryFields(e *loggingpb.LogEntry) map[string]any {
	b, err := protojson.Marshal(e)
	if err != nil {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// apply is the extracted value and whether there was one.
func (x extractor) apply(fields map[string]any) (string, bool) {
	var cur any = fields
	for _, seg := range x.path {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		if cur, ok = m[seg]; !ok {
			return "", false
		}
	}
	var v string
	switch c := cur.(type) {
	case string:
		v = c
	case float64:
		v = strconv.FormatFloat(c, 'g', -1, 64)
	case bool:
		v = strconv.FormatBool(c)
	case nil:
		return "", false
	default:
		b, _ := json.Marshal(c)
		v = string(b)
	}
	if x.re == nil {
		return v, true
	}
	sm := x.re.FindStringSubmatch(v)
	if len(sm) < 2 {
		return "", false
	}
	return sm[1], true
}

// MatchEntries are a project's stored entries the filter matches that were
// received in (after, until], with label extractors applied, for Cloud
// Monitoring's log-match alert conditions (#1964). The filter is compiled
// as ListLogEntries compiles it.
func (s *Server) MatchEntries(project, filter string, extractors map[string]string, after, until time.Time) ([]MetricSample, error) {
	match, err := compile(filter)
	if err != nil {
		return nil, err
	}
	labels := map[string]extractor{}
	for k, expr := range extractors {
		x, err := parseExtractor(expr)
		if err != nil {
			return nil, err
		}
		labels[k] = x
	}
	var out []MetricSample
	for _, e := range s.store.snapshot() {
		p, _, err := resource.ParseLogName(e.GetLogName())
		if err != nil || p != project {
			continue
		}
		received := e.GetTimestamp().AsTime()
		if e.GetReceiveTimestamp() != nil {
			received = e.GetReceiveTimestamp().AsTime()
		}
		if !received.After(after) || received.After(until) || !match(e) {
			continue
		}
		smp := MetricSample{Entry: e, Labels: map[string]string{}}
		if len(labels) > 0 {
			fields := entryFields(e)
			for k, x := range labels {
				v, _ := x.apply(fields)
				smp.Labels[k] = v
			}
		}
		out = append(out, smp)
	}
	return out, nil
}
