package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// DML statements (#1008).
//
// The emulator runs INSERT, UPDATE, DELETE, MERGE and TRUNCATE TABLE and
// changes the rows as BigQuery does, but reports none of it: measured
// against the pinned image through the front, a query job of each
// answered statistics.query.statementType "SELECT" and no
// numDmlAffectedRows or dmlStats, in jobs.insert's answer, jobs.get and
// jobs.list, and jobs.query answered with neither. BigQuery reports the
// statement's type and the rows it changed
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobStatistics2,
// https://cloud.google.com/bigquery/docs/reference/rest/v2/jobs/query#response-body),
// and the official clients read them (the Go client's
// QueryStatistics.NumDMLAffectedRows and DMLStats). And a MERGE whose
// source is a subquery failed 400 "MERGE: source must be a single-table
// reference, got *googlesql.ResolvedProjectScan", where BigQuery takes any
// table expression.
//
// So the front counts a lone DML statement's rows itself (serveDML),
// from the table, with queries of its own that change nothing:
//
//   - INSERT, DELETE and TRUNCATE TABLE: the table's rows are counted
//     before and after the statement; the difference is what it inserted
//     or deleted.
//   - UPDATE: the rows its WHERE matches are counted before it runs,
//     SELECT COUNT(*) FROM target WHERE cond. BigQuery counts a row it
//     updates whether or not its values change, and so does this. An
//     UPDATE with a FROM clause is sent on as it was: the emulator refuses
//     it (measured: 400 "Update with joins not supported").
//   - MERGE: each WHEN clause's rows are counted before it runs, as
//     BigQuery picks them: a pair of target and source rows ON matches
//     takes the first WHEN MATCHED clause whose AND condition holds; a
//     source row no target row matches, the first WHEN NOT MATCHED [BY
//     TARGET]; a target row no source row matches, the first WHEN NOT
//     MATCHED BY SOURCE. The inserted, updated and deleted rows are the
//     sums of the INSERT, UPDATE and DELETE clauses'.
//
// A MERGE from a subquery is run from a table: the subquery is made a
// table of the front's in the target's dataset (CREATE TABLE ... AS, with
// the query's parameters), the statement is sent with the table in its
// place (and the subquery's alias), and the table is deleted after it. The
// job shows the client's text (jobTexts). Inside a script of several
// statements, where the front cannot do that between them, such a MERGE
// is 501 (mergeFromSubquery).
//
// The counts are exact as long as nothing else writes the table while the
// statement runs: the emulator serves one instance's clients, and the
// front does not lock the table. A DML statement inside a script is not
// counted: its job reports what the emulator reports.

// dmlStmt is a lone DML statement.
type dmlStmt struct {
	// kind is its statementType: INSERT, UPDATE, DELETE, MERGE or
	// TRUNCATE_TABLE.
	kind string
	// target is the table's path, pathText the path as written, and
	// targetText the path and its alias as written.
	target               []string
	pathText, targetText string
	// where is an UPDATE's WHERE condition, as written.
	where string
	// source is a MERGE's source as written, with its alias; subquery is
	// the source's query when it is one, at subPos:subEnd in the text
	// (parentheses included), and aliasText its alias as written, or "".
	source         string
	subquery       string
	subPos, subEnd int
	aliasText      string
	on             string
	clauses        []mergeClause
}

// mergeClause is one WHEN clause of a MERGE.
type mergeClause struct {
	// group is which rows the clause is for: matchedRows,
	// notMatchedByTarget or notMatchedBySource.
	group int
	// cond is its AND condition as written, or "".
	cond string
	// action is UPDATE, DELETE or INSERT.
	action string
}

const (
	matchedRows = iota
	notMatchedByTarget
	notMatchedBySource
)

// dmlCounts are what a DML statement did, as the front reports them.
type dmlCounts struct {
	statementType              string
	inserted, updated, deleted int64
}

func (d dmlCounts) affected() int64 { return d.inserted + d.updated + d.deleted }

// dmlStats is a DmlStats resource: each count, as a string (the REST
// API's int64), left out when it is 0.
func (d dmlCounts) dmlStats() map[string]any {
	s := map[string]any{}
	for k, n := range map[string]int64{"insertedRowCount": d.inserted, "updatedRowCount": d.updated, "deletedRowCount": d.deleted} {
		if n != 0 {
			s[k] = strconv.FormatInt(n, 10)
		}
	}
	return s
}

