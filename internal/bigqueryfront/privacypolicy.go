package bigqueryfront

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// A table's or view's privacy_policy, enforced (#1426), as BigQuery
// documents its analysis rules
// (https://cloud.google.com/bigquery/docs/analysis-rules) and the
// AGGREGATION_THRESHOLD clause
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/query-syntax#agg_threshold_clause).
//
// Where a policy is set: CREATE TABLE, CREATE VIEW, ALTER TABLE and ALTER
// VIEW ... SET OPTIONS(privacy_policy = '...') (tableoptions.go, kept by the
// front), and tables.insert, patch and update of a view with a
// view.privacyPolicy (the REST resource has it only on a view; kept by the
// emulator in the resource, and the front watches the view). The engine
// knows nothing of either, so the front enforces them in its query
// rewrite, before the engine runs anything:
//
//   - Aggregation threshold (aggregation_threshold_policy: threshold,
//     privacy_unit_column): a statement that reads the table must be a lone
//     SELECT WITH AGGREGATION_THRESHOLD [OPTIONS(...)]; anything else is
//     400. Its select list must aggregate, with only the functions the
//     clause allows, and must not project the privacy unit column. The
//     query's threshold must be at least the policy's, its
//     privacy_unit_column the policy's. It is sent on without the clause
//     and with HAVING COUNT(DISTINCT unit) >= threshold, so a group with
//     fewer distinct privacy units is dropped. A NULL privacy unit is not
//     counted (COUNT DISTINCT), which can only drop more groups.
//     The clause with OPTIONS(threshold, privacy_unit_column) on tables
//     with no policy is carried out the same way.
//   - Join restriction (join_restriction_policy: join_condition,
//     join_allowed_columns): each read of the table is checked against the
//     INNER JOINs of its FROM clause, the columns of the table their ON or
//     USING name: JOIN_ALL, JOIN_ANY, JOIN_BLOCKED and JOIN_NOT_REQUIRED
//     with BigQuery's documented errors.
//   - Differential privacy (differential_privacy_policy, SELECT WITH
//     DIFFERENTIAL_PRIVACY): carried out with the noise, the contribution
//     bounding, the group thresholding and the epsilon and delta budget
//     (#1455, differentialprivacy.go). A statement that reads a table with
//     a differential_privacy_policy must be a lone SELECT WITH
//     DIFFERENTIAL_PRIVACY.
//
// What the front cannot check is refused 501, never run as if the table
// had no policy: a read of the table in a subquery, CTE or another view's
// query, time travel or a decorator of it, a join that is not INNER, a
// script, a set operation, QUALIFY, WINDOW, an analytic function or a
// function called through a path (a UDF) in a SELECT WITH
// AGGREGATION_THRESHOLD; and tabledata.list, copy, extract and Storage Read
// of the table (protectedRead).

// privacyPolicy is a privacy_policy, read from the DDL's JSON or the
// resource's.
type privacyPolicy struct {
	threshold int64
	unit      string
	hasAT     bool
	join      string
	allowed   []string
	hasJoin   bool
	dp        bool
	dpPolicy  dpPolicy // differentialprivacy.go (#1455)
}

