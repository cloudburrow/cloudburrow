package bigqueryfront

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The filters of a query of INFORMATION_SCHEMA.JOBS, applied to the jobs
// before they are written as rows (#1369).
//
// Measured on an instance with 5,000 jobs (2026-10-15): SELECT ... FROM
// `region-us`.INFORMATION_SCHEMA.JOBS WHERE job_id = '...' took 54 s, the
// engine's jobs.list (8.7 s, read again for each of five pages) and the
// rows of every job inlined in the query. The front now reads the list
// once (the engine's patch 0016 reads it in one scan), and keeps only the
// jobs that can pass the query's WHERE, so a query that names its jobs
// does not grow with the history.
//
// What is pushed down never changes the answer: the query still runs with
// its WHERE, so a job kept that fails it is dropped by the engine, and a
// job is left out only when it cannot pass. That is the case for the
// top-level WHERE of the one SELECT whose only FROM item is the view, a
// conjunct of it at a time (one the front does not read is ignored):
// job_id, state, job_type, project_id and parent_job_id = or IN string
// literals, parent_job_id IS [NOT] NULL, and creation_time compared with a
// TIMESTAMP literal or (a lower bound only, a minute early)
// CURRENT_TIMESTAMP() less an INTERVAL. ORDER BY creation_time [ASC|DESC]
// LIMIT n [OFFSET m] keeps the first n+m jobs and those of the same
// time, when every conjunct was read exactly and the select list
// aggregates nothing.

// jobKeys are the values of a job the filter reads, as its row has them;
// "" stands for NULL.
type jobKeys struct {
	id, state, parent, jobType, project string
	created                             int64 // ms
	timed                               bool
}

func keysOf(project string, job map[string]any) jobKeys {
	ref, _ := job["jobReference"].(map[string]any)
	conf, _ := job["configuration"].(map[string]any)
	stats, _ := job["statistics"].(map[string]any)
	created, timed := msValue(stats["creationTime"])
	return jobKeys{id: str(ref, "jobId"), state: jobStateOf(job), parent: str(stats, "parentJobId"),
		jobType: jobTypeOf(conf), project: project, created: created, timed: timed}
}

// jobsFilter keeps the jobs a query can read. A nil filter keeps every
// job.
type jobsFilter struct {
	keep         []func(jobKeys) bool
	minUs, maxUs *int64 // inclusive bounds of creation_time, microseconds
	limit        int    // with ORDER BY creation_time; -1 for none
	desc         bool
	// ids, when set, are the only job IDs a job_id = or IN conjunct
	// lets through (jobIDs, #1633).
	ids map[string]bool
}

// jobIDsMax is the most job IDs jobIDs gives, which a jobs.list's URL
// names (bigquery-emulator patch 0064), as a background sync's does
// (jobsPendingMax); past it the list is of every job.
const jobIDsMax = jobsPendingMax

