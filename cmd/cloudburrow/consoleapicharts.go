package main

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/metrics"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

// The APIs & Services charts over a time range (#1866), per project, from
// the request history Cloud Monitoring serves as
// serviceruntime.googleapis.com/api/request_count and request_latencies:
// the dashboard's Traffic, Errors and Median latency, and an API's
// Overview (Traffic by response code) and Metrics graphs.
// docs.cloud.google.com/apis/docs/monitoring names them.

// apiRange is one of the time ranges the pages offer.
type apiRange struct {
	ID, Label string
	Span      time.Duration
}

// apiRanges are the ranges, shortest first; the first is the default.
var apiRanges = []apiRange{
	{"PT1H", "1 hour", time.Hour},
	{"PT6H", "6 hours", 6 * time.Hour},
	{"PT12H", "12 hours", 12 * time.Hour},
	{"P1D", "1 day", 24 * time.Hour},
	{"P2D", "2 days", 48 * time.Hour},
	{"P4D", "4 days", 96 * time.Hour},
	{"P7D", "7 days", 7 * 24 * time.Hour},
	{"P14D", "14 days", 14 * 24 * time.Hour},
	{"P30D", "30 days", 30 * 24 * time.Hour},
}

func lookupAPIRange(id string) (apiRange, error) {
	if id == "" {
		return apiRanges[0], nil
	}
	for _, r := range apiRanges {
		if r.ID == id {
			return r, nil
		}
	}
	return apiRange{}, fmt.Errorf("unknown time range %q", id)
}

// apiWindow is the request history of one project over one range.
type apiWindow struct {
	Range      apiRange
	Start, End time.Time
	// Began is when the history starts: no chart point is drawn before it.
	Began  time.Time
	Deltas []monitoring.RequestDelta
}

// apiPoint is one chart point: Value is nil where nothing is known (a rate
// of no calls is 0; an error share or a latency of no calls is nil).
type apiPoint struct {
	At    time.Time `json:"at"`
	Value *float64  `json:"value"`
}

type apiSeries struct {
	Name   string     `json:"name"`
	Points []apiPoint `json:"points"`
}

// apiChart is one graph: Default ones are shown until Select Graphs
// changes the choice.
type apiChart struct {
	ID      string      `json:"id"`
	Title   string      `json:"title"`
	Unit    string      `json:"unit"`
	Default bool        `json:"default"`
	Series  []apiSeries `json:"series"`
}

// step is the width of one chart point: a minute, or a sixtieth of the
// range, whichever is longer.
func (w apiWindow) step() time.Duration {
	s := w.Range.Span / 60
	if s < time.Minute {
		s = time.Minute
	}
	return s
}

func deltaPoint(d monitoring.RequestDelta) metrics.Point {
	return metrics.Point{Service: d.Service, Method: d.Method, Code: d.Code, Count: d.Count, Sum: d.Sum, Buckets: d.Buckets}
}