// parsePrivacyPolicy reads s; why is why the front cannot use it.
func parsePrivacyPolicy(s string) (p privacyPolicy, why string) {
	var raw map[string]json.RawMessage
	if json.Unmarshal([]byte(s), &raw) != nil {
		return p, "it is not a JSON object"
	}
	pick := func(m map[string]json.RawMessage, names ...string) json.RawMessage {
		for _, n := range names {
			if v, ok := m[n]; ok && string(v) != "null" {
				return v
			}
		}
		return nil
	}
	if at := pick(raw, "aggregation_threshold_policy", "aggregationThresholdPolicy"); at != nil {
		var m map[string]json.RawMessage
		if json.Unmarshal(at, &m) != nil {
			return p, "aggregation_threshold_policy is not an object"
		}
		p.hasAT = true
		if v := pick(m, "threshold"); v != nil {
			var n json.Number
			if json.Unmarshal(v, &n) != nil {
				var str string
				if json.Unmarshal(v, &str) != nil {
					return p, "threshold is not a number"
				}
				n = json.Number(str)
			}
			t, err := n.Int64()
			if err != nil || t <= 0 {
				return p, "threshold is not a positive integer"
			}
			p.threshold = t
		}
		if p.threshold == 0 {
			p.threshold = 1
		}
		v := pick(m, "privacy_unit_column", "privacy_unit_columns", "privacyUnitColumns", "privacyUnitColumn")
		var one string
		var many []string
		switch {
		case v == nil:
		case json.Unmarshal(v, &one) == nil:
			p.unit = one
		case json.Unmarshal(v, &many) == nil && len(many) == 1:
			p.unit = many[0]
		default:
			return p, "privacy_unit_column is not one column name"
		}
		if p.unit == "" {
			return p, "it names no privacy_unit_column"
		}
	}
	if jr := pick(raw, "join_restriction_policy", "joinRestrictionPolicy"); jr != nil {
		var m map[string]json.RawMessage
		if json.Unmarshal(jr, &m) != nil {
			return p, "join_restriction_policy is not an object"
		}
		p.hasJoin = true
		if v := pick(m, "join_condition", "joinCondition"); v != nil && json.Unmarshal(v, &p.join) != nil {
			return p, "join_condition is not a string"
		}
		switch p.join {
		case "JOIN_ALL", "JOIN_ANY", "JOIN_BLOCKED", "JOIN_NOT_REQUIRED":
		default:
			return p, fmt.Sprintf("join_condition %q is none of JOIN_ALL, JOIN_ANY, JOIN_BLOCKED and JOIN_NOT_REQUIRED", p.join)
		}
		if v := pick(m, "join_allowed_columns", "joinAllowedColumns"); v != nil && json.Unmarshal(v, &p.allowed) != nil {
			return p, "join_allowed_columns is not an array of column names"
		}
	}
	if dp := pick(raw, "differential_privacy_policy", "differentialPrivacyPolicy"); dp != nil {
		p.dp = true
		if p.dpPolicy, why = parseDPPolicy(dp); why != "" {
			return p, why
		}
	}
	if !p.hasAT && !p.hasJoin && !p.dp {
		return p, "it has no aggregation_threshold_policy, join_restriction_policy or differential_privacy_policy"
	}
	return p, ""
}

// privacyRead is a table or view of a statement's reads with a policy.
type privacyRead struct {
	tr      tableRead
	name    string // dataset.table, as BigQuery's errors name it
	pol     privacyPolicy
	columns map[string]bool // its columns, lower case
}

// privacyCheckedKey marks a request whose query the front rewrote by the
// policies, so the rewritten query is not checked again.
type privacyCheckedKey struct{}

// privacyOf reads the policy of dataset.table, whose resource is meta: the
// one the front keeps, else the resource's view.privacyPolicy. text is "" when
// it has none.
func (f front) privacyOf(project, dataset, table string, meta map[string]any) string {
	if s := f.tables.get(streamKey(project, dataset, table), str(meta, "creationTime")); s.PrivacyPolicy != nil {
		return *s.PrivacyPolicy
	}
	v, _ := meta["view"].(map[string]any)
	if pp, ok := v["privacyPolicy"].(map[string]any); ok && len(pp) > 0 {
		b, _ := json.Marshal(pp)
		return string(b)
	}
	return ""
}

// privacyRefusal is the 501 of what the front cannot check of a table
// with a privacy policy.
func privacyRefusal(what, name string) string {
	return "Not implemented here: " + what + " of " + name + ", which has a privacy_policy. CloudBurrow enforces " +
		"an aggregation threshold and a join restriction only in a query that reads the table or view in its " +
		"top-level FROM clause (docs/compatibility.md). Nothing was run."
}

// hasPrivacyClause reports whether sql has SELECT WITH AGGREGATION_THRESHOLD
// or SELECT WITH DIFFERENTIAL_PRIVACY.
func hasPrivacyClause(sql string) bool {
	u := strings.ToUpper(sql)
	return strings.Contains(u, "AGGREGATION_THRESHOLD") || strings.Contains(u, "DIFFERENTIAL_PRIVACY")
}

// createsView reports whether s is CREATE [OR REPLACE] VIEW.
func createsView(s []token) bool {
	i := 1
	if len(s) > 3 && s[1].is("OR") && s[2].is("REPLACE") {
		i = 3
	}
	return len(s) > i && s[0].is("CREATE") && s[i].is("VIEW")
}

// privacyDepths returns each token's parenthesis depth in stmt.
func privacyDepths(stmt []token) []int {
	d := make([]int, len(stmt))
	depth := 0
	for i, t := range stmt {
		if t.punct(")") || t.punct("]") {
			depth--
		}
		d[i] = depth
		if t.punct("(") || t.punct("[") {
			depth++
		}
	}
	return d
}