// patch puts the counts in a Job resource (statistics.query), a
// QueryResponse (numDmlAffectedRows and dmlStats) or a
// GetQueryResultsResponse (numDmlAffectedRows), whichever obj is.
func (d dmlCounts) patch(obj map[string]any) {
	n := strconv.FormatInt(d.affected(), 10)
	_, conf := obj["configuration"]
	_, stats := obj["statistics"]
	switch {
	case conf || stats:
		st, _ := obj["statistics"].(map[string]any)
		if st == nil {
			st = map[string]any{}
			obj["statistics"] = st
		}
		q, _ := st["query"].(map[string]any)
		if q == nil {
			q = map[string]any{}
			st["query"] = q
		}
		q["statementType"] = d.statementType
		q["numDmlAffectedRows"] = n
		q["dmlStats"] = d.dmlStats()
	case obj["kind"] == "bigquery#getQueryResultsResponse":
		obj["numDmlAffectedRows"] = n
	default:
		obj["numDmlAffectedRows"] = n
		obj["dmlStats"] = d.dmlStats()
	}
}

// runQuery serves a query from jobs.query or (insert) a query job: a lone
// DML statement through serveDML, a script with a MERGE from a subquery
// 501, a query with CREATE VIEW through keepViewTexts (#1014), anything
// else through serveQuery.
func (f front) runQuery(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) {
	if q.UseLegacySQL != nil && *q.UseLegacySQL || dryRun(r, insert) {
		f.serveQuery(w, r, q, insert)
		return
	}
	if sent, changed, code, msg := bytesParameters(q); code != 0 { // #1078, bytesparams.go
		reason := "notImplemented"
		if code == http.StatusBadRequest {
			reason = "invalidQuery"
		}
		writeError(w, code, reason, msg)
		return
	} else if changed {
		f.runBytesQuery(w, r, q, sent, insert)
		return
	}
	v := checkDDL(q.Query)
	if v.code != 0 {
		f.serveQuery(w, r, q, insert)
		return
	}
	if v.statements == 1 {
		if _, ok := parseDML(q.Query); ok {
			// Its table names qualified first (#1015), as serveQuery
			// qualifies them, so its counts are of the tables it changes.
			f.rewriteQuery(w, r, q, insert, func(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) {
				if d, ok := parseDML(q.Query); ok {
					f.serveDML(w, r, q, d, insert)
					return
				}
				f.checkQuery(w, r, q, insert)
			})
			return
		}
	} else if msg := mergeFromSubquery(q.Query); msg != "" {
		writeError(w, http.StatusNotImplemented, "notImplemented", msg)
		return
	}
	if f.views != nil && madeViews(v) {
		f.keepViewTexts(w, r, q, v, insert)
		return
	}
	f.serveQuery(w, r, q, insert)
}

// dryRun reports whether r's query is a dry run: jobs.query's dryRun, or
// (insert) a job's configuration.dryRun.
func dryRun(r *http.Request, insert bool) bool {
	b, err := readBody(r)
	if err != nil {
		return false
	}
	var body struct {
		DryRun        bool `json:"dryRun"`
		Configuration struct {
			DryRun bool `json:"dryRun"`
		} `json:"configuration"`
	}
	if json.Unmarshal(b, &body) != nil {
		return false
	}
	if insert {
		return body.Configuration.DryRun
	}
	return body.DryRun
}

