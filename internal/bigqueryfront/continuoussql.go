package bigqueryfront

import (
	"fmt"
	"strconv"
	"strings"
)

// A continuous query's SQL (#2193): what it may be, and how each run
// reads only the rows that arrived since the last.
//
// https://docs.cloud.google.com/bigquery/docs/continuous-queries-introduction
// (Supported operations, Limitations, read 2026-11-23) allows an INSERT
// into a BigQuery table and an EXPORT DATA to Pub/Sub, Bigtable or
// Spanner, with SQL user-defined functions made by CREATE TEMP FUNCTION
// before it; stateless GoogleSQL functions, JSON functions and
// unnesting; AI.GENERATE, AI.GENERATE_TEXT (and ML.GENERATE_TEXT, its
// earlier name), ML.UNDERSTAND_TEXT, ML.TRANSLATE and ML.NORMALIZER of
// BigQuery ML; APPENDS, and CHANGES when exporting to Pub/Sub. It refuses
// PIVOT, UNPIVOT, TABLESAMPLE, set operators other than UNION ALL, SELECT
// DISTINCT, EXISTS and NOT EXISTS, recursive CTEs, window function calls,
// JavaScript and Python UDFs, remote functions, user-defined aggregate
// functions, any other DDL or DML, DCL, procedural statements; and as
// sources, external tables, INFORMATION_SCHEMA views, wildcard tables,
// materialized views, tables with row-level or column-level security and
// change data capture tables (continuousjob.go checks those, which need
// the tables' metadata).
//
// Each run of the query reads its tables' rows appended in its window,
// [low, high) of the instance's clock: a table the query names is read as
// APPENDS(TABLE t, low, high) without APPENDS' own columns, so the query
// sees the table's columns as it would; an APPENDS or CHANGES the query
// calls itself takes its start for the first window, and keeps its
// pseudo-columns. A view is read as its query, rewritten the same way.
//
// The stateful operations (JOIN, aggregation and windowing), a Preview of
// which BigQuery documents "some ... but not all" of, are not run here:
// each window's rows alone would make them answer differently from
// BigQuery, which keeps their state across windows. They are refused 501
// (gaps.md, #2193).

// cqSource is a table a continuous query reads, at [pos, end) of its text.
type cqSource struct {
	pos, end int
	parts    []string
	// alias is whether an alias follows the name.
	alias bool
	// call is APPENDS or CHANGES when the query calls it; start its
	// start expression ("" for NULL).
	call  string
	start string
	// view is the view's query, rewritten, when the table is a view.
	view *cqQuery
}

// cqQuery is a query whose sources each window rewrites.
type cqQuery struct {
	text    string
	sources []cqSource
}

// cqPlan is a continuous query's statement.
type cqPlan struct {
	// prefix are the CREATE TEMP FUNCTION statements before it.
	prefix string
	// insert is the INSERT's text up to its query, or "" for EXPORT DATA.
	insert string
	// target is the INSERT's table.
	target []string
	query  *cqQuery
	// format, uri and options are EXPORT DATA's.
	format  string
	uri     string
	options map[string]string
	changes bool
}

// cqRefusal is a continuous query the instance does not run: code 400
// for one BigQuery refuses, 501 for one it runs and the instance cannot.
type cqRefusal struct {
	code int
	msg  string
}

func (e *cqRefusal) Error() string { return e.msg }

func cqInvalid(format string, a ...any) *cqRefusal {
	return &cqRefusal{400, "Continuous queries do not support " + fmt.Sprintf(format, a...) +
		" (https://cloud.google.com/bigquery/docs/continuous-queries-introduction#limitations)"}
}

// cqAllowedML are the BigQuery ML and AI functions continuous queries
// support.
var cqAllowedML = map[string]bool{"AI.GENERATE": true, "AI.GENERATE_TEXT": true, "ML.GENERATE_TEXT": true,
	"ML.UNDERSTAND_TEXT": true, "ML.TRANSLATE": true, "ML.NORMALIZER": true}