// enforcePrivacy checks q, whose statements are stmts and whose reads of
// tables with a policy are prot, against the policies (above). It answers
// q (a refusal, or the rewritten query's result) and returns true, or
// returns false to leave q to the rest of runQuery.
func (f front) enforcePrivacy(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool, stmts [][]token, prot []privacyRead) bool {
	if r.Context().Value(privacyCheckedKey{}) != nil {
		return false
	}
	fail := func(msg string) bool {
		dataset := "_cloudburrow"
		if len(prot) > 0 {
			dataset = prot[0].tr.dataset
		}
		f.failBeforeRun(w, r, q, insert, dataset, http.StatusBadRequest, rowError{Reason: "invalidQuery", Message: msg})
		return true
	}
	refuse := func(msg string) bool {
		writeError(w, http.StatusNotImplemented, "notImplemented", msg)
		return true
	}
	clause := func(s []token) string {
		if len(s) >= 3 && s[0].is("SELECT") && s[1].is("WITH") && (s[2].is("AGGREGATION_THRESHOLD") || s[2].is("DIFFERENTIAL_PRIVACY")) {
			return strings.ToUpper(s[2].text)
		}
		return ""
	}
	for _, s := range stmts {
		for i := 0; i+2 < len(s); i++ {
			if i > 0 && s[i].is("SELECT") && s[i+1].is("WITH") && s[i+2].is("DIFFERENTIAL_PRIVACY") {
				return refuse("Not implemented here: SELECT WITH DIFFERENTIAL_PRIVACY other than as a lone query's " +
					"top-level SELECT. Nothing was run.")
			}
			if i > 0 && s[i].is("SELECT") && s[i+1].is("WITH") && s[i+2].is("AGGREGATION_THRESHOLD") {
				return refuse("Not implemented here: SELECT WITH AGGREGATION_THRESHOLD other than as a lone query's " +
					"top-level SELECT. Nothing was run.")
			}
		}
	}
	var atReads, dpReads []privacyRead
	for _, p := range prot {
		if createsView(p.tr.stmt) {
			// A view's query reads nothing until the view is read, and a
			// read of the view is checked (viewReadsPrivate).
			continue
		}
		s := p.tr.stmt
		if p.tr.decorator != "" {
			return refuse(privacyRefusal("a read of a partition decorator", p.name))
		}
		if p.tr.next < len(s) && s[p.tr.next].is("FOR") {
			return fail("Time travel is disabled on " + p.name + ", which has a privacy policy.")
		}
		if privacyDepths(s)[p.tr.at] != 0 || len(s) > 0 && s[0].is("WITH") {
			return refuse(privacyRefusal("a read in a subquery or CTE", p.name))
		}
		if p.pol.hasJoin {
			if msg, unsupported := checkJoinRestriction(p); unsupported {
				return refuse(msg)
			} else if msg != "" {
				return fail(msg)
			}
		}
		if p.pol.dp {
			if clause(s) != "DIFFERENTIAL_PRIVACY" {
				return fail("You must use SELECT WITH DIFFERENTIAL_PRIVACY for this query because a privacy policy " +
					"is set on " + p.name + ".")
			}
			dpReads = append(dpReads, p)
			continue
		}
		if p.pol.hasAT {
			if clause(s) != "AGGREGATION_THRESHOLD" {
				return fail("You must use SELECT WITH AGGREGATION_THRESHOLD for this query because a privacy policy " +
					"is set on " + p.name + ".")
			}
			atReads = append(atReads, p)
		}
	}
	for _, s := range stmts {
		if clause(s) != "DIFFERENTIAL_PRIVACY" {
			continue
		}
		if len(stmts) > 1 {
			return refuse("Not implemented here: SELECT WITH DIFFERENTIAL_PRIVACY in a script; CloudBurrow carries it " +
				"out as a lone query. Nothing was run.")
		}
		if len(dpReads) > 1 {
			return refuse("Not implemented here: a SELECT WITH DIFFERENTIAL_PRIVACY of more than one table or view " +
				"with a differential_privacy_policy. Nothing was run.")
		}
		var at *privacyRead
		if len(dpReads) == 1 {
			at = &dpReads[0]
		}
		f.enforceDifferentialPrivacy(w, r, q, insert, s, at)
		return true
	}
	var rewrite []token
	for _, s := range stmts {
		if clause(s) == "AGGREGATION_THRESHOLD" {
			rewrite = s
		}
	}
	if rewrite == nil {
		return false
	}
	if len(stmts) > 1 {
		return refuse("Not implemented here: SELECT WITH AGGREGATION_THRESHOLD in a script; CloudBurrow carries it " +
			"out as a lone query. Nothing was run.")
	}
	if len(atReads) > 1 {
		return refuse("Not implemented here: a SELECT WITH AGGREGATION_THRESHOLD of more than one table or view " +
			"with an aggregation threshold policy. Nothing was run.")
	}
	var at *privacyRead
	text := q.Query
	if len(atReads) == 1 {
		at = &atReads[0]
		if implicitAlias(at.tr) {
			// The engine takes no implicit alias of a quoted path
			// (measured: "Unrecognized name"), so the read is given its
			// table's name as BigQuery's implicit alias.
			e := at.tr.stmt[at.tr.next-1].end
			text = text[:e] + " AS " + quotePath([]string{at.tr.table}) + text[e:]
			toks, ok := lex(text)
			split := splitStatements(toks)
			if !ok || len(split) != 1 {
				return refuse(privacyRefusal("a query CloudBurrow could not read", at.name))
			}
			rewrite, _, _ = stripControlFlow(split[0])
			// The alias's two tokens are at the read's next.
			a := *at
			a.tr.stmt = rewrite
			at = &a
		}
	}
	sql, msg, unsupported := rewriteAggregationThreshold(text, rewrite, at)
	switch {
	case unsupported:
		return refuse(msg)
	case msg != "":
		return fail(msg)
	}
	r = r.WithContext(context.WithValue(r.Context(), privacyCheckedKey{}, true))
	if !setQueryText(r, insert, sql) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return true
	}
	client := jobText{query: q.Query}
	q.Query = sql
	rec := newRecorder()
	f.runQuery(rec, r, q, insert)
	f.clientText(w, rec, client)
	return true
}

