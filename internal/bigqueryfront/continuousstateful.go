package bigqueryfront

import (
	"fmt"
	"strconv"
	"strings"
)

// Stateful continuous queries (#2193): stream-to-stream JOINs, window
// aggregations and the TUMBLE windowing TVF, a Preview BigQuery documents
// in https://docs.cloud.google.com/bigquery/docs/continuous-queries-introduction#supported_stateful_operations,
// https://docs.cloud.google.com/bigquery/docs/continuous-query-joins and
// https://docs.cloud.google.com/bigquery/docs/window-aggregations (read
// 2026-11-23).
//
// What BigQuery documents, and what is checked when the job is inserted:
//
//   - JOINs: only INNER JOIN between streams ("Stream-to-static-table
//     JOINs", other forms than INNER and "JOINs that don't have
//     time-oriented JOIN clauses" are unsupported); "Each side of the
//     INNER JOIN operation must specify a starting time", so each side is
//     an APPENDS call with a start (or a subquery or CTE whose every source
//     is one); the JOIN's condition (its ON, or the WHERE of its SELECT, as
//     Google's own example has it) restricts the sides' _CHANGE_TIMESTAMP
//     to a constant interval, in either direction, as BETWEEN, comparisons
//     with INTERVAL offsets, TIMESTAMP_ADD/SUB or TIMESTAMP_DIFF; "The
//     BigQuery system time column, defined by _CHANGE_TIMESTAMP, is the
//     only supported timestamp column for join operations"; CHANGES is not
//     supported.
//   - TUMBLE(TABLE table, "timestamp_column", window_size): table is a
//     table read through APPENDS or a CTE; timestamp_column a STRING
//     literal naming _CHANGE_TIMESTAMP, or a CTE's column defined as it
//     (Google's examples alias it bq_changed_ts); window_size an INTERVAL
//     of at most 24 hours. Its rows are the input's with window_start
//     (inclusive) and window_end (exclusive); windows are aligned to the
//     Unix epoch, as the documented [12:00, 12:05) windows are.
//   - Aggregation is window aggregation: a SELECT that aggregates reads a
//     TUMBLE and groups by its window_start or window_end ("You must
//     include at least one of these columns in the GROUP BY"); only the
//     documented aggregate functions, ARRAY_AGG with a LIMIT of at most
//     10,000 and STRING_AGG with one of at most 100; none with DISTINCT,
//     GROUPING, ST_CENTROID_AGG, ST_EXTENT, ST_UNION_AGG or the
//     differential privacy aggregates.
//   - HOP and SESSION: BigQuery documents no other windowing TVF than
//     TUMBLE, so they are what any unknown table-valued function is.
//
// How each run computes them, the state kept being the source tables'
// own change history (the engine keeps seven days of it, more than the
// 24-hour windows and the JOIN's interval reach back):
//
//   - The watermark is the latest _CHANGE_TIMESTAMP the job has read from
//     its sources. "A windowed aggregation doesn't produce output for a
//     specific time interval until BigQuery finalizes or closes that
//     window": the results for [10:15, 10:20) "are only emitted after the
//     query processes records with a _CHANGE_TIMESTAMP of 10:20 or later".
//     A run whose watermark closes windows writes those windows' rows,
//     each window once; rows of a window already closed (late data) are
//     left out. A run that closes none writes nothing.
//   - A JOIN with no window after it writes each matched pair once, in the
//     run that reads the later of its rows: the run reads its sources back
//     by the JOIN's interval, and keeps the pairs whose later row is new.
//
// UNVERIFIED (docs re-read 2026-11-23: the three pages above): the
// refusals' messages and codes (Google gives none); the
// watermark as the latest row read rather than a lower bound of the
// sources' progress (Google documents only the closing rule above).

// cqTumble is a TUMBLE call in a continuous query.
type cqTumble struct {
	pos, end int // the call, in the query's text
	// cte is the input CTE's name as written, or "" for a table, which is
	// the source with tumbleInput inside the call.
	cte    string
	column string
	sizeUs int64
}

// cqJoinScope is a SELECT that joins streams.
type cqJoinScope struct {
	sides []string // the stream sides' aliases
	// where is the WHERE's position, -1 for none; whereEnd the end of its
	// condition; fromEnd the end of the FROM clause.
	where, whereEnd, fromEnd int
}

// cqTumbleAlias names a TUMBLE's input in the engine's statement.
const cqTumbleAlias = "_cloudburrow_tumble"

// cqStatefulAggregates are the aggregate functions window aggregations
// support.
var cqStatefulAggregates = map[string]bool{"ANY_VALUE": true, "APPROX_COUNT_DISTINCT": true, "APPROX_QUANTILES": true,
	"APPROX_TOP_COUNT": true, "APPROX_TOP_SUM": true, "ARRAY_AGG": true, "ARRAY_CONCAT_AGG": true, "AVG": true,
	"BIT_AND": true, "BIT_OR": true, "BIT_XOR": true, "CORR": true, "COUNT": true, "COUNTIF": true, "COVAR_POP": true,
	"COVAR_SAMP": true, "LOGICAL_AND": true, "LOGICAL_OR": true, "MAX": true, "MAX_BY": true, "MIN": true, "MIN_BY": true,
	"STDDEV": true, "STDDEV_POP": true, "STDDEV_SAMP": true, "STRING_AGG": true, "SUM": true, "VAR_POP": true,
	"VAR_SAMP": true, "VARIANCE": true}

// cqAggLimit are the aggregates that need a LIMIT, and its maximum.
var cqAggLimit = map[string]int64{"ARRAY_AGG": 10000, "STRING_AGG": 100}

