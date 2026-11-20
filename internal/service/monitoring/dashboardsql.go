package monitoring

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// A dashboard widget's SQL (#2031): Monitoring API v1's TimeSeriesQuery
// has "opsAnalyticsQuery: A query used to fetch a time series, category
// series, or numeric series with SQL" (Preview), an OpsAnalyticsQuery
// {sql}, which is how a Log Analytics chart saved to a custom dashboard is
// kept (docs.cloud.google.com/logging/docs/analyze/query-and-view, "Save
// to dashboard", and the projects.dashboards reference, read 2026-11-20).
// google.golang.org/api/monitoring/v1 carries the field; the
// google.monitoring.dashboard.v1 protos in cloud.google.com/go/monitoring,
// which the transcoder and the store use, do not. So the JSON API takes it
// off each timeSeriesQuery before the transcoder sees the dashboard, keeps
// it beside the dashboard by the query's place in it, and puts it back in
// every dashboard it answers. Over gRPC the field does not exist, as for a
// client built on those protos. The etag covers the rest of the dashboard
// (UNVERIFIED whether Google's also covers the SQL).

// kindDashboardSQL keeps each dashboard's opsAnalyticsQuery values, a
// Struct from the query's place ("widgets/0/xyChart/dataSets/0") to the
// OpsAnalyticsQuery.
const kindDashboardSQL = "dashboardOpsAnalyticsQueries"

var dashboardPathRE = regexp.MustCompile(`^/v1/projects/[^/]+/dashboards(/[^/:]+)?$`)

// opsQueries walks a dashboard's JSON and calls f with each timeSeriesQuery
// object and its place.
func opsQueries(v any, path string, f func(path string, tsq map[string]any)) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := k
			if path != "" {
				p = path + "/" + k
			}
			if k == "timeSeriesQuery" {
				if tsq, ok := x[k].(map[string]any); ok {
					f(p, tsq)
					continue
				}
			}
			opsQueries(x[k], p, f)
		}
	case []any:
		for i, e := range x {
			opsQueries(e, path+"/"+strconv.Itoa(i), f)
		}
	}
}

// takeOpsQueries removes every opsAnalyticsQuery from dashboard d, a
// decoded Dashboard, and returns them by place.
func takeOpsQueries(d map[string]any) (map[string]any, error) {
	out := map[string]any{}
	var err error
	opsQueries(d, "", func(p string, tsq map[string]any) {
		q, ok := tsq["opsAnalyticsQuery"]
		if !ok {
			return
		}
		delete(tsq, "opsAnalyticsQuery")
		m, isMap := q.(map[string]any)
		sql, isString := m["sql"].(string)
		if !isMap || (m["sql"] != nil && !isString) || len(m) > 1 || (len(m) == 1 && m["sql"] == nil) {
			err = apierror.InvalidArgument("Invalid JSON payload received. Field %s/opsAnalyticsQuery is an OpsAnalyticsQuery {sql}", p)
			return
		}
		if strings.TrimSpace(sql) == "" {
			err = apierror.InvalidArgument("Field %s/opsAnalyticsQuery.sql is required", p)
			return
		}
		for k := range tsq {
			if k == "timeSeriesFilter" || k == "timeSeriesFilterRatio" || k == "timeSeriesQueryLanguage" || k == "prometheusQuery" {
				err = apierror.InvalidArgument("Field %s has both opsAnalyticsQuery and %s: a query has one source", p, k)
				return
			}
		}
		out[p] = map[string]any{"sql": sql}
	})
	return out, err
}

// putOpsQueries puts a dashboard's kept opsAnalyticsQuery values back.
func putOpsQueries(d map[string]any, kept map[string]any) {
	if len(kept) == 0 {
		return
	}
	opsQueries(d, "", func(p string, tsq map[string]any) {
		if q, ok := kept[p]; ok {
			tsq["opsAnalyticsQuery"] = q
		}
	})
}

// DashboardSQL is the dashboard's kept opsAnalyticsQuery values: each
// query's place and its SQL.
func (s *Server) DashboardSQL(name string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, _ := s.get(kindDashboardSQL, name).(*structpb.Struct)
	out := map[string]string{}
	for p, v := range st.AsMap() {
		if m, ok := v.(map[string]any); ok {
			if sql, ok := m["sql"].(string); ok {
				out[p] = sql
			}
		}
	}
	return out
}