// aggregationThresholdFunctions are the aggregate functions the clause
// allows (query-syntax#agg_threshold_clause).
var aggregationThresholdFunctions = map[string]bool{"APPROX_COUNT_DISTINCT": true, "AVG": true, "COUNT": true,
	"COUNTIF": true, "LOGICAL_AND": true, "LOGICAL_OR": true, "SUM": true, "COVAR_POP": true, "COVAR_SAMP": true,
	"STDDEV_POP": true, "STDDEV_SAMP": true, "VAR_POP": true, "VAR_SAMP": true}

// otherAggregates are GoogleSQL's other aggregate functions, refused in the
// clause.
var otherAggregates = map[string]bool{}

func init() {
	for _, n := range strings.Fields(`ANY_VALUE ARRAY_AGG ARRAY_CONCAT_AGG BIT_AND BIT_OR BIT_XOR GROUPING MAX MAX_BY MIN
		MIN_BY STRING_AGG APPROX_QUANTILES APPROX_TOP_COUNT APPROX_TOP_SUM CORR STDDEV VARIANCE ST_UNION_AGG
		ST_CENTROID_AGG ST_EXTENT ST_NEAREST_NEIGHBORS JSON_AGG PERCENTILE_CONT PERCENTILE_DISC AVG_IF SUM_IF ARRAY_CONCAT
		HLL_COUNT KLL_QUANTILES ANON_COUNT ANON_SUM ANON_AVG`) {
		otherAggregates[n] = true
	}
}