// cqUnits are INTERVAL's parts a window or a JOIN's interval can use, in
// microseconds.
var cqUnits = map[string]int64{"MICROSECOND": 1, "MILLISECOND": 1000, "SECOND": 1e6, "MINUTE": 60e6, "HOUR": 3600e6,
	"DAY": 86400e6, "WEEK": 7 * 86400e6}

const cqMaxWindowUs = 24 * 3600e6

// cqBlocks gives each token the SELECT its clause belongs to (the token
// index of that SELECT, -1 for none) and its bracket depth.
func cqBlocks(toks []token) (blk, depth []int) {
	type frame struct{ sel int }
	stack := []frame{{sel: -1}}
	blk, depth = make([]int, len(toks)), make([]int, len(toks))
	for i, t := range toks {
		cur := &stack[len(stack)-1]
		switch {
		case t.punct("(") || t.punct("["):
			blk[i], depth[i] = cur.sel, len(stack)-1
			f := frame{sel: cur.sel}
			if i+1 < len(toks) && (toks[i+1].is("SELECT") || toks[i+1].is("WITH")) {
				f.sel = -1
			}
			stack = append(stack, f)
			continue
		case t.punct(")") || t.punct("]"):
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			cur = &stack[len(stack)-1]
			blk[i], depth[i] = cur.sel, len(stack)-1
			continue
		case t.is("SELECT"):
			cur.sel = i
		}
		blk[i], depth[i] = cur.sel, len(stack)-1
	}
	return blk, depth
}

// matchClose returns the index of the bracket closing toks[i].
func matchClose(toks []token, i int) int {
	d := 0
	for j := i; j < len(toks); j++ {
		switch {
		case toks[j].punct("(") || toks[j].punct("["):
			d++
		case toks[j].punct(")") || toks[j].punct("]"):
			d--
			if d == 0 {
				return j
			}
		}
	}
	return len(toks) - 1
}

// cqClauseWords end a FROM or WHERE clause.
var cqClauseWords = map[string]bool{"GROUP": true, "HAVING": true, "QUALIFY": true, "WINDOW": true, "ORDER": true,
	"LIMIT": true, "UNION": true, "INTERSECT": true, "EXCEPT": true}

// clauseEnd is the token index where the clause of block b, from token i,
// ends: at the next of words, at a bracket closing the block, or at
// another block.
func clauseEnd(toks []token, blk, depth []int, b, i int, words map[string]bool) int {
	d := depth[b]
	for j := i + 1; j < len(toks); j++ {
		if depth[j] < d || depth[j] == d && blk[j] != b {
			return j
		}
		if depth[j] == d && toks[j].kind == tokWord && words[strings.ToUpper(toks[j].text)] {
			return j
		}
	}
	return len(toks)
}

func posOf(toks []token, j int, text string) int {
	if j < len(toks) {
		return toks[j].pos
	}
	return len(text)
}

// cteBodies are a query's CTEs: name (lower case) to the token indexes
// of its body's brackets.
func cteBodies(toks []token) map[string][2]int {
	out := map[string][2]int{}
	for i := 0; i+2 < len(toks); i++ {
		if (toks[i].kind == tokWord || toks[i].kind == tokQuoted) && toks[i+1].is("AS") && toks[i+2].punct("(") &&
			(i == 0 || !toks[i-1].is("CAST") && !toks[i-1].is("SAFE_CAST") && !toks[i-1].punct("(")) &&
			(i == 0 || toks[i-1].is("WITH") || toks[i-1].punct(",") || toks[i-1].is("RECURSIVE")) {
			out[strings.ToLower(toks[i].text)] = [2]int{i + 2, matchClose(toks, i+2)}
		}
	}
	return out
}

// analyzeStateful checks q's stateful operations as BigQuery documents
// them and records what each run needs to compute them.
func analyzeStateful(out *cqQuery, toks []token) error {
	blk, depth := cqBlocks(toks)
	ctes := cteBodies(toks)
	for i, t := range toks {
		if t.is("WITH") && i+1 < len(toks) && toks[i+1].is("DIFFERENTIAL_PRIVACY") {
			return cqInvalid("the differential privacy aggregate functions")
		}
	}
	// TUMBLE calls.
	for i := 0; i+1 < len(toks); i++ {
		if !toks[i].is("TUMBLE") || !toks[i+1].punct("(") || i > 0 && toks[i-1].punct(".") {
			continue
		}
		tb, err := parseTumble(out, toks, i, ctes)
		if err != nil {
			return err
		}
		out.tumbles = append(out.tumbles, tb)
	}
	stateful := len(out.tumbles) > 0
	// Per SELECT: its joins and its aggregation.
	seen := map[int]bool{}
	for i := range toks {
		b := blk[i]
		if b < 0 || seen[b] {
			continue
		}
		seen[b] = true
		js, isJoin, err := joinScope(out, toks, blk, depth, b, ctes)
		if err != nil {
			return err
		}
		if isJoin {
			out.joins = append(out.joins, js)
			stateful = true
		}
		agg, err := checkAggregation(out, toks, blk, depth, b)
		if err != nil {
			return err
		}
		if agg {
			out.aggregates = true
			stateful = true
		}
	}
	if stateful {
		for _, s := range out.sources {
			if s.call == "CHANGES" {
				return cqInvalid("the CHANGES function with a stateful operation (a JOIN, an aggregation or a windowing TVF): use APPENDS")
			}
		}
	}
	return nil
}

