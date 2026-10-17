package bigqueryfront

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Materialized view statements in a script with a TEMP table or function
// (#1434).
//
// A materialized view's CREATE, ALTER and DROP, and CALL
// BQ.REFRESH_MATERIALIZED_VIEW, are carried out by the front a statement
// at a time, each its own request and child job (#1415,
// materializedviews.go). A TEMP table or function lasts only for the
// engine request that made it, so a script with one was 501. BigQuery
// keeps a TEMP table, and a TEMP function, for the script
// (https://cloud.google.com/bigquery/docs/multi-statement-queries#temporary_tables).
//
// So a script of several statements with a TEMP table or function and a
// materialized view statement is run, as #1416 runs DDL given by EXECUTE
// IMMEDIATE in such a script, as the text it amounts to: each TEMP table
// is a table of the hidden results dataset for the script (CREATE TEMP
// TABLE t is CREATE TABLE of it, and each name of t where a table is named
// is its path, AS t), each TEMP function a function there (each call of
// it the function's path), and that text, with no TEMP left, is carried
// out by the front a statement at a time, as a script with no TEMP table
// is, with its child jobs. Once it is done, failed or not, the tables and
// functions are deleted. The child jobs' texts and the answer's messages
// name the script's own names. A materialized view's query cannot read a
// TEMP table (BigQuery: a materialized view references base tables only):
// such a statement is 400, nothing run.

// tempSession is a script's TEMP tables and functions made tables and
// functions of the hidden dataset (above).
type tempSession struct {
	orig, text      string
	tables, funcs   []string // the hidden names made
	pairs           [][2]string
	readsTempFromMV bool
	// mv: the script has a materialized view statement (#1434).
	mv bool
	// tableNames and funcNames are the hidden paths, by lower-case name
	// (rename).
	tableNames, funcNames map[string]string
}

// sessionKey marks the request of a session's script (tempSession), its
// value the *tempSession.
type sessionKey struct{}

// tempEdit replaces sql[pos:end] with with.
type tempEdit struct {
	pos, end int
	with     string
}

// tempSessionScript reads sql as a script of several statements with a
// TEMP table or function and no transaction, and returns it as the text it
// amounts to (above), or false for any other query. Whether it is run so
// is useSession's verdict.
func tempSessionScript(sql string) (*tempSession, bool) {
	upper := strings.ToUpper(sql)
	if !strings.Contains(upper, "TEMP") || !strings.Contains(sql, ";") {
		return nil, false
	}
	toks, ok := lex(sql)
	if !ok || !tokensHaveTemp(toks) {
		return nil, false
	}
	stmts, mv := 0, false
	for _, st := range splitStatements(toks) {
		body, _, _ := stripControlFlow(st)
		if len(st) == 0 || len(st) == 1 && st[0].punct(";") {
			continue
		}
		if isTransaction(body) {
			return nil, false // transactions.go keeps its own TEMP tables
		}
		stmts++
		if len(body) > 0 {
			if ms, ok := parseMVStatement(sql[body[0].pos:body[len(body)-1].end]); ok && ms.kind != "" {
				mv = true
			}
		}
	}
	if stmts < 2 {
		return nil, false
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	prefix := scratchPrefix + "session_" + hex.EncodeToString(b) + "_"
	s := &tempSession{orig: sql, mv: mv}
	tables, funcs := map[string]string{}, map[string]string{} // lower-case name: hidden name
	created := map[string]int{}                               // lower-case table name: where it is first made
	var edits []tempEdit
	hidden := func(name string) string { return quotePath([]string{resultsDataset, prefix + strings.ToLower(name)}) }
	// The TEMP tables' and functions' statements.
	for _, st := range splitStatements(toks) {
		body, _, _ := stripControlFlow(st)
		if len(body) < 4 || !body[0].is("CREATE") {
			continue
		}
		i := 1
		if body[i].is("OR") && i+1 < len(body) && body[i+1].is("REPLACE") {
			i += 2
		}
		if !(body[i].is("TEMP") || body[i].is("TEMPORARY")) {
			continue
		}
		tempTok := body[i]
		i++
		kind := ""
		switch {
		case i+1 < len(body) && body[i].is("TABLE") && body[i+1].is("FUNCTION"):
			kind = "function" // a TEMP table function (#1459)
			i++
		case i < len(body) && body[i].is("TABLE"):
			kind = "table"
		case i < len(body) && body[i].is("FUNCTION"):
			kind = "function"
		case i+1 < len(body) && body[i].is("AGGREGATE") && body[i+1].is("FUNCTION"):
			kind = "function"
			i++
		default:
			return nil, false
		}
		i++
		if i+2 < len(body) && body[i].is("IF") && body[i+1].is("NOT") && body[i+2].is("EXISTS") {
			i += 3
		}
		parts, next := path(body, i)
		name, ok := tempName(parts)
		if !ok || next <= i {
			return nil, false
		}
		h := hidden(name)
		if kind == "table" {
			if _, seen := tables[strings.ToLower(name)]; !seen {
				s.tables = append(s.tables, prefix+strings.ToLower(name))
				created[strings.ToLower(name)] = body[0].pos
			}
			tables[strings.ToLower(name)] = h
		} else {
			if _, seen := funcs[strings.ToLower(name)]; !seen {
				s.funcs = append(s.funcs, prefix+strings.ToLower(name))
			}
			funcs[strings.ToLower(name)] = h
		}
		// "TEMP " goes, and the name is the hidden one.
		with := sql[tempTok.end:body[i].pos] + h
		with = strings.TrimLeft(with, " \t\n")
		edits = append(edits, tempEdit{tempTok.pos, body[next-1].end, with})
		s.pairs = append(s.pairs, [2]string{with, sql[tempTok.pos:body[next-1].end]})
		s.pairs = append(s.pairs, [2]string{h, name})
	}
	if len(tables) == 0 && len(funcs) == 0 {
		return nil, false
	}
	s.tableNames, s.funcNames = tables, funcs
	skip := map[int]bool{}
	for _, e := range edits {
		for k, t := range toks {
			if t.pos >= e.pos && t.end <= e.end {
				skip[k] = true
			}
		}
	}
	refs, pairs, readsMV := renameTemps(sql, toks, skip, tables, funcs, created)
	edits = append(edits, refs...)
	s.pairs = append(s.pairs, pairs...)
	s.readsTempFromMV = readsMV
	s.text = applyTempEdits(sql, edits)
	if rt, ok := lex(s.text); !ok || tokensHaveTemp(rt) {
		return nil, false // a TEMP form not read here stays as it was (checkDDL)
	}
	s.sortPairs()
	return s, true
}

// sortPairs puts the pairs longest first, so that "path AS t" is restored
// as t, not "t AS t".
func (s *tempSession) sortPairs() {
	sort.SliceStable(s.pairs, func(i, j int) bool { return len(s.pairs[i][0]) > len(s.pairs[j][0]) })
}

// applyTempEdits is sql with edits made.
func applyTempEdits(sql string, edits []tempEdit) string {
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].pos < edits[j].pos })
	var out strings.Builder
	last := 0
	for _, e := range edits {
		if e.pos < last {
			continue
		}
		out.WriteString(sql[last:e.pos])
		out.WriteString(e.with)
		last = e.end
	}
	out.WriteString(sql[last:])
	return out.String()
}