// rewriteAggregationThreshold returns sql, whose one statement s is a
// SELECT WITH AGGREGATION_THRESHOLD, sent on as a plain SELECT whose groups
// with fewer distinct privacy units than the threshold are dropped; at is
// the read it makes of a table or view with an aggregation threshold
// policy, or nil. msg is why it is refused, 501 when unsupported, else 400.
func rewriteAggregationThreshold(sql string, s []token, at *privacyRead) (out, msg string, unsupported bool) {
	bad := func(m string) (string, string, bool) { return "", m, false }
	no := func(m string) (string, string, bool) {
		return "", "Not implemented here: " + m + " in a SELECT WITH AGGREGATION_THRESHOLD. Nothing was run.", true
	}
	depth := privacyDepths(s)
	i := 3
	threshold := int64(0)
	var unitText string
	var unitParts []string
	if i < len(s) && s[i].is("OPTIONS") {
		if i+1 >= len(s) || !s[i+1].punct("(") {
			return bad("Syntax error: expected ( after OPTIONS")
		}
		j := i + 2
		for j < len(s) && !s[j].punct(")") {
			if s[j].kind != tokWord || j+2 >= len(s) || !s[j+1].punct("=") {
				return bad("Syntax error: expected an option name = value in AGGREGATION_THRESHOLD OPTIONS")
			}
			name := strings.ToLower(s[j].text)
			j += 2
			switch name {
			case "threshold":
				n, err := strconv.ParseInt(s[j].text, 10, 64)
				if s[j].kind != tokWord || err != nil || n <= 0 {
					return bad("The threshold option of AGGREGATION_THRESHOLD must be a positive INT64 literal")
				}
				threshold = n
				j++
			case "privacy_unit_column":
				parts, next := path(s, j)
				if next <= j {
					return bad("The privacy_unit_column option of AGGREGATION_THRESHOLD must be a column name")
				}
				unitParts, unitText = parts, sql[s[j].pos:s[next-1].end]
				j = next
			default:
				return bad("Unknown AGGREGATION_THRESHOLD option: " + s[j-2].text)
			}
			if j < len(s) && s[j].punct(",") {
				j++
			}
		}
		if j >= len(s) {
			return bad("Syntax error: expected ) after AGGREGATION_THRESHOLD OPTIONS")
		}
		i = j + 1
	}
	bodyAt := i
	if bodyAt >= len(s) {
		return bad("Syntax error: expected a select list after AGGREGATION_THRESHOLD")
	}
	// The policy's threshold and privacy unit column, or the query's.
	unitRef := unitText
	unitName := ""
	if len(unitParts) > 0 {
		unitName = unitParts[len(unitParts)-1]
	}
	if at != nil {
		if threshold == 0 {
			threshold = at.pol.threshold
		} else if threshold < at.pol.threshold {
			return bad(fmt.Sprintf("The threshold for the query (%d) must be equal to or greater than the threshold "+
				"for %s (%d)", threshold, at.name, at.pol.threshold))
		}
		if unitName != "" && !strings.EqualFold(unitName, at.pol.unit) {
			return bad(fmt.Sprintf("The privacy_unit_column of the query (%s) must match the privacy_unit_column of %s (%s)",
				unitText, at.name, at.pol.unit))
		}
		unitName = at.pol.unit
		unitRef = quotePath([]string{readAlias(at.tr)}) + "." + quotePath([]string{at.pol.unit})
	}
	if threshold == 0 || unitRef == "" {
		return bad("SELECT WITH AGGREGATION_THRESHOLD of tables with no aggregation threshold policy needs " +
			"OPTIONS(threshold = ..., privacy_unit_column = ...)")
	}
	// The clauses at depth 0.
	from, group, having, end := -1, -1, -1, len(s)
	for j := bodyAt; j < len(s); j++ {
		if depth[j] != 0 {
			continue
		}
		t := s[j]
		switch {
		case t.is("FROM") && from < 0:
			from = j
		case t.is("GROUP") && from >= 0:
			group = j
		case t.is("HAVING"):
			having = j
		case t.is("UNION") || t.is("INTERSECT") || t.is("EXCEPT") && from >= 0:
			return no("a set operation")
		case t.is("QUALIFY") || t.is("WINDOW"):
			return no(strings.ToUpper(t.text))
		case (t.is("ORDER") || t.is("LIMIT")) && from >= 0 && end == len(s):
			end = j
		}
	}
	if from < 0 {
		return bad("SELECT WITH AGGREGATION_THRESHOLD must read a table in its FROM clause")
	}
	if bodyAt < len(s) && (s[bodyAt].is("DISTINCT") || s[bodyAt].is("ALL") || s[bodyAt].is("AS")) {
		return no("SELECT " + strings.ToUpper(s[bodyAt].text))
	}
	// The functions it calls, and the privacy unit column outside them.
	aggregates := 0
	inAgg := make([]int, len(s)) // > 0 inside an aggregate's parentheses
	var stack []bool
	inside := 0
	for j := bodyAt; j < len(s); j++ {
		t := s[j]
		if t.punct(")") && len(stack) > 0 {
			if stack[len(stack)-1] {
				inside--
			}
			stack = stack[:len(stack)-1]
		}
		inAgg[j] = inside
		if t.punct("(") {
			agg := j > 0 && isAggregateName(s[j-1]) && (j < 2 || !s[j-2].punct("."))
			stack = append(stack, agg)
			if agg {
				inside++
			}
		}
	}
	for j := bodyAt; j < len(s); j++ {
		t := s[j]
		if t.is("OVER") {
			return no("an analytic function (OVER)")
		}
		if t.kind != tokWord || j+1 >= len(s) || !s[j+1].punct("(") {
			continue
		}
		name := strings.ToUpper(t.text)
		if j > 0 && s[j-1].punct(".") {
			return no("a function called through a path (" + t.text + ")")
		}
		switch {
		case aggregationThresholdFunctions[name]:
			if inAgg[j] > 0 {
				return bad("SELECT WITH AGGREGATION_THRESHOLD cannot be used with a multi-level aggregation query")
			}
			aggregates++
		case otherAggregates[name]:
			return bad("Aggregate function " + name + " is not supported in SELECT WITH AGGREGATION_THRESHOLD; use " +
				"APPROX_COUNT_DISTINCT, AVG, COUNT, COUNTIF, LOGICAL_AND, LOGICAL_OR, SUM, COVAR_POP, COVAR_SAMP, " +
				"STDDEV_POP, STDDEV_SAMP, VAR_POP or VAR_SAMP")
		}
	}
	if aggregates == 0 {
		return bad("SELECT WITH AGGREGATION_THRESHOLD must use an aggregate function")
	}
	alias := ""
	if at != nil {
		alias = readAlias(at.tr)
	} else if len(unitParts) > 1 {
		alias = unitParts[0]
	}
	for j := bodyAt; j < from; j++ {
		t := s[j]
		if t.punct("*") && (j == bodyAt || s[j-1].punct(",") || s[j-1].punct(".")) && depth[j] == 0 {
			return bad("SELECT WITH AGGREGATION_THRESHOLD cannot select * : the privacy unit column " + unitName +
				" cannot be projected")
		}
		if t.kind != tokWord && t.kind != tokQuoted || !strings.EqualFold(t.text, unitName) || inAgg[j] > 0 ||
			j > 0 && s[j-1].is("AS") || j+1 < len(s) && s[j+1].punct("(") {
			continue
		}
		if j >= 2 && s[j-1].punct(".") && alias != "" && !strings.EqualFold(s[j-2].text, alias) {
			continue
		}
		return bad("The privacy unit column " + unitName + " cannot be projected in SELECT WITH AGGREGATION_THRESHOLD; " +
			"aggregate it instead")
	}
	cond := "COUNT(DISTINCT " + unitRef + ") >= " + strconv.FormatInt(threshold, 10)
	stmtEnd := s[len(s)-1].end
	var b strings.Builder
	b.WriteString(sql[:s[0].end])
	b.WriteString(" ")
	// With no GROUP BY, one group of every row: the engine takes no HAVING
	// without one, nor GROUP BY () or a literal (measured).
	const oneGroup = " GROUP BY CAST(NULL AS BOOL) "
	if having >= 0 {
		// HAVING (theirs) AND the threshold.
		if having+1 >= end {
			return bad("Syntax error: expected an expression after HAVING")
		}
		if group < 0 {
			b.WriteString(sql[s[bodyAt].pos:s[having-1].end])
			b.WriteString(oneGroup)
			b.WriteString("HAVING")
		} else {
			b.WriteString(sql[s[bodyAt].pos:s[having].end])
		}
		b.WriteString(" (")
		b.WriteString(sql[s[having+1].pos:endPos(s, end, stmtEnd)])
		b.WriteString(") AND ")
	} else {
		b.WriteString(sql[s[bodyAt].pos:endPos(s, end, stmtEnd)])
		if group < 0 {
			b.WriteString(oneGroup)
		}
		b.WriteString(" HAVING ")
	}
	b.WriteString(cond)
	if end < len(s) {
		b.WriteString(" ")
		b.WriteString(sql[s[end].pos:stmtEnd])
	}
	b.WriteString(sql[stmtEnd:])
	return b.String(), "", false
}

