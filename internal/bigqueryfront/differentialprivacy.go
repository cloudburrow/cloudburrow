package bigqueryfront

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/google/differential-privacy/go/v4/dpagg"
	"github.com/google/differential-privacy/go/v4/noise"
)

// Differential privacy (#1455): SELECT WITH DIFFERENTIAL_PRIVACY
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/query-syntax#dp_clause),
// its aggregate functions
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/aggregate-dp-functions)
// and a view's or table's differential_privacy_policy
// (https://cloud.google.com/bigquery/docs/analysis-rules#dp_analysis_rule).
//
// The engine knows nothing of it, so the front carries a query out in
// three steps:
//
//  1. It sends a query of its own that groups the rows by the query's
//     GROUP BY and the privacy unit column: one row per privacy unit and
//     group, with each DP aggregate's contribution of that unit to that
//     group (COUNT(*) or COUNT(x) its rows, SUM(x) its sum, AVG(x) its
//     average, PERCENTILE_CONT(x, p) its values).
//  2. In Go, with Google's differential privacy library
//     (github.com/google/differential-privacy/go/v4, Apache 2.0, pinned in
//     go.mod), it bounds each unit's contributions: at most
//     max_groups_contributed groups of each unit (a uniform sample of them;
//     default 1, NULL no limit), each contribution clamped to the
//     aggregate's contribution_bounds_per_group (PERCENTILE_CONT's
//     contribution_bounds_per_row clamps each value, and a unit gives one
//     sampled value per group); bounds not given are found from the data
//     with half the aggregate's epsilon (a noisy histogram of powers of two,
//     the library's approximate bounds algorithm). The epsilon is split as
//     the reference says: n aggregates and the group selection each get
//     epsilon/(n+1), the l0 sensitivity of each being max_groups_contributed.
//     Each group's noisy count of distinct privacy units, Laplace noise, is
//     thresholded with the whole delta (the library's Count.ThresholdedResult):
//     groups with few units are dropped. COUNT and SUM get Laplace noise on
//     the clamped sum (an INT64 one integer noise), AVG is the library's
//     BoundedMean of the units' averages, PERCENTILE_CONT its
//     BoundedQuantiles.
//  3. It answers the client's query, through jobs.query or jobs.insert,
//     with a query of the noisy rows as literals, with the client's text.
//
// A differential_privacy_policy (privacy_unit_column, max_epsilon_per_query,
// delta_per_query, epsilon_budget, delta_budget, max_groups_contributed)
// requires SELECT WITH DIFFERENTIAL_PRIVACY of the table or view, gives the
// query's defaults, and refuses with BigQuery's documented errors: an
// epsilon above max_epsilon_per_query ("Epsilon is too high: …"), and,
// once the epsilon or delta of the queries run reaches the budget,
// "Privacy budget is not sufficient for table '…' in this query.". The
// budget spent is kept by the front (dpbudget.json beside
// tablesettings.json), per table, its creation time and its policy, so a
// policy changed by ALTER VIEW starts again ("When you update a
// differential privacy analysis rule, the privacy budgets are reset").

// dpOptions are a DIFFERENTIAL_PRIVACY clause's OPTIONS.
type dpOptions struct {
	eps, delta          float64
	hasEps, hasDelta    bool
	maxGroups           int64 // 0: unset
	groupsSet, groupsNo bool  // set; set to NULL (no limit)
	unitText            string
	unitName            string
	// public: group_selection_strategy = PUBLIC_GROUPS, the groups those
	// of a public group join, kept without thresholding (#1482).
	public bool
}

// dpAgg is one DP aggregate function of the select list.
type dpAgg struct {
	fn      string // COUNT, SUM, AVG, PERCENTILE_CONT
	star    bool   // COUNT(*)
	arg     string // the expression's text
	pct     float64
	lo, hi  float64
	bounded bool
	sumInt  bool // a SUM of an INT64, whose result is INT64
}

// dpItem is an item of the select list: an aggregate, or a grouping
// expression's text.
type dpItem struct {
	agg   *dpAgg
	text  string
	alias string // the aggregate's alias, "" when it has none
	// key is an aggregate call's tokens, to find it again in HAVING;
	// hidden an aggregate only HAVING calls, not in the select list.
	key    string
	hidden bool
}

// dpPart is a piece of a HAVING condition: text as it is, or a reference
// to an item (an aggregate call, or a select list name), item >= 0.
type dpPart struct {
	text string
	item int
}

// dpQuery is a SELECT WITH DIFFERENTIAL_PRIVACY.
type dpQuery struct {
	opts  dpOptions
	items []dpItem
	from  string // FROM … [WHERE …]
	group string // the GROUP BY list, "" when none
	tail  string // ORDER BY … / LIMIT …, applied to the noisy rows
	// having is the HAVING condition, applied to the noisy rows (#1482);
	// nil when none.
	having []dpPart
}

var dpFunctions = map[string]bool{"COUNT": true, "SUM": true, "AVG": true, "PERCENTILE_CONT": true}