// renameTemps are the edits naming each TEMP table of tables where toks
// name a table, and calling each TEMP function of funcs, as their hidden
// paths; with the pairs that restore them. A table named before created
// gives its position (where the script first makes it) is another table
// of that name, and kept. readsMV: a materialized view's statement names
// one.
func renameTemps(sql string, toks []token, skip map[int]bool, tables, funcs map[string]string, created map[string]int) (edits []tempEdit, pairs [][2]string, readsMV bool) {
	// The names of a TEMP table where a table is named, and the calls of
	// a TEMP function.
	type frame struct{ from bool }
	stack := []frame{{}}
	inMV := false
	for k := 0; k < len(toks); k++ {
		t := toks[k]
		top := &stack[len(stack)-1]
		switch {
		case t.punct("("):
			stack = append(stack, frame{})
			continue
		case t.punct(")"):
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			continue
		case t.punct(";"):
			stack, inMV = []frame{{}}, false
			continue
		case t.is("MATERIALIZED") && k+1 < len(toks) && toks[k+1].is("VIEW"):
			inMV = true
		case t.is("FROM") || t.is("JOIN"):
			top.from = true
		case t.is("WHERE") || t.is("GROUP") || t.is("HAVING") || t.is("QUALIFY") || t.is("WINDOW") || t.is("ORDER") ||
			t.is("LIMIT") || t.is("UNION") || t.is("INTERSECT") || t.is("EXCEPT") || t.is("SELECT") ||
			t.is("SET") || t.is("WHEN") || t.is("VALUES"):
			top.from = false
		}
		if skip[k] || t.kind != tokWord && t.kind != tokQuoted {
			continue
		}
		prevDot := k > 0 && toks[k-1].punct(".")
		nextDot := k+1 < len(toks) && toks[k+1].punct(".")
		name := strings.ToLower(t.text)
		start := k
		if t.is("_SESSION") && nextDot && k+2 < len(toks) && !prevDot {
			name, start, k = strings.ToLower(toks[k+2].text), k, k+2
			nextDot = k+1 < len(toks) && toks[k+1].punct(".")
		} else if prevDot || nextDot {
			continue
		}
		if nextDot {
			continue
		}
		if h, ok := funcs[name]; ok && k+1 < len(toks) && toks[k+1].punct("(") {
			edits = append(edits, tempEdit{toks[start].pos, t.end, h})
			if start != k {
				edits[len(edits)-1].end = toks[k].end
			}
			continue
		}
		h, ok := tables[name]
		if !ok || start == 0 {
			continue
		}
		if at, made := created[name]; made && toks[start].pos < at {
			continue // before the script makes it: another table of that name
		}
		prev := toks[start-1]
		alias := false
		switch {
		case prev.is("FROM") || prev.is("JOIN") || prev.is("UPDATE") || prev.is("MERGE") || prev.is("DELETE") ||
			prev.punct(",") && top.from:
			alias = true
		case prev.is("USING"):
			alias = true
		case prev.is("INTO") && start >= 2 && toks[start-2].is("MERGE"):
			alias = true
		case prev.is("INTO") || prev.is("TABLE") || prev.is("INSERT") || prev.is("EXISTS") && start >= 3 && toks[start-3].is("TABLE"):
		default:
			continue
		}
		if inMV {
			readsMV = true
		}
		with := h
		if alias && !tableAliased(toks, k+1) {
			with += " AS " + quotePath([]string{toks[k].text})
		}
		edits = append(edits, tempEdit{toks[start].pos, toks[k].end, with})
		pairs = append(pairs, [2]string{with, sql[toks[start].pos:toks[k].end]})
	}
	return edits, pairs, readsMV
}