// endPos is the end of the text of s before s[end], or stmtEnd.
func endPos(s []token, end, stmtEnd int) int {
	if end < len(s) {
		return s[end-1].end
	}
	return stmtEnd
}

func isAggregateName(t token) bool {
	n := strings.ToUpper(t.text)
	return t.kind == tokWord && (aggregationThresholdFunctions[n] || otherAggregates[n])
}

// implicitAlias reports whether tr has no alias of its own.
func implicitAlias(tr tableRead) bool {
	s, n := tr.stmt, tr.next
	return !(n+1 < len(s) && s[n].is("AS")) && !(n < len(s) && (s[n].kind == tokQuoted || s[n].kind == tokWord &&
		!reservedWords[strings.ToUpper(s[n].text)] && !s[n].is("TABLESAMPLE")))
}

// readAlias is the name a query refers to the read tr by: its alias, else
// its table's name.
func readAlias(tr tableRead) string {
	s, n := tr.stmt, tr.next
	switch {
	case n+1 < len(s) && s[n].is("AS"):
		return s[n+1].text
	case n < len(s) && (s[n].kind == tokQuoted || s[n].kind == tokWord && !reservedWords[strings.ToUpper(s[n].text)] &&
		!s[n].is("TABLESAMPLE")):
		return s[n].text
	}
	return tr.table
}