// parseDPQuery reads s, the one statement of sql, a SELECT WITH
// DIFFERENTIAL_PRIVACY. msg is why it is refused, 501 when unsupported,
// else 400.
func parseDPQuery(sql string, s []token) (q dpQuery, msg string, unsupported bool) {
	bad := func(m string) (dpQuery, string, bool) { return dpQuery{}, m, false }
	no := func(m string) (dpQuery, string, bool) {
		return dpQuery{}, "Not implemented here: " + m + " in a SELECT WITH DIFFERENTIAL_PRIVACY. Nothing was run.", true
	}
	depth := privacyDepths(s)
	i := 3
	if i < len(s) && s[i].is("OPTIONS") {
		if i+1 >= len(s) || !s[i+1].punct("(") {
			return bad("Syntax error: expected ( after OPTIONS")
		}
		j := i + 2
		for j < len(s) && !(s[j].punct(")") && depth[j] == depth[i]) {
			if s[j].kind != tokWord || j+2 >= len(s) || !s[j+1].punct("=") {
				return bad("Syntax error: expected an option name = value in DIFFERENTIAL_PRIVACY OPTIONS")
			}
			name := strings.ToLower(s[j].text)
			j += 2
			k := j
			for k < len(s) && !(depth[k] == depth[i]+1 && s[k].punct(",")) && !(depth[k] == depth[i] && s[k].punct(")")) {
				k++
			}
			if k == j || k >= len(s) {
				return bad("Syntax error: expected a value for the DIFFERENTIAL_PRIVACY option " + name)
			}
			text := strings.TrimSpace(sql[s[j].pos:s[k-1].end])
			num := func() (float64, bool) {
				v, err := strconv.ParseFloat(strings.ReplaceAll(text, " ", ""), 64)
				return v, err == nil
			}
			switch name {
			case "epsilon":
				v, ok := num()
				if !ok || !(v > 0) || math.IsInf(v, 0) {
					return bad("The epsilon option of DIFFERENTIAL_PRIVACY must be a positive FLOAT64 literal")
				}
				q.opts.eps, q.opts.hasEps = v, true
			case "delta":
				v, ok := num()
				if !ok || v < 0 || v > 1 {
					return bad("The delta option of DIFFERENTIAL_PRIVACY must be a FLOAT64 literal in [0, 1]")
				}
				q.opts.delta, q.opts.hasDelta = v, true
			case "max_groups_contributed":
				q.opts.groupsSet = true
				if strings.EqualFold(text, "NULL") {
					q.opts.groupsNo = true
					break
				}
				n, err := strconv.ParseInt(text, 10, 64)
				if err != nil || n <= 0 {
					return bad("The max_groups_contributed option of DIFFERENTIAL_PRIVACY must be a positive INT64 literal or NULL")
				}
				q.opts.maxGroups = n
			case "privacy_unit_column":
				parts, next := path(s, j)
				if next != k || len(parts) == 0 {
					return bad("The privacy_unit_column option of DIFFERENTIAL_PRIVACY must be a column name")
				}
				q.opts.unitText, q.opts.unitName = text, parts[len(parts)-1]
			case "group_selection_strategy":
				switch strings.ToUpper(text) {
				case "LAPLACE_THRESHOLD":
				case "PUBLIC_GROUPS":
					q.opts.public = true
				default:
					return bad("The group_selection_strategy option of DIFFERENTIAL_PRIVACY must be LAPLACE_THRESHOLD or PUBLIC_GROUPS")
				}
			case "max_rows_contributed":
				// Neither BigQuery's reference nor GoogleSQL's documents
				// it (#1482).
				return no("the option max_rows_contributed, which BigQuery's reference does not document")
			default:
				return bad("Unknown DIFFERENTIAL_PRIVACY option: " + s[j-2].text)
			}
			j = k
			if s[j].punct(",") {
				j++
			}
		}
		if j >= len(s) {
			return bad("Syntax error: expected ) after DIFFERENTIAL_PRIVACY OPTIONS")
		}
		i = j + 1
	}
	bodyAt := i
	if bodyAt >= len(s) {
		return bad("Syntax error: expected a select list after DIFFERENTIAL_PRIVACY")
	}
	if s[bodyAt].is("DISTINCT") || s[bodyAt].is("ALL") || s[bodyAt].is("AS") {
		return no("SELECT " + strings.ToUpper(s[bodyAt].text))
	}
	from, where, group, having, end := -1, -1, -1, -1, len(s)
	for j := bodyAt; j < len(s); j++ {
		if depth[j] != 0 {
			continue
		}
		t := s[j]
		switch {
		case t.is("FROM") && from < 0:
			from = j
		case from < 0:
		case t.is("WHERE") && where < 0:
			where = j
		case t.is("GROUP") && group < 0 && having < 0:
			group = j
		case t.is("HAVING") && having < 0 && end == len(s):
			having = j
		case t.is("HAVING") || t.is("QUALIFY") || t.is("WINDOW") || t.is("UNION") || t.is("INTERSECT") || t.is("EXCEPT"):
			return no(strings.ToUpper(t.text))
		case (t.is("ORDER") || t.is("LIMIT")) && end == len(s):
			end = j
		}
	}
	if from < 0 {
		return bad("SELECT WITH DIFFERENTIAL_PRIVACY must read a table in its FROM clause")
	}
	fromEnd, groupEnd := end, end
	if having >= 0 {
		fromEnd, groupEnd = having, having
		if having+1 >= end {
			return bad("Syntax error: expected a condition after HAVING")
		}
	}
	if group >= 0 {
		fromEnd = group
		if group+1 >= groupEnd || !s[group+1].is("BY") || group+2 >= groupEnd {
			return bad("Syntax error: expected BY after GROUP")
		}
		g := s[group+2]
		if g.is("ROLLUP") || g.is("CUBE") || g.is("GROUPING") || g.is("ALL") {
			return no("GROUP BY " + strings.ToUpper(g.text))
		}
		q.group = sql[g.pos:s[groupEnd-1].end]
	}
	q.from = sql[s[from].pos:s[fromEnd-1].end]
	if end < len(s) {
		q.tail = sql[s[end].pos:s[len(s)-1].end]
	}
	// The select list's items.
	start := bodyAt
	for j := bodyAt; j <= from; j++ {
		if j < from && !(depth[j] == 0 && s[j].punct(",")) {
			continue
		}
		if j == start {
			if j < from {
				return bad("Syntax error: an empty item in the select list")
			}
			break // a trailing comma
		}
		item, m, unsup := dpParseItem(sql, s[start:j], depth[start:j])
		if m != "" {
			if unsup {
				return no(m)
			}
			return bad(m)
		}
		q.items = append(q.items, item)
		start = j + 1
	}
	aggs := 0
	for _, it := range q.items {
		if it.agg != nil {
			aggs++
		}
	}
	if aggs == 0 {
		return bad("SELECT WITH DIFFERENTIAL_PRIVACY must use a differentially private aggregate function: AVG, COUNT, PERCENTILE_CONT or SUM")
	}
	if having >= 0 {
		if m, unsup := q.parseHaving(sql, s[having+1:end], depth[having+1:end]); m != "" {
			if unsup {
				return no(m)
			}
			return bad(m)
		}
	}
	if q.opts.public {
		if m := dpPublicGroups(s[from:fromEnd], depth[from:fromEnd], q.group); m != "" {
			return bad(m)
		}
	}
	return q, "", false
}

// dpKey is a call's tokens, as HAVING finds the select list's aggregate
// again: words in upper case, the rest as written.
func dpKey(t []token) string {
	var b strings.Builder
	for _, tk := range t {
		if tk.kind == tokWord {
			b.WriteString(strings.ToUpper(tk.text))
		} else {
			b.WriteString(tk.text)
		}
		b.WriteByte(' ')
	}
	return b.String()
}

// parseHaving reads HAVING's condition, t (depths d): applied to the noisy
// rows, as a HAVING applies to the groups' aggregates (query-syntax#having_clause).
// A DP aggregate call that is one of the select list's is that column; another
// is a hidden aggregate of the query, which takes its share of the epsilon as
// any other (query-syntax#dp_epsilon); a name of the select list (an alias, a
// grouping column) is that column.
func (q *dpQuery) parseHaving(sql string, t []token, d []int) (msg string, unsupported bool) {
	names := map[string]int{}
	for i, it := range q.items {
		n := it.alias
		if it.agg == nil {
			n = ""
			if !strings.ContainsAny(it.text, " ()") {
				n = it.text
				if k := strings.LastIndex(n, "."); k >= 0 {
					n = n[k+1:]
				}
				n = strings.Trim(n, "`")
			}
		}
		if n != "" {
			if _, dup := names[strings.ToLower(n)]; !dup {
				names[strings.ToLower(n)] = i
			}
		}
	}
	last := t[0].pos
	add := func(upTo int, item int) {
		if upTo > last {
			q.having = append(q.having, dpPart{text: sql[last:upTo], item: -1})
		}
		if item >= 0 {
			q.having = append(q.having, dpPart{item: item})
		}
	}
	for k := 0; k < len(t); k++ {
		tk := t[k]
		if tk.kind != tokWord && tk.kind != tokQuoted || k > 0 && t[k-1].punct(".") || k+1 < len(t) && t[k+1].punct(".") {
			continue
		}
		if tk.kind == tokWord && k+1 < len(t) && t[k+1].punct("(") {
			n := strings.ToUpper(tk.text)
			if !dpFunctions[n] {
				if otherAggregates[n] || aggregationThresholdFunctions[n] || strings.HasPrefix(n, "ANON_") {
					return "Aggregate function " + n + " is not supported in SELECT WITH DIFFERENTIAL_PRIVACY; use AVG, COUNT, PERCENTILE_CONT or SUM", false
				}
				continue
			}
			closeAt := -1
			for j := k + 2; j < len(t); j++ {
				if d[j] == d[k] && t[j].punct(")") {
					closeAt = j
					break
				}
			}
			if closeAt < 0 {
				return "Syntax error: expected ) after " + n + " in HAVING", false
			}
			call := t[k : closeAt+1]
			key := dpKey(call)
			at := -1
			for i, it := range q.items {
				if it.agg != nil && it.key == key {
					at = i
					break
				}
			}
			if at < 0 {
				it, m, unsup := dpParseItem(sql, call, d[k:closeAt+1])
				if m != "" {
					return m, unsup
				}
				it.hidden = true
				q.items = append(q.items, it)
				at = len(q.items) - 1
			}
			add(tk.pos, at)
			last = call[len(call)-1].end
			k = closeAt
			continue
		}
		if i, ok := names[strings.ToLower(tk.text)]; ok {
			add(tk.pos, i)
			last = tk.end
		}
	}
	add(t[len(t)-1].end, -1)
	return "", false
}