func (s *Server) keptOpsQueries(name string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, _ := s.get(kindDashboardSQL, name).(*structpb.Struct)
	return st.AsMap()
}

// keepOpsQueries records (or, with none, forgets) a dashboard's values.
func (s *Server) keepOpsQueries(name string, q map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(q) == 0 {
		if s.get(kindDashboardSQL, name) == nil {
			return nil
		}
		delete(s.objects[kindDashboardSQL], name)
		return s.save()
	}
	st, err := structpb.NewStruct(q)
	if err != nil {
		return apierror.Internal(err, "keeping the dashboard's SQL")
	}
	s.put(kindDashboardSQL, name, st)
	return s.save()
}

// withDashboardSQL is the JSON API with opsAnalyticsQuery served on
// dashboards' timeSeriesQuery.
func withDashboardSQL(s *Server, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !dashboardPathRE.MatchString(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		var taken map[string]any
		if r.Method == http.MethodPost || r.Method == http.MethodPatch {
			raw, err := readBody(r)
			if err != nil {
				rest.WriteError(w, apierror.InvalidArgument("reading the request: %v", err))
				return
			}
			var d map[string]any
			if len(bytes.TrimSpace(raw)) > 0 && json.Unmarshal(raw, &d) == nil {
				if taken, err = takeOpsQueries(d); err != nil {
					rest.WriteError(w, err)
					return
				}
				if raw, err = json.Marshal(d); err != nil {
					rest.WriteError(w, apierror.Internal(err, "re-encoding the dashboard"))
					return
				}
			}
			r.Body = readCloser{bytes.NewReader(raw)}
			r.ContentLength = int64(len(raw))
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		body := rec.Body.Bytes()
		if rec.Code == http.StatusOK {
			body = s.dashboardAnswer(r, body, taken)
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(rec.Code)
		_, _ = w.Write(body)
	})
}

// dashboardAnswer keeps what a write took and puts the kept values back in
// the dashboards an answer holds.
func (s *Server) dashboardAnswer(r *http.Request, body []byte, taken map[string]any) []byte {
	if r.Method == http.MethodDelete {
		_ = s.keepOpsQueries(strings.TrimPrefix(r.URL.Path, "/v1/"), nil)
		return body
	}
	var out map[string]any
	if json.Unmarshal(body, &out) != nil {
		return body
	}
	validateOnly := r.URL.Query().Get("validateOnly") == "true"
	put := func(d map[string]any) {
		name, _ := d["name"].(string)
		if name == "" {
			return
		}
		kept := s.keptOpsQueries(name)
		if r.Method == http.MethodPost || r.Method == http.MethodPatch {
			kept = taken
			if !validateOnly {
				_ = s.keepOpsQueries(name, taken)
			}
		}
		putOpsQueries(d, kept)
	}
	if list, ok := out["dashboards"].([]any); ok {
		for _, d := range list {
			if m, ok := d.(map[string]any); ok {
				put(m)
			}
		}
	} else {
		put(out)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return body
	}
	return b
}

type readCloser struct{ *bytes.Reader }

func (readCloser) Close() error { return nil }

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	var b bytes.Buffer
	_, err := b.ReadFrom(http.MaxBytesReader(nil, r.Body, 32<<20))
	return b.Bytes(), err
}

// dashboardsREST is the JSON API the console calls in process.
var dashboardsREST struct {
	sync.Mutex
	h map[*Server]http.Handler
}

// CallDashboardsJSON calls the dashboards JSON API in process, for the
// console, and returns the status and body a client would get.
func (s *Server) CallDashboardsJSON(method, path string, body []byte) (int, []byte) {
	if !dashboardPathRE.MatchString(strings.SplitN(path, "?", 2)[0]) {
		return http.StatusNotFound, []byte(`{"error":{"message":"not a dashboards path"}}`)
	}
	dashboardsREST.Lock()
	if dashboardsREST.h == nil {
		dashboardsREST.h = map[*Server]http.Handler{}
	}
	h := dashboardsREST.h[s]
	if h == nil {
		h = NewRESTHandler(s)
		dashboardsREST.h[s] = h
	}
	dashboardsREST.Unlock()
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	if len(body) > 0 {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}