// cqAggregates are GoogleSQL's aggregate functions, a call of which (with
// no OVER, refused on its own) aggregates.
var cqAggregates = map[string]bool{"COUNT": true, "SUM": true, "AVG": true, "MIN": true, "MAX": true, "ARRAY_AGG": true,
	"STRING_AGG": true, "ANY_VALUE": true, "COUNTIF": true, "LOGICAL_AND": true, "LOGICAL_OR": true, "BIT_AND": true,
	"BIT_OR": true, "BIT_XOR": true, "ARRAY_CONCAT_AGG": true, "APPROX_COUNT_DISTINCT": true, "APPROX_QUANTILES": true,
	"APPROX_TOP_COUNT": true, "APPROX_TOP_SUM": true, "STDDEV": true, "STDDEV_POP": true, "STDDEV_SAMP": true,
	"VARIANCE": true, "VAR_POP": true, "VAR_SAMP": true, "CORR": true, "COVAR_POP": true, "COVAR_SAMP": true,
	"HLL_COUNT.INIT": true, "HLL_COUNT.MERGE": true, "JSON_AGG": true, "MAX_BY": true, "MIN_BY": true, "GROUPING": true}

// parseContinuous reads a continuous query's text.
func parseContinuous(sql string) (*cqPlan, error) {
	toks, ok := lex(sql)
	if !ok {
		return nil, &cqRefusal{400, "Syntax error: the query cannot be read"}
	}
	var stmts [][]token
	for _, s := range splitStatements(toks) {
		if len(s) > 0 {
			stmts = append(stmts, s)
		}
	}
	if len(stmts) == 0 {
		return nil, &cqRefusal{400, "Syntax error: the query is empty"}
	}
	p := &cqPlan{}
	for _, st := range stmts[:len(stmts)-1] {
		if err := checkTempFunction(st); err != nil {
			return nil, err
		}
		p.prefix += sql[st[0].pos:st[len(st)-1].end] + ";\n"
	}
	main := stmts[len(stmts)-1]
	text := sql[main[0].pos:main[len(main)-1].end]
	base := main[0].pos
	switch {
	case main[0].is("INSERT"):
		i := 1
		if i < len(main) && main[i].is("INTO") {
			i++
		}
		parts, next := path(main, i)
		if len(parts) == 0 {
			return nil, &cqRefusal{400, "Syntax error: INSERT names no table"}
		}
		p.target = parts
		if next < len(main) && main[next].punct("(") {
			depth := 0
			for ; next < len(main); next++ {
				if main[next].punct("(") {
					depth++
				} else if main[next].punct(")") {
					depth--
					if depth == 0 {
						next++
						break
					}
				}
			}
		}
		if next >= len(main) || !(main[next].is("SELECT") || main[next].is("WITH") || main[next].punct("(")) {
			return nil, cqInvalid("INSERT … VALUES: a continuous INSERT inserts the rows of a query")
		}
		p.insert = sql[main[0].pos:main[next].pos]
		q, err := parseCQQuery(sql[main[next].pos:main[len(main)-1].end])
		if err != nil {
			return nil, err
		}
		p.query = q
	case main[0].is("EXPORT") && len(main) > 1 && main[1].is("DATA"):
		i := 2
		if i+2 < len(main) && main[i].is("WITH") && main[i+1].is("CONNECTION") {
			_, next := path(main, i+2)
			i = next
		}
		if i >= len(main) || !main[i].is("OPTIONS") || i+1 >= len(main) || !main[i+1].punct("(") {
			return nil, &cqRefusal{400, "Syntax error: EXPORT DATA needs OPTIONS(format = …, uri = …)"}
		}
		opts, next, err := cqOptions(main, i+2)
		if err != nil {
			return nil, err
		}
		p.options = opts
		p.format, p.uri = strings.ToUpper(opts["format"]), opts["uri"]
		if next >= len(main) || !main[next].is("AS") || next+1 >= len(main) {
			return nil, &cqRefusal{400, "Syntax error: EXPORT DATA … OPTIONS(…) is followed by AS and its query"}
		}
		qt := sql[main[next+1].pos:main[len(main)-1].end]
		qt = strings.TrimSpace(qt)
		for strings.HasPrefix(qt, "(") && strings.HasSuffix(qt, ")") && balanced(qt[1:len(qt)-1]) {
			qt = strings.TrimSpace(qt[1 : len(qt)-1])
		}
		q, err := parseCQQuery(qt)
		if err != nil {
			return nil, err
		}
		p.query = q
		switch p.format {
		case "CLOUD_PUBSUB", "CLOUD_BIGTABLE", "CLOUD_SPANNER":
		default:
			return nil, cqInvalid("EXPORT DATA with format %q: a continuous query exports to Pub/Sub (CLOUD_PUBSUB), Bigtable (CLOUD_BIGTABLE) or Spanner (CLOUD_SPANNER)", opts["format"])
		}
		for _, s := range q.sources {
			if s.call == "CHANGES" && p.format != "CLOUD_PUBSUB" {
				return nil, cqInvalid("the CHANGES function when exporting to %s: CHANGES is supported only when exporting to Pub/Sub", p.format)
			}
		}
	default:
		first := strings.ToUpper(main[0].text)
		if main[0].is("CREATE") || main[0].is("ALTER") || main[0].is("DROP") {
			return nil, cqInvalid("DDL statements other than CREATE TEMP FUNCTION")
		}
		if main[0].is("UPDATE") || main[0].is("DELETE") || main[0].is("MERGE") || main[0].is("TRUNCATE") {
			return nil, cqInvalid("DML statements other than INSERT")
		}
		if main[0].is("GRANT") || main[0].is("REVOKE") {
			return nil, cqInvalid("DCL statements")
		}
		if main[0].is("SELECT") || main[0].is("WITH") || main[0].punct("(") {
			return nil, cqInvalid("a query whose results are not written: a continuous query is an INSERT or an EXPORT DATA statement")
		}
		return nil, cqInvalid("the %s statement: a continuous query is an INSERT or an EXPORT DATA statement", first)
	}
	_ = base
	_ = text
	for _, s := range p.query.sources {
		if s.call == "CHANGES" {
			p.changes = true
		}
	}
	return p, nil
}