// dpParseItem reads one item of the select list, t, whose depths are d.
func dpParseItem(sql string, t []token, d []int) (it dpItem, msg string, unsupported bool) {
	base := d[0]
	for k, tk := range t {
		if tk.punct("*") && d[k] == base && (k == 0 || t[k-1].punct(".")) {
			return it, "SELECT WITH DIFFERENTIAL_PRIVACY cannot select *: only grouping columns and differentially private aggregates can be projected", false
		}
		if tk.is("OVER") {
			return it, "an analytic function (OVER)", true
		}
	}
	isAgg := len(t) >= 3 && t[0].kind == tokWord && t[1].punct("(")
	name := ""
	if isAgg {
		name = strings.ToUpper(t[0].text)
		isAgg = dpFunctions[name]
	}
	// An aggregate anywhere else: in an expression, or another function.
	for k := 0; k+1 < len(t); k++ {
		if t[k].kind != tokWord || !t[k+1].punct("(") || k > 0 && t[k-1].punct(".") {
			continue
		}
		n := strings.ToUpper(t[k].text)
		if k == 0 && isAgg {
			continue
		}
		switch {
		case dpFunctions[n]:
			if isAgg {
				return it, "SELECT WITH DIFFERENTIAL_PRIVACY cannot be used with a multi-level aggregation query", false
			}
			return it, "a differentially private aggregate inside an expression (" + n + ")", true
		case strings.HasPrefix(n, "ANON_"):
			return it, "the function " + n + " (the deprecated ANONYMIZATION clause's)", true
		case otherAggregates[n] || aggregationThresholdFunctions[n]:
			return it, "Aggregate function " + n + " is not supported in SELECT WITH DIFFERENTIAL_PRIVACY; use AVG, COUNT, PERCENTILE_CONT or SUM", false
		}
	}
	if !isAgg {
		it.text = sql[t[0].pos:t[len(t)-1].end]
		return it, "", false
	}
	// The call's closing parenthesis, and its alias.
	closeAt := -1
	for k := 2; k < len(t); k++ {
		if d[k] == base && t[k].punct(")") {
			closeAt = k
			break
		}
	}
	if closeAt < 0 {
		return it, "Syntax error: expected ) after " + name, false
	}
	it.key = dpKey(t[:closeAt+1])
	rest := t[closeAt+1:]
	switch {
	case len(rest) == 0:
	case len(rest) == 2 && rest[0].is("AS") && (rest[1].kind == tokWord || rest[1].kind == tokQuoted):
		it.alias = rest[1].text
	case len(rest) == 1 && (rest[0].kind == tokWord || rest[0].kind == tokQuoted):
		it.alias = rest[0].text
	default:
		return it, "a differentially private aggregate inside an expression (" + name + ")", true
	}
	// Its arguments, split at the commas of its depth.
	var args [][]token
	a := 2
	for k := 2; k <= closeAt; k++ {
		if k == closeAt || d[k] == base+1 && t[k].punct(",") {
			if k > a {
				args = append(args, t[a:k])
			}
			a = k + 1
		}
	}
	g := &dpAgg{fn: name}
	var positional [][]token
	for _, arg := range args {
		if len(arg) >= 3 && arg[0].kind == tokWord && arg[1].punct("=") && arg[2].punct(">") {
			opt := strings.ToLower(arg[0].text)
			if len(arg) < 4 {
				return it, "Syntax error: expected (lower_bound, upper_bound) after " + opt, false
			}
			text := strings.TrimSpace(sql[arg[3].pos:arg[len(arg)-1].end])
			lo, hi, ok := dpBounds(text)
			if !ok {
				return it, "The " + opt + " argument of " + name + " must be a pair of numeric literals (lower_bound, upper_bound) with lower_bound <= upper_bound", false
			}
			want := "contribution_bounds_per_group"
			if name == "PERCENTILE_CONT" {
				want = "contribution_bounds_per_row"
			}
			if opt != want {
				return it, "Named argument " + opt + " is not supported by " + name + " in SELECT WITH DIFFERENTIAL_PRIVACY; it takes " + want, false
			}
			g.lo, g.hi, g.bounded = lo, hi, true
			continue
		}
		positional = append(positional, arg)
	}
	text := func(arg []token) string { return sql[arg[0].pos:arg[len(arg)-1].end] }
	switch name {
	case "COUNT":
		if len(positional) != 1 {
			return it, "COUNT in SELECT WITH DIFFERENTIAL_PRIVACY takes * or one expression", false
		}
		if len(positional[0]) == 1 && positional[0][0].punct("*") {
			g.star = true
		} else if positional[0][0].is("DISTINCT") {
			// Neither BigQuery's reference nor GoogleSQL's documents it
			// (aggregate-dp-functions#dp_count; #1482).
			return it, "COUNT(DISTINCT …), which BigQuery's reference does not document for differential privacy", true
		} else {
			g.arg = text(positional[0])
		}
	case "SUM", "AVG":
		if len(positional) != 1 {
			return it, name + " in SELECT WITH DIFFERENTIAL_PRIVACY takes one expression", false
		}
		if positional[0][0].is("DISTINCT") {
			return it, name + "(DISTINCT …)", true
		}
		g.arg = text(positional[0])
	case "PERCENTILE_CONT":
		if len(positional) != 2 {
			return it, "PERCENTILE_CONT in SELECT WITH DIFFERENTIAL_PRIVACY takes an expression and a percentile", false
		}
		p, err := strconv.ParseFloat(strings.ReplaceAll(text(positional[1]), " ", ""), 64)
		if err != nil || p < 0 || p > 1 {
			return it, "The percentile of PERCENTILE_CONT must be a literal in the range [0, 1]", false
		}
		if !g.bounded {
			return it, "PERCENTILE_CONT in SELECT WITH DIFFERENTIAL_PRIVACY requires contribution_bounds_per_row => (lower_bound, upper_bound)", false
		}
		g.arg, g.pct = text(positional[0]), p
	}
	it.agg = g
	return it, "", false
}