// checkJoinRestriction checks p's read against its join restriction
// policy: msg is BigQuery's 400, or, when unsupported, a 501's.
func checkJoinRestriction(p privacyRead) (msg string, unsupported bool) {
	s := p.tr.stmt
	depth := privacyDepths(s)
	// The FROM clause of the read, at depth 0.
	from := -1
	for j := p.tr.at - 1; j >= 0; j-- {
		if depth[j] == 0 && s[j].is("FROM") {
			from = j
			break
		}
	}
	if from < 0 {
		return privacyRefusal("a read outside a FROM clause", p.name), true
	}
	end := len(s)
	for j := from + 1; j < len(s); j++ {
		t := s[j]
		if depth[j] == 0 && (t.is("WHERE") || t.is("GROUP") || t.is("HAVING") || t.is("QUALIFY") || t.is("WINDOW") ||
			t.is("ORDER") || t.is("LIMIT") || t.is("UNION") || t.is("INTERSECT") || t.is("EXCEPT")) {
			end = j
			break
		}
	}
	alias := readAlias(p.tr)
	joined := false
	used := map[string]bool{}
	onCols := func(a, b int) {
		for k := a; k < b; k++ {
			t := s[k]
			if t.kind != tokWord && t.kind != tokQuoted || k+1 < b && s[k+1].punct("(") {
				continue
			}
			name := strings.ToLower(t.text)
			switch {
			case k+2 < b && s[k+1].punct(".") && strings.EqualFold(t.text, alias):
				used[strings.ToLower(s[k+2].text)] = true
				k += 2
			case k+1 < b && s[k+1].punct("."):
				k += 2
			case k > 0 && s[k-1].punct("."):
			case p.columns[name]:
				used[name] = true
			}
		}
	}
	for j := from + 1; j < end; j++ {
		if depth[j] != 0 {
			continue
		}
		t := s[j]
		switch {
		case t.punct(","):
			if p.pol.join == "JOIN_BLOCKED" {
				return "Join occurring on a restricted column.", false
			}
			return privacyRefusal("a comma join", p.name), true
		case t.is("LEFT") || t.is("RIGHT") || t.is("FULL") || t.is("CROSS") || t.is("OUTER") || t.is("NATURAL"):
			if p.pol.join == "JOIN_BLOCKED" {
				return "Join occurring on a restricted column.", false
			}
			return privacyRefusal("a "+strings.ToUpper(t.text)+" JOIN", p.name), true
		case t.is("JOIN"):
			joined = true
		case t.is("ON") || t.is("USING"):
			k := j + 1
			for k < end && !(depth[k] == 0 && (s[k].is("JOIN") || s[k].is("INNER") || s[k].is("LEFT") ||
				s[k].is("RIGHT") || s[k].is("FULL") || s[k].is("CROSS") || s[k].punct(","))) {
				k++
			}
			onCols(j+1, k)
			j = k - 1
		}
	}
	allowed := map[string]bool{}
	for _, c := range p.pol.allowed {
		allowed[strings.ToLower(c)] = true
	}
	restricted := false
	for c := range used {
		if !allowed[c] {
			restricted = true
		}
	}
	list := "[" + strings.Join(p.pol.allowed, ", ") + "]"
	switch p.pol.join {
	case "JOIN_BLOCKED":
		if joined {
			return "Join occurring on a restricted column.", false
		}
	case "JOIN_NOT_REQUIRED":
		if !joined && len(p.pol.allowed) == 0 {
			return "At least one allowed column must be specified with join_condition = 'JOIN_NOT_REQUIRED'.", false
		}
		if joined && (restricted || len(used) == 0) {
			return "Join occurring on a restricted column.", false
		}
	case "JOIN_ANY":
		any := false
		for c := range used {
			any = any || allowed[c]
		}
		if !joined || !any {
			return "Joining must occur on at least one of the following columns " + list + " on table " + p.name + ".", false
		}
		if restricted {
			return "Join occurring on a restricted column.", false
		}
	case "JOIN_ALL":
		for c := range allowed {
			if !used[c] || !joined {
				return "Joining must occur on all of the following columns " + list + " on table " + p.name + ".", false
			}
		}
		if restricted {
			return "Join occurring on a restricted column.", false
		}
	}
	return "", false
}