// parseTumble reads TUMBLE(TABLE table, "timestamp_column", window_size)
// at toks[i].
func parseTumble(out *cqQuery, toks []token, i int, ctes map[string][2]int) (cqTumble, error) {
	end := matchClose(toks, i+1)
	tb := cqTumble{pos: toks[i].pos, end: toks[end].end}
	args := splitArgs(toks[i+2 : end])
	if len(args) != 3 {
		return tb, &cqRefusal{400, "No matching signature for function TUMBLE: TUMBLE(TABLE table, timestamp_column STRING, window_size INTERVAL)"}
	}
	a := args[0]
	if len(a) < 2 || !a[0].is("TABLE") {
		return tb, &cqRefusal{400, "TUMBLE's first argument is a table: TUMBLE(TABLE table, timestamp_column, window_size)"}
	}
	parts, next := path(a, 1)
	if len(parts) == 0 || next != len(a) {
		return tb, &cqRefusal{400, "TUMBLE's first argument is a table: TUMBLE(TABLE table, timestamp_column, window_size)"}
	}
	if len(args[1]) != 1 || args[1][0].kind != tokString {
		return tb, &cqRefusal{400, "TUMBLE's timestamp_column is a STRING literal naming the input's timestamp column"}
	}
	col, _ := stringLiteral(args[1][0])
	tb.column = col
	size, ok := intervalUs(args[2])
	if !ok {
		return tb, &cqRefusal{400, "TUMBLE's window_size is an INTERVAL literal, such as INTERVAL 30 SECOND"}
	}
	if size <= 0 {
		return tb, &cqRefusal{400, "TUMBLE's window_size must be positive"}
	}
	if size > cqMaxWindowUs {
		return tb, cqInvalid("a window size of more than 24 hours: TUMBLE's window sizes can be a maximum of 24 hours")
	}
	tb.sizeUs = size
	userCol := cqInvalid("the timestamp_column %q: the _CHANGE_TIMESTAMP column, which defines the BigQuery system time, "+
		"is the only supported timestamp_column; user-defined columns aren't supported", col)
	if body, isCTE := ctes[strings.ToLower(parts[0])]; len(parts) == 1 && isCTE {
		tb.cte = a[1].text
		if a[1].kind == tokQuoted {
			tb.cte = quotePath([]string{a[1].text})
		}
		if !cteHasChangeTimestamp(toks[body[0]+1:body[1]], col) {
			return tb, userCol
		}
		return tb, nil
	}
	if !strings.EqualFold(col, "_CHANGE_TIMESTAMP") {
		return tb, userCol
	}
	out.sources = append(out.sources, cqSource{pos: a[1].pos, end: a[len(a)-1].end, parts: parts, tumbleInput: true})
	return tb, nil
}

// cteHasChangeTimestamp reports whether a CTE's body gives col as the
// system time: _CHANGE_TIMESTAMP AS col, or _CHANGE_TIMESTAMP itself.
func cteHasChangeTimestamp(body []token, col string) bool {
	for k := 0; k < len(body); k++ {
		t := body[k]
		if (t.kind == tokWord || t.kind == tokQuoted) && strings.EqualFold(t.text, "_CHANGE_TIMESTAMP") {
			if k+2 < len(body) && body[k+1].is("AS") && strings.EqualFold(body[k+2].text, col) {
				return true
			}
			if strings.EqualFold(col, "_CHANGE_TIMESTAMP") {
				return true
			}
		}
		if strings.EqualFold(col, "_CHANGE_TIMESTAMP") && t.punct("*") && k > 0 && (body[k-1].is("SELECT") || body[k-1].punct(".")) {
			return true
		}
	}
	return false
}

// splitArgs splits a call's arguments at its top-level commas.
func splitArgs(t []token) [][]token {
	var out [][]token
	d, start := 0, 0
	for k, tk := range t {
		switch {
		case tk.punct("(") || tk.punct("[") || tk.punct("<"):
			d++
		case tk.punct(")") || tk.punct("]") || tk.punct(">"):
			d--
		case tk.punct(",") && d == 0:
			out = append(out, t[start:k])
			start = k + 1
		}
	}
	if len(t) > 0 {
		out = append(out, t[start:])
	}
	return out
}

// intervalUs reads INTERVAL [-]n UNIT, in microseconds.
func intervalUs(t []token) (int64, bool) {
	if len(t) < 3 || !t[0].is("INTERVAL") {
		return 0, false
	}
	k, sign := 1, int64(1)
	if t[k].punct("-") {
		sign, k = -1, k+1
	}
	if k+1 >= len(t) || k+2 != len(t) {
		return 0, false
	}
	num := t[k].text
	if t[k].kind == tokString {
		s, ok := stringLiteral(t[k])
		if !ok {
			return 0, false
		}
		num = strings.TrimSpace(s)
	} else if t[k].kind != tokWord {
		return 0, false
	}
	n, err := strconv.ParseInt(num, 10, 64)
	unit := cqUnits[strings.ToUpper(t[k+1].text)]
	if err != nil || unit == 0 {
		return 0, false
	}
	return sign * n * unit, true
}

// cqSide is one FROM item of a SELECT that joins.
type cqSide struct {
	from, to int // its tokens
	alias    string
	unnest   bool
}

