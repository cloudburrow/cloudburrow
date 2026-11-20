package monitoring

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	promparser "github.com/prometheus/prometheus/promql/parser"
)

// The Prometheus HTTP API Cloud Monitoring serves (#1966) at
// monitoring.googleapis.com/v1/projects/PROJECT/location/global/prometheus/api/v1/
// (docs.cloud.google.com/stackdriver/docs/managed-prometheus/query-api-ui,
// read 2026-11-20): query, query_range, labels, series,
// label/NAME/values and metadata, GET or POST with form parameters, in
// Prometheus' JSON format (prometheus.io/docs/prometheus/latest/querying/api).

// promAPIPrefix is the path before the project.
const promAPIPrefix = "/v1/projects/"

// promAPIPath splits a Prometheus API path into the project and the
// endpoint after api/v1/.
func promAPIPath(p string) (project, endpoint string, ok bool) {
	rest, ok := strings.CutPrefix(p, promAPIPrefix)
	if !ok {
		return "", "", false
	}
	project, rest, ok = strings.Cut(rest, "/")
	if !ok || project == "" {
		return "", "", false
	}
	endpoint, ok = strings.CutPrefix(rest, "location/global/prometheus/api/v1/")
	return project, endpoint, ok
}

// withPrometheusAPI serves the Prometheus API in front of next.
func withPrometheusAPI(s *Server, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		project, endpoint, ok := promAPIPath(r.URL.Path)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		s.servePrometheus(w, r, project, endpoint)
	})
}

type promError struct {
	status int
	kind   string
	msg    string
}

func (e *promError) Error() string { return e.msg }

func badData(msg string) error { return &promError{http.StatusBadRequest, "bad_data", msg} }

func (s *Server) servePrometheus(w http.ResponseWriter, r *http.Request, project, endpoint string) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeProm(w, nil, &promError{http.StatusMethodNotAllowed, "bad_data", "method not allowed"})
		return
	}
	if !projectNameRE.MatchString("projects/" + project) {
		writeProm(w, nil, &promError{http.StatusBadRequest, "bad_data", "invalid project " + strconv.Quote(project)})
		return
	}
	if err := r.ParseForm(); err != nil {
		writeProm(w, nil, badData(err.Error()))
		return
	}
	ctx := r.Context()
	if t := r.Form.Get("timeout"); t != "" {
		d, err := parsePromDuration(t)
		if err != nil {
			writeProm(w, nil, badData("invalid parameter \"timeout\": "+err.Error()))
			return
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	var data any
	var err error
	switch {
	case endpoint == "query":
		data, err = s.promQueryEndpoint(ctx, project, r)
	case endpoint == "query_range":
		data, err = s.promRangeEndpoint(ctx, project, r)
	case endpoint == "labels":
		data, err = s.promLabelsEndpoint(ctx, project, r, "")
	case strings.HasPrefix(endpoint, "label/") && strings.HasSuffix(endpoint, "/values"):
		name := strings.TrimSuffix(strings.TrimPrefix(endpoint, "label/"), "/values")
		if name == "" || strings.Contains(name, "/") {
			err = badData("invalid label name " + strconv.Quote(name))
			break
		}
		data, err = s.promLabelsEndpoint(ctx, project, r, name)
	case endpoint == "series":
		data, err = s.promSeriesEndpoint(ctx, project, r)
	case endpoint == "metadata":
		data, err = s.promMetadataEndpoint(project, r)
	default:
		err = &promError{http.StatusNotFound, "not_found", "unknown API endpoint " + endpoint}
	}
	writeProm(w, data, err)
}

func writeProm(w http.ResponseWriter, data any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		pe := &promError{}
		if !errors.As(err, &pe) {
			pe = &promError{http.StatusUnprocessableEntity, "execution", err.Error()}
			var perr promparser.ParseErrors
			if errors.As(err, &perr) || errors.Is(err, errMultipleResources) {
				pe.status, pe.kind = http.StatusBadRequest, "bad_data"
			}
			if errors.Is(err, context.DeadlineExceeded) {
				pe.status, pe.kind = http.StatusServiceUnavailable, "timeout"
			}
		}
		w.WriteHeader(pe.status)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "errorType": pe.kind, "error": pe.msg})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": data})
}