// binned groups the deltas of the given metrics services into the
// window's steps, by group. It returns each step's end and, per step, the
// totals of each group.
func (w apiWindow) binned(services map[string]bool, group func(monitoring.RequestDelta) string) ([]time.Time, []map[string]*totalsAcc, []string) {
	step := w.step()
	var ends []time.Time
	for at := w.Start.Add(step); !at.After(w.End.Add(step - 1)); at = at.Add(step) {
		if !at.After(w.Began) {
			continue
		}
		ends = append(ends, at)
	}
	bins := make([]map[string]*totalsAcc, len(ends))
	for i := range bins {
		bins[i] = map[string]*totalsAcc{}
	}
	seen := map[string]bool{}
	for _, d := range w.Deltas {
		if !services[d.Service] {
			continue
		}
		i := sort.Search(len(ends), func(i int) bool { return !ends[i].Before(d.End) })
		if i == len(ends) {
			i = len(ends) - 1
		}
		if i < 0 {
			continue
		}
		g := group(d)
		seen[g] = true
		a := bins[i][g]
		if a == nil {
			a = &totalsAcc{}
			bins[i][g] = a
		}
		a.add(deltaPoint(d))
	}
	var groups []string
	for g := range seen {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	return ends, bins, groups
}

// apiValue reads one figure of a step's totals: nil when it is undefined.
type apiValue func(a *totalsAcc, secs float64) *float64

func rateValue(a *totalsAcc, secs float64) *float64 {
	v := 0.0
	if a != nil {
		v = float64(a.n) / secs
	}
	return &v
}

func errorPctValue(a *totalsAcc, _ float64) *float64 {
	if a == nil || a.n == 0 {
		return nil
	}
	v := float64(a.errs) / float64(a.n) * 100
	return &v
}

func latencyValue(q float64) apiValue {
	return func(a *totalsAcc, _ float64) *float64 {
		if a == nil || a.n == 0 {
			return nil
		}
		return quantileMS(q, a.buckets, a.n)
	}
}

func round3(v *float64) *float64 {
	if v == nil {
		return nil
	}
	r := math.Round(*v*1000) / 1000
	return &r
}

// seriesOf draws one series per group, each with value.
func (w apiWindow) seriesOf(services map[string]bool, group func(monitoring.RequestDelta) string, value apiValue) []apiSeries {
	ends, bins, groups := w.binned(services, group)
	secs := w.step().Seconds()
	out := []apiSeries{}
	for _, g := range groups {
		s := apiSeries{Name: g, Points: make([]apiPoint, len(ends))}
		for i, at := range ends {
			s.Points[i] = apiPoint{At: at, Value: round3(value(bins[i][g], secs))}
		}
		out = append(out, s)
	}
	return out
}

// overall is one series over every call, named name, with each value.
func (w apiWindow) overall(services map[string]bool, names []string, values ...apiValue) []apiSeries {
	ends, bins, _ := w.binned(services, func(monitoring.RequestDelta) string { return "" })
	secs := w.step().Seconds()
	out := []apiSeries{}
	for k, value := range values {
		s := apiSeries{Name: names[k], Points: make([]apiPoint, len(ends))}
		for i, at := range ends {
			s.Points[i] = apiPoint{At: at, Value: round3(value(bins[i][""], secs))}
		}
		out = append(out, s)
	}
	return out
}

// dashboardCharts are Enabled APIs & services' Traffic, Errors and Median
// latency over the project's enabled, counted APIs.
func (w apiWindow) dashboardCharts(services map[string]bool) []apiChart {
	return []apiChart{
		{ID: "traffic", Title: "Traffic", Unit: "Requests/sec", Default: true, Series: w.overall(services, []string{"Requests"}, rateValue)},
		{ID: "errors", Title: "Errors", Unit: "Percent of requests", Default: true, Series: w.overall(services, []string{"Errors"}, errorPctValue)},
		{ID: "latency", Title: "Median latency", Unit: "Milliseconds", Default: true, Series: w.overall(services, []string{"Median"}, latencyValue(0.5))},
	}
}

func byCode(d monitoring.RequestDelta) string   { return d.ResponseCode }
func byMethod(d monitoring.RequestDelta) string { return d.Method }

// apiPageCharts are an API's graphs: the four Google shows by default on
// its metrics, and Traffic by API method, which Select Graphs adds.
func (w apiWindow) apiPageCharts(services map[string]bool) []apiChart {
	return []apiChart{
		{ID: "traffic-by-code", Title: "Traffic by response code", Unit: "Requests/sec", Default: true, Series: w.seriesOf(services, byCode, rateValue)},
		{ID: "errors-by-method", Title: "Errors by API method", Unit: "Percent of requests", Default: true, Series: w.seriesOf(services, byMethod, errorPctValue)},
		{ID: "latency-overall", Title: "Overall latency", Unit: "Milliseconds", Default: true,
			Series: w.overall(services, []string{"50th percentile", "95th percentile", "99th percentile"}, latencyValue(0.5), latencyValue(0.95), latencyValue(0.99))},
		{ID: "latency-by-method", Title: "Latency by API method (median)", Unit: "Milliseconds", Default: true, Series: w.seriesOf(services, byMethod, latencyValue(0.5))},
		{ID: "traffic-by-method", Title: "Traffic by API method", Unit: "Requests/sec", Series: w.seriesOf(services, byMethod, rateValue)},
	}
}