// jobIDs are the only job IDs the filter lets through, sorted, or nil
// when it lets any through or names more than jobIDsMax: a job_id = or
// IN conjunct (#1633), whose jobs alone the list needs to read.
func (f *jobsFilter) jobIDs() []string {
	if f == nil || f.ids == nil || len(f.ids) > jobIDsMax {
		return nil
	}
	ids := make([]string, 0, len(f.ids))
	for id := range f.ids {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// listBounds sets jobs.list's creation-time bounds, milliseconds and
// inclusive, to those of the filter or wider.
func (f *jobsFilter) listBounds(q url.Values) {
	if f == nil {
		return
	}
	if f.minUs != nil && *f.minUs >= 0 {
		q.Set("minCreationTime", strconv.FormatInt(*f.minUs/1000, 10))
	}
	if f.maxUs != nil && *f.maxUs >= 0 {
		q.Set("maxCreationTime", strconv.FormatInt(*f.maxUs/1000, 10))
	}
}

// bounded reports whether listBounds cuts the list.
func (f *jobsFilter) bounded() bool {
	return f != nil && (f.minUs != nil && *f.minUs >= 0 || f.maxUs != nil && *f.maxUs >= 0)
}

// apply returns the jobs the filter keeps, in their order.
func (f *jobsFilter) apply(project string, jobs []map[string]any) []map[string]any {
	if f == nil {
		return jobs
	}
	var out []map[string]any
	var keys []jobKeys
	for _, job := range jobs {
		k := keysOf(project, job)
		if f.minUs != nil && (!k.timed || k.created*1000 < *f.minUs) || f.maxUs != nil && (!k.timed || k.created*1000 > *f.maxUs) {
			continue
		}
		ok := true
		for _, keep := range f.keep {
			if !keep(k) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, job)
			keys = append(keys, k)
		}
	}
	if f.limit < 0 || len(out) <= f.limit {
		return out
	}
	// A job with no creation time sorts first ascending and last
	// descending, as NULL does.
	at := func(k jobKeys) int64 {
		if !k.timed {
			return -1 << 63
		}
		return k.created
	}
	order := make([]int, len(out))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		if f.desc {
			return at(keys[order[a]]) > at(keys[order[b]])
		}
		return at(keys[order[a]]) < at(keys[order[b]])
	})
	if f.limit == 0 {
		return nil
	}
	edge := at(keys[order[f.limit-1]])
	kept := map[int]bool{}
	for n, i := range order {
		if n < f.limit || at(keys[i]) == edge {
			kept[i] = true
		}
	}
	var limited []map[string]any
	for i, job := range out {
		if kept[i] {
			limited = append(limited, job)
		}
	}
	return limited
}

// aggregateFunctions are GoogleSQL's aggregate functions; one in the
// select list stops a LIMIT being pushed down.
var aggregateFunctions = map[string]bool{"COUNT": true, "COUNTIF": true, "SUM": true, "AVG": true, "MIN": true, "MAX": true,
	"ARRAY_AGG": true, "ARRAY_CONCAT_AGG": true, "STRING_AGG": true, "ANY_VALUE": true, "LOGICAL_AND": true,
	"LOGICAL_OR": true, "BIT_AND": true, "BIT_OR": true, "BIT_XOR": true, "APPROX_COUNT_DISTINCT": true,
	"APPROX_QUANTILES": true, "APPROX_TOP_COUNT": true, "APPROX_TOP_SUM": true, "STDDEV": true, "STDDEV_POP": true,
	"STDDEV_SAMP": true, "VARIANCE": true, "VAR_POP": true, "VAR_SAMP": true, "CORR": true, "COVAR_POP": true,
	"COVAR_SAMP": true, "GROUPING": true, "HLL_COUNT": true, "MAX_BY": true, "MIN_BY": true, "ST_UNION_AGG": true,
	"ST_EXTENT": true, "ST_CENTROID_AGG": true, "JSON_AGG": true, "PERCENTILE_CONT": true, "PERCENTILE_DISC": true}

// clauseEnds are the words that end a WHERE clause.
var clauseEnds = map[string]bool{"GROUP": true, "ORDER": true, "LIMIT": true, "QUALIFY": true, "HAVING": true,
	"WINDOW": true, "UNION": true, "INTERSECT": true, "EXCEPT": true}

// pushedJobsFilter is the filter of sql's one JOBS reference among refs,
// or nil when there is not one or nothing can be pushed down.
func pushedJobsFilter(sql string, refs []jobsRef, now time.Time) *jobsFilter {
	var ref *jobsRef
	for i := range refs {
		if jobsViews[refs[i].view] {
			if ref != nil {
				return nil
			}
			ref = &refs[i]
		}
	}
	if ref == nil {
		return nil
	}
	w, ok := whereOf(sql, ref.pos, ref.end, ref.alias, ref.view)
	if !ok {
		return nil
	}
	t, i := w.tokens, w.after
	f := &jobsFilter{limit: -1}
	exact := w.all
	for _, c := range w.conjuncts {
		if !f.conjunct(c, w.qualifier, now) {
			exact = false
		}
	}
	if exact && i+1 < len(t) && t[i].is("ORDER") && t[i+1].is("BY") {
		f.orderLimit(t, i+2, w.from, w.qualifier, sql)
	}
	if len(f.keep) == 0 && f.minUs == nil && f.maxUs == nil && f.limit < 0 {
		return nil
	}
	return f
}