// joinScope reads the joins of block b: whether it joins streams, and
// refuses what BigQuery does not support.
func joinScope(out *cqQuery, toks []token, blk, depth []int, b int, ctes map[string][2]int) (cqJoinScope, bool, error) {
	js := cqJoinScope{where: -1}
	d := depth[b]
	at := func(j int) bool { return j < len(toks) && blk[j] == b && depth[j] == d }
	from := -1
	for j := b + 1; j < len(toks); j++ {
		if depth[j] < d || depth[j] == d && blk[j] != b {
			break
		}
		if at(j) && toks[j].is("FROM") && !isExtractArg(toks, j) {
			from = j
			break
		}
	}
	if from < 0 {
		return js, false, nil
	}
	fromEnd := clauseEnd(toks, blk, depth, b, from, map[string]bool{"WHERE": true, "GROUP": true, "HAVING": true,
		"QUALIFY": true, "WINDOW": true, "ORDER": true, "LIMIT": true, "UNION": true, "INTERSECT": true, "EXCEPT": true})
	joins := false
	for j := from + 1; j < fromEnd; j++ {
		if at(j) && (toks[j].is("JOIN") || toks[j].punct(",")) {
			joins = true
		}
	}
	if !joins {
		return js, false, nil
	}
	// The FROM items.
	var sides []cqSide
	var conds [][]token
	j := from + 1
	for j < fromEnd {
		// A join's keywords before its item.
		kind := ""
		for j < fromEnd && at(j) && (toks[j].is("INNER") || toks[j].is("LEFT") || toks[j].is("RIGHT") || toks[j].is("FULL") ||
			toks[j].is("CROSS") || toks[j].is("OUTER") || toks[j].is("NATURAL") || toks[j].is("JOIN") || toks[j].punct(",")) {
			if !toks[j].is("JOIN") {
				kind += strings.ToUpper(toks[j].text) + " "
			} else {
				kind += "JOIN"
			}
			j++
		}
		if len(sides) > 0 && kind == "" {
			break
		}
		start := j
		if j >= fromEnd {
			break
		}
		unnest := false
		switch {
		case toks[j].punct("("):
			j = matchClose(toks, j) + 1
		case toks[j].is("UNNEST") && j+1 < len(toks) && toks[j+1].punct("("):
			unnest = true
			j = matchClose(toks, j+1) + 1
		default:
			_, nx := path(toks, j)
			if nx == j {
				nx = j + 1
			}
			j = nx
			if j < len(toks) && toks[j].punct("(") {
				j = matchClose(toks, j) + 1
			}
		}
		item := toks[start:j]
		alias := ""
		if j < fromEnd && toks[j].is("AS") && j+1 < fromEnd {
			alias, j = toks[j+1].text, j+2
		} else if j < fromEnd && (toks[j].kind == tokQuoted || toks[j].kind == tokWord && !reservedWords[strings.ToUpper(toks[j].text)] &&
			!aliasBefore[strings.ToUpper(toks[j].text)]) {
			alias, j = toks[j].text, j+1
		}
		if j < fromEnd && toks[j].is("WITH") && j+1 < fromEnd && toks[j+1].is("OFFSET") {
			j += 2
			if j < fromEnd && toks[j].is("AS") {
				j++
			}
			if j < fromEnd && (toks[j].kind == tokWord || toks[j].kind == tokQuoted) && !aliasBefore[strings.ToUpper(toks[j].text)] {
				j++
			}
		}
		// An alias.array path after a comma is an implicit UNNEST.
		if !unnest && len(sides) > 0 && strings.HasPrefix(kind, ",") && len(item) >= 3 && item[1].punct(".") {
			for _, s := range sides {
				if strings.EqualFold(s.alias, item[0].text) {
					unnest = true
				}
			}
		}
		if len(sides) > 0 && !unnest {
			k := strings.TrimSpace(kind)
			switch {
			case strings.HasPrefix(k, ","):
				return js, true, cqInvalid("a comma (CROSS) join: only INNER JOIN operations are supported")
			case k != "JOIN" && k != "INNER JOIN":
				return js, true, cqInvalid("%s: only INNER JOIN operations are supported; other forms, like LEFT or FULL OUTER, are not supported", k)
			}
		}
		if alias == "" && !unnest {
			// A table without an alias is named by its own name.
			if parts, nx := path(item, 0); len(parts) > 0 && nx == len(item) {
				alias = parts[len(parts)-1]
			}
		}
		sides = append(sides, cqSide{from: start, to: start + len(item), alias: alias, unnest: unnest})
		// ON or USING.
		if j < fromEnd && toks[j].is("ON") {
			k := j + 1
			for k < fromEnd && !(at(k) && (toks[k].is("JOIN") || toks[k].is("INNER") || toks[k].is("LEFT") || toks[k].is("RIGHT") ||
				toks[k].is("FULL") || toks[k].is("CROSS") || toks[k].punct(","))) {
				k++
			}
			conds = append(conds, toks[j+1:k])
			j = k
		} else if j < fromEnd && toks[j].is("USING") && j+1 < fromEnd && toks[j+1].punct("(") {
			j = matchClose(toks, j+1) + 1
		}
	}
	js.fromEnd = posOf(toks, fromEnd, out.text)
	if fromEnd < len(toks) && at(fromEnd) && toks[fromEnd].is("WHERE") {
		js.where = toks[fromEnd].pos
		we := clauseEnd(toks, blk, depth, b, fromEnd, cqClauseWords)
		js.whereEnd = posOf(toks, we, out.text)
		conds = append(conds, toks[fromEnd+1:we])
	}
	var streams []int
	for k, s := range sides {
		if !s.unnest {
			streams = append(streams, k)
		}
	}
	if len(streams) < 2 {
		return js, false, nil
	}
	for _, k := range streams {
		if err := streamSide(out, toks, sides[k], ctes, 0); err != nil {
			return js, true, err
		}
	}
	noTime := cqInvalid("JOINs that don't have time-oriented JOIN clauses: in addition to a join key, the JOIN must restrict " +
		"the _CHANGE_TIMESTAMP of each side to a constant interval of another's (for example a._CHANGE_TIMESTAMP BETWEEN " +
		"b._CHANGE_TIMESTAMP - INTERVAL 5 MINUTE AND b._CHANGE_TIMESTAMP); _CHANGE_TIMESTAMP is the only supported " +
		"timestamp column for join operations, user-defined columns are not supported")
	bounds := map[[2]string]*cqBound{}
	for _, c := range conds {
		for _, conj := range conjuncts(c) {
			addBounds(bounds, conj)
		}
	}
	// Every stream side reaches the others through bounded intervals.
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if p, ok := parent[x]; ok && p != x {
			r := find(p)
			parent[x] = r
			return r
		}
		return x
	}
	for _, k := range streams {
		a := strings.ToLower(sides[k].alias)
		if a == "" {
			return js, true, noTime
		}
		parent[a] = a
		js.sides = append(js.sides, sides[k].alias)
	}
	var reach int64
	for key, bd := range bounds {
		if !bd.hasLo || !bd.hasHi {
			continue
		}
		if _, ok := parent[key[0]]; !ok {
			continue
		}
		if _, ok := parent[key[1]]; !ok {
			continue
		}
		parent[find(key[0])] = find(key[1])
		reach += max(abs64(bd.lo), abs64(bd.hi))
	}
	root := find(strings.ToLower(sides[streams[0]].alias))
	for _, k := range streams[1:] {
		if find(strings.ToLower(sides[k].alias)) != root {
			return js, true, noTime
		}
	}
	out.lookbackUs += reach
	return js, true, nil
}