func balanced(s string) bool {
	d := 0
	for _, c := range s {
		switch c {
		case '(':
			d++
		case ')':
			d--
			if d < 0 {
				return false
			}
		}
	}
	return d == 0
}

// checkTempFunction checks a statement before the continuous one: a
// CREATE TEMP FUNCTION of a SQL UDF.
func checkTempFunction(st []token) error {
	i := 0
	if !st[i].is("CREATE") {
		return cqInvalid("several statements: a continuous query is one INSERT or EXPORT DATA statement, after CREATE TEMP FUNCTION statements")
	}
	i++
	if i+1 < len(st) && st[i].is("OR") && st[i+1].is("REPLACE") {
		i += 2
	}
	if i >= len(st) || !st[i].is("TEMP") && !st[i].is("TEMPORARY") {
		return cqInvalid("DDL statements other than CREATE TEMP FUNCTION")
	}
	i++
	if i < len(st) && st[i].is("AGGREGATE") {
		return cqInvalid("user-defined aggregate functions")
	}
	if i < len(st) && st[i].is("TABLE") {
		return cqInvalid("DDL statements other than CREATE TEMP FUNCTION")
	}
	if i >= len(st) || !st[i].is("FUNCTION") {
		return cqInvalid("DDL statements other than CREATE TEMP FUNCTION")
	}
	for _, t := range st {
		if t.is("LANGUAGE") {
			return cqInvalid("JavaScript or Python user-defined functions: only SQL UDFs are supported")
		}
		if t.is("REMOTE") {
			return cqInvalid("remote functions")
		}
	}
	return nil
}