// viewWhere is the WHERE clause of the SELECT whose only FROM item is a
// view reference (whereOf).
type viewWhere struct {
	tokens    []token
	from      int       // the index of the reference's first token
	after     int       // the index of the token that ends the WHERE clause (or of the one after the reference)
	qualifier string    // the reference's alias, or the view's name
	conjuncts [][]token // the WHERE clause's top-level conjuncts
	all       bool      // false when the WHERE clause has a top-level OR (conjuncts is then empty)
}

// whereOf reads the WHERE clause of the SELECT whose FROM is the view
// reference sql[pos:end]; ok is false when that FROM has another item (a
// join, a comma, a pipe) or the reference is not after FROM. A SELECT with
// no WHERE has no conjuncts.
func whereOf(sql string, pos, end int, alias bool, view string) (viewWhere, bool) {
	t, ok := lex(sql)
	if !ok {
		return viewWhere{}, false
	}
	a, b := -1, -1
	for i, tok := range t {
		if tok.pos == pos {
			a = i
		}
		if tok.end == end {
			b = i + 1
		}
	}
	if a < 1 || b <= a || !t[a-1].is("FROM") {
		return viewWhere{}, false
	}
	w := viewWhere{tokens: t, from: a, qualifier: view, all: true}
	i := b
	if alias {
		if i < len(t) && t[i].is("AS") {
			i++
		}
		if i >= len(t) {
			return viewWhere{}, false
		}
		w.qualifier = t[i].text
		i++
	}
	ends := func(i int) bool {
		return i >= len(t) || t[i].punct(")") || t[i].punct(";") || t[i].kind == tokWord && clauseEnds[strings.ToUpper(t[i].text)]
	}
	if i < len(t) && t[i].is("WHERE") {
		start, depth := i+1, 0
		i++
		for ; i < len(t); i++ {
			if t[i].punct("(") || t[i].punct("[") {
				depth++
			} else if t[i].punct(")") || t[i].punct("]") {
				if depth == 0 {
					break
				}
				depth--
			} else if depth == 0 && ends(i) {
				break
			}
		}
		w.conjuncts, w.all = splitAnd(t[start:i])
	} else if !ends(i) {
		return viewWhere{}, false // another FROM item, a join, a pipe
	}
	w.after = i
	return w, true
}

// splitAnd splits a WHERE clause at its top-level ANDs; all is false when
// it has a top-level OR (none of it is then returned).
func splitAnd(t []token) (out [][]token, all bool) {
	depth, start, between := 0, 0, false
	for i, tok := range t {
		switch {
		case tok.punct("(") || tok.punct("["):
			depth++
		case tok.punct(")") || tok.punct("]"):
			depth--
		case depth > 0:
		case tok.is("OR"):
			return nil, false
		case tok.is("BETWEEN"):
			between = true
		case tok.is("AND") && between:
			between = false
		case tok.is("AND"):
			out = append(out, t[start:i])
			start = i + 1
		}
	}
	return append(out, t[start:]), true
}

// column is the JOBS column t names, unqualified or qualified by the
// view's alias (or name), lower case, and the tokens after it.
func column(t []token, qualifier string) (string, []token) {
	if len(t) >= 3 && (t[0].kind == tokWord || t[0].kind == tokQuoted) && strings.EqualFold(t[0].text, qualifier) && t[1].punct(".") {
		t = t[2:]
	}
	if len(t) == 0 || t[0].kind != tokWord && t[0].kind != tokQuoted {
		return "", nil
	}
	return strings.ToLower(t[0].text), t[1:]
}