// parsePromTime reads a time as RFC 3339 or Unix seconds.
func parsePromTime(v string, def time.Time) (time.Time, error) {
	if v == "" {
		return def, nil
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		sec, frac := math.Modf(f)
		return time.Unix(int64(sec), int64(math.Round(frac*1e9))).UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t, nil
	}
	return time.Time{}, errors.New("cannot parse " + strconv.Quote(v) + " to a valid timestamp")
}

func parsePromDuration(v string) (time.Duration, error) {
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		if f <= 0 || math.IsInf(f, 0) || math.IsNaN(f) {
			return 0, errors.New("must be a positive duration")
		}
		return time.Duration(f * float64(time.Second)), nil
	}
	d, err := promDuration(v)
	if err != nil {
		return 0, errors.New("cannot parse " + strconv.Quote(v) + " to a valid duration")
	}
	return d, nil
}

func (s *Server) promQueryEndpoint(ctx context.Context, project string, r *http.Request) (any, error) {
	q := r.Form.Get("query")
	if q == "" {
		return nil, badData("invalid parameter \"query\": query must not be empty")
	}
	t, err := parsePromTime(r.Form.Get("time"), s.now())
	if err != nil {
		return nil, badData("invalid parameter \"time\": " + err.Error())
	}
	res, err := s.PromInstant(ctx, project, q, t)
	if err != nil {
		return nil, err
	}
	return promResultJSON(res.Value), nil
}

// MaxPromPoints is Prometheus' limit on a range query's resolution.
const MaxPromPoints = 11000

func (s *Server) promRangeEndpoint(ctx context.Context, project string, r *http.Request) (any, error) {
	q := r.Form.Get("query")
	if q == "" {
		return nil, badData("invalid parameter \"query\": query must not be empty")
	}
	if r.Form.Get("start") == "" || r.Form.Get("end") == "" || r.Form.Get("step") == "" {
		return nil, badData("invalid parameter: start, end and step are required")
	}
	start, err := parsePromTime(r.Form.Get("start"), time.Time{})
	if err != nil {
		return nil, badData("invalid parameter \"start\": " + err.Error())
	}
	end, err := parsePromTime(r.Form.Get("end"), time.Time{})
	if err != nil {
		return nil, badData("invalid parameter \"end\": " + err.Error())
	}
	if end.Before(start) {
		return nil, badData("invalid parameter \"end\": end timestamp must not be before start time")
	}
	step, err := parsePromDuration(r.Form.Get("step"))
	if err != nil {
		return nil, badData("invalid parameter \"step\": " + err.Error())
	}
	if end.Sub(start)/step > MaxPromPoints {
		return nil, badData("exceeded maximum resolution of 11,000 points per timeseries. Try decreasing the query resolution (?step=XX)")
	}
	res, err := s.PromRange(ctx, project, q, start, end, step)
	if err != nil {
		return nil, err
	}
	return promResultJSON(res.Value), nil
}

// promMatchers reads match[]; none selects every series.
func promMatchers(r *http.Request, required bool) ([][]*labels.Matcher, error) {
	ms := r.Form["match[]"]
	if len(ms) == 0 {
		if required {
			return nil, badData("no match[] parameter provided")
		}
		return [][]*labels.Matcher{{labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, ".+")}}, nil
	}
	var out [][]*labels.Matcher
	for _, m := range ms {
		sel, err := promParser.ParseMetricSelector(m)
		if err != nil {
			return nil, badData("invalid parameter \"match[]\": " + err.Error())
		}
		out = append(out, sel)
	}
	return out, nil
}

// promWindow reads start and end, by default the last 24 hours.
func (s *Server) promWindow(r *http.Request) (time.Time, time.Time, error) {
	end, err := parsePromTime(r.Form.Get("end"), s.now())
	if err != nil {
		return end, end, badData("invalid parameter \"end\": " + err.Error())
	}
	start, err := parsePromTime(r.Form.Get("start"), end.Add(-24*time.Hour))
	if err != nil {
		return end, end, badData("invalid parameter \"start\": " + err.Error())
	}
	return start, end, nil
}