// viewReadsPrivate returns why a read of dataset.table, a view, is refused
// when its query reads a table or view with a privacy policy, directly or
// through other views, or "".
func (f front) viewReadsPrivate(r *http.Request, project, dataset, table string, meta map[string]any, depth int) string {
	q := ""
	if v, _ := meta["view"].(map[string]any); v != nil {
		q = str(v, "query")
	} else if v, _ := meta["materializedView"].(map[string]any); v != nil {
		q = str(v, "query")
	}
	if q == "" {
		return ""
	}
	if t, ok := f.views.get(viewKey(project, dataset, table)); ok && t.engine == q {
		q = t.client
	} else if why := f.engineTextPrivate(r, project, q, dataset+"."+table); why != "" {
		// The emulator's text of a view made by CREATE VIEW names its
		// engine's tables, and has the views it reads inlined (views.go).
		return why
	}
	if depth > 4 {
		return privacyRefusal("a read through views nested this deep", dataset+"."+table)
	}
	toks, ok := lex(q)
	if !ok {
		return ""
	}
	for j := 0; j < len(toks); j++ {
		if toks[j].kind != tokWord && toks[j].kind != tokQuoted {
			continue
		}
		parts, next := path(toks, j)
		if next <= j {
			continue
		}
		if ds, t, named := resolve(parts, project); named && (ds != dataset || t != table) {
			if i := strings.Index(t, "$"); i > 0 {
				t = t[:i]
			}
			if m, ok := f.metaOf(r, ds, t); ok {
				if f.tables.watched(streamKey(project, ds, t)) && f.privacyOf(project, ds, t, m) != "" {
					return privacyRefusal("a read of the view "+dataset+"."+table+", whose query reads it,", ds+"."+t)
				}
				if why := f.viewReadsPrivate(r, project, ds, t, m, depth+1); why != "" {
					return why
				}
			}
		}
		j = next - 1
	}
	return ""
}

// metaOf is dataset.table's resource, ok false when it is not there.
func (f front) metaOf(r *http.Request, dataset, table string) (map[string]any, bool) {
	st, got := f.get(r, tablePath(dataset, table))
	var meta map[string]any
	if st != http.StatusOK || json.Unmarshal(got, &meta) != nil {
		return nil, false
	}
	return meta, true
}

// privateRead returns why a read of dataset.table the front cannot check
// (what: tabledata.list, an extract job, ...) is refused, or "".
func (f front) privateRead(r *http.Request, project, dataset, table, what string) string {
	if !f.tables.watched(streamKey(project, dataset, table)) {
		return ""
	}
	if m, ok := f.metaOf(r, dataset, table); ok && f.privacyOf(project, dataset, table, m) != "" {
		return privacyRefusal(what, dataset+"."+table)
	}
	return ""
}

// schemaColumns are the top-level column names of meta, lower case.
func schemaColumns(meta map[string]any) map[string]bool {
	cols := map[string]bool{}
	sch, _ := meta["schema"].(map[string]any)
	fields, _ := sch["fields"].([]any)
	for _, fl := range fields {
		if m, ok := fl.(map[string]any); ok {
			cols[strings.ToLower(str(m, "name"))] = true
		}
	}
	return cols
}

// engineTextPrivate returns why a read of view, whose query is q as the
// emulator keeps it, is refused when q names the engine table of a table
// with a privacy policy, or holds the emulator's query of a view with one
// (inlined), or "".
func (f front) engineTextPrivate(r *http.Request, project, q, view string) string {
	for _, key := range f.tables.watchedKeys() {
		parts := strings.Split(key, "\x00")
		if len(parts) != 3 || parts[0] != project || parts[1]+"."+parts[2] == view {
			continue
		}
		m, ok := f.metaOf(r, parts[1], parts[2])
		if !ok || f.privacyOf(project, parts[1], parts[2], m) == "" {
			continue
		}
		hit := strings.Contains(q, "`"+project+"_"+parts[1]+"_"+parts[2]+"`")
		if v, _ := m["view"].(map[string]any); !hit && v != nil {
			if inner := strings.TrimSpace(str(v, "query")); len(inner) > 16 && strings.Contains(q, inner) {
				hit = true
			}
		}
		if hit {
			return privacyRefusal("a read of the view "+view+", whose query reads it,", parts[1]+"."+parts[2])
		}
	}
	return ""
}

// watchedKeys are the tables whose reads the front checks.
func (s *tableSettings) watchedKeys() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.watch))
	for k := range s.watch {
		out = append(out, k)
	}
	return out
}