// literal is a plain '...' or "..." string literal's value; ok is false
// for one with a prefix, triple quotes or an escape.
func literal(tok token) (string, bool) {
	s := tok.text
	if tok.kind != tokString || len(s) < 2 || s[0] != '\'' && s[0] != '"' || strings.ContainsRune(s, '\\') ||
		strings.HasPrefix(s, `'''`) || strings.HasPrefix(s, `"""`) {
		return "", false
	}
	return s[1 : len(s)-1], true
}

// stringColumns read a job's values of the STRING columns a filter can
// read.
var stringColumns = map[string]func(jobKeys) string{
	"job_id":        func(k jobKeys) string { return k.id },
	"state":         func(k jobKeys) string { return k.state },
	"job_type":      func(k jobKeys) string { return k.jobType },
	"project_id":    func(k jobKeys) string { return k.project },
	"parent_job_id": func(k jobKeys) string { return k.parent },
}

// conjunct adds the conjunct t to f, and reports whether f now applies it
// exactly; false leaves f as it was, or wider than t.
func (f *jobsFilter) conjunct(t []token, qualifier string, now time.Time) bool {
	t = unwrap(t)
	if col, vals, ok := stringIn(t, qualifier); ok {
		return f.equal(col, vals)
	}
	col, rest := column(t, qualifier)
	switch {
	case col == "parent_job_id" && len(rest) == 2 && rest[0].is("IS") && rest[1].is("NULL"):
		f.keep = append(f.keep, func(k jobKeys) bool { return k.parent == "" })
		return true
	case col == "parent_job_id" && len(rest) == 3 && rest[0].is("IS") && rest[1].is("NOT") && rest[2].is("NULL"):
		f.keep = append(f.keep, func(k jobKeys) bool { return k.parent != "" })
		return true
	case col == "creation_time":
		return f.timeBound(rest, now)
	}
	return false
}

// unwrap drops the parentheses around the whole of a conjunct.
func unwrap(t []token) []token {
	for len(t) >= 2 && t[0].punct("(") && t[len(t)-1].punct(")") && closes(t) {
		t = t[1 : len(t)-1]
	}
	return t
}

// stringIn reads the conjunct t as column = 'v', 'v' = column or column
// IN ('v', ...), the column unqualified or qualified by qualifier: the
// column, lower case, and the literals' values.
func stringIn(t []token, qualifier string) (string, []string, bool) {
	col, rest := column(t, qualifier)
	if col == "" {
		// 'x' = job_id
		if len(t) >= 3 {
			if v, ok := literal(t[0]); ok && t[1].punct("=") {
				if c, after := column(t[2:], qualifier); c != "" && len(after) == 0 {
					return c, []string{v}, true
				}
			}
		}
		return "", nil, false
	}
	switch {
	case len(rest) == 2 && rest[0].punct("=") && rest[1].kind == tokString:
		v, ok := literal(rest[1])
		return col, []string{v}, ok
	case len(rest) >= 4 && rest[0].is("IN") && rest[1].punct("(") && rest[len(rest)-1].punct(")"):
		var vals []string
		for j := 2; j < len(rest)-1; j += 2 {
			v, ok := literal(rest[j])
			if !ok || !rest[j+1].punct(",") && !rest[j+1].punct(")") {
				return "", nil, false
			}
			vals = append(vals, v)
		}
		return col, vals, true
	}
	return "", nil, false
}

// closes reports whether t's first parenthesis closes at its last token.
func closes(t []token) bool {
	depth := 0
	for i, tok := range t {
		if tok.punct("(") {
			depth++
		} else if tok.punct(")") {
			depth--
			if depth == 0 {
				return i == len(t)-1
			}
		}
	}
	return false
}

func (f *jobsFilter) equal(col string, vals []string) bool {
	get := stringColumns[col]
	if get == nil {
		return false
	}
	set := map[string]bool{}
	for _, v := range vals {
		set[v] = true
	}
	f.keep = append(f.keep, func(k jobKeys) bool { v := get(k); return v != "" && set[v] })
	if col == "job_id" {
		// What every job_id conjunct lets through (#1633).
		ids := map[string]bool{}
		for v := range set {
			if v != "" && (f.ids == nil || f.ids[v]) {
				ids[v] = true
			}
		}
		f.ids = ids
	}
	return true
}