// dpPublicGroups checks the public group join of a query with
// group_selection_strategy = PUBLIC_GROUPS, whose FROM clause is t (depths
// d) and GROUP BY list group: as GoogleSQL's reference has it
// (query-syntax#dp_group_selection_strategy), a LEFT OUTER JOIN or RIGHT
// OUTER JOIN whose public side is a SELECT DISTINCT subquery, joined on each
// grouping item. msg is why it is refused.
func dpPublicGroups(t []token, d []int, group string) string {
	const need = "group_selection_strategy = PUBLIC_GROUPS requires a public group join for every grouping item: " +
		"a LEFT OUTER JOIN or RIGHT OUTER JOIN of the private table with a SELECT DISTINCT subquery of the public groups, " +
		"joined on each grouping item"
	if strings.TrimSpace(group) == "" {
		return need
	}
	distinct := func(open int) bool { return open+2 < len(t) && t[open+1].is("SELECT") && t[open+2].is("DISTINCT") }
	name := func(tk token) bool { return tk.kind == tokWord || tk.kind == tokQuoted }
	joined := map[string]bool{}
	for k := range t {
		if d[k] != 0 || !t[k].is("LEFT") && !t[k].is("RIGHT") {
			continue
		}
		j := k + 1
		if j < len(t) && t[j].is("OUTER") {
			j++
		}
		if j >= len(t) || !t[j].is("JOIN") {
			continue
		}
		j++
		public := false
		if t[k].is("RIGHT") {
			public = j < len(t) && t[j].punct("(") && distinct(j)
		} else {
			b := k - 1
			if b >= 0 && name(t[b]) {
				b-- // an alias
				if b >= 0 && t[b].is("AS") {
					b--
				}
			}
			if b >= 0 && t[b].punct(")") {
				for o := b - 1; o >= 0; o-- {
					if d[o] == d[b] && t[o].punct("(") {
						public = distinct(o)
						break
					}
				}
			}
		}
		if !public {
			continue
		}
		for j < len(t) && !(d[j] == 0 && (t[j].is("USING") || t[j].is("ON"))) {
			j++
		}
		if j >= len(t) {
			continue
		}
		if t[j].is("USING") {
			for m := j + 1; m < len(t) && !(d[m] == 0 && t[m].punct(")")); m++ {
				if name(t[m]) {
					joined[strings.ToLower(t[m].text)] = true
				}
			}
			continue
		}
		for m := j + 1; m < len(t); m++ {
			if d[m] == 0 && (t[m].punct(",") || t[m].is("JOIN") || t[m].is("LEFT") || t[m].is("RIGHT") ||
				t[m].is("INNER") || t[m].is("FULL") || t[m].is("CROSS") || t[m].is("WHERE")) {
				break
			}
			if name(t[m]) && !(m+1 < len(t) && t[m+1].punct(".")) {
				joined[strings.ToLower(t[m].text)] = true
			}
		}
	}
	for _, g := range strings.Split(group, ",") {
		g = strings.TrimSpace(g)
		if k := strings.LastIndex(g, "."); k >= 0 {
			g = g[k+1:]
		}
		if !joined[strings.ToLower(strings.Trim(g, "`"))] {
			return need + "; there is none for " + strings.TrimSpace(g)
		}
	}
	return ""
}

// dpBounds reads "(lo, hi)".
func dpBounds(text string) (lo, hi float64, ok bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "(") || !strings.HasSuffix(text, ")") {
		return 0, 0, false
	}
	parts := strings.Split(text[1:len(text)-1], ",")
	if len(parts) != 2 {
		return 0, 0, false
	}
	var err1, err2 error
	lo, err1 = strconv.ParseFloat(strings.ReplaceAll(parts[0], " ", ""), 64)
	hi, err2 = strconv.ParseFloat(strings.ReplaceAll(parts[1], " ", ""), 64)
	if err1 != nil || err2 != nil || lo > hi || math.IsInf(lo, 0) || math.IsInf(hi, 0) {
		return 0, 0, false
	}
	return lo, hi, true
}

// dpPolicy is a differential_privacy_policy.
type dpPolicy struct {
	unit                     string
	maxEps, deltaPerQuery    float64
	epsBudget, deltaBudget   float64
	maxGroups                int64
	hasEpsBudget, hasDeltaBg bool
}

// parseDPPolicy reads a differential_privacy_policy; why is why it is
// refused.
func parseDPPolicy(raw json.RawMessage) (p dpPolicy, why string) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return p, "differential_privacy_policy is not an object"
	}
	num := func(names ...string) (float64, bool, string) {
		for _, n := range names {
			v, ok := m[n]
			if !ok || string(v) == "null" {
				continue
			}
			var f json.Number
			if json.Unmarshal(v, &f) != nil {
				var s string
				if json.Unmarshal(v, &s) != nil {
					return 0, false, n + " is not a number"
				}
				f = json.Number(s)
			}
			x, err := f.Float64()
			if err != nil {
				return 0, false, n + " is not a number"
			}
			return x, true, ""
		}
		return 0, false, ""
	}
	for _, n := range []string{"privacy_unit_column", "privacyUnitColumn"} {
		if v, ok := m[n]; ok && json.Unmarshal(v, &p.unit) != nil {
			return p, "privacy_unit_column is not a column name"
		}
	}
	if p.unit == "" {
		return p, "its differential_privacy_policy names no privacy_unit_column"
	}
	var ok bool
	var why2 string
	if p.maxEps, ok, why2 = num("max_epsilon_per_query", "maxEpsilonPerQuery"); why2 != "" {
		return p, why2
	} else if !ok || p.maxEps < 0.001 || p.maxEps > 1e15 {
		return p, "max_epsilon_per_query must be a number from 0.001 to 1e+15"
	}
	if p.deltaPerQuery, ok, why2 = num("delta_per_query", "deltaPerQuery"); why2 != "" {
		return p, why2
	} else if !ok || p.deltaPerQuery < 1e-15 || p.deltaPerQuery > 1 {
		return p, "delta_per_query must be a number from 1e-15 to 1"
	}
	if p.epsBudget, p.hasEpsBudget, why2 = num("epsilon_budget", "epsilonBudget"); why2 != "" {
		return p, why2
	} else if p.hasEpsBudget && (p.epsBudget < 0.001 || p.epsBudget > 1e15 || p.epsBudget <= p.maxEps) {
		return p, "epsilon_budget must be a number from 0.001 to 1e+15, larger than max_epsilon_per_query"
	}
	if p.deltaBudget, p.hasDeltaBg, why2 = num("delta_budget", "deltaBudget"); why2 != "" {
		return p, why2
	} else if p.hasDeltaBg && (p.deltaBudget < 1e-15 || p.deltaBudget > 1000 || p.deltaBudget <= p.deltaPerQuery) {
		return p, "delta_budget must be a number from 1e-15 to 1000, larger than delta_per_query"
	}
	g, has, why2 := num("max_groups_contributed", "maxGroupsContributed")
	if why2 != "" {
		return p, why2
	}
	if has {
		if g < 0 || g != math.Trunc(g) {
			return p, "max_groups_contributed must be a non-negative integer"
		}
		p.maxGroups = int64(g)
	}
	return p, ""
}

// dpBudgetStore is the epsilon and delta spent of each table or view with a
// differential_privacy_policy, kept in path when it is not "".
type dpBudgetStore struct {
	mu    sync.Mutex
	path  string
	spent map[string][2]float64
}

var dpBudgetStores sync.Map // *tableSettings or nil -> *dpBudgetStore

// dpBudgets is the store of f's state.
func (f front) dpBudgets() *dpBudgetStore {
	var key any = f.tables
	path := ""
	if f.tables != nil && f.tables.path != "" {
		path = filepath.Join(filepath.Dir(f.tables.path), "dpbudget.json")
	}
	if v, ok := dpBudgetStores.Load(key); ok {
		return v.(*dpBudgetStore)
	}
	s := &dpBudgetStore{path: path, spent: map[string][2]float64{}}
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &s.spent)
		}
	}
	v, _ := dpBudgetStores.LoadOrStore(key, s)
	return v.(*dpBudgetStore)
}