// readOptions reads OPTIONS(name = value, …) from t[i], after its "(":
// string, boolean and number values, and returns the index after ")".
func cqOptions(t []token, i int) (map[string]string, int, error) {
	out := map[string]string{}
	for i < len(t) {
		if t[i].punct(")") {
			return out, i + 1, nil
		}
		if i+2 >= len(t) || t[i].kind != tokWord || !t[i+1].punct("=") {
			return nil, 0, &cqRefusal{400, "Syntax error: OPTIONS takes name = value pairs"}
		}
		name := strings.ToLower(t[i].text)
		if _, dup := out[name]; dup {
			return nil, 0, &cqRefusal{400, "Duplicate option " + name}
		}
		v := t[i+2]
		switch {
		case v.kind == tokString:
			s, ok := stringLiteral(v)
			if !ok {
				return nil, 0, &cqRefusal{400, "Syntax error: OPTIONS " + name + " is not a string"}
			}
			out[name] = s
		case v.kind == tokWord:
			out[name] = v.text
		default:
			return nil, 0, &cqRefusal{400, "Syntax error: OPTIONS " + name + " has no value"}
		}
		i += 3
		if i < len(t) && t[i].punct(",") {
			i++
		}
	}
	return nil, 0, &cqRefusal{400, "Syntax error: OPTIONS is not closed"}
}