// timeBound adds creation_time op rest's bound.
func (f *jobsFilter) timeBound(rest []token, now time.Time) bool {
	if len(rest) < 2 || rest[0].kind != tokPunct {
		if len(rest) > 0 && rest[0].is("BETWEEN") {
			for j := 1; j < len(rest); j++ {
				if rest[j].is("AND") {
					lo, ok1 := jobsTimestamp(rest[1:j])
					hi, ok2 := jobsTimestamp(rest[j+1:])
					if !ok1 || !ok2 {
						return false
					}
					f.bound(&f.minUs, lo, true)
					f.bound(&f.maxUs, hi, false)
					return true
				}
			}
		}
		return false
	}
	op, val := rest[0].text, rest[1:]
	if len(val) > 0 && val[0].punct("=") && (op == "<" || op == ">") {
		op, val = op+"=", val[1:]
	}
	us, ok := jobsTimestamp(val)
	if !ok {
		// A lower bound of CURRENT_TIMESTAMP() less an INTERVAL, a minute
		// early: the engine's clock reads later than the front's.
		ago, ok := sinceNow(val)
		if !ok || op != ">" && op != ">=" {
			return false
		}
		f.bound(&f.minUs, now.Add(-ago-time.Minute).UnixMicro(), true)
		return false
	}
	switch op {
	case ">":
		f.bound(&f.minUs, us+1, true)
	case ">=":
		f.bound(&f.minUs, us, true)
	case "<":
		f.bound(&f.maxUs, us-1, false)
	case "<=":
		f.bound(&f.maxUs, us, false)
	case "=":
		f.bound(&f.minUs, us, true)
		f.bound(&f.maxUs, us, false)
	default:
		return false
	}
	return true
}

// bound narrows *b to v.
func (f *jobsFilter) bound(b **int64, v int64, lower bool) {
	if *b == nil || lower && v > **b || !lower && v < **b {
		*b = &v
	}
}

// timestampLayouts are the forms of a TIMESTAMP literal the filter reads,
// after a 'T' becomes a space; a literal of another is not pushed down.
var timestampLayouts = []string{"2006-01-02", "2006-01-02 15:04", "2006-01-02 15:04:05", "2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05Z07", "2006-01-02 15:04Z07:00"}

// jobsTimestamp reads TIMESTAMP '...' or '...' as microseconds.
func jobsTimestamp(t []token) (int64, bool) {
	if len(t) == 2 && t[0].is("TIMESTAMP") {
		t = t[1:]
	}
	if len(t) != 1 {
		return 0, false
	}
	s, ok := literal(t[0])
	if !ok {
		return 0, false
	}
	s = strings.TrimSpace(s)
	loc := time.UTC
	if strings.HasSuffix(s, " UTC") {
		s = strings.TrimSuffix(s, " UTC")
	}
	if len(s) > 10 && s[10] == 'T' {
		s = s[:10] + " " + s[11:]
	}
	frac := ""
	if dot := strings.IndexByte(s, '.'); dot > 0 {
		end := dot + 1
		for end < len(s) && s[end] >= '0' && s[end] <= '9' {
			end++
		}
		frac, s = s[dot:end], s[:dot]+s[end:]
		if len(frac) > 7 {
			return 0, false
		}
	}
	for _, layout := range timestampLayouts {
		if tm, err := time.ParseInLocation(layout, s, loc); err == nil {
			us := tm.UnixMicro()
			if frac != "" {
				digits := (frac[1:] + "000000")[:6]
				n, _ := strconv.ParseInt(digits, 10, 64)
				us += n
			}
			return us, true
		}
	}
	return 0, false
}