// streamSide checks a JOIN's side is a stream with a starting time: an
// APPENDS call with a start, or a subquery or CTE all of whose sources
// are.
func streamSide(out *cqQuery, toks []token, s cqSide, ctes map[string][2]int, depth int) error {
	static := cqInvalid("stream-to-static-table JOINs: each side of the INNER JOIN operation must be a stream, read with " +
		"APPENDS from a starting time")
	lo, hi := toks[s.from].pos, toks[s.to-1].end
	if len(toks[s.from:s.to]) == 1 && depth < 8 {
		if body, ok := ctes[strings.ToLower(toks[s.from].text)]; ok {
			return streamSide(out, toks, cqSide{from: body[0], to: body[1] + 1}, ctes, depth+1)
		}
	}
	found := false
	for _, src := range out.sources {
		if src.pos < lo || src.pos >= hi {
			continue
		}
		found = true
		switch {
		case src.call == "CHANGES":
			return cqInvalid("the CHANGES function in a JOIN: when starting a continuous query with a stream-to-stream join, only the APPENDS function is supported")
		case src.call == "":
			return static
		case src.start == "":
			return cqInvalid("an APPENDS without a starting time in a JOIN: each side of the INNER JOIN operation must specify a starting time for the continuous query")
		}
	}
	// A CTE named inside a subquery side.
	for k := s.from; k < s.to && !found; k++ {
		if body, ok := ctes[strings.ToLower(toks[k].text)]; ok && (toks[k].kind == tokWord || toks[k].kind == tokQuoted) && k > 0 &&
			(toks[k-1].is("FROM") || toks[k-1].is("JOIN")) && depth < 8 {
			if err := streamSide(out, toks, cqSide{from: body[0], to: body[1] + 1}, ctes, depth+1); err != nil {
				return err
			}
			found = true
		}
	}
	if !found {
		return static
	}
	return nil
}

// cqBound bounds a._CHANGE_TIMESTAMP - b._CHANGE_TIMESTAMP, microseconds.
type cqBound struct {
	lo, hi       int64
	hasLo, hasHi bool
}

func (b *cqBound) lower(v int64) {
	if !b.hasLo || v > b.lo {
		b.lo, b.hasLo = v, true
	}
}

func (b *cqBound) upper(v int64) {
	if !b.hasHi || v < b.hi {
		b.hi, b.hasHi = v, true
	}
}

// conjuncts splits a condition at its top-level ANDs (not BETWEEN's).
func conjuncts(t []token) [][]token {
	var out [][]token
	d, start, between := 0, 0, 0
	for k, tk := range t {
		switch {
		case tk.punct("(") || tk.punct("["):
			d++
		case tk.punct(")") || tk.punct("]"):
			d--
		case d == 0 && tk.is("BETWEEN"):
			between++
		case d == 0 && tk.is("AND"):
			if between > 0 {
				between--
				continue
			}
			out = append(out, t[start:k])
			start = k + 1
		}
	}
	return append(out, t[start:])
}

// stripParens removes brackets around all of t.
func stripParens(t []token) []token {
	for len(t) >= 2 && t[0].punct("(") && matchClose(t, 0) == len(t)-1 {
		t = t[1 : len(t)-1]
	}
	return t
}

// tsOperand reads alias._CHANGE_TIMESTAMP, offset by INTERVALs: + or -,
// TIMESTAMP_ADD or TIMESTAMP_SUB.
func tsOperand(t []token) (alias string, off int64, ok bool) {
	t = stripParens(t)
	if len(t) >= 4 && (t[0].is("TIMESTAMP_ADD") || t[0].is("TIMESTAMP_SUB")) && t[1].punct("(") && matchClose(t, 1) == len(t)-1 {
		args := splitArgs(t[2 : len(t)-1])
		if len(args) != 2 {
			return "", 0, false
		}
		a, o, ok := tsOperand(args[0])
		iv, ok2 := intervalUs(args[1])
		if !ok || !ok2 {
			return "", 0, false
		}
		if t[0].is("TIMESTAMP_SUB") {
			iv = -iv
		}
		return a, o + iv, true
	}
	if len(t) < 3 || !(t[0].kind == tokWord || t[0].kind == tokQuoted) || !t[1].punct(".") || !strings.EqualFold(t[2].text, "_CHANGE_TIMESTAMP") {
		return "", 0, false
	}
	alias = strings.ToLower(t[0].text)
	rest := t[3:]
	for len(rest) > 0 {
		if !rest[0].punct("+") && !rest[0].punct("-") {
			return "", 0, false
		}
		// The INTERVAL runs to the next + or - outside it.
		k := 2
		for k < len(rest) && !rest[k].punct("+") && !rest[k].punct("-") {
			k++
		}
		iv, ok := intervalUs(stripParens(rest[1:k]))
		if !ok {
			return "", 0, false
		}
		if rest[0].punct("-") {
			iv = -iv
		}
		off += iv
		rest = rest[k:]
	}
	return alias, off, true
}