// charge spends eps and delta of key's budget, ok false (nothing spent)
// when either would pass it.
func (s *dpBudgetStore) charge(key string, p dpPolicy, eps, delta float64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	got := s.spent[key]
	if p.hasEpsBudget && got[0]+eps > p.epsBudget*(1+1e-12) || p.hasDeltaBg && got[1]+delta > p.deltaBudget*(1+1e-12) {
		return false
	}
	s.spent[key] = [2]float64{got[0] + eps, got[1] + delta}
	if s.path != "" {
		if b, err := json.Marshal(s.spent); err == nil {
			tmp := s.path + ".tmp"
			if os.WriteFile(tmp, b, 0o600) == nil {
				_ = os.Rename(tmp, s.path)
			}
		}
	}
	return true
}

// dpMemoKey holds a *dpMemo: a script's (#1482), set when the front runs a
// script with SELECT WITH DIFFERENTIAL_PRIVACY a statement at a time.
type dpMemoKey struct{}

// dpReplayKey marks the script's last query run once more as the client's
// request (scriptflow.go): its noisy rows are the ones its child job had,
// not new noise of another charge of the budget.
type dpReplayKey struct{}

// dpMemo is each statement's last noisy query, by its text.
type dpMemo struct {
	mu   sync.Mutex
	last map[string]string
}

// dpScript carries out q, a script of statements stmts with SELECT WITH
// DIFFERENTIAL_PRIVACY (#1482) or SELECT WITH AGGREGATION_THRESHOLD, or
// reading a table or view with a privacy policy (prot, #1502), a statement
// at a time (scriptflow.go), each in its turn, so it reads what the
// statements before it made and is checked as a lone query; ok false when
// q is not such a script.
func (f front) dpScript(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool, stmts [][]token, prot []privacyRead) bool {
	if len(stmts) < 2 {
		return false
	}
	has := false
	for _, s := range stmts {
		for i := 0; i+2 < len(s) && !has; i++ {
			has = s[i].is("SELECT") && s[i+1].is("WITH") && (s[i+2].is("DIFFERENTIAL_PRIVACY") || s[i+2].is("AGGREGATION_THRESHOLD"))
		}
	}
	for _, p := range prot {
		has = has || !createsView(p.tr.stmt)
	}
	if !has {
		return false
	}
	if r.Context().Value(dpMemoKey{}) != nil || txnRequest(r) {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a script that reads a table "+
			"or view with a privacy_policy, or has SELECT WITH DIFFERENTIAL_PRIVACY or AGGREGATION_THRESHOLD, which "+
			"CloudBurrow cannot run a statement at a time. Nothing was run.")
		return true
	}
	f.runScript(w, r.WithContext(context.WithValue(r.Context(), dpMemoKey{}, &dpMemo{})), q, insert)
	return true
}

// dpSpanOf is the span of spans (a SELECT WITH DIFFERENTIAL_PRIVACY's tokens)
// whose FROM clause reads p, or -1.
func dpSpanOf(spans [][2]int, p privacyRead) int {
	if len(spans) == 0 {
		return -1
	}
	depth := privacyDepths(p.tr.stmt)
	for k, sp := range spans {
		if p.tr.at > sp[0] && p.tr.at < sp[1] && depth[p.tr.at] == depth[sp[0]] {
			return k
		}
	}
	return -1
}

// dpFailure is why a differentially private query is not answered: its
// status (400, 501 or 500) and message.
type dpFailure struct {
	status int
	msg    string
}

// enforceDifferentialPrivacy carries out s, the one statement of q, a
// SELECT WITH DIFFERENTIAL_PRIVACY; at is its read of a table or view with
// a differential_privacy_policy, or nil.
func (f front) enforceDifferentialPrivacy(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool, s []token, at *privacyRead) {
	final, fl := f.dpResult(r, q, s, at)
	if fl != nil {
		f.dpFail(w, r, q, insert, at, fl)
		return
	}
	f.runRewritten(w, r, q, insert, final)
}

// dpFail answers fl.
func (f front) dpFail(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool, at *privacyRead, fl *dpFailure) {
	switch fl.status {
	case http.StatusBadRequest:
		dataset := "_cloudburrow"
		if at != nil {
			dataset = at.tr.dataset
		}
		f.failBeforeRun(w, r, q, insert, dataset, http.StatusBadRequest, rowError{Reason: "invalidQuery", Message: fl.msg})
	case http.StatusNotImplemented:
		writeError(w, fl.status, "notImplemented", fl.msg)
	default:
		writeError(w, fl.status, "internalError", fl.msg)
	}
}

// runRewritten answers q with final, a query of the front's, in its place:
// the job keeps the client's text.
func (f front) runRewritten(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool, final string) {
	r = r.WithContext(context.WithValue(r.Context(), privacyCheckedKey{}, true))
	if !setQueryText(r, insert, final) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return
	}
	client := jobText{query: q.Query}
	q.Query = final
	rec := newRecorder()
	f.runQuery(rec, r, q, insert)
	f.clientText(w, rec, client)
}

// dpResult carries out s, a SELECT WITH DIFFERENTIAL_PRIVACY of q's text (a
// lone query's, or a subquery's), and returns final, a query of its noisy
// rows; at is its read of a table or view with a differential_privacy_policy,
// or nil.
func (f front) dpResult(r *http.Request, q queryOptions, s []token, at *privacyRead) (final string, fl *dpFailure) {
	text := q.Query[s[0].pos:s[len(s)-1].end]
	memo, _ := r.Context().Value(dpMemoKey{}).(*dpMemo)
	if memo != nil && r.Context().Value(dpReplayKey{}) != nil {
		memo.mu.Lock()
		got, ok := memo.last[text]
		memo.mu.Unlock()
		if ok {
			return got, nil
		}
	}
	final, fl = f.dpCompute(r, q, s, at)
	if fl == nil && memo != nil {
		memo.mu.Lock()
		if memo.last == nil {
			memo.last = map[string]string{}
		}
		memo.last[text] = final
		memo.mu.Unlock()
	}
	return final, fl
}

