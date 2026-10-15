package bigqueryfront

import "strings"

// CREATE TABLE ... LIKE and CREATE TABLE ... COPY (#1239). The emulator
// refuses both (measured, #937: 400 "CREATE TABLE LIKE is not supported"),
// so a query that is one such statement is sent on as the CREATE TABLE AS
// SELECT it amounts to: LIKE makes an empty table with the source's columns
// (SELECT * FROM src LIMIT 0), COPY one with its columns and rows (SELECT *
// FROM src). OR REPLACE, IF NOT EXISTS and the options after the source
// (PARTITION BY, CLUSTER BY, OPTIONS(...)) are kept, in the place CREATE
// TABLE AS SELECT takes them. Not carried over: the source's own options,
// partitioning and clustering, column descriptions and REQUIRED modes, which
// a query's result does not have. A LIKE or COPY inside a multi-statement
// script, one with a column list, and CLONE stay 501 (checkDDL).

// rewriteTableLikeCopy returns the CREATE TABLE AS SELECT a lone CREATE TABLE
// ... LIKE or COPY statement amounts to, and false for any other query.
func rewriteTableLikeCopy(sql string) (string, bool) {
	t, ok := lex(sql)
	if !ok || len(t) < 5 || !t[0].is("CREATE") {
		return "", false
	}
	end := len(t)
	if t[end-1].punct(";") {
		end--
	}
	for _, tok := range t[:end] {
		if tok.punct(";") {
			return "", false
		}
	}
	i := 1
	if i+1 < end && t[i].is("OR") && t[i+1].is("REPLACE") {
		i += 2
	}
	for i < end && (t[i].is("TEMP") || t[i].is("TEMPORARY")) {
		i++
	}
	if i >= end || !t[i].is("TABLE") {
		return "", false
	}
	i++
	if i+2 < end && t[i].is("IF") && t[i+1].is("NOT") && t[i+2].is("EXISTS") {
		i += 3
	}
	// The new table's name: a path of names and dots.
	i, ok = pathEnd(t, i, end)
	if !ok || i >= end || !(t[i].is("LIKE") || t[i].is("COPY")) {
		return "", false
	}
	like := t[i].is("LIKE")
	src := i + 1
	after, ok := pathEnd(t, src, end)
	if !ok {
		return "", false
	}
	source := sql[t[src].pos:t[after-1].end]
	var options string
	if after < end {
		for _, tok := range t[after:end] {
			if tok.is("AS") {
				return "", false // LIKE ... AS query: BigQuery's own form, not this rewrite's
			}
		}
		options = " " + sql[t[after].pos:t[end-1].end]
	}
	query := " AS SELECT * FROM " + source
	if like {
		query += " LIMIT 0"
	}
	return strings.TrimRight(sql[:t[i].pos], " \t\r\n") + options + query, true
}

// pathEnd is the index after the table path that starts at i: names joined
// by dots. ok is false when no name starts there.
func pathEnd(t []token, i, end int) (int, bool) {
	if i >= end || (t[i].kind != tokWord && t[i].kind != tokQuoted) {
		return i, false
	}
	i++
	for i+1 < end && t[i].punct(".") && (t[i+1].kind == tokWord || t[i+1].kind == tokQuoted) {
		i += 2
	}
	return i, true
}
