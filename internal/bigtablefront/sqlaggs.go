package bigtablefront

import (
	"fmt"
	"math"
	"sort"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
)

// The aggregates #1704 adds: the statistical aggregates CORR, COVAR_POP,
// COVAR_SAMP, STDDEV, STDDEV_POP, STDDEV_SAMP, VAR_POP, VAR_SAMP and
// VARIANCE (https://cloud.google.com/bigtable/docs/reference/sql/statistical_aggregate_functions),
// the approximate aggregates APPROX_COUNT_DISTINCT, APPROX_QUANTILES,
// APPROX_TOP_COUNT and APPROX_TOP_SUM
// (https://cloud.google.com/bigtable/docs/reference/sql/approximate_aggregate_functions)
// and ARRAY_CONCAT_AGG
// (https://cloud.google.com/bigtable/docs/reference/sql/aggregate_functions#array_concat_agg).
//
// The approximate aggregates answer exactly: "The value returned is a
// statistical estimate, not necessarily the actual value", and the
// emulator's estimate is the actual value, so a result can differ from
// Google's on large inputs, where Google's sketch is approximate.
// APPROX_QUANTILES' boundary i of n over m sorted values is the value at
// floor(i*(m-1)/n), which reproduces each of the reference's examples. Ties
// in APPROX_TOP_COUNT and APPROX_TOP_SUM are listed in the order their
// values were first read.

var moreAggregates = map[string]bool{"CORR": true, "COVAR_POP": true, "COVAR_SAMP": true, "STDDEV": true,
	"STDDEV_POP": true, "STDDEV_SAMP": true, "VAR_POP": true, "VAR_SAMP": true, "VARIANCE": true,
	"APPROX_COUNT_DISTINCT": true, "APPROX_QUANTILES": true, "APPROX_TOP_COUNT": true, "APPROX_TOP_SUM": true,
	"ARRAY_CONCAT_AGG": true}

func init() {
	for k := range moreAggregates {
		aggregates[k] = true
	}
}

// checkMoreAgg types one of moreAggregates; c.args are checked already.
func (c *call) checkMoreAgg(ts []*btpb.Type) error {
	n := len(ts)
	bad := func() error { return sigErr(c, ts) }
	count := func(i int) error {
		if !isKind(ts[i], "INT64") || !constant(c.args[i]) {
			return fmt.Errorf("%s: number must be an INT64 literal or query parameter", c.name)
		}
		if l, ok := c.args[i].(*literal); ok {
			if v, _ := l.v.(int64); v < 1 {
				return fmt.Errorf("%s: number must be at least 1", c.name)
			}
		}
		return nil
	}
	switch c.name {
	case "CORR", "COVAR_POP", "COVAR_SAMP":
		if n != 2 || !isNumeric(ts[0]) || !isNumeric(ts[1]) {
			return bad()
		}
		if c.distinct {
			return fmt.Errorf("DISTINCT is not allowed for %s", c.name)
		}
		c.t = float64Type
	case "STDDEV", "STDDEV_POP", "STDDEV_SAMP", "VAR_POP", "VAR_SAMP", "VARIANCE":
		if n != 1 || !isNumeric(ts[0]) {
			return bad()
		}
		c.t = float64Type
	case "APPROX_COUNT_DISTINCT":
		if n != 1 || isKind(ts[0], "ARRAY<", "STRUCT<") || !groupable(ts[0]) {
			return bad()
		}
		c.t = int64Type
	case "APPROX_QUANTILES":
		if n != 2 || isKind(ts[0], "ARRAY<", "STRUCT<", "MAP<") {
			return bad()
		}
		if err := count(1); err != nil {
			return err
		}
		c.t = arrayType(ts[0])
	case "APPROX_TOP_COUNT":
		if n != 2 || !groupable(ts[0]) {
			return bad()
		}
		if err := count(1); err != nil {
			return err
		}
		c.t = arrayType(structType([]string{"value", "count"}, []*btpb.Type{ts[0], int64Type}))
	case "APPROX_TOP_SUM":
		if n != 3 || !groupable(ts[0]) || !isKind(ts[1], "INT64", "FLOAT64") {
			return bad()
		}
		if err := count(2); err != nil {
			return err
		}
		c.t = arrayType(structType([]string{"value", "sum"}, []*btpb.Type{ts[0], ts[1]}))
	case "ARRAY_CONCAT_AGG":
		if n != 1 || !isKind(ts[0], "ARRAY<") {
			return bad()
		}
		c.t = ts[0]
		if c.distinct && !groupable(ts[0]) {
			return fmt.Errorf("Aggregate functions with DISTINCT cannot be used with arguments of type %s", kind(ts[0]))
		}
		return nil
	}
	if c.distinct && !groupable(ts[0]) {
		return fmt.Errorf("Aggregate functions with DISTINCT cannot be used with arguments of type %s", kind(ts[0]))
	}
	return nil
}