// dpCompute is dpResult's work: the noise, the budget's charge.
func (f front) dpCompute(r *http.Request, q queryOptions, s []token, at *privacyRead) (string, *dpFailure) {
	failed := func(msg string) (string, *dpFailure) { return "", &dpFailure{http.StatusBadRequest, msg} }
	dq, msg, unsupported := parseDPQuery(q.Query, s)
	if unsupported {
		return "", &dpFailure{http.StatusNotImplemented, msg}
	}
	if msg != "" {
		return failed(msg)
	}
	o := &dq.opts
	unitRef := o.unitText
	var pol dpPolicy
	budgetKey := ""
	if at != nil {
		pol = at.pol.dpPolicy
		if o.unitName != "" && !strings.EqualFold(o.unitName, pol.unit) {
			return failed(fmt.Sprintf("The privacy_unit_column of the query (%s) must match the privacy_unit_column of %s (%s)",
				o.unitText, at.name, pol.unit))
		}
		if unitRef == "" {
			unitRef = quotePath([]string{pol.unit})
		}
		if !o.hasEps {
			o.eps, o.hasEps = pol.maxEps, true
		} else if o.eps > pol.maxEps {
			return failed("Epsilon is too high: " + strconv.FormatFloat(o.eps, 'g', -1, 64) + ", policy for table " + at.name +
				" allows max " + strconv.FormatFloat(pol.maxEps, 'g', -1, 64))
		}
		if !o.hasDelta {
			o.delta, o.hasDelta = pol.deltaPerQuery, true
		} else if o.delta != pol.deltaPerQuery {
			return failed("Delta must be the delta_per_query of the policy for table " + at.name + ": " +
				strconv.FormatFloat(pol.deltaPerQuery, 'g', -1, 64) + ", not " + strconv.FormatFloat(o.delta, 'g', -1, 64))
		}
		if pol.maxGroups > 0 {
			switch {
			case !o.groupsSet:
				o.maxGroups = pol.maxGroups
			case o.groupsNo || o.maxGroups > pol.maxGroups:
				return failed(fmt.Sprintf("max_groups_contributed of the query must be at most %d, the max_groups_contributed of "+
					"the policy for table %s", pol.maxGroups, at.name))
			}
		}
		project := projectOf(f.base)
		created, text := "", ""
		if meta, ok := f.metaOf(r, at.tr.dataset, at.tr.table); ok {
			created, text = str(meta, "creationTime"), f.privacyOf(project, at.tr.dataset, at.tr.table, meta)
		}
		budgetKey = streamKey(project, at.tr.dataset, at.tr.table) + "\x00" + created + "\x00" + text
		b := f.dpBudgets()
		b.mu.Lock()
		got := b.spent[budgetKey]
		b.mu.Unlock()
		if pol.hasEpsBudget && got[0]+o.eps > pol.epsBudget*(1+1e-12) || pol.hasDeltaBg && got[1]+o.delta > pol.deltaBudget*(1+1e-12) {
			return failed("Privacy budget is not sufficient for table '" + at.name + "' in this query.")
		}
	}
	switch {
	case !o.hasEps:
		return failed("The epsilon option of DIFFERENTIAL_PRIVACY must be set")
	case !o.hasDelta:
		return failed("The delta option of DIFFERENTIAL_PRIVACY must be set")
	case unitRef == "":
		return failed("A differentially private query must include one and only one privacy unit column: " +
			"set privacy_unit_column in the DIFFERENTIAL_PRIVACY OPTIONS")
	case o.delta == 0 && !o.public:
		// Public groups are not selected, so need no delta.
		return failed("The delta option of DIFFERENTIAL_PRIVACY must be greater than 0 to select groups")
	}
	if !o.groupsSet && o.maxGroups == 0 {
		o.maxGroups = 1 // "The default value is 1."
	}
	// 1. Each privacy unit's contributions to each group.
	inner := dq.innerSQL(unitRef)
	schema, rows, code, emsg := f.dpRows(r, q, inner)
	if code != 0 {
		if code != http.StatusNotImplemented {
			code = http.StatusBadRequest
		}
		return "", &dpFailure{code, emsg}
	}
	if len(schema) != len(dq.items)+1 {
		return "", &dpFailure{http.StatusInternalServerError, "cloudburrow: the differentially private query's columns could not be read"}
	}
	for i, it := range dq.items {
		if it.agg != nil && it.agg.fn == "SUM" {
			t, _ := sqlTypeOf(schema[i])
			it.agg.sumInt = t == "INT64"
		}
	}
	// 2. The noise.
	out, emsg := dq.compute(rows)
	if emsg != "" {
		return failed(emsg)
	}
	if budgetKey != "" && !f.dpBudgets().charge(budgetKey, pol, o.eps, o.delta) {
		return failed("Privacy budget is not sufficient for table '" + at.name + "' in this query.")
	}
	// 3. The answer.
	final, emsg := dq.resultSQL(schema, out)
	if emsg != "" {
		return "", &dpFailure{http.StatusNotImplemented, emsg}
	}
	return final, nil
}

// innerSQL is the query of each privacy unit's contribution to each group:
// the select list's items in their places (an aggregate's contribution
// named __dp_a<i>), then the unit as __dp_unit.
func (dq dpQuery) innerSQL(unitRef string) string {
	var b strings.Builder
	b.WriteString("SELECT ")
	for i, it := range dq.items {
		if i > 0 {
			b.WriteString(", ")
		}
		if it.agg == nil {
			b.WriteString(it.text)
			continue
		}
		g := it.agg
		switch g.fn {
		case "COUNT":
			if g.star {
				b.WriteString("COUNT(*)")
			} else {
				b.WriteString("COUNT(" + g.arg + ")")
			}
		case "SUM":
			b.WriteString("SUM(" + g.arg + ")")
		case "AVG":
			b.WriteString("AVG(CAST(" + g.arg + " AS FLOAT64))")
		case "PERCENTILE_CONT":
			b.WriteString("ARRAY_AGG(CAST(" + g.arg + " AS FLOAT64) IGNORE NULLS)")
		}
		fmt.Fprintf(&b, " AS `__dp_a%d`", i)
	}
	b.WriteString(", " + unitRef + " AS `__dp_unit` ")
	b.WriteString(dq.from)
	b.WriteString(" GROUP BY ")
	if dq.group != "" {
		b.WriteString(dq.group + ", ")
	}
	b.WriteString(unitRef)
	return b.String()
}

// dpRows runs sql, a query of the front's, as q's, and returns its schema
// and rows' cells. code is a failure's status, with its message.
func (f front) dpRows(r *http.Request, q queryOptions, sql string) ([]field, [][]any, int, string) {
	legacy := false
	opts := queryOptions{Query: sql, UseLegacySQL: &legacy, DefaultDataset: q.DefaultDataset,
		ParameterMode: q.ParameterMode, QueryParameters: q.QueryParameters}
	body, err := json.Marshal(struct {
		queryOptions
		MaxResults int `json:"maxResults"`
	}{opts, 100000})
	if err != nil {
		return nil, nil, http.StatusInternalServerError, err.Error()
	}
	req, err := f.queryRequest(r, body)
	if err != nil {
		return nil, nil, http.StatusInternalServerError, err.Error()
	}
	req = req.WithContext(context.WithValue(req.Context(), privacyCheckedKey{}, true))
	rec := newRecorder()
	f.runQuery(rec, req, opts, false)
	var job map[string]any
	_ = json.Unmarshal(rec.body.Bytes(), &job)
	project, id := jobRef(job)
	if project == "" {
		project = projectOf(f.base)
	}
	if id != "" {
		f.records.markInternal(project, id)
	}
	if msg, failed := queryFailure(rec, job); failed {
		code := rec.status
		if code == http.StatusOK || code == 0 {
			code = http.StatusBadRequest
		}
		return nil, nil, code, msg
	}
	type page struct {
		Schema tableSchema `json:"schema"`
		Rows   []struct {
			F []struct{ V any } `json:"f"`
		} `json:"rows"`
		PageToken string `json:"pageToken"`
	}
	var res page
	dec := json.NewDecoder(bytes.NewReader(rec.body.Bytes()))
	dec.UseNumber()
	if err := dec.Decode(&res); err != nil {
		return nil, nil, http.StatusInternalServerError, err.Error()
	}
	schema := res.Schema.Fields
	var rows [][]any
	for {
		for _, row := range res.Rows {
			cells := make([]any, len(row.F))
			for i, c := range row.F {
				cells[i] = c.V
			}
			rows = append(rows, cells)
		}
		if res.PageToken == "" {
			break
		}
		got, flow := (&scriptRun{f: f, r: r}).page(id, res.PageToken, sql)
		if flow != nil {
			return nil, nil, http.StatusInternalServerError, flow.msg
		}
		res = page{}
		dec := json.NewDecoder(bytes.NewReader(got))
		dec.UseNumber()
		if err := dec.Decode(&res); err != nil {
			return nil, nil, http.StatusInternalServerError, err.Error()
		}
	}
	return schema, rows, 0, ""
}