// parseCQQuery reads a continuous query's query: its sources, and the
// refusals of what continuous queries do not support.
func parseCQQuery(q string) (*cqQuery, error) {
	toks, ok := lex(q)
	if !ok {
		return nil, &cqRefusal{400, "Syntax error: the query cannot be read"}
	}
	ctes := map[string]bool{}
	for i := 0; i+2 < len(toks); i++ {
		if (toks[i].kind == tokWord || toks[i].kind == tokQuoted) && toks[i+1].is("AS") && toks[i+2].punct("(") &&
			(i == 0 || !toks[i-1].is("CAST") && !toks[i-1].is("SAFE_CAST") && !toks[i-1].punct("(")) {
			ctes[strings.ToLower(toks[i].text)] = true
		}
	}
	for i, t := range toks {
		next := func(k int) token {
			if i+k < len(toks) {
				return toks[i+k]
			}
			return token{}
		}
		switch {
		case t.is("PIVOT") && next(1).punct("("), t.is("UNPIVOT") && (next(1).punct("(") || next(1).is("INCLUDE") || next(1).is("EXCLUDE")):
			return nil, cqInvalid("the %s operator", strings.ToUpper(t.text))
		case t.is("TABLESAMPLE"):
			return nil, cqInvalid("the TABLESAMPLE operator")
		case t.is("UNION") && !next(1).is("ALL"):
			return nil, cqInvalid("set operators other than UNION ALL")
		case t.is("INTERSECT"), t.is("EXCEPT") && (next(1).is("DISTINCT") || next(1).is("ALL")):
			return nil, cqInvalid("set operators other than UNION ALL")
		case t.is("SELECT") && next(1).is("DISTINCT"):
			return nil, cqInvalid("SELECT DISTINCT")
		case t.is("EXISTS") && next(1).punct("("):
			return nil, cqInvalid("EXISTS or NOT EXISTS subqueries")
		case t.is("WITH") && next(1).is("RECURSIVE"):
			return nil, cqInvalid("recursive CTEs")
		case t.is("OVER") && i > 0 && (next(1).punct("(") || next(1).kind == tokWord):
			return nil, cqInvalid("window function calls")
		case t.is("INFORMATION_SCHEMA"), t.kind == tokQuoted && strings.Contains(strings.ToUpper(t.text), "INFORMATION_SCHEMA"):
			return nil, cqInvalid("INFORMATION_SCHEMA views as sources")
		case t.is("JOIN"):
			return nil, &cqRefusal{501, "Not implemented here: JOINs in a continuous query, a stateful operation (Preview) BigQuery runs " +
				"with state kept across the rows as they arrive; this instance runs stateless continuous queries only (#2193)"}
		case t.is("GROUP") && next(1).is("BY"):
			return nil, &cqRefusal{501, "Not implemented here: aggregation in a continuous query, a stateful operation (Preview) BigQuery runs " +
				"with state kept across the rows as they arrive; this instance runs stateless continuous queries only (#2193)"}
		case (t.kind == tokWord) && next(1).punct("("):
			name := strings.ToUpper(t.text)
			if i >= 2 && toks[i-1].punct(".") {
				name = strings.ToUpper(toks[i-2].text) + "." + name
			}
			if (strings.HasPrefix(name, "ML.") || strings.HasPrefix(name, "AI.")) && !cqAllowedML[name] {
				return nil, cqInvalid("the BigQuery ML function %s", name)
			}
			if i > 0 && toks[i-1].punct(".") && name != "HLL_COUNT.INIT" && name != "HLL_COUNT.MERGE" {
				break
			}
			if cqAggregates[name] {
				return nil, &cqRefusal{501, "Not implemented here: the aggregate function " + name + " in a continuous query, a stateful " +
					"operation (Preview) BigQuery runs with state kept across the rows as they arrive; this instance runs stateless continuous queries only (#2193)"}
			}
			if name == "TUMBLE" || name == "HOP" || name == "SESSION" {
				return nil, &cqRefusal{501, "Not implemented here: windowing (" + name + ") in a continuous query, a stateful operation (Preview); " +
					"this instance runs stateless continuous queries only (#2193)"}
			}
		}
	}
	calls, ok := changeFuncs(q, "", "")
	if !ok {
		return nil, &cqRefusal{400, "Syntax error: an APPENDS or CHANGES call cannot be read"}
	}
	out := &cqQuery{text: q}
	inCall := func(pos int) bool {
		for _, c := range calls {
			if pos >= c.pos && pos < c.end {
				return true
			}
		}
		return false
	}
	for _, c := range calls {
		if c.stop != "" && !strings.EqualFold(strings.TrimSpace(c.stop), "NULL") {
			return nil, cqInvalid("an end_timestamp argument to %s: a continuous query reads the rows as they arrive", c.name)
		}
		start := strings.TrimSpace(c.start)
		if strings.EqualFold(start, "NULL") {
			start = ""
		}
		// The table's path, as the call names it.
		ct, _ := lex(q[c.pos:c.end])
		parts, _ := path(ct, 3)
		out.sources = append(out.sources, cqSource{pos: c.pos, end: c.end, parts: parts, call: c.name, start: start})
	}
	fromAt := map[int]bool{}
	depth := 0
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		addAt := func(j int) error {
			if j >= len(toks) || toks[j].kind != tokWord && toks[j].kind != tokQuoted || inCall(toks[j].pos) {
				return nil
			}
			if toks[j].kind == tokWord && reservedWords[strings.ToUpper(toks[j].text)] {
				return nil // UNNEST, SELECT, …
			}
			parts, nx := path(toks, j)
			if len(parts) == 0 || nx < len(toks) && toks[nx].punct("(") {
				return nil // a table-valued function
			}
			if len(parts) == 1 && ctes[strings.ToLower(parts[0])] {
				return nil
			}
			last := parts[len(parts)-1]
			if strings.HasSuffix(last, "*") || nx < len(toks) && toks[nx].punct("*") {
				return cqInvalid("wildcard tables as sources")
			}
			alias := nx < len(toks) && (toks[nx].is("AS") || toks[nx].kind == tokQuoted ||
				toks[nx].kind == tokWord && !reservedWords[strings.ToUpper(toks[nx].text)] && !aliasBefore[strings.ToUpper(toks[nx].text)])
			if nx < len(toks) && toks[nx].is("FOR") {
				return cqInvalid("FOR SYSTEM_TIME AS OF on a source: a continuous query reads the rows as they arrive")
			}
			out.sources = append(out.sources, cqSource{pos: toks[j].pos, end: toks[nx-1].end, parts: parts, alias: alias})
			return nil
		}
		var err error
		switch {
		case t.punct("(") || t.punct("["):
			depth++
		case t.punct(")") || t.punct("]"):
			delete(fromAt, depth)
			depth--
		case t.is("FROM") && i >= 2 && toks[i-1].is("DISTINCT") && (toks[i-2].is("IS") || toks[i-2].is("NOT")):
		case t.is("FROM") && i >= 2 && toks[i-2].is("EXTRACT"):
		case t.is("FROM") && i >= 1 && depth > 0 && isExtractArg(toks, i):
		case t.is("FROM"):
			fromAt[depth] = true
			err = addAt(i + 1)
		case t.punct(",") && fromAt[depth]:
			if i+1 < len(toks) && toks[i+1].is("UNNEST") {
				break
			}
			// A comma between two tables is a cross join.
			if j := i + 1; j < len(toks) && (toks[j].kind == tokWord || toks[j].kind == tokQuoted) && !reservedWords[strings.ToUpper(toks[j].text)] {
				parts, _ := path(toks, j)
				if len(parts) > 1 || len(parts) == 1 && !ctes[strings.ToLower(parts[0])] {
					// An alias.array path (t.arr) is an implicit UNNEST.
					if len(parts) == 2 && aliasedIn(out.sources, toks, parts[0]) {
						break
					}
				}
			}
			return nil, &cqRefusal{501, "Not implemented here: JOINs in a continuous query (a comma join), a stateful operation (Preview); " +
				"this instance runs stateless continuous queries only (#2193)"}
		case t.is("WHERE") || t.is("GROUP") || t.is("HAVING") || t.is("QUALIFY") || t.is("WINDOW") || t.is("ORDER") ||
			t.is("LIMIT") || t.is("UNION") || t.is("ON") || t.is("USING") || t.is("SELECT"):
			delete(fromAt, depth)
		}
		if err != nil {
			return nil, err
		}
	}
	sortSources(out.sources)
	return out, nil
}

