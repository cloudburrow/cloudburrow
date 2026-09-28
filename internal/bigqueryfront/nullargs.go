package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"strings"
)

// NULL arguments the engine does not take (#1109).
//
// In BigQuery "If an operand is NULL, the function result is NULL", unless
// a function's description says otherwise (function call rules,
// https://cloud.google.com/bigquery/docs/reference/standard-sql/functions-reference),
// and ARRAY_CONCAT "returns NULL if any input argument is NULL"
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/array_functions).
//
// The emulator's engine (goccy/googlesqlite v0.3.1, in the pinned image)
// passes a NULL argument to a function as a nil value, and some of its
// functions call a method on it without looking: ARRAY_TO_STRING (every
// argument, internal/functions/array/array_to_string.go), ARRAY_CONCAT
// (every argument, array_concat.go), ARRAY_REVERSE (array_reverse.go),
// NET.IP_TO_STRING, NET.IP_TRUNC, NET.IPV4_FROM_INT64, NET.IPV4_TO_INT64,
// NET.PUBLIC_SUFFIX and NET.REG_DOMAIN (internal/functions/net). Nothing in
// the engine recovers the panic (SAFE. does not either); the emulator's
// recovery middleware answers it 500 internalError, the panic's text, and
// the Go client retries a 500 until its deadline. Measured through the
// front with the official Go client: SELECT ARRAY_TO_STRING(CAST(NULL AS
// ARRAY<STRING>), ',') was answered 500 "runtime error: invalid memory
// address or nil pointer dereference" and retried for 60 s.
//
// So the front sends each call of one of those functions (nullSafeFuncs,
// with or without SAFE.) as IF(arg IS NULL OR ..., NULL, call): NULL when
// an argument is, as in BigQuery, and the call as it was otherwise. Each
// argument's text is then in the query twice, so it is evaluated twice
// (the engine inlines a SQL function's arguments so too); a call whose
// arguments hold a positional parameter (?) is left as it is, as it would
// then take two. A parameter alone as an argument is not tested: whether
// its value is NULL is known (the emulator types a NULL parameter by
// where it is used, and refused `@d IS NULL` beside ARRAY_TO_STRING(...,
// @d): "Undeclared parameter 'd' is used assuming different types",
// measured). Calls inside the arguments are sent so too. A string literal
// or a comment is not changed (lex). A REPEATED column a streamed row left
// out holds NULL in the engine, where BigQuery holds an empty array, so
// ARRAY_TO_STRING of it reads NULL, not '' (#1124).
//
// Other functions of the engine fail so on a NULL argument (JSON_OBJECT's
// key, the step of GENERATE_DATE_ARRAY and GENERATE_TIMESTAMP_ARRAY,
// MAKE_INTERVAL, ST_BUFFER's radius and others, #1121). Since
// the emulator answers 500 only for a panic (a query it cannot run is
// 400), the front answers a 500 to a query (jobs.query, or jobs.insert of
// a query job) 400 invalidQuery with the emulator's text (noRetry), which
// the client does not retry: the same query fails the same way each time.

// nullSafeFuncs are the functions sent guarded (above), by upper-case
// name.
var nullSafeFuncs = map[string]bool{
	"ARRAY_TO_STRING": true, "ARRAY_CONCAT": true, "ARRAY_REVERSE": true,
	"NET.IP_TO_STRING": true, "NET.IP_TRUNC": true, "NET.IPV4_FROM_INT64": true, "NET.IPV4_TO_INT64": true,
	"NET.PUBLIC_SUFFIX": true, "NET.REG_DOMAIN": true,
}

// guardNullArguments returns sql, whose query parameters are params,
// with each call of a nullSafeFuncs function sent as above, and whether it
// changed any.
func guardNullArguments(sql string, params json.RawMessage) (string, bool) {
	toks, ok := lex(sql)
	if !ok {
		return sql, false
	}
	g := guard{sql: sql, nulls: nullParameters(params)}
	out, changed := g.tokens(toks)
	if !changed {
		return sql, false
	}
	return sql[:toks[0].pos] + out + sql[toks[len(toks)-1].end:], true
}

// guard is guardNullArguments' query text, and whether each named
// parameter's value is NULL, by lower-case name.
type guard struct {
	sql   string
	nulls map[string]bool
}

// nullParameters returns whether each named parameter in params has a
// NULL value (no value, no arrayValues and no structValues), by lower-case
// name.
func nullParameters(params json.RawMessage) map[string]bool {
	var ps []struct {
		Name  string         `json:"name"`
		Value map[string]any `json:"parameterValue"`
	}
	if len(params) == 0 || json.Unmarshal(params, &ps) != nil {
		return nil
	}
	out := map[string]bool{}
	for _, p := range ps {
		v, hasValue := p.Value["value"]
		_, arr := p.Value["arrayValues"]
		_, st := p.Value["structValues"]
		out[strings.ToLower(p.Name)] = (!hasValue || v == nil) && !arr && !st
	}
	return out
}