// serveDML runs a lone DML statement and reports its counts (above). A
// statement whose table the front cannot read or count is sent on as
// serveQuery sends it, and the emulator answers it.
func (f front) serveDML(w http.ResponseWriter, r *http.Request, q queryOptions, d dmlStmt, insert bool) {
	ds, table, ok := tableOf(q, d.target)
	if !ok {
		f.serveQuery(w, r, q, insert)
		return
	}
	status, got := f.get(r, tablePath(ds, table))
	var meta struct {
		Type string `json:"type"`
	}
	if status != http.StatusOK || json.Unmarshal(got, &meta) != nil || meta.Type != "" && !strings.EqualFold(meta.Type, "TABLE") {
		f.serveQuery(w, r, q, insert)
		return
	}
	counts := dmlCounts{statementType: d.kind}
	text := q.Query
	var before int64
	switch d.kind {
	case "INSERT", "DELETE", "TRUNCATE_TABLE":
		n, ok := f.countRows1(r, q, "SELECT COUNT(*) FROM "+d.pathText)
		if !ok {
			f.serveQuery(w, r, q, insert)
			return
		}
		before = n[0]
	case "UPDATE":
		n, ok := f.countRows1(r, q, "SELECT COUNT(*) FROM "+d.targetText+" WHERE "+d.where)
		if !ok {
			f.serveQuery(w, r, q, insert)
			return
		}
		counts.updated = n[0]
	case "MERGE":
		source := d.source
		if d.subquery != "" {
			scratch := strings.Replace(scratchTable(), "_replace_", "_merge_", 1)
			scratchPath := quotePath(append(append([]string{}, d.target[:len(d.target)-1]...), scratch))
			body, err := json.Marshal(queryOptions{Query: "CREATE TABLE " + scratchPath + " AS " + d.subquery,
				UseLegacySQL: q.UseLegacySQL, DefaultDataset: q.DefaultDataset, ParameterMode: q.ParameterMode,
				QueryParameters: q.QueryParameters})
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internalError", err.Error())
				return
			}
			status, got := f.send(r, http.MethodPost, "/queries", body)
			if status != http.StatusOK {
				// The subquery does not run: its error is the statement's.
				f.send(r, http.MethodDelete, tablePath(ds, scratch), nil)
				writeRaw(w, status, []byte(strings.ReplaceAll(string(got), scratch, "the MERGE's source")))
				return
			}
			defer f.send(r, http.MethodDelete, tablePath(ds, scratch), nil)
			text = q.Query[:d.subPos] + scratchPath + q.Query[d.subEnd:]
			source = scratchPath + " " + d.aliasText
		}
		n, ok := f.countRows1(r, q, mergeCounts(d, source))
		if !ok || len(n) != len(d.clauses) {
			f.serveQuery(w, r, q, insert)
			return
		}
		for i, c := range d.clauses {
			switch c.action {
			case "INSERT":
				counts.inserted += n[i]
			case "UPDATE":
				counts.updated += n[i]
			case "DELETE":
				counts.deleted += n[i]
			}
		}
	}
	var client jobText
	if text != q.Query {
		if !setQueryText(r, insert, text) {
			writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
			return
		}
		client.query = q.Query
	}
	rec := newRecorder()
	f.next.ServeHTTP(rec, r)
	var job map[string]any
	if insert && rec.status == http.StatusOK {
		_ = json.Unmarshal(rec.body.Bytes(), &job)
	}
	if _, failed := queryFailure(rec, job); failed {
		f.answer(w, rec, client)
		return
	}
	switch d.kind {
	case "INSERT", "DELETE", "TRUNCATE_TABLE":
		n, ok := f.countRows1(r, q, "SELECT COUNT(*) FROM "+d.pathText)
		if !ok {
			// The statement ran, and what it did cannot be told.
			f.answer(w, rec, client)
			return
		}
		if d.kind == "INSERT" {
			counts.inserted = n[0] - before
		} else {
			counts.deleted = before - n[0]
		}
	}
	client.dml = &counts
	f.answer(w, rec, client)
}