// dpValue reads a cell as a number.
func dpValue(v any) (float64, bool) {
	switch x := v.(type) {
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	}
	return 0, false
}

// dpGroup is one group's contributions.
type dpGroup struct {
	cells []any                // its grouping columns' cells, by item
	units map[string][]float64 // unit -> each aggregate's contribution (NaN: none)
	order int
}

// dpOut is one noisy row: each item's value, nil for NULL.
type dpOut []any

// compute bounds the contributions in rows (innerSQL's) and adds the
// noise, returning the groups kept, each item's value.
func (dq dpQuery) compute(rows [][]any) ([]dpOut, string) {
	n := len(dq.items)
	nAgg := 0
	for _, it := range dq.items {
		if it.agg != nil {
			nAgg++
		}
	}
	groups := map[string]*dpGroup{}
	unitGroups := map[string][]string{}
	for _, row := range rows {
		if len(row) != n+1 || row[n] == nil && !dq.opts.public {
			continue // a NULL privacy unit contributes nothing
		}
		var key []any
		for i, it := range dq.items {
			if it.agg == nil {
				key = append(key, row[i])
			}
		}
		kb, _ := json.Marshal(key)
		g := groups[string(kb)]
		if g == nil {
			g = &dpGroup{cells: row[:n], units: map[string][]float64{}, order: len(groups)}
			groups[string(kb)] = g
		}
		if row[n] == nil {
			// A public group of no privacy unit (PUBLIC_GROUPS: the outer
			// join's row): the group is kept, with nothing of it.
			continue
		}
		ub, _ := json.Marshal(row[n])
		unit := string(ub)
		vals := make([]float64, n)
		for i, it := range dq.items {
			vals[i] = math.NaN()
			if it.agg == nil {
				continue
			}
			if it.agg.fn == "PERCENTILE_CONT" {
				// One of the unit's values in the group, sampled.
				cells, _ := row[i].([]any)
				if len(cells) > 0 {
					m, _ := cells[rand.IntN(len(cells))].(map[string]any)
					if v, ok := dpValue(m["v"]); ok {
						vals[i] = v
					}
				}
				continue
			}
			if v, ok := dpValue(row[i]); ok {
				vals[i] = v
			} else if it.agg.fn == "COUNT" || it.agg.fn == "SUM" {
				vals[i] = 0
			}
		}
		if _, seen := g.units[unit]; !seen {
			unitGroups[unit] = append(unitGroups[unit], string(kb))
		}
		g.units[unit] = vals
	}
	// Contribution bounding: at most maxGroups groups of each unit.
	l0 := dq.opts.maxGroups
	most := int64(1)
	for unit, gs := range unitGroups {
		if dq.opts.maxGroups > 0 && int64(len(gs)) > dq.opts.maxGroups {
			rand.Shuffle(len(gs), func(a, b int) { gs[a], gs[b] = gs[b], gs[a] })
			for _, k := range gs[dq.opts.maxGroups:] {
				delete(groups[k].units, unit)
			}
		}
		if int64(len(gs)) > most {
			most = int64(len(gs))
		}
	}
	if l0 <= 0 {
		l0 = most // NULL: no limit, the sensitivity what the data has
	}
	// The group selection is one more aggregate (query-syntax#dp_epsilon),
	// none with public groups, which are not selected.
	shares := nAgg + 1
	if dq.opts.public {
		shares = nAgg
	}
	eps := dq.opts.eps / float64(shares)
	// Implicit bounds, found with half of each aggregate's epsilon.
	bounds := make([][2]float64, n)
	aggEps := make([]float64, n)
	for i, it := range dq.items {
		if it.agg == nil {
			continue
		}
		aggEps[i] = eps
		if it.agg.bounded {
			bounds[i] = [2]float64{it.agg.lo, it.agg.hi}
			continue
		}
		var all []float64
		for _, g := range groups {
			for _, v := range g.units {
				if !math.IsNaN(v[i]) {
					all = append(all, v[i])
				}
			}
		}
		lo, hi, ok := dpApproxBounds(all, l0, eps/2)
		if !ok {
			// "Implicit bounding can fail with datasets that contain a
			// low number of privacy units, returning no results."
			return nil, ""
		}
		if it.agg.fn == "COUNT" || it.agg.fn == "SUM" {
			lo, hi = math.Min(lo, 0), math.Max(hi, 0)
		}
		bounds[i] = [2]float64{lo, hi}
		aggEps[i] = eps / 2
	}
	ordered := make([]*dpGroup, 0, len(groups))
	for _, g := range groups {
		if len(g.units) > 0 || dq.opts.public {
			ordered = append(ordered, g)
		}
	}
	sort.Slice(ordered, func(a, b int) bool { return ordered[a].order < ordered[b].order })
	var out []dpOut
	for _, g := range ordered {
		if keep, msg := dq.selected(g, eps, l0); msg != "" {
			return nil, msg
		} else if !keep {
			continue
		}
		row := make(dpOut, len(dq.items))
		for i, it := range dq.items {
			if it.agg == nil {
				row[i] = g.cells[i]
				continue
			}
			v, msg := dpAggregate(it.agg, g, i, bounds[i][0], bounds[i][1], l0, aggEps[i])
			if msg != "" {
				return nil, msg
			}
			row[i] = v
		}
		out = append(out, row)
	}
	return out, ""
}

// selected reports whether group g is kept: a noisy count of its units,
// Laplace noise with epsilon eps, thresholded with the query's delta (the
// library's Count.ThresholdedResult); with PUBLIC_GROUPS every group is.
func (dq dpQuery) selected(g *dpGroup, eps float64, l0 int64) (bool, string) {
	if dq.opts.public {
		return true, ""
	}
	c, err := dpagg.NewCount(&dpagg.CountOptions{Epsilon: eps, MaxPartitionsContributed: l0, Noise: noise.Laplace()})
	if err != nil {
		return false, "Invalid differential privacy parameters: " + err.Error()
	}
	if err := c.IncrementBy(int64(len(g.units))); err != nil {
		return false, err.Error()
	}
	kept, err := c.ThresholdedResult(dq.opts.delta)
	if err != nil {
		return false, "Invalid differential privacy parameters: " + err.Error()
	}
	return kept != nil, ""
}