// aggregateMore computes one of moreAggregates over rows.
func (c *call) aggregateMore(envs []env) (any, error) {
	keepNulls := c.name == "APPROX_TOP_COUNT" || c.name == "APPROX_TOP_SUM"
	var tuples [][]any
	seen := map[string]bool{}
	for _, e0 := range envs {
		e := e0
		var tup []any
		null := false
		for _, a := range c.args {
			v, err := a.eval(&e)
			if err != nil {
				return nil, err
			}
			null = null || v == nil
			tup = append(tup, v)
		}
		if null && !keepNulls {
			if c.name == "APPROX_QUANTILES" && tup[1] == nil {
				return nil, fmt.Errorf("APPROX_QUANTILES: number must not be NULL")
			}
			continue // pairs and values with a NULL are ignored
		}
		if c.distinct {
			k := groupKey(tup[0])
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		tuples = append(tuples, tup)
	}
	floats := func(i int) []float64 {
		out := make([]float64, len(tuples))
		for j, t := range tuples {
			out[j] = toFloat(t[i])
		}
		return out
	}
	switch c.name {
	case "STDDEV", "STDDEV_POP", "STDDEV_SAMP", "VAR_POP", "VAR_SAMP", "VARIANCE":
		xs := floats(0)
		pop := c.name == "STDDEV_POP" || c.name == "VAR_POP"
		if len(xs) == 0 || !pop && len(xs) < 2 {
			return nil, nil
		}
		v := covariance(xs, xs, pop)
		if c.name == "STDDEV" || c.name == "STDDEV_POP" || c.name == "STDDEV_SAMP" {
			return math.Sqrt(v), nil
		}
		return v, nil
	case "COVAR_POP", "COVAR_SAMP":
		xs, ys := floats(0), floats(1)
		pop := c.name == "COVAR_POP"
		if len(xs) == 0 || !pop && len(xs) < 2 {
			return nil, nil
		}
		return covariance(xs, ys, pop), nil
	case "CORR":
		xs, ys := floats(0), floats(1)
		if len(xs) < 2 {
			return nil, nil
		}
		return covariance(xs, ys, true) / math.Sqrt(covariance(xs, xs, true)*covariance(ys, ys, true)), nil
	case "APPROX_COUNT_DISTINCT":
		d := map[string]bool{}
		for _, t := range tuples {
			d[groupKey(t[0])] = true
		}
		return int64(len(d)), nil
	case "APPROX_QUANTILES":
		if len(tuples) == 0 {
			return nil, nil
		}
		n := tuples[0][1].(int64)
		if n < 1 {
			return nil, fmt.Errorf("APPROX_QUANTILES: number must be at least 1")
		}
		vals := make([]any, len(tuples))
		for i, t := range tuples {
			vals[i] = t[0]
		}
		sort.SliceStable(vals, func(a, b int) bool { c, _ := compare(vals[a], vals[b]); return c < 0 })
		m := int64(len(vals))
		out := make([]any, n+1)
		for i := int64(0); i <= n; i++ {
			out[i] = vals[i*(m-1)/n]
		}
		return out, nil
	case "APPROX_TOP_COUNT", "APPROX_TOP_SUM":
		if len(tuples) == 0 {
			return nil, nil
		}
		ni := len(c.args) - 1
		if tuples[0][ni] == nil {
			return nil, fmt.Errorf("%s: number must not be NULL", c.name)
		}
		n := tuples[0][ni].(int64)
		if n < 1 {
			return nil, fmt.Errorf("%s: number must be at least 1", c.name)
		}
		type entry struct {
			value any
			sum   any
		}
		idx := map[string]int{}
		var entries []*entry
		for _, t := range tuples {
			k := groupKey(t[0])
			i, ok := idx[k]
			if !ok {
				i = len(entries)
				idx[k] = i
				e := &entry{value: t[0]}
				if c.name == "APPROX_TOP_COUNT" {
					e.sum = int64(0)
				}
				entries = append(entries, e)
			}
			e := entries[i]
			if c.name == "APPROX_TOP_COUNT" {
				e.sum = e.sum.(int64) + 1
				continue
			}
			w := t[1]
			if w == nil {
				continue
			}
			if f, ok := w.(float64); ok && (math.IsNaN(f) || f < 0) || !ok && w.(int64) < 0 {
				return nil, fmt.Errorf("APPROX_TOP_SUM: weight must not be negative or NaN")
			}
			if e.sum == nil {
				e.sum = w
				continue
			}
			s, err := arith("+", e.sum, w, isKind(c.argTypes[1], "INT64"))
			if err != nil {
				return nil, err
			}
			e.sum = s
		}
		sort.SliceStable(entries, func(a, b int) bool {
			x, y := entries[a].sum, entries[b].sum
			if x == nil || y == nil {
				return y == nil && x != nil
			}
			return toFloat(x) > toFloat(y)
		})
		if int64(len(entries)) > n {
			entries = entries[:n]
		}
		out := make([]any, len(entries))
		for i, e := range entries {
			out[i] = []any{e.value, e.sum}
		}
		return out, nil
	case "ARRAY_CONCAT_AGG":
		if len(tuples) == 0 {
			return nil, nil
		}
		out := []any{}
		for _, t := range tuples {
			out = append(out, t[0].([]any)...)
		}
		return out, nil
	}
	return nil, unsupportedErr{"the aggregate " + c.name}
}

// covariance is the population (or sample) covariance of xs and ys.
func covariance(xs, ys []float64, pop bool) float64 {
	var mx, my float64
	for i := range xs {
		mx += xs[i]
		my += ys[i]
	}
	n := float64(len(xs))
	mx, my = mx/n, my/n
	var s float64
	for i := range xs {
		s += (xs[i] - mx) * (ys[i] - my)
	}
	if pop {
		return s / n
	}
	return s / (n - 1)
}