// addBounds reads one conjunct's bound on two sides' _CHANGE_TIMESTAMP.
func addBounds(bounds map[[2]string]*cqBound, c []token) {
	c = stripParens(c)
	put := func(x, y string, lo, hi *int64) {
		if x == y || x == "" || y == "" {
			return
		}
		if x > y {
			x, y = y, x
			nlo, nhi := hi, lo
			if nlo != nil {
				v := -*nlo
				nlo = &v
			}
			if nhi != nil {
				v := -*nhi
				nhi = &v
			}
			lo, hi = nlo, nhi
		}
		k := [2]string{x, y}
		b := bounds[k]
		if b == nil {
			b = &cqBound{}
			bounds[k] = b
		}
		if lo != nil {
			b.lower(*lo)
		}
		if hi != nil {
			b.upper(*hi)
		}
	}
	// TIMESTAMP_DIFF(x, y, UNIT) op n, or BETWEEN n AND m.
	if len(c) > 4 && c[0].is("TIMESTAMP_DIFF") && c[1].punct("(") {
		e := matchClose(c, 1)
		args := splitArgs(c[2:e])
		if len(args) != 3 || len(args[2]) != 1 {
			return
		}
		x, ox, ok1 := tsOperand(args[0])
		y, oy, ok2 := tsOperand(args[1])
		unit := cqUnits[strings.ToUpper(args[2][0].text)]
		if !ok1 || !ok2 || unit == 0 {
			return
		}
		rest := c[e+1:]
		num := func(t []token) (int64, bool) {
			sign := int64(1)
			if len(t) == 2 && t[0].punct("-") {
				sign, t = -1, t[1:]
			}
			if len(t) != 1 {
				return 0, false
			}
			n, err := strconv.ParseInt(t[0].text, 10, 64)
			return sign * n * unit, err == nil
		}
		// TIMESTAMP_DIFF truncates: x - y in [n, m] units is x - y in
		// [n, m+1) units; the reach is what matters, so m+1 is kept.
		shift := oy - ox
		if len(rest) > 0 && rest[0].is("BETWEEN") {
			for k := 1; k < len(rest); k++ {
				if rest[k].is("AND") {
					lo, ok1 := num(rest[1:k])
					hi, ok2 := num(rest[k+1:])
					if ok1 && ok2 {
						lo, hi = lo+shift, hi+unit+shift
						put(x, y, &lo, &hi)
					}
					return
				}
			}
			return
		}
		op, n := cmpOp(rest)
		if op == "" {
			return
		}
		v, ok := num(rest[n:])
		if !ok {
			return
		}
		lo, hi := v+shift, v+unit+shift
		switch op {
		case ">=", ">":
			put(x, y, &lo, nil)
		case "<=", "<":
			put(x, y, nil, &hi)
		case "=":
			put(x, y, &lo, &hi)
		}
		return
	}
	// x BETWEEN y + a AND y + b.
	for k := 0; k < len(c); k++ {
		if !c[k].is("BETWEEN") {
			continue
		}
		x, ox, ok := tsOperand(c[:k])
		if !ok {
			return
		}
		for m := k + 1; m < len(c); m++ {
			if c[m].is("AND") && balancedTokens(c[k+1:m]) {
				y1, o1, ok1 := tsOperand(c[k+1 : m])
				y2, o2, ok2 := tsOperand(c[m+1:])
				if ok1 && ok2 && y1 == y2 {
					lo, hi := o1-ox, o2-ox
					put(x, y1, &lo, &hi)
				}
				return
			}
		}
		return
	}
	// x op y.
	d := 0
	for k := 0; k < len(c); k++ {
		switch {
		case c[k].punct("("):
			d++
		case c[k].punct(")"):
			d--
		case d == 0 && (c[k].punct("<") || c[k].punct(">") || c[k].punct("=")):
			op, n := cmpOp(c[k:])
			if op == "" {
				return
			}
			x, ox, ok1 := tsOperand(c[:k])
			y, oy, ok2 := tsOperand(c[k+n:])
			if !ok1 || !ok2 {
				return
			}
			v := oy - ox // x - y op oy - ox
			switch op {
			case ">=", ">":
				put(x, y, &v, nil)
			case "<=", "<":
				put(x, y, nil, &v)
			case "=":
				put(x, y, &v, &v)
			}
			return
		}
	}
}

// cmpOp reads a comparison operator at t[0]: the operator and its tokens.
func cmpOp(t []token) (string, int) {
	if len(t) == 0 {
		return "", 0
	}
	if len(t) > 1 && t[1].punct("=") && t[1].pos == t[0].end && (t[0].punct("<") || t[0].punct(">")) {
		return t[0].text + "=", 2
	}
	if len(t) > 1 && t[0].punct("<") && t[1].punct(">") {
		return "", 0
	}
	switch {
	case t[0].punct("<"), t[0].punct(">"), t[0].punct("="):
		return t[0].text, 1
	}
	return "", 0
}

func balancedTokens(t []token) bool {
	d := 0
	for _, tk := range t {
		if tk.punct("(") {
			d++
		} else if tk.punct(")") {
			d--
		}
	}
	return d == 0
}