// intervalUnits are the INTERVAL units sinceNow reads.
var intervalUnits = map[string]time.Duration{"MICROSECOND": time.Microsecond, "MILLISECOND": time.Millisecond,
	"SECOND": time.Second, "MINUTE": time.Minute, "HOUR": time.Hour, "DAY": 24 * time.Hour}

// sinceNow reads TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL n UNIT) or
// CURRENT_TIMESTAMP() - INTERVAL n UNIT as the interval.
func sinceNow(t []token) (time.Duration, bool) {
	words := make([]string, len(t))
	for i, tok := range t {
		words[i] = strings.ToUpper(tok.text)
	}
	s := strings.Join(words, " ")
	var rest string
	switch {
	case strings.HasPrefix(s, "TIMESTAMP_SUB ( CURRENT_TIMESTAMP ( ) , INTERVAL ") && strings.HasSuffix(s, " )"):
		rest = strings.TrimSuffix(strings.TrimPrefix(s, "TIMESTAMP_SUB ( CURRENT_TIMESTAMP ( ) , INTERVAL "), " )")
	case strings.HasPrefix(s, "CURRENT_TIMESTAMP ( ) - INTERVAL "):
		rest = strings.TrimPrefix(s, "CURRENT_TIMESTAMP ( ) - INTERVAL ")
	default:
		return 0, false
	}
	parts := strings.Fields(rest)
	if len(parts) != 2 {
		return 0, false
	}
	n, err := strconv.ParseInt(parts[0], 10, 64)
	unit, ok := intervalUnits[parts[1]]
	if err != nil || !ok || n < 0 || n > int64(1<<62)/int64(unit) {
		return 0, false
	}
	return time.Duration(n) * unit, true
}

// orderLimit reads ORDER BY creation_time [ASC|DESC] LIMIT n [OFFSET m]
// from t[i:], the end of the SELECT whose FROM is t[from-1], and sets f's
// limit when the select list aggregates nothing.
func (f *jobsFilter) orderLimit(t []token, i, from int, qualifier, sql string) {
	col, rest := column(t[i:], qualifier)
	if col != "creation_time" {
		return
	}
	desc := false
	if len(rest) > 0 && (rest[0].is("ASC") || rest[0].is("DESC")) {
		desc = rest[0].is("DESC")
		rest = rest[1:]
	}
	if len(rest) < 2 || !rest[0].is("LIMIT") {
		return
	}
	n, err := strconv.Atoi(rest[1].text)
	if err != nil || n < 0 || n > 1<<20 {
		return
	}
	rest = rest[2:]
	if len(rest) >= 2 && rest[0].is("OFFSET") {
		m, err := strconv.Atoi(rest[1].text)
		if err != nil || m < 0 || m > 1<<20 {
			return
		}
		n += m
		rest = rest[2:]
	}
	if len(rest) > 0 && !rest[0].punct(")") && !rest[0].punct(";") {
		return
	}
	// The select list: from its SELECT to the FROM.
	depth, sel := 0, -1
	for j := from - 2; j >= 0; j-- {
		if t[j].punct(")") {
			depth++
		} else if t[j].punct("(") {
			if depth == 0 {
				return
			}
			depth--
		} else if depth == 0 && t[j].is("SELECT") {
			sel = j
			break
		}
	}
	if sel < 0 {
		return
	}
	for _, tok := range t {
		if tok.is("AGGREGATE") { // a user-defined aggregate function
			return
		}
	}
	list := t[sel+1 : from-1]
	for j, tok := range list {
		if tok.is("DISTINCT") || tok.is("OVER") || tok.kind == tokWord && aggregateFunctions[strings.ToUpper(tok.text)] &&
			j+1 < len(list) && list[j+1].punct("(") {
			return
		}
		// ORDER BY reads an alias of the select list first.
		if (tok.kind == tokWord || tok.kind == tokQuoted) && strings.EqualFold(tok.text, "creation_time") && j > 0 &&
			!list[j-1].punct(",") && !list[j-1].punct(".") {
			return
		}
	}
	f.limit, f.desc = n, desc
}