// tokens is guardNullArguments of toks, the tokens of g.sql[toks[0].pos:
// toks[len-1].end]; it returns that text.
func (g guard) tokens(toks []token) (string, bool) {
	sql := g.sql
	if len(toks) == 0 {
		return "", false
	}
	var b strings.Builder
	last := toks[0].pos
	changed := false
	for i := 0; i < len(toks); i++ {
		first, open := nullSafeCall(toks, i)
		if open == 0 {
			continue
		}
		end := closeParen(toks, open)
		if end < 0 {
			break
		}
		args := splitArgs(toks[open+1 : end])
		if len(args) == 0 || hasPositional(toks[open+1:end]) {
			continue
		}
		start := first
		if first >= 2 && toks[first-1].punct(".") && toks[first-2].is("SAFE") {
			start = first - 2
		}
		texts := make([]string, len(args))
		var conds []string
		null := false
		for j, a := range args {
			texts[j], _ = g.tokens(a)
			if len(a) == 2 && a[0].punct("@") && a[1].pos == a[0].end && (a[1].kind == tokWord || a[1].kind == tokQuoted) {
				// A parameter alone: its value is known. The emulator
				// types a NULL one by where it is used, so IS NULL of it
				// beside the call is refused ("Undeclared parameter ...
				// used assuming different types", measured).
				if isNull, ok := g.nulls[strings.ToLower(a[1].text)]; ok {
					null = null || isNull
					continue
				}
			}
			conds = append(conds, "("+texts[j]+") IS NULL")
		}
		switch {
		case null:
			conds = []string{"TRUE"}
		case len(conds) == 0:
			conds = []string{"FALSE"}
		}
		b.WriteString(sql[last:toks[start].pos])
		b.WriteString("IF(" + strings.Join(conds, " OR ") + ", NULL, " + sql[toks[start].pos:toks[open].end] +
			strings.Join(texts, ", ") + "))")
		last = toks[end].end
		changed = true
		i = end
	}
	b.WriteString(sql[last:toks[len(toks)-1].end])
	return b.String(), changed
}

// nullSafeCall returns, for a call of a nullSafeFuncs function whose name
// ends at toks[i], the index of its name's first token and of its "(";
// open is 0 for anything else.
func nullSafeCall(toks []token, i int) (first, open int) {
	if toks[i].kind != tokWord {
		return 0, 0
	}
	name := strings.ToUpper(toks[i].text)
	first = i
	if i >= 2 && toks[i-1].punct(".") && toks[i-2].is("NET") {
		name, first = "NET."+name, i-2
	}
	open = i + 1
	if open >= len(toks) || !toks[open].punct("(") || !nullSafeFuncs[name] {
		return 0, 0
	}
	// A name after a "." is a function of a dataset, or a field, unless
	// the "." follows SAFE.
	if first >= 1 && toks[first-1].punct(".") && !(first >= 2 && toks[first-2].is("SAFE") &&
		(first < 3 || !toks[first-3].punct("."))) {
		return 0, 0
	}
	return first, open
}

// splitArgs splits a call's argument tokens at the commas outside
// parentheses and brackets; nil when an argument is empty.
func splitArgs(toks []token) [][]token {
	var args [][]token
	depth, from := 0, 0
	for i, t := range toks {
		switch {
		case t.punct("(") || t.punct("["):
			depth++
		case t.punct(")") || t.punct("]"):
			depth--
		case depth == 0 && t.punct(","):
			if i == from {
				return nil
			}
			args = append(args, toks[from:i])
			from = i + 1
		}
	}
	if from >= len(toks) {
		return nil
	}
	return append(args, toks[from:])
}

// hasPositional reports whether toks hold a positional parameter.
func hasPositional(toks []token) bool {
	for _, t := range toks {
		if t.punct("?") {
			return true
		}
	}
	return false
}

// queryFailures notes whether the emulator answered a query sent through
// it (jobs.query or jobs.insert) 500: that is a failure of the query, where
// a 500 to anything else (a datasets.insert the front makes first, say) is
// not.
type queryFailures struct{ failed bool }

// watch returns next, noting in q each 500 it answers to a query.
func (q *queryFailures) watch(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || jobsRoute.FindStringSubmatch(r.URL.EscapedPath()) == nil {
			next.ServeHTTP(w, r)
			return
		}
		rec := newRecorder()
		next.ServeHTTP(rec, r)
		if rec.status == http.StatusInternalServerError {
			q.failed = true
		}
		rec.copyTo(w)
	})
}

// noRetry answers w with rec, a query's answer (jobs.query, or jobs.insert
// of a query job), with a 500 answered 400 invalidQuery (above) when the
// emulator answered a query so (q).
func noRetry(w http.ResponseWriter, rec *recorder, q *queryFailures) {
	if rec.status != http.StatusInternalServerError || !q.failed {
		rec.copyTo(w)
		return
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.body.Bytes(), &e)
	msg := e.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(rec.body.String())
	}
	writeError(w, http.StatusBadRequest, "invalidQuery", "The emulator behind CloudBurrow failed on this query: "+msg+
		" (its answer was 500 internalError, which a client retries; CloudBurrow answers 400, as the query fails the same way "+
		"each time; see docs/compatibility.md).")
}