// checkAggregation refuses what window aggregation does not support in
// block b, and reports whether it aggregates.
func checkAggregation(out *cqQuery, toks []token, blk, depth []int, b int) (bool, error) {
	d := depth[b]
	at := func(j int) bool { return j < len(toks) && blk[j] == b && depth[j] == d }
	group := -1
	agg := false
	for j := b; j < len(toks); j++ {
		if depth[j] < d || depth[j] == d && blk[j] != b {
			break
		}
		if blk[j] != b {
			continue
		}
		if at(j) && toks[j].is("GROUP") && j+1 < len(toks) && toks[j+1].is("BY") {
			group = j
		}
		if toks[j].kind != tokWord || j+1 >= len(toks) || !toks[j+1].punct("(") {
			continue
		}
		name := strings.ToUpper(toks[j].text)
		if j >= 2 && toks[j-1].punct(".") {
			name = strings.ToUpper(toks[j-2].text) + "." + name
		} else if j+2 < len(toks) && toks[j+2].punct(".") {
			continue
		}
		switch name {
		case "ST_CENTROID_AGG", "ST_EXTENT", "ST_UNION_AGG", "GROUPING", "HLL_COUNT.INIT", "HLL_COUNT.MERGE",
			"HLL_COUNT.MERGE_PARTIAL", "JSON_AGG", "KLL_QUANTILES.INIT_INT64", "KLL_QUANTILES.INIT_FLOAT64",
			"KLL_QUANTILES.MERGE_INT64", "KLL_QUANTILES.MERGE_FLOAT64", "APPROX_COUNT_DISTINCT_HLL", "AVG_DP", "SUM_DP":
			return true, cqInvalid("the aggregate function %s: window aggregations support %s", name, cqAggList())
		}
		if !cqStatefulAggregates[name] || j > 0 && toks[j-1].punct(".") {
			continue
		}
		agg = true
		e := matchClose(toks, j+1)
		if j+2 < e && toks[j+2].is("DISTINCT") {
			return true, cqInvalid("aggregate functions containing DISTINCT expressions: %s(DISTINCT …)", name)
		}
		if limit, ok := cqAggLimit[name]; ok {
			n := int64(-1)
			inner := depth[j+1] + 1
			for k := j + 2; k < e; k++ {
				if depth[k] == inner && toks[k].is("LIMIT") && k+1 < e {
					v, err := strconv.ParseInt(toks[k+1].text, 10, 64)
					if err == nil {
						n = v
					}
				}
			}
			if n < 0 {
				return true, cqInvalid("%s without a LIMIT clause: in a window aggregation, %s requires a LIMIT, with a maximum value of %d", name, name, limit)
			}
			if n > limit {
				return true, cqInvalid("%s with LIMIT %d: in a window aggregation, %s's LIMIT has a maximum value of %d", name, n, name, limit)
			}
		}
	}
	if !agg && group < 0 {
		return false, nil
	}
	// The SELECT reads a TUMBLE.
	tumble := false
	for j := b + 1; j+1 < len(toks); j++ {
		if depth[j] < d || depth[j] == d && blk[j] != b {
			break
		}
		if at(j) && toks[j].is("TUMBLE") && toks[j+1].punct("(") && j > 0 && toks[j-1].is("FROM") {
			tumble = true
		}
	}
	if !tumble {
		return true, cqInvalid("aggregation outside a window: a continuous query aggregates over a windowing TVF, " +
			"FROM TUMBLE(TABLE table, \"timestamp_column\", window_size), grouped by its window_start or window_end")
	}
	missing := cqInvalid("a window aggregation whose GROUP BY has neither window_start nor window_end: TUMBLE produces two " +
		"additional output columns, window_start and window_end, and you must include at least one of these columns in the GROUP BY")
	if group < 0 {
		return true, missing
	}
	gend := clauseEnd(toks, blk, depth, b, group+1, map[string]bool{"HAVING": true, "QUALIFY": true, "WINDOW": true,
		"ORDER": true, "LIMIT": true, "UNION": true, "INTERSECT": true, "EXCEPT": true})
	items := selectItems(toks, blk, depth, b)
	isWindow := func(t []token) bool {
		if len(t) == 0 {
			return false
		}
		last := t[len(t)-1]
		ok := strings.EqualFold(last.text, "window_start") || strings.EqualFold(last.text, "window_end")
		return ok && (len(t) == 1 || len(t) == 3 && t[1].punct("."))
	}
	for _, g := range splitArgs(toks[group+2 : gend]) {
		switch {
		case isWindow(g):
			return true, nil
		case len(g) == 1 && g[0].is("ALL"):
			for _, it := range items {
				if isWindow(it) || len(it) >= 2 && it[len(it)-2].is("AS") && isWindow(it[:len(it)-2]) {
					return true, nil
				}
			}
		case len(g) == 1 && g[0].kind == tokWord:
			if n, err := strconv.Atoi(g[0].text); err == nil && n >= 1 && n <= len(items) {
				it := items[n-1]
				if len(it) >= 2 && it[len(it)-2].is("AS") {
					it = it[:len(it)-2]
				}
				if isWindow(it) {
					return true, nil
				}
			}
			for _, it := range items {
				if len(it) >= 3 && it[len(it)-2].is("AS") && strings.EqualFold(it[len(it)-1].text, g[0].text) && isWindow(it[:len(it)-2]) {
					return true, nil
				}
			}
		}
	}
	return true, missing
}

// selectItems are block b's select list items.
func selectItems(toks []token, blk, depth []int, b int) [][]token {
	end := clauseEnd(toks, blk, depth, b, b, map[string]bool{"FROM": true, "WHERE": true, "GROUP": true, "HAVING": true,
		"QUALIFY": true, "WINDOW": true, "ORDER": true, "LIMIT": true, "UNION": true, "INTERSECT": true, "EXCEPT": true})
	start := b + 1
	for start < end && (toks[start].is("ALL") || toks[start].is("AS") && start+1 < end && (toks[start+1].is("STRUCT") || toks[start+1].is("VALUE"))) {
		start++
		if toks[start-1].is("AS") {
			start++
		}
	}
	return splitArgs(toks[start:end])
}