// isExtractArg reports whether the FROM at t[i] is inside EXTRACT(… FROM …).
func isExtractArg(t []token, i int) bool {
	depth := 0
	for k := i - 1; k >= 0; k-- {
		switch {
		case t[k].punct(")"):
			depth++
		case t[k].punct("("):
			if depth == 0 {
				return k > 0 && t[k-1].is("EXTRACT")
			}
			depth--
		}
	}
	return false
}

// aliasedIn reports whether name is the alias (or last name) of a source
// already read.
func aliasedIn(sources []cqSource, toks []token, name string) bool {
	for _, s := range sources {
		if strings.EqualFold(s.parts[len(s.parts)-1], name) {
			return true
		}
		for k, t := range toks {
			if t.pos == s.end || k > 0 && toks[k-1].end == s.end {
				if t.is("AS") && k+1 < len(toks) && strings.EqualFold(toks[k+1].text, name) || strings.EqualFold(t.text, name) {
					return true
				}
			}
		}
	}
	return false
}

func sortSources(s []cqSource) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].pos < s[j-1].pos; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// cqWindow is one run's window of the instance's clock, in milliseconds:
// [low, high); first marks the job's first run, where a call's own start
// stands.
type cqWindow struct {
	low, high int64
	first     bool
}

// render writes q for window w: each source read as its rows appended in
// the window (above). dataset is the default dataset.
func (q *cqQuery) render(w cqWindow, dataset string) string {
	var b strings.Builder
	last := 0
	ts := func(ms int64) string { return "TIMESTAMP_MILLIS(" + strconv.FormatInt(ms, 10) + ")" }
	for _, s := range q.sources {
		if s.pos < last {
			continue
		}
		b.WriteString(q.text[last:s.pos])
		parts := s.parts
		if len(parts) == 1 && dataset != "" {
			parts = []string{dataset, parts[0]}
		}
		name := quotePath(parts)
		switch {
		case s.call != "":
			low := ts(w.low)
			if w.first {
				low = "NULL"
				if s.start != "" {
					low = "(" + s.start + ")"
				}
			}
			fmt.Fprintf(&b, "%s(TABLE %s, %s, %s)", s.call, name, low, ts(w.high))
		case s.view != nil:
			b.WriteString("(" + s.view.render(w, dataset) + ")")
			if !s.alias {
				b.WriteString(" AS " + quotePath([]string{s.parts[len(s.parts)-1]}))
			}
		default:
			fmt.Fprintf(&b, "(SELECT * EXCEPT (_CHANGE_TYPE, _CHANGE_TIMESTAMP) FROM APPENDS(TABLE %s, %s, %s))", name, ts(w.low), ts(w.high))
			if !s.alias {
				b.WriteString(" AS " + quotePath([]string{s.parts[len(s.parts)-1]}))
			}
		}
		last = s.end
	}
	b.WriteString(q.text[last:])
	return b.String()
}

// statement is the plan's statement for window w.
func (p *cqPlan) statement(w cqWindow, dataset string) string {
	return p.prefix + p.insert + p.query.render(w, dataset)
}