// rename is text, a statement the script runs that is not in its text
// (EXECUTE IMMEDIATE's), with the session's TEMP tables and functions
// named as they are in the session's text (#1459).
func (s *tempSession) rename(text string) string {
	toks, ok := lex(text)
	if !ok {
		return text
	}
	edits, pairs, _ := renameTemps(text, toks, nil, s.tableNames, s.funcNames, nil)
	if len(edits) == 0 {
		return text
	}
	s.pairs = append(s.pairs, pairs...)
	s.sortPairs()
	return applyTempEdits(text, edits)
}

// tableAliased reports whether toks[k] begins an alias of the table named
// before it: AS, or a name that is not a keyword of what may follow.
func tableAliased(toks []token, k int) bool {
	if k >= len(toks) {
		return false
	}
	t := toks[k]
	if t.is("AS") || t.kind == tokQuoted {
		return true
	}
	if t.kind != tokWord {
		return false
	}
	switch strings.ToUpper(t.text) {
	case "WHERE", "JOIN", "INNER", "LEFT", "RIGHT", "FULL", "CROSS", "ON", "USING", "GROUP", "ORDER", "LIMIT", "UNION",
		"INTERSECT", "EXCEPT", "WINDOW", "QUALIFY", "HAVING", "SET", "WHEN", "FOR", "TABLESAMPLE", "PIVOT", "UNPIVOT",
		"WITH", "SELECT", "INSERT", "VALUES", "OPTIONS", "PARTITION", "CLUSTER", "THEN", "END", "ELSE", "DO", "LOOP":
		return false
	}
	return true
}

// restore is text with the session's hidden names its own again.
func (s *tempSession) restore(text string) string {
	for _, p := range s.pairs {
		text = strings.ReplaceAll(text, p[0], p[1])
	}
	for _, p := range s.pairs {
		// As the engine's messages name a table: dataset.table, or
		// project:dataset.table.
		bare := strings.Trim(p[0], "`")
		if strings.Contains(bare, " ") {
			continue
		}
		text = strings.ReplaceAll(text, bare, p[1])
	}
	return text
}

// sessionOf is the session of r's script, or nil.
func sessionOf(r *http.Request) *tempSession {
	s, _ := r.Context().Value(sessionKey{}).(*tempSession)
	return s
}

// runTempSession carries out s, the script of q (above).
func (f front) runTempSession(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool, s *tempSession) {
	if s.readsTempFromMV {
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", http.StatusBadRequest, rowError{Reason: "invalidQuery",
			Message: "A materialized view cannot reference a temporary table; it reads base tables only. Nothing was run."})
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey{}, s))
	if !setQueryText(r, insert, s.text) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return
	}
	client := jobText{query: q.Query}
	q.Query = s.text
	rec := newRecorder()
	// A statement at a time (#1459), each its own request and child job,
	// as the hidden tables last for the script.
	f.runScript(rec, r, q, insert)
	for _, t := range s.tables {
		f.send(r, http.MethodDelete, tablePath(resultsDataset, t), nil)
	}
	for _, fn := range s.funcs {
		f.send(r, http.MethodDelete, "/datasets/"+url.PathEscape(resultsDataset)+"/routines/"+url.PathEscape(fn), nil)
	}
	body := s.restore(rec.body.String())
	rec.body.Reset()
	rec.body.WriteString(body)
	f.clientText(w, rec, client)
}

// useSession reports whether s's script is run as its session's text
// (#1459): when it has a materialized view statement (#1434), when the
// front carries the text out a statement at a time (control flow, an
// ALTER TABLE, a query before the last statement, an ASSERT, EXECUTE
// IMMEDIATE, @@row_count, OPTIONS, a view's column list), and for every
// other script whose statements are not refused when each runs alone
// (splitCures), as BigQuery runs every script a statement at a time, each
// a child job, a failure keeping what the statements before it did. A
// TEMP table or function lasts only for the engine request that made it,
// and the hidden tables and functions of the session for the script.
func (s *tempSession) useSession() bool {
	if s.mv || hasControlFlow(s.text) || runsInTurn(s.text) || earlierQuery(s.text) == scriptSplits {
		return true
	}
	return splitCures(s.text)
}