// dpAggregate is aggregate a's noisy value over g's units, the i-th
// contribution of each, clamped to [lo, hi].
func dpAggregate(a *dpAgg, g *dpGroup, i int, lo, hi float64, l0 int64, eps float64) (any, string) {
	clamp := func(v float64) float64 { return math.Max(lo, math.Min(hi, v)) }
	var vals []float64
	units := make([]string, 0, len(g.units))
	for u := range g.units {
		units = append(units, u)
	}
	sort.Strings(units)
	for _, u := range units {
		if v := g.units[u][i]; !math.IsNaN(v) {
			vals = append(vals, clamp(v))
		}
	}
	bad := func(err error) (any, string) { return nil, "Invalid differential privacy parameters: " + err.Error() }
	switch a.fn {
	case "COUNT", "SUM":
		lInf := math.Max(math.Abs(lo), math.Abs(hi))
		if a.fn == "COUNT" || a.sumInt {
			sum := int64(0)
			for _, v := range vals {
				sum += int64(math.Round(v))
			}
			li := int64(math.Ceil(lInf))
			if li < 1 {
				li = 1
			}
			x, err := noise.Laplace().AddNoiseInt64(sum, l0, li, eps, 0)
			if err != nil {
				return bad(err)
			}
			return x, ""
		}
		sum := 0.0
		for _, v := range vals {
			sum += v
		}
		if lInf == 0 {
			lInf = 1
		}
		x, err := noise.Laplace().AddNoiseFloat64(sum, l0, lInf, eps, 0)
		if err != nil {
			return bad(err)
		}
		return x, ""
	case "AVG":
		if hi == lo {
			if len(vals) == 0 {
				return nil, ""
			}
			return lo, ""
		}
		m, err := dpagg.NewBoundedMean(&dpagg.BoundedMeanOptions{Epsilon: eps, MaxPartitionsContributed: l0,
			MaxContributionsPerPartition: 1, Lower: lo, Upper: hi, Noise: noise.Laplace()})
		if err != nil {
			return bad(err)
		}
		for _, v := range vals {
			if err := m.Add(v); err != nil {
				return bad(err)
			}
		}
		x, err := m.Result()
		if err != nil {
			return bad(err)
		}
		return x, ""
	case "PERCENTILE_CONT":
		if hi == lo {
			return lo, ""
		}
		qt, err := dpagg.NewBoundedQuantiles(&dpagg.BoundedQuantilesOptions{Epsilon: eps, MaxPartitionsContributed: l0,
			MaxContributionsPerPartition: 1, Lower: lo, Upper: hi, Noise: noise.Laplace()})
		if err != nil {
			return bad(err)
		}
		for _, v := range vals {
			if err := qt.Add(v); err != nil {
				return bad(err)
			}
		}
		x, err := qt.Result(a.pct)
		if err != nil {
			return bad(err)
		}
		return x, ""
	}
	return nil, ""
}

// dpApproxBounds finds bounds of vals, each unit giving at most l0 of
// them, with epsilon eps: a histogram of the values' signs and powers of
// two, each bin's count with Laplace noise, and the smallest and largest
// bins whose noisy count passes the threshold (the approximate bounds
// algorithm of Google's differential privacy library). ok is false when
// no bin passes.
func dpApproxBounds(vals []float64, l0 int64, eps float64) (lo, hi float64, ok bool) {
	const minK, maxK = -20, 63
	type bin struct{ sign, k int } // sign 0: the zero bin
	counts := map[bin]int64{}
	for _, v := range vals {
		switch {
		case math.Abs(v) < math.Ldexp(1, minK):
			counts[bin{0, 0}]++
		default:
			k := int(math.Ceil(math.Log2(math.Abs(v))))
			k = max(minK, min(maxK, k))
			s := 1
			if v < 0 {
				s = -1
			}
			counts[bin{s, k}]++
		}
	}
	bins := 1 + 2*(maxK-minK+1)
	threshold, err := noise.Laplace().Threshold(l0, 1, eps, 0, 1e-9/float64(bins))
	if err != nil {
		return 0, 0, false
	}
	first := true
	edges := func(b bin) (float64, float64) {
		switch b.sign {
		case 0:
			return 0, 0
		case 1:
			return math.Ldexp(1, b.k-1), math.Ldexp(1, b.k)
		}
		return -math.Ldexp(1, b.k), -math.Ldexp(1, b.k-1)
	}
	for sign := -1; sign <= 1; sign++ {
		for k := minK; k <= maxK; k++ {
			b := bin{sign, k}
			if sign == 0 {
				if k != 0 {
					continue
				}
				b = bin{0, 0}
			}
			x, err := noise.Laplace().AddNoiseInt64(counts[b], l0, 1, eps, 0)
			if err != nil || float64(x) < threshold {
				continue
			}
			a, z := edges(b)
			if first {
				lo, hi, first = a, z, false
				continue
			}
			lo, hi = math.Min(lo, a), math.Max(hi, z)
		}
	}
	return lo, hi, !first
}

// resultSQL is a query whose rows are out, with the columns the client's
// query has: a grouping column's name and type from schema (innerSQL's),
// an aggregate's its alias (else f<n>_) and its type. The rows' columns are
// __dp_c<i>, item i, so HAVING (on them) and ORDER BY (on the names) apply
// to the noisy values; a hidden aggregate is not selected.
func (dq dpQuery) resultSQL(schema []field, out []dpOut) (string, string) {
	var cols, types []string
	anon := 0
	for i, it := range dq.items {
		if it.hidden {
			typ := "FLOAT64"
			if it.agg.fn == "COUNT" || it.agg.sumInt {
				typ = "INT64"
			}
			cols, types = append(cols, ""), append(types, typ)
			continue
		}
		if it.agg == nil {
			if i >= len(schema) {
				return "", "cloudburrow: the differentially private query's columns could not be read"
			}
			typ, msg := sqlTypeOf(schema[i])
			if msg != "" {
				return "", msg
			}
			name := schema[i].Name
			if name == "" || strings.HasPrefix(name, "$") {
				name = fmt.Sprintf("f%d_", anon)
				anon++
			}
			cols, types = append(cols, name), append(types, typ)
			continue
		}
		name := it.alias
		if name == "" {
			name = fmt.Sprintf("f%d_", anon)
			anon++
		}
		typ := "FLOAT64"
		if it.agg.fn == "COUNT" {
			typ = "INT64"
		}
		if it.agg.sumInt {
			typ = "INT64"
		}
		cols, types = append(cols, name), append(types, typ)
	}
	var st, sel []string
	for i := range cols {
		c := fmt.Sprintf("`__dp_c%d`", i)
		st = append(st, c+" "+types[i])
		if !dq.items[i].hidden {
			sel = append(sel, c+" AS "+quotePath([]string{cols[i]}))
		}
	}
	structType := "STRUCT<" + strings.Join(st, ", ") + ">"
	var lits []string
	for _, row := range out {
		var vs []string
		for i, it := range dq.items {
			v := row[i]
			if it.agg == nil {
				lit, msg := literalOf(schema[i], v)
				if msg != "" {
					return "", msg
				}
				vs = append(vs, lit.lit)
				continue
			}
			switch x := v.(type) {
			case nil:
				vs = append(vs, "CAST(NULL AS "+types[i]+")")
			case int64:
				if types[i] == "INT64" {
					vs = append(vs, strconv.FormatInt(x, 10))
				} else {
					vs = append(vs, "CAST("+strconv.FormatInt(x, 10)+" AS FLOAT64)")
				}
			case float64:
				if types[i] == "INT64" {
					vs = append(vs, strconv.FormatInt(int64(math.Round(x)), 10))
				} else {
					vs = append(vs, "CAST('"+strconv.FormatFloat(x, 'g', -1, 64)+"' AS FLOAT64)")
				}
			}
		}
		lits = append(lits, "STRUCT("+strings.Join(vs, ", ")+")")
	}
	sql := "SELECT " + strings.Join(sel, ", ") + " FROM UNNEST(ARRAY<" + structType + ">[" + strings.Join(lits, ", ") + "])"
	if len(dq.having) > 0 {
		var b strings.Builder
		for _, p := range dq.having {
			if p.item >= 0 {
				fmt.Fprintf(&b, "`__dp_c%d`", p.item)
				continue
			}
			b.WriteString(p.text)
		}
		sql += " WHERE " + b.String()
	}
	if dq.tail != "" {
		sql += " " + dq.tail
	}
	return sql, ""
}
