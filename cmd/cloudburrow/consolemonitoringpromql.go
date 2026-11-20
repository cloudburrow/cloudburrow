package main

// Metrics explorer's PromQL tab (#1991): Google's Metrics explorer has a
// PromQL code editor beside the builder
// (docs.cloud.google.com/monitoring/promql/promql-in-monitoring), whose query is
// charted over the selected time range. Here each Run query is one
// Prometheus range query over the project's local series, evaluated by
// the same engine as the Prometheus HTTP API's query_range (#1966).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/prometheus/prometheus/promql"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// promRanger is the Monitoring server's PromQL range evaluation.
type promRanger interface {
	PromRange(ctx context.Context, project, q string, start, end time.Time, step time.Duration) (*promql.Result, error)
}

// promqlQueryFields are the PromQL tab's controls.
func promqlQueryFields() []console.Field {
	return []console.Field{
		{Name: "query", Label: "PromQL query", Type: "textarea", Required: true,
			Help: `Such as rate({"custom.googleapis.com/shop/sales"}[5m]) or custom_googleapis_com:shop_sales.`},
		{Name: "step", Label: "Minimum interval (seconds)", Type: "number",
			Help: "Empty to fit the time range: about 240 points."},
		{Name: "range", Label: "Time range (seconds)", Type: "select", Options: explorerRanges, Default: "3600"},
	}
}

// promqlWindow is the range query's start, end and step for the tab's
// values at now.
func promqlWindow(values map[string]string, now time.Time) (time.Time, time.Time, time.Duration, error) {
	rng := int64(3600)
	if v := strings.TrimSpace(values["range"]); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return time.Time{}, time.Time{}, 0, fmt.Errorf("time range %q is not a number of seconds", v)
		}
		rng = n
	}
	step := max(rng/240, 10)
	if v := strings.TrimSpace(values["step"]); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return time.Time{}, time.Time{}, 0, fmt.Errorf("minimum interval %q is not a number of seconds", v)
		}
		step = max(step, n)
	}
	end := now.Truncate(time.Second)
	return end.Add(-time.Duration(rng) * time.Second), end, time.Duration(step) * time.Second, nil
}

// promqlListing runs the tab's query: one row per result series, with its
// labels as columns, its latest value and its points (oldest first) for
// the chart, as the builder's rows are.
func promqlListing(ctx context.Context, a monitoringpb.MetricServiceServer, project string, values map[string]string, now time.Time) (console.Listing, error) {
	pr, ok := a.(promRanger)
	if !ok {
		return console.Listing{}, errors.New("this Monitoring server does not evaluate PromQL")
	}
	q := strings.TrimSpace(values["query"])
	if q == "" {
		return console.Listing{}, errors.New("enter a PromQL query")
	}
	start, end, step, err := promqlWindow(values, now)
	if err != nil {
		return console.Listing{}, err
	}
	res, err := pr.PromRange(ctx, project, q, start, end, step)
	if err != nil {
		return console.Listing{}, err
	}
	out := console.Listing{NameColumn: "Time series", Noun: "time series",
		Note: fmt.Sprintf("query_range %s, step %s", q, step)}
	cols := map[string]bool{}
	add := func(lset map[string]string, pts []explorerPoint) error {
		fields := map[string]string{}
		var parts []string
		keys := make([]string, 0, len(lset))
		for k := range lset {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fields[k], cols[k] = lset[k], true
			parts = append(parts, k+"="+lset[k])
		}
		fields["Value"] = "—"
		for i := len(pts) - 1; i >= 0; i-- {
			if pts[i].Value != nil {
				fields["Value"] = strconv.FormatFloat(*pts[i].Value, 'g', 6, 64)
				break
			}
		}
		b, err := json.Marshal(pts)
		if err != nil {
			return err
		}
		fields["points"] = string(b)
		name := "{" + strings.Join(parts, ", ") + "}"
		out.Items = append(out.Items, console.Resource{Name: name, Fields: fields})
		return nil
	}
	point := func(t int64, f float64) explorerPoint {
		p := explorerPoint{At: time.UnixMilli(t).UTC().Format(time.RFC3339Nano)}
		if !math.IsNaN(f) && !math.IsInf(f, 0) {
			p.Value = &f
		}
		return p
	}
	switch v := res.Value.(type) {
	case promql.Matrix:
		for _, s := range v {
			var pts []explorerPoint
			for _, f := range s.Floats {
				pts = append(pts, point(f.T, f.F))
			}
			if err := add(s.Metric.Map(), pts); err != nil {
				return console.Listing{}, err
			}
		}
	case promql.Vector:
		for _, s := range v {
			if err := add(s.Metric.Map(), []explorerPoint{point(s.T, s.F)}); err != nil {
				return console.Listing{}, err
			}
		}
	case promql.Scalar:
		if err := add(map[string]string{}, []explorerPoint{point(v.T, v.V)}); err != nil {
			return console.Listing{}, err
		}
	default:
		return console.Listing{}, fmt.Errorf("the query's result is a %s, which cannot be charted", res.Value.Type())
	}
	for k := range cols {
		out.Columns = append(out.Columns, k)
	}
	sort.Strings(out.Columns)
	out.Columns = append(out.Columns, "Value")
	sort.SliceStable(out.Items, func(i, j int) bool { return out.Items[i].Name < out.Items[j].Name })
	out.Total = len(out.Items)
	return out, nil
}