// countRows1 runs sel, a query giving one row of INT64 columns, as the
// statement q's options give it (its default dataset and parameters), and
// returns the row's values.
func (f front) countRows1(r *http.Request, q queryOptions, sel string) ([]int64, bool) {
	legacy := false
	body, err := json.Marshal(queryOptions{Query: sel, UseLegacySQL: &legacy, DefaultDataset: q.DefaultDataset,
		ParameterMode: q.ParameterMode, QueryParameters: q.QueryParameters})
	if err != nil {
		return nil, false
	}
	status, got := f.send(r, http.MethodPost, "/queries", body)
	var res struct {
		Rows []struct {
			F []struct {
				V any `json:"v"`
			} `json:"f"`
		} `json:"rows"`
	}
	if status != http.StatusOK || json.Unmarshal(got, &res) != nil || len(res.Rows) != 1 {
		return nil, false
	}
	out := make([]int64, 0, len(res.Rows[0].F))
	for _, c := range res.Rows[0].F {
		s, _ := c.V.(string)
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// mergeCounts is the query that counts the rows each of a MERGE's clauses
// takes (above), one column per clause, in order, source being the
// source's table (or subquery) and alias.
func mergeCounts(d dmlStmt, source string) string {
	holds := func(cond string) string {
		if cond == "" {
			return "TRUE"
		}
		return "COALESCE((" + cond + "), FALSE)"
	}
	var parts [3][]string
	var prior [3][]string
	for i, c := range d.clauses {
		pick := append(append([]string{}, prior[c.group]...), holds(c.cond))
		parts[c.group] = append(parts[c.group], fmt.Sprintf("COUNTIF(%s) AS c%d", strings.Join(pick, " AND "), i))
		prior[c.group] = append(prior[c.group], "NOT "+holds(c.cond))
	}
	from := [3]string{
		matchedRows:        "FROM " + d.targetText + " INNER JOIN " + source + " ON " + d.on,
		notMatchedByTarget: "FROM " + source + " WHERE NOT EXISTS (SELECT 1 FROM " + d.targetText + " WHERE " + d.on + ")",
		notMatchedBySource: "FROM " + d.targetText + " WHERE NOT EXISTS (SELECT 1 FROM " + source + " WHERE " + d.on + ")",
	}
	var subs []string
	for g := range parts {
		if len(parts[g]) > 0 {
			subs = append(subs, "(SELECT "+strings.Join(parts[g], ", ")+" "+from[g]+")")
		}
	}
	cols := make([]string, len(d.clauses))
	for i := range d.clauses {
		cols[i] = fmt.Sprintf("c%d", i)
	}
	return "SELECT " + strings.Join(cols, ", ") + " FROM " + strings.Join(subs, " CROSS JOIN ")
}

// parseDML reads sql as one INSERT, UPDATE, DELETE, MERGE or TRUNCATE
// TABLE statement, and reports false for anything else, or a statement
// whose parts it cannot find (which is then sent on as it is).
func parseDML(sql string) (dmlStmt, bool) {
	var d dmlStmt
	toks, ok := lex(sql)
	if !ok {
		return d, false
	}
	var t []token
	for _, s := range splitStatements(toks) {
		if len(s) == 0 {
			continue
		}
		if t != nil {
			return d, false
		}
		t = s
	}
	if len(t) < 2 || t[0].kind != tokWord {
		return d, false
	}
	text := func(a, b int) string { return sql[t[a].pos:t[b-1].end] }
	readPath := func(i int) ([]string, int, bool) {
		parts, j := path(t, i)
		return parts, j, len(parts) > 0 && j > i
	}
	switch strings.ToUpper(t[0].text) {
	case "INSERT", "DELETE", "TRUNCATE":
		i := 1
		switch {
		case t[0].is("TRUNCATE") && !t[1].is("TABLE"):
			return d, false
		case t[0].is("TRUNCATE"), t[0].is("INSERT") && t[1].is("INTO"), t[0].is("DELETE") && t[1].is("FROM"):
			i = 2
		}
		parts, j, ok := readPath(i)
		if !ok {
			return d, false
		}
		d.kind = strings.ToUpper(t[0].text)
		if d.kind == "TRUNCATE" {
			d.kind = "TRUNCATE_TABLE"
		}
		d.target, d.pathText = parts, text(i, j)
		return d, true
	case "UPDATE":
		parts, j, ok := readPath(1)
		if !ok {
			return d, false
		}
		k := aliasEnd(t, j, "SET")
		if k >= len(t) || !t[k].is("SET") {
			return d, false
		}
		d.kind, d.target, d.pathText, d.targetText = "UPDATE", parts, text(1, j), text(1, k)
		wh := topWord(t, k+1, "FROM", "WHERE")
		if wh < 0 || t[wh].is("FROM") || wh+1 >= len(t) {
			return d, false
		}
		d.where = text(wh+1, len(t))
		return d, true
	case "MERGE":
		return parseMerge(sql, t)
	}
	return d, false
}

// parseMerge reads a MERGE statement's tokens t (parseDML).
func parseMerge(sql string, t []token) (dmlStmt, bool) {
	var d dmlStmt
	text := func(a, b int) string { return sql[t[a].pos:t[b-1].end] }
	i := 1
	if t[i].is("INTO") {
		i++
	}
	parts, j := path(t, i)
	if len(parts) == 0 || j == i {
		return d, false
	}
	k := aliasEnd(t, j, "USING")
	if k >= len(t) || !t[k].is("USING") || k+1 >= len(t) {
		return d, false
	}
	d.kind, d.target, d.pathText, d.targetText = "MERGE", parts, text(i, j), text(i, k)
	s := k + 1
	e := s
	if t[s].punct("(") {
		c := closeParen(t, s)
		if c < 0 || c == s+1 {
			return d, false
		}
		d.subquery, d.subPos, d.subEnd = text(s+1, c), t[s].pos, t[c].end
		e = c + 1
	} else {
		var src []string
		if src, e = path(t, s); len(src) == 0 || e == s {
			return d, false
		}
	}
	m := aliasEnd(t, e, "ON")
	if m >= len(t) || !t[m].is("ON") {
		return d, false
	}
	d.source = text(s, m)
	if m > e {
		d.aliasText = text(e, m)
	}
	w := topWord(t, m+1, "WHEN")
	if w <= m+1 {
		return d, false
	}
	d.on = text(m+1, w)
	for w >= 0 {
		next := topWord(t, w+1, "WHEN")
		end := next
		if end < 0 {
			end = len(t)
		}
		c, ok := mergeClauseOf(sql, t[w:end])
		if !ok {
			return d, false
		}
		d.clauses = append(d.clauses, c)
		w = next
	}
	return d, true
}

// mergeClauseOf reads one WHEN clause's tokens.
func mergeClauseOf(sql string, t []token) (mergeClause, bool) {
	var c mergeClause
	i := 1
	switch {
	case i+1 < len(t) && t[i].is("NOT") && t[i+1].is("MATCHED"):
		c.group, i = notMatchedByTarget, i+2
		if i+1 < len(t) && t[i].is("BY") {
			switch {
			case t[i+1].is("TARGET"):
			case t[i+1].is("SOURCE"):
				c.group = notMatchedBySource
			default:
				return c, false
			}
			i += 2
		}
	case i < len(t) && t[i].is("MATCHED"):
		c.group, i = matchedRows, i+1
	default:
		return c, false
	}
	if i < len(t) && t[i].is("AND") {
		th := topWord(t, i+1, "THEN")
		if th <= i+1 {
			return c, false
		}
		c.cond = sql[t[i+1].pos:t[th-1].end]
		i = th
	}
	if i+1 >= len(t) || !t[i].is("THEN") {
		return c, false
	}
	c.action = strings.ToUpper(t[i+1].text)
	switch {
	case t[i+1].kind != tokWord:
		return c, false
	case c.group == notMatchedByTarget && c.action == "INSERT":
	case c.group != notMatchedByTarget && (c.action == "UPDATE" || c.action == "DELETE"):
	default:
		return c, false
	}
	return c, true
}

// aliasEnd returns the index after a table's alias at t[i] ([AS] alias),
// or i when it has none: stop is the keyword that follows the table.
func aliasEnd(t []token, i int, stop string) int {
	switch {
	case i+1 < len(t) && t[i].is("AS") && (t[i+1].kind == tokWord || t[i+1].kind == tokQuoted):
		return i + 2
	case i < len(t) && (t[i].kind == tokQuoted || t[i].kind == tokWord && !t[i].is(stop)):
		return i + 1
	}
	return i
}

// topWord returns the index of the first of words at t[i:] outside
// parentheses, brackets and CASE ... END, or -1.
func topWord(t []token, i int, words ...string) int {
	depth, cases := 0, 0
	for ; i < len(t); i++ {
		switch {
		case t[i].punct("(") || t[i].punct("["):
			depth++
		case t[i].punct(")") || t[i].punct("]"):
			depth--
		case depth == 0 && t[i].is("CASE"):
			cases++
		case depth == 0 && cases > 0 && t[i].is("END"):
			cases--
		case depth == 0 && cases == 0:
			for _, w := range words {
				if t[i].is(w) {
					return i
				}
			}
		}
	}
	return -1
}

// closeParen returns the index of the ")" that closes the "(" at t[i], or
// -1.
func closeParen(t []token, i int) int {
	depth := 0
	for ; i < len(t); i++ {
		switch {
		case t[i].punct("("):
			depth++
		case t[i].punct(")"):
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return -1
}

// mergeFromSubquery returns why a script of several statements with a
// MERGE whose source is a subquery is not run, or "".
func mergeFromSubquery(sql string) string {
	toks, ok := lex(sql)
	if !ok {
		return ""
	}
	for _, s := range splitStatements(toks) {
		body, _, _ := stripControlFlow(s)
		if len(body) == 0 || !body[0].is("MERGE") {
			continue
		}
		if d, ok := parseMerge(sql, body); ok && d.subquery != "" {
			return "Not implemented here: a MERGE whose source is a subquery, inside a script of several statements. " +
				"BigQuery runs it, but the emulator behind CloudBurrow takes only a table as a MERGE's source (measured: " +
				"400 \"MERGE: source must be a single-table reference\"), and CloudBurrow runs a subquery source from a " +
				"table of its own only for a MERGE run on its own. Nothing was run. Run the MERGE as a query of its own, " +
				"or make the source a table first (CREATE TABLE ... AS the subquery)."
		}
	}
	return ""
}