func (s *Server) promLabelsEndpoint(ctx context.Context, project string, r *http.Request, name string) (any, error) {
	sets, err := promMatchers(r, false)
	if err != nil {
		return nil, err
	}
	start, end, err := s.promWindow(r)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, ms := range sets {
		series, err := s.promSelect(ctx, project, start, end, ms)
		if err != nil {
			return nil, err
		}
		for _, sr := range series {
			if name == "" {
				sr.Labels().Range(func(l labels.Label) { seen[l.Name] = true })
			} else if v := sr.Labels().Get(name); v != "" {
				seen[v] = true
			}
		}
	}
	return limitStrings(sortedKeys(seen), r), nil
}

func limitStrings(v []string, r *http.Request) []string {
	if n, err := strconv.Atoi(r.Form.Get("limit")); err == nil && n > 0 && n < len(v) {
		return v[:n]
	}
	return v
}

func (s *Server) promSeriesEndpoint(ctx context.Context, project string, r *http.Request) (any, error) {
	sets, err := promMatchers(r, true)
	if err != nil {
		return nil, err
	}
	start, end, err := s.promWindow(r)
	if err != nil {
		return nil, err
	}
	seen := map[string]labels.Labels{}
	for _, ms := range sets {
		series, err := s.promSelect(ctx, project, start, end, ms)
		if err != nil {
			return nil, err
		}
		for _, sr := range series {
			seen[sr.Labels().String()] = sr.Labels()
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []map[string]string{}
	for _, k := range keys {
		out = append(out, seen[k].Map())
	}
	if n, err := strconv.Atoi(r.Form.Get("limit")); err == nil && n > 0 && n < len(out) {
		out = out[:n]
	}
	return out, nil
}

// promMetadataEndpoint is each metric's type, help and unit, by legacy
// name.
func (s *Server) promMetadataEndpoint(project string, r *http.Request) (any, error) {
	want := r.Form.Get("metric")
	s.mu.Lock()
	descs := s.allDescriptors(project)
	s.mu.Unlock()
	out := map[string][]map[string]string{}
	for _, md := range descs {
		name := legacyPromName(md.GetType())
		if want != "" && want != name && want != md.GetType() {
			continue
		}
		kind := "gauge"
		switch {
		case md.GetValueType().String() == "DISTRIBUTION":
			kind = "histogram"
		case md.GetMetricKind().String() == "CUMULATIVE":
			kind = "counter"
		}
		out[name] = append(out[name], map[string]string{"type": kind, "help": md.GetDescription(), "unit": md.GetUnit()})
	}
	return out, nil
}

// promResultJSON is a result in Prometheus' JSON format.
func promResultJSON(v promparser.Value) map[string]any {
	ts := func(t int64) float64 { return float64(t) / 1000 }
	val := func(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
	switch x := v.(type) {
	case promql.Vector:
		out := []map[string]any{}
		for _, s := range x {
			out = append(out, map[string]any{"metric": s.Metric.Map(), "value": []any{ts(s.T), val(s.F)}})
		}
		return map[string]any{"resultType": "vector", "result": out}
	case promql.Matrix:
		out := []map[string]any{}
		for _, s := range x {
			pts := [][]any{}
			for _, p := range s.Floats {
				pts = append(pts, []any{ts(p.T), val(p.F)})
			}
			out = append(out, map[string]any{"metric": s.Metric.Map(), "values": pts})
		}
		return map[string]any{"resultType": "matrix", "result": out}
	case promql.Scalar:
		return map[string]any{"resultType": "scalar", "result": []any{ts(x.T), val(x.V)}}
	case promql.String:
		return map[string]any{"resultType": "string", "result": []any{ts(x.T), x.V}}
	}
	return map[string]any{"resultType": "vector", "result": []any{}}
}