func cqAggList() string {
	return "ANY_VALUE, APPROX_COUNT_DISTINCT, APPROX_QUANTILES, APPROX_TOP_COUNT, APPROX_TOP_SUM, ARRAY_AGG, ARRAY_CONCAT_AGG, " +
		"AVG, BIT_AND, BIT_OR, BIT_XOR, CORR, COUNT, COUNTIF, COVAR_POP, COVAR_SAMP, LOGICAL_AND, LOGICAL_OR, MAX, MAX_BY, MIN, " +
		"MIN_BY, STDDEV, STDDEV_POP, STDDEV_SAMP, STRING_AGG, SUM, VAR_POP, VAR_SAMP and VARIANCE"
}

// unknownWindowTVF refuses HOP and SESSION, which BigQuery does not
// document, as any table-valued function it does not have: sql is the
// whole statement, for the error's position.
func unknownWindowTVF(sql string, toks []token) error {
	for i := 1; i+1 < len(toks); i++ {
		t := toks[i]
		if !(t.is("HOP") || t.is("SESSION")) || !toks[i+1].punct("(") {
			continue
		}
		if p := toks[i-1]; !(p.is("FROM") || p.is("JOIN") || p.punct(",")) {
			continue
		}
		line, col := 1, 1
		for _, c := range sql[:t.pos] {
			if c == '\n' {
				line, col = line+1, 1
			} else {
				col++
			}
		}
		return &cqRefusal{400, fmt.Sprintf("Table-valued function not found: %s at [%d:%d]", t.text, line, col)}
	}
	return nil
}

// stateful reports whether q, or a view it reads, has stateful
// operations.
func (q *cqQuery) stateful() bool {
	if len(q.tumbles) > 0 || len(q.joins) > 0 || q.aggregates {
		return true
	}
	for _, s := range q.sources {
		if s.view != nil && s.view.stateful() {
			return true
		}
	}
	return false
}

// lookback is how far back, in microseconds, a run reads its sources
// for the JOINs' intervals.
func (q *cqQuery) lookback() int64 {
	l := q.lookbackUs
	for _, s := range q.sources {
		if s.view != nil {
			l += s.view.lookback()
		}
	}
	return l
}

// windowSizes are the TUMBLE window sizes of q and its views.
func (q *cqQuery) windowSizes() []int64 {
	var out []int64
	for _, t := range q.tumbles {
		out = append(out, t.sizeUs)
	}
	for _, s := range q.sources {
		if s.view != nil {
			out = append(out, s.view.windowSizes()...)
		}
	}
	return out
}

// baseSources are q's table sources, through its views.
func (q *cqQuery) baseSources() []*cqSource {
	var out []*cqSource
	for i := range q.sources {
		s := &q.sources[i]
		if s.view != nil {
			out = append(out, s.view.baseSources()...)
			continue
		}
		out = append(out, s)
	}
	return out
}

// floorDiv is a / b rounded down.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// closesWindow reports whether a watermark moving from prev to cur, in
// microseconds, closes a window of one of sizes.
func closesWindow(sizes []int64, prev, cur int64) bool {
	for _, s := range sizes {
		if floorDiv(cur, s) > floorDiv(prev, s) {
			return true
		}
	}
	return false
}

// statefulRead is where, in milliseconds, a run of a stateful query
// reads its sources from: back from the earliest window still open, or
// from the run's own start, by the JOINs' intervals.
func statefulRead(q *cqQuery, w cqWindow) int64 {
	if w.first {
		return 0 // each source's start
	}
	look := q.lookback()
	sizes := q.windowSizes()
	if len(sizes) == 0 {
		return w.low - (look+999)/1000
	}
	from := int64(-1)
	for _, s := range sizes {
		ws := floorDiv(w.prevWM, s) * s
		if from < 0 || ws < from {
			from = ws
		}
	}
	return floorDiv(from-look, 1000)
}

// renderTumble writes a TUMBLE call for window w: the input's rows with
// window_start and window_end, of the windows the run closes only.
func (q *cqQuery) renderTumble(t cqTumble, w cqWindow, dataset string) string {
	input := t.cte
	if input == "" {
		for _, s := range q.sources {
			if s.tumbleInput && s.pos >= t.pos && s.pos < t.end {
				input = q.renderSource(s, w, dataset)
			}
		}
	}
	size := strconv.FormatInt(t.sizeUs, 10)
	col := cqTumbleAlias + "." + quotePath([]string{t.column})
	start := "DIV(UNIX_MICROS(" + col + "), " + size + ") * " + size
	return fmt.Sprintf("(SELECT * FROM (SELECT %s.*, TIMESTAMP_MICROS(%s) AS window_start, TIMESTAMP_MICROS(%s + %s) AS window_end "+
		"FROM %s AS %s) WHERE window_end > TIMESTAMP_MICROS(%d) AND window_end <= TIMESTAMP_MICROS(%d))",
		cqTumbleAlias, start, start, size, input, cqTumbleAlias, w.prevWM, w.curWM)
}

// joinFilter keeps a JOIN's pairs whose later row the run read new.
func joinFilter(js cqJoinScope, w cqWindow) string {
	var ts []string
	for _, s := range js.sides {
		ts = append(ts, quotePath([]string{s})+"._CHANGE_TIMESTAMP")
	}
	return fmt.Sprintf("GREATEST(%s) >= TIMESTAMP_MILLIS(%d)", strings.Join(ts, ", "), w.low)
}
