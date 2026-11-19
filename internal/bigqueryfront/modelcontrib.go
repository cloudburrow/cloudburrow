package bigqueryfront

import (
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Contribution analysis models (#1929): CREATE MODEL of model_type
// CONTRIBUTION_ANALYSIS finds the segments of the test and control rows,
// and ML.GET_INSIGHTS answers their metrics, as Google documents them:
//
//   - https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-contribution-analysis
//   - https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-get-insights
//   - https://docs.cloud.google.com/bigquery/docs/contribution-analysis
//
// The options: contribution_metric (required) in one of three forms,
// SUM(m) (summable), SUM(a)/SUM(b) (summable ratio) and
// SUM(m)/COUNT(DISTINCT c) (summable by category), case insensitive;
// is_test_col (required, a BOOL column); dimension_id_cols (INT64, BOOL or
// STRING columns, every other column when not given; at most 13 when
// min_apriori_support is 0, else 50); min_apriori_support in [0, 1],
// 0.1 by default; top_k_insights_by_apriori_support in [1, 1,000,000],
// not with min_apriori_support; pruning_method NO_PRUNING (the default)
// or PRUNE_REDUNDANT_INSIGHTS. The query holds exactly the columns the
// options name; a row with a NULL dimension is removed; a metric value
// below 0 is refused unless min_apriori_support is 0.
//
// A segment is a combination of dimension values present in the rows;
// the "all" segment is every row. Training finds every segment whose
// apriori support reaches the threshold (support only falls as a segment
// grows when the values are not negative, so a segment below it is not
// grown), and ML.GET_INSIGHTS gives each its documented columns, ordered
// by contribution descending.
//
// UNVERIFIED (docs re-read 2026-11-19, the pages above): the column order
// of ML.GET_INSIGHTS beyond the documented lists (contributors, then the
// dimension columns in dimension_id_cols order, NULL where the segment
// does not fix them, then the metrics); the metric columns' type (FLOAT64);
// a contributor's text for a BOOL or INT64 value (its SQL text, as the
// STRING tutorials' "city=DES MOINES"); the order of a row's contributors
// (dimension_id_cols order); a division by zero (NULL); the "all" row's
// expected_metric_test (its metric_control, which the tutorial's equal
// difference and unexpected_difference show); the summable ratio's
// apriori_support (the greater of the test and control numerator shares,
// as the other metrics', where the page names only "the numerator");
// aumann_shapley_attribution (the Aumann-Shapley value of the segment's
// numerator and denominator on the straight path from the control to the
// test rows, of the population's ratio; the "all" row's is the ratio's
// change, as the tutorial shows); top_k_insights_by_apriori_support's
// ties (broken by contributors) and the "all" row (always kept, not
// counted in k); rows whose is_test_col is NULL (left out); PRUNE_REDUNDANT_
// INSIGHTS' "metric values are equal" (metric_test and metric_control
// both equal); ties in contribution (broken by apriori_support, then
// contributors); and the error texts, which Google does not give.

// contribOptions are a CONTRIBUTION_ANALYSIS model's options.
type contribOptions struct {
	Metric string `json:"contributionMetric"`
	// Kind is "SUMMABLE", "SUMMABLE_RATIO" or "SUMMABLE_BY_CATEGORY";
	// Num the summed column, Den the ratio's denominator column, Cat the
	// counted column.
	Kind         string   `json:"kind"`
	Num          string   `json:"num"`
	Den          string   `json:"den,omitempty"`
	Cat          string   `json:"cat,omitempty"`
	IsTestCol    string   `json:"isTestCol"`
	Dims         []string `json:"dimensionIdCols,omitempty"`
	MinSupport   float64  `json:"minAprioriSupport"`
	TopK         int      `json:"topKInsightsByAprioriSupport,omitempty"`
	Pruning      string   `json:"pruningMethod"`
	SupportGiven bool     `json:"minAprioriSupportGiven,omitempty"`
}

// contribInsight is one segment's row of ML.GET_INSIGHTS.
type contribInsight struct {
	// Values are the segment's dimension values, by index into Dims (nil
	// where the segment does not fix the dimension).
	Values       []*string           `json:"values"`
	Contributors []string            `json:"contributors"`
	Metrics      map[string]*float64 `json:"metrics"`
	Support      float64             `json:"support"`
}

// contribTrained is what training found.
type contribTrained struct {
	// Dims are the dimension columns and DimTypes their BigQuery types.
	Dims     []string         `json:"dims"`
	DimTypes []string         `json:"dimTypes"`
	Insights []contribInsight `json:"insights"`
}

var (
	contribIdent    = `(\x60[^\x60]+\x60|[A-Za-z_][A-Za-z0-9_]*)`
	contribSum      = regexp.MustCompile(`(?i)^\s*SUM\s*\(\s*` + contribIdent + `\s*\)\s*$`)
	contribRatio    = regexp.MustCompile(`(?i)^\s*SUM\s*\(\s*` + contribIdent + `\s*\)\s*/\s*SUM\s*\(\s*` + contribIdent + `\s*\)\s*$`)
	contribCategory = regexp.MustCompile(`(?i)^\s*SUM\s*\(\s*` + contribIdent + `\s*\)\s*/\s*COUNT\s*\(\s*DISTINCT\s+` + contribIdent + `\s*\)\s*$`)
)

func contribUnquote(s string) string { return strings.Trim(s, "`") }

// parseContribMetric reads contribution_metric into o.
func parseContribMetric(s string, o *contribOptions) bool {
	o.Metric = s
	if m := contribSum.FindStringSubmatch(s); m != nil {
		o.Kind, o.Num = "SUMMABLE", contribUnquote(m[1])
		return true
	}
	if m := contribRatio.FindStringSubmatch(s); m != nil {
		o.Kind, o.Num, o.Den = "SUMMABLE_RATIO", contribUnquote(m[1]), contribUnquote(m[2])
		return true
	}
	if m := contribCategory.FindStringSubmatch(s); m != nil {
		o.Kind, o.Num, o.Cat = "SUMMABLE_BY_CATEGORY", contribUnquote(m[1]), contribUnquote(m[2])
		return true
	}
	return false
}

// contribOptionNames are the options a CONTRIBUTION_ANALYSIS model takes.
var contribOptionNames = []string{"contribution_metric", "is_test_col", "dimension_id_cols", "min_apriori_support",
	"top_k_insights_by_apriori_support", "pruning_method"}

// parseContribOption reads one of a contribution analysis model's
// options; ok is false for any other option.
func parseContribOption(name string, val []token, o *mlOptions) (code int, msg string, ok bool) {
	known := false
	for _, n := range contribOptionNames {
		known = known || n == name
	}
	if !known {
		return 0, "", false
	}
	if o.Contrib == nil {
		o.Contrib = &contribOptions{MinSupport: 0.1, Pruning: "NO_PRUNING"}
	}
	c := o.Contrib
	bad := func() (int, string, bool) { return 400, "Invalid value for option " + name, true }
	str := func() (string, bool) {
		if len(val) != 1 || val[0].kind != tokString {
			return "", false
		}
		return unquoteString(val[0].text)
	}
	num := func() (string, bool) {
		s := ""
		for _, t := range val {
			s += t.text
		}
		return s, s != ""
	}
	switch name {
	case "contribution_metric":
		s, ok := str()
		if !ok {
			return bad()
		}
		if !parseContribMetric(s, c) {
			return 400, "Invalid contribution_metric '" + s + "': it must be SUM(metric_column), " +
				"SUM(numerator_column)/SUM(denominator_column) or SUM(metric_column)/COUNT(DISTINCT categorical_column)", true
		}
	case "is_test_col":
		s, ok := str()
		if !ok || s == "" {
			return bad()
		}
		c.IsTestCol = s
	case "dimension_id_cols":
		if len(val) < 2 || !val[0].punct("[") || !val[len(val)-1].punct("]") {
			return bad()
		}
		c.Dims = nil
		for _, t := range val[1 : len(val)-1] {
			if t.punct(",") {
				continue
			}
			s, ok := unquoteString(t.text)
			if t.kind != tokString || !ok || s == "" {
				return bad()
			}
			c.Dims = append(c.Dims, s)
		}
		if len(c.Dims) == 0 {
			return bad()
		}
	case "min_apriori_support":
		s, _ := num()
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || v < 0 || v > 1 {
			return 400, "Invalid value for option min_apriori_support: it must be in the range [0, 1]", true
		}
		c.MinSupport, c.SupportGiven = v, true
	case "top_k_insights_by_apriori_support":
		s, _ := num()
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v < 1 || v > 1000000 {
			return 400, "Invalid value for option top_k_insights_by_apriori_support: it must be an INT64 between 1 and 1,000,000", true
		}
		c.TopK = int(v)
	case "pruning_method":
		s, ok := str()
		if !ok {
			return bad()
		}
		s = strings.ToUpper(s)
		if s != "NO_PRUNING" && s != "PRUNE_REDUNDANT_INSIGHTS" {
			return 400, "Invalid value for option pruning_method: it must be 'NO_PRUNING' or 'PRUNE_REDUNDANT_INSIGHTS'", true
		}
		c.Pruning = s
	}
	return 0, "", true
}

// checkContribOptions checks a contribution analysis model's options
// once every option is read.
func checkContribOptions(o mlOptions) (int, string) {
	c := o.Contrib
	if c == nil || c.Metric == "" {
		return 400, "Option contribution_metric is required for model type CONTRIBUTION_ANALYSIS"
	}
	if c.IsTestCol == "" {
		return 400, "Option is_test_col is required for model type CONTRIBUTION_ANALYSIS"
	}
	if c.SupportGiven && c.TopK > 0 {
		return 400, "Options min_apriori_support and top_k_insights_by_apriori_support cannot be used together"
	}
	if len(c.Dims) > 0 {
		if msg := contribDimCount(c, len(c.Dims)); msg != "" {
			return 400, msg
		}
	}
	return 0, ""
}

// contribDimCount refuses more dimensions than the documented limit:
// 13 when min_apriori_support is 0, 50 otherwise.
func contribDimCount(c *contribOptions, n int) string {
	limit := 50
	if c.TopK == 0 && c.MinSupport == 0 {
		limit = 13
	}
	if n > limit {
		return fmt.Sprintf("dimension_id_cols has %d columns; it can have at most %d when min_apriori_support is %s", n, limit,
			map[bool]string{true: "0", false: "greater than 0"}[limit == 13])
	}
	return ""
}

func contribNumeric(t string) bool {
	switch strings.ToUpper(t) {
	case "INTEGER", "INT64", "FLOAT", "FLOAT64", "NUMERIC", "BIGNUMERIC":
		return true
	}
	return false
}

func contribDimType(t string) string {
	switch strings.ToUpper(t) {
	case "INTEGER", "INT64":
		return "INT64"
	case "BOOLEAN", "BOOL":
		return "BOOL"
	case "STRING":
		return "STRING"
	}
	return ""
}

func contribCategoryType(t string) bool {
	switch strings.ToUpper(t) {
	case "BOOLEAN", "BOOL", "DATE", "DATETIME", "TIME", "TIMESTAMP", "STRING", "INTEGER", "INT64":
		return true
	}
	return false
}

// contribRow is one input row.
type contribRow struct {
	dims []string
	test bool
	a, b float64
	cat  *string
}

// contribStats are the sums of a set of rows, test or control.
type contribStats struct {
	a, b float64
	cats map[string]bool
}

// maxContribSegments bounds the segments one model's training finds.
const maxContribSegments = 1000000

// trainContrib trains a CONTRIBUTION_ANALYSIS model.
func trainContrib(opts mlOptions, cols []mlColumn, rows [][]*string) (mlTrained, *mlFail) {
	var t mlTrained
	c := opts.Contrib
	if c == nil {
		return t, mlInvalid("Option contribution_metric is required for model type CONTRIBUTION_ANALYSIS")
	}
	idx := map[string]int{}
	for i, col := range cols {
		idx[strings.ToLower(col.Name)] = i
	}
	find := func(name string) (int, *mlFail) {
		i, ok := idx[strings.ToLower(name)]
		if !ok {
			return 0, mlInvalid("Column %s is not found in the input data", name)
		}
		return i, nil
	}
	ti, fe := find(c.IsTestCol)
	if fe != nil {
		return t, fe
	}
	if !strings.EqualFold(cols[ti].Type, "BOOLEAN") && !strings.EqualFold(cols[ti].Type, "BOOL") {
		return t, mlInvalid("is_test_col %s must have type BOOL, not %s", cols[ti].Name, cols[ti].Type)
	}
	ai, fe := find(c.Num)
	if fe != nil {
		return t, fe
	}
	if !contribNumeric(cols[ai].Type) {
		return t, mlInvalid("The metric column %s of contribution_metric must have a numeric type, not %s", cols[ai].Name, cols[ai].Type)
	}
	bi, ci := -1, -1
	if c.Den != "" {
		if bi, fe = find(c.Den); fe != nil {
			return t, fe
		}
		if !contribNumeric(cols[bi].Type) {
			return t, mlInvalid("The metric column %s of contribution_metric must have a numeric type, not %s", cols[bi].Name, cols[bi].Type)
		}
	}
	if c.Cat != "" {
		if ci, fe = find(c.Cat); fe != nil {
			return t, fe
		}
		if !contribCategoryType(cols[ci].Type) {
			return t, mlInvalid("The categorical column %s of contribution_metric must have type BOOL, DATE, DATETIME, TIME, TIMESTAMP, STRING or INT64, not %s",
				cols[ci].Name, cols[ci].Type)
		}
	}
	used := map[int]bool{ti: true, ai: true}
	if bi >= 0 {
		used[bi] = true
	}
	if ci >= 0 {
		used[ci] = true
	}
	var di []int
	if len(c.Dims) == 0 {
		// "all columns in the query statement that are not specified in
		// the contribution_metric or is_test_col options".
		for i := range cols {
			if !used[i] {
				di = append(di, i)
			}
		}
		if len(di) == 0 {
			return t, mlInvalid("The input data has no dimension columns")
		}
		if msg := contribDimCount(c, len(di)); msg != "" {
			return t, mlInvalid("%s", msg)
		}
	} else {
		for _, d := range c.Dims {
			i, fe := find(d)
			if fe != nil {
				return t, fe
			}
			if used[i] {
				return t, mlInvalid("Column %s cannot be both a dimension and a column of contribution_metric or is_test_col", cols[i].Name)
			}
			used[i] = true
			di = append(di, i)
		}
		// "must contain exactly the columns that you reference".
		for i, col := range cols {
			if !used[i] {
				return t, mlInvalid("The input data has column %s, which is not referenced by contribution_metric, dimension_id_cols or is_test_col", col.Name)
			}
		}
	}
	tr := &contribTrained{}
	for _, i := range di {
		ty := contribDimType(cols[i].Type)
		if ty == "" {
			return t, mlInvalid("Dimension column %s must have type INT64, BOOL or STRING, not %s", cols[i].Name, cols[i].Type)
		}
		tr.Dims = append(tr.Dims, cols[i].Name)
		tr.DimTypes = append(tr.DimTypes, ty)
	}
	negOK := c.TopK == 0 && c.MinSupport == 0
	var data []contribRow
	parse := func(v *string, col string) (float64, *mlFail) {
		if v == nil {
			return 0, nil // SUM leaves out NULL
		}
		x, err := strconv.ParseFloat(*v, 64)
		if err != nil {
			return 0, mlInvalid("Value %s of column %s is not numeric", *v, col)
		}
		if x < 0 && !negOK {
			return 0, mlInvalid("The values of the metric column %s must be non-negative unless min_apriori_support is 0", col)
		}
		return x, nil
	}
rowLoop:
	for _, row := range rows {
		if row[ti] == nil {
			continue
		}
		cr := contribRow{test: strings.EqualFold(*row[ti], "true")}
		for _, i := range di {
			if row[i] == nil {
				continue rowLoop // "rows ... that contain NULL values are removed"
			}
			cr.dims = append(cr.dims, *row[i])
		}
		var fe *mlFail
		if cr.a, fe = parse(row[ai], cols[ai].Name); fe != nil {
			return t, fe
		}
		if bi >= 0 {
			if cr.b, fe = parse(row[bi], cols[bi].Name); fe != nil {
				return t, fe
			}
		}
		if ci >= 0 {
			cr.cat = row[ci]
		}
		data = append(data, cr)
	}
	if len(data) == 0 {
		return t, mlInvalid("The input data has no rows")
	}
	ins, fe := contribInsights(c, tr.Dims, tr.DimTypes, data)
	if fe != nil {
		return t, fe
	}
	tr.Insights = ins
	t.Contrib = tr
	t.Rows = len(data)
	return t, nil
}

// contribSegment is a segment being grown: its fixed dimensions (value
// index into dims, or -1), the last fixed one, and its rows.
type contribSegment struct {
	vals []int
	last int
	rows []int
}

// contribInsights finds the segments of data and their metrics.
func contribInsights(c *contribOptions, dims, types []string, data []contribRow) ([]contribInsight, *mlFail) {
	// Each dimension's values, interned.
	valueIDs := make([]map[string]int, len(dims))
	values := make([][]string, len(dims))
	rowVals := make([][]int, len(data))
	for d := range dims {
		valueIDs[d] = map[string]int{}
	}
	for i, r := range data {
		rowVals[i] = make([]int, len(dims))
		for d, v := range r.dims {
			id, ok := valueIDs[d][v]
			if !ok {
				id = len(values[d])
				valueIDs[d][v] = id
				values[d] = append(values[d], v)
			}
			rowVals[i][d] = id
		}
	}
	stats := func(rows []int) (test, ctrl contribStats) {
		test.cats, ctrl.cats = map[string]bool{}, map[string]bool{}
		for _, i := range rows {
			s := &ctrl
			if data[i].test {
				s = &test
			}
			s.a += data[i].a
			s.b += data[i].b
			if data[i].cat != nil {
				s.cats[*data[i].cat] = true
			}
		}
		return
	}
	all := make([]int, len(data))
	for i := range all {
		all[i] = i
	}
	popT, popC := stats(all)
	share := func(x, of float64) float64 {
		if of == 0 {
			return 0
		}
		return x / of
	}
	support := func(t, ctl contribStats) float64 {
		return math.Max(share(t.a, popT.a), share(ctl.a, popC.a))
	}
	threshold := c.MinSupport
	if c.TopK > 0 {
		threshold = math.Inf(-1)
	}
	type found struct {
		seg     contribSegment
		t, ctl  contribStats
		support float64
	}
	var segs []found
	// topK keeps the k greatest supports seen, ascending, to bound the
	// search: a segment below the k-th cannot have a child above it.
	var topK []float64
	bound := func() float64 {
		if c.TopK > 0 && len(topK) >= c.TopK {
			return topK[0]
		}
		return threshold
	}
	note := func(s float64) {
		if c.TopK == 0 {
			return
		}
		k := sort.SearchFloat64s(topK, s)
		topK = append(topK, 0)
		copy(topK[k+1:], topK[k:])
		topK[k] = s
		if len(topK) > c.TopK {
			topK = topK[1:]
		}
	}
	root := contribSegment{vals: make([]int, len(dims)), last: -1, rows: all}
	for d := range root.vals {
		root.vals[d] = -1
	}
	level := []contribSegment{root}
	for len(level) > 0 {
		var next []contribSegment
		for _, p := range level {
			for d := p.last + 1; d < len(dims); d++ {
				groups := map[int][]int{}
				var order []int
				for _, i := range p.rows {
					v := rowVals[i][d]
					if _, ok := groups[v]; !ok {
						order = append(order, v)
					}
					groups[v] = append(groups[v], i)
				}
				sort.Ints(order)
				for _, v := range order {
					rows := groups[v]
					t, ctl := stats(rows)
					s := support(t, ctl)
					if s < bound() {
						continue
					}
					vals := append([]int(nil), p.vals...)
					vals[d] = v
					seg := contribSegment{vals: vals, last: d, rows: rows}
					segs = append(segs, found{seg: seg, t: t, ctl: ctl, support: s})
					note(s)
					if len(segs) > maxContribSegments {
						return nil, mlUnimplemented("a contribution analysis of more than %d segments: raise min_apriori_support or use fewer dimensions", maxContribSegments)
					}
					next = append(next, seg)
				}
			}
		}
		level = next
	}
	contributors := func(seg contribSegment) ([]string, []*string) {
		var cs []string
		vs := make([]*string, len(dims))
		for d, v := range seg.vals {
			if v < 0 {
				continue
			}
			s := values[d][v]
			vs[d] = &s
			cs = append(cs, dims[d]+"="+s)
		}
		return cs, vs
	}
	label := func(cs []string) string { return strings.Join(cs, "\x00") }
	if c.TopK > 0 {
		// The k segments of the greatest support, ties by contributors.
		sort.SliceStable(segs, func(i, j int) bool {
			if segs[i].support != segs[j].support {
				return segs[i].support > segs[j].support
			}
			ci, _ := contributors(segs[i].seg)
			cj, _ := contributors(segs[j].seg)
			return label(ci) < label(cj)
		})
		if len(segs) > c.TopK {
			segs = segs[:c.TopK]
		}
	}
	var out []contribInsight
	ai := contribInsight{Contributors: []string{"all"}, Values: make([]*string, len(dims)), Support: 1}
	ai.Metrics = contribMetrics(c, popT, popC, popT, popC, true)
	out = append(out, ai)
	for _, s := range segs {
		cs, vs := contributors(s.seg)
		in := contribInsight{Contributors: cs, Values: vs, Support: s.support}
		in.Metrics = contribMetrics(c, s.t, s.ctl, popT, popC, false)
		out = append(out, in)
	}
	if c.Pruning == "PRUNE_REDUNDANT_INSIGHTS" {
		out = contribPrune(out)
	}
	for i := range out {
		out[i].Metrics["apriori_support"] = f64(out[i].Support)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ci, cj := out[i].Metrics["contribution"], out[j].Metrics["contribution"]
		vi, vj := math.Inf(-1), math.Inf(-1)
		if ci != nil {
			vi = *ci
		}
		if cj != nil {
			vj = *cj
		}
		if vi != vj {
			return vi > vj
		}
		if out[i].Support != out[j].Support {
			return out[i].Support > out[j].Support
		}
		return label(out[i].Contributors) < label(out[j].Contributors)
	})
	return out, nil
}

// div is a / b, or nil when b is 0.
func contribDiv(a, b float64) *float64 {
	if b == 0 || math.IsNaN(a) || math.IsNaN(b) {
		return nil
	}
	return f64(a / b)
}

// contribMetrics are a segment's metrics but apriori_support, of its test
// and control rows and the population's.
func contribMetrics(c *contribOptions, t, ctl, popT, popC contribStats, isAll bool) map[string]*float64 {
	m := map[string]*float64{}
	switch c.Kind {
	case "SUMMABLE":
		mt, mc := t.a, ctl.a
		diff := mt - mc
		m["metric_test"], m["metric_control"], m["difference"] = f64(mt), f64(mc), f64(diff)
		m["relative_difference"] = contribDiv(diff, mc)
		var expected *float64
		if isAll {
			expected = f64(mc)
		} else if ratio := contribDiv(popT.a-mt, popC.a-mc); ratio != nil {
			expected = f64(mc * *ratio)
		}
		if expected != nil {
			u := mt - *expected
			m["unexpected_difference"] = f64(u)
			m["relative_unexpected_difference"] = contribDiv(u, *expected)
		}
		m["contribution"] = f64(math.Abs(diff))
	case "SUMMABLE_RATIO":
		mt, mc := contribDiv(t.a, t.b), contribDiv(ctl.a, ctl.b)
		m["metric_test"], m["metric_control"] = mt, mc
		if mt != nil && mc != nil {
			m["metric_test_over_metric_control"] = contribDiv(*mt, *mc)
		}
		if !isAll {
			if mt != nil {
				if comp := contribDiv(popT.a-t.a, popT.b-t.b); comp != nil {
					m["metric_test_over_complement"] = contribDiv(*mt, *comp)
				}
			}
			if mc != nil {
				if comp := contribDiv(popC.a-ctl.a, popC.b-ctl.b); comp != nil {
					m["metric_control_over_complement"] = contribDiv(*mc, *comp)
				}
			}
		}
		var as *float64
		if isAll {
			if mt != nil && mc != nil {
				as = f64(*mt - *mc)
			}
		} else {
			as = aumannShapley(t.a-ctl.a, t.b-ctl.b, popC.a, popC.b, popT.a-popC.a, popT.b-popC.b)
		}
		m["aumann_shapley_attribution"] = as
		if as != nil {
			m["contribution"] = f64(math.Abs(*as))
		}
	case "SUMMABLE_BY_CATEGORY":
		mt, mc := contribDiv(t.a, float64(len(t.cats))), contribDiv(ctl.a, float64(len(ctl.cats)))
		m["metric_test"], m["metric_control"] = mt, mc
		if mt != nil && mc != nil {
			diff := *mt - *mc
			m["difference"] = f64(diff)
			m["relative_difference"] = contribDiv(diff, *mc)
			m["contribution"] = f64(math.Abs(diff))
		}
		pt, pc := contribDiv(popT.a, float64(len(popT.cats))), contribDiv(popC.a, float64(len(popC.cats)))
		if mt != nil && pt != nil {
			m["metric_test_over_population"] = contribDiv(*mt, *pt)
		}
		if mc != nil && pc != nil {
			m["metric_control_over_population"] = contribDiv(*mc, *pc)
		}
	}
	return m
}

// aumannShapley is the Aumann-Shapley attribution to a segment whose
// numerator and denominator change by da and db, of the population's
// ratio A/B going from (a0, b0) by (dA, dB): the integral over t in [0,
// 1] of da/B(t) - db·A(t)/B(t)², A(t) = a0 + t·dA, B(t) = b0 + t·dB
// (Simpson's rule). nil when B is 0 at either end.
func aumannShapley(da, db, a0, b0, dA, dB float64) *float64 {
	if b0 == 0 || b0+dB == 0 {
		return nil
	}
	const n = 1000
	g := func(t float64) float64 {
		A, B := a0+t*dA, b0+t*dB
		return da/B - db*A/(B*B)
	}
	sum := g(0) + g(1)
	for i := 1; i < n; i++ {
		w := 2.0
		if i%2 == 1 {
			w = 4
		}
		sum += w * g(float64(i)/n)
	}
	v := sum / (3 * n)
	return &v
}

// contribPrune leaves out a segment whose metric_test and metric_control
// equal those of a segment that fixes every dimension value it does and
// more ("the more descriptive row is kept"); the "all" row is never
// pruned.
func contribPrune(in []contribInsight) []contribInsight {
	key := func(c contribInsight) string {
		f := func(p *float64) string {
			if p == nil {
				return "null"
			}
			return strconv.FormatFloat(*p, 'g', -1, 64)
		}
		return f(c.Metrics["metric_test"]) + "/" + f(c.Metrics["metric_control"])
	}
	groups := map[string][]int{}
	for i, c := range in {
		groups[key(c)] = append(groups[key(c)], i)
	}
	subset := func(a, b contribInsight) bool { // a's values ⊂ b's, strictly
		n := 0
		for d, v := range a.Values {
			if v == nil {
				continue
			}
			if b.Values[d] == nil || *b.Values[d] != *v {
				return false
			}
			n++
		}
		m := 0
		for _, v := range b.Values {
			if v != nil {
				m++
			}
		}
		return m > n
	}
	var out []contribInsight
	for i, c := range in {
		if i == 0 && len(c.Contributors) == 1 && c.Contributors[0] == "all" {
			out = append(out, c)
			continue
		}
		pruned := false
		for _, j := range groups[key(c)] {
			if j != i && j != 0 && subset(c, in[j]) {
				pruned = true
				break
			}
		}
		if !pruned {
			out = append(out, c)
		}
	}
	return out
}

// contribColumns are ML.GET_INSIGHTS' metric columns of a metric kind, in
// the documented order.
func contribColumns(kind string) []string {
	switch kind {
	case "SUMMABLE_RATIO":
		return []string{"metric_test", "metric_control", "metric_test_over_metric_control", "metric_test_over_complement",
			"metric_control_over_complement", "aumann_shapley_attribution", "apriori_support", "contribution"}
	case "SUMMABLE_BY_CATEGORY":
		return []string{"metric_test", "metric_control", "difference", "relative_difference", "metric_test_over_population",
			"metric_control_over_population", "apriori_support", "contribution"}
	}
	return []string{"metric_test", "metric_control", "difference", "relative_difference", "unexpected_difference",
		"relative_unexpected_difference", "apriori_support", "contribution"}
}

// contribSubquery is an ML function call of a contribution analysis
// model, or ML.GET_INSIGHTS of another.
func (f front) contribSubquery(_ *http.Request, _ queryOptions, md bqModel, c mlCall) (string, int, string) {
	typ := md.Options.ModelType
	if md.Options.AutoML != nil {
		typ = md.Options.AutoML.Type
	}
	if typ != "CONTRIBUTION_ANALYSIS" || c.fn != "GET_INSIGHTS" || c.ai {
		return "", 400, "ML." + c.fn + " is not supported for model type " + typ
	}
	if c.input != "" || len(c.struc) > 0 || len(c.named) > 0 || c.thresholds != nil {
		return "", 400, "ML.GET_INSIGHTS takes only a MODEL argument"
	}
	tr := md.Trained.Contrib
	if tr == nil {
		return "", 500, "cloudburrow: the contribution analysis model has no insights"
	}
	metrics := contribColumns(md.Options.Contrib.Kind)
	cols := []string{"contributors ARRAY<STRING>"}
	for d, n := range tr.Dims {
		cols = append(cols, sqlIdent(n)+" "+tr.DimTypes[d])
	}
	for _, n := range metrics {
		cols = append(cols, n+" FLOAT64")
	}
	var rows []string
	for _, in := range tr.Insights {
		var cs []string
		for _, s := range in.Contributors {
			cs = append(cs, sqlString(s))
		}
		vals := []string{"ARRAY<STRING>[" + strings.Join(cs, ", ") + "]"}
		for d, v := range in.Values {
			switch {
			case v == nil:
				vals = append(vals, "CAST(NULL AS "+tr.DimTypes[d]+")")
			case tr.DimTypes[d] == "STRING":
				vals = append(vals, sqlString(*v))
			case tr.DimTypes[d] == "BOOL":
				vals = append(vals, strings.ToUpper(*v))
			default:
				vals = append(vals, "CAST("+sqlString(*v)+" AS INT64)")
			}
		}
		for _, n := range metrics {
			vals = append(vals, sqlFloat(in.Metrics[n]))
		}
		rows = append(rows, "("+strings.Join(vals, ", ")+")")
	}
	return mlRowsSubquery("STRUCT<"+strings.Join(cols, ", ")+">", rows), 0, ""
}

// contribResource fills a contribution analysis model's REST resource:
// its trainingOptions' contributionMetric, dimensionIdColumns,
// isTestColumn and minAprioriSupport (the REST Model's fields; the
// reference has none for top_k_insights_by_apriori_support or
// pruning_method).
func (md bqModel) contribResource() map[string]any {
	c := md.Options.Contrib
	opts := map[string]any{}
	if c == nil {
		return opts
	}
	opts["contributionMetric"] = c.Metric
	opts["isTestColumn"] = c.IsTestCol
	if md.Trained.Contrib != nil {
		opts["dimensionIdColumns"] = md.Trained.Contrib.Dims
	}
	if c.TopK == 0 {
		opts["minAprioriSupport"] = c.MinSupport
	}
	return opts
}
