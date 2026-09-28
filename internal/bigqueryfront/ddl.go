package bigqueryfront

import (
	"fmt"
	"strings"
)

// ddlVerdict is what the front makes of the statements in a query.
type ddlVerdict struct {
	// code is 0 when the statements may be sent on, else the status they
	// are answered with: 400 invalidQuery for a name BigQuery refuses, 501
	// notImplemented for a statement the emulator would answer as done
	// without doing it.
	code   int
	reason string
	msg    string
	// creates are the statements' CREATE TABLE and CREATE VIEW, in order.
	// The columns of those with a query and no column list come from the
	// query, which the front runs alone, before the statement, to read them
	// (ctasColumns); the OR REPLACE ones the front may carry out itself
	// (replaceTable).
	creates []createStmt
	// statements is how many statements the query has.
	statements int
	// handler is whether the script has a BEGIN ... EXCEPTION block.
	handler bool
}

// createStmt is a CREATE TABLE or CREATE VIEW statement.
type createStmt struct {
	view, replace, temp bool
	// path is the table's path as written: [project.]dataset.table, or
	// table alone for a TEMP table or the default dataset.
	path []string
	// query is the statement's AS query, and columnList whether it names
	// its columns itself.
	query      string
	columnList bool
	// text is the whole statement, at offset pos in the query; pathPos and
	// pathEnd, and queryPos and queryEnd, are the offsets in text of the
	// path and the query.
	text               string
	pos                int
	pathPos, pathEnd   int
	queryPos, queryEnd int
	// later is how many statements of the script follow it.
	later int
}

// selects returns the queries whose result's columns become a table's or a
// view's: those of the statements with a query and no column list.
func (v ddlVerdict) selects() []createStmt {
	var out []createStmt
	for _, c := range v.creates {
		if c.query != "" && !c.columnList {
			out = append(out, c)
		}
	}
	return out
}

// checkDDL reads the statements in sql and returns what the front makes of
// them (#881, #901, #916, #918).
//
// The emulator's SQL engine refuses a column named twice (measured, 400)
// and a name that does not lex, but a quoted name is taken whatever it
// holds: CREATE TABLE ds.`t!`, a column `a b!`, CREATE SCHEMA `bad-name`,
// CREATE TABLE ds.c AS SELECT 1 AS `x!`, CREATE VIEW ds.`v!` and CREATE
// VIEW ds.v AS SELECT 1 AS `x!` all succeeded. Those names are held here to
// the rules tables.insert and datasets.insert are held to: the table or
// view ID, each column in the column list and each field of a STRUCT
// column type, and the dataset ID of CREATE SCHEMA; in ALTER TABLE, each
// ADD COLUMN's name and STRUCT fields, RENAME TO's table ID and RENAME
// COLUMN's new name. The columns of CREATE TABLE ... AS SELECT and CREATE
// VIEW come from their query, which the caller runs (creates).
//
// Every statement of a script is read, including those inside a
// control-flow block (IF, LOOP, WHILE, REPEAT, FOR, CASE) and a BEGIN ...
// EXCEPTION block's handler, whether or not the branch would run.
//
// Statements the emulator answers as done and does nothing with, or does
// not support, (measured against the pinned image, #901, #916, #918) are
// then 501:
//
//   - ALTER TABLE, whatever it does: ADD COLUMN, DROP COLUMN, RENAME TO,
//     SET OPTIONS and ALTER COLUMN ... SET OPTIONS each returned success,
//     and the first four left the table as it was; RENAME COLUMN it
//     refuses itself.
//   - A script with a control-flow block: the statements inside IF (either
//     branch), LOOP, WHILE, REPEAT, FOR and a script CASE were not run, and
//     the script still returned success. A BEGIN ... END block is run.
//   - RAISE: it returned success, where BigQuery ends the script with an
//     error.
//   - CREATE MATERIALIZED VIEW and DROP MATERIALIZED VIEW: 400 "Statement
//     not supported".
//   - CREATE VIEW with a column list: 400 "CREATE VIEW with explicit column
//     list is not supported".
//
// It reads the statements with a lexer, not a parser. A statement in a
// string, such as EXECUTE IMMEDIATE's, is not read.
func checkDDL(sql string) ddlVerdict {
	upper := strings.ToUpper(sql)
	if !strings.Contains(upper, "CREATE") && !strings.Contains(upper, "ALTER") && !strings.Contains(upper, "DROP") &&
		!strings.Contains(upper, "IF") && !strings.Contains(upper, "LOOP") && !strings.Contains(upper, "WHILE") &&
		!strings.Contains(upper, "REPEAT") && !strings.Contains(upper, "FOR") && !strings.Contains(upper, "CASE") &&
		!strings.Contains(upper, "RAISE") && !strings.Contains(upper, "EXCEPTION") {
		return ddlVerdict{statements: 1}
	}
	toks, ok := lex(sql)
	if !ok {
		// The engine reports its own syntax error.
		return ddlVerdict{statements: 1}
	}
	var v ddlVerdict
	var flow, alter, unsupported string
	stmts := splitStatements(toks)
	for _, stmt := range stmts {
		if len(stmt) > 0 {
			v.statements++
		}
	}
	seen := 0
	for _, stmt := range stmts {
		if len(stmt) > 0 {
			seen++
		}
		body, block, handler := stripControlFlow(stmt)
		if block != "" && flow == "" {
			flow = block
		}
		v.handler = v.handler || handler
		var msg string
		switch {
		case len(body) > 0 && body[0].is("CREATE"):
			var c createStmt
			var kind string
			msg, kind, c = checkCreate(body)
			if msg == "" {
				switch {
				case kind == "MATERIALIZED VIEW" && unsupported == "":
					unsupported = "CREATE MATERIALIZED VIEW"
				case kind == "VIEW" && c.columnList && unsupported == "":
					unsupported = "CREATE VIEW with a column list"
				case kind == "TABLE" || kind == "VIEW":
					c.text, c.pos = sql[body[0].pos:body[len(body)-1].end], body[0].pos
					base := body[0].pos
					c.pathPos, c.pathEnd = c.pathPos-base, c.pathEnd-base
					if c.query != "" {
						c.queryPos, c.queryEnd = c.queryPos-base, c.queryEnd-base
						c.query = c.text[c.queryPos:c.queryEnd]
					}
					c.later = v.statements - seen
					v.creates = append(v.creates, c)
				}
			}
		case len(body) > 2 && body[0].is("DROP") && body[1].is("MATERIALIZED") && body[2].is("VIEW"):
			if unsupported == "" {
				unsupported = "DROP MATERIALIZED VIEW"
			}
		case len(body) > 0 && body[0].is("RAISE"):
			if unsupported == "" {
				unsupported = "RAISE"
			}
		case len(body) > 1 && body[0].is("ALTER") && body[1].is("TABLE"):
			msg = checkAlter(body)
			alter = "ALTER TABLE"
		}
		if msg != "" {
			return ddlVerdict{code: 400, reason: "invalidQuery", msg: msg}
		}
	}
	switch {
	case flow != "":
		return ddlVerdict{code: 501, reason: "notImplemented", msg: fmt.Sprintf(
			"Not implemented here: the script has a control-flow block (%s). BigQuery runs it, but the emulator behind CloudBurrow "+
				"does not run the statements inside IF, LOOP, WHILE, REPEAT, FOR or CASE blocks and still reports success "+
				"(measured), so nothing was run. A BEGIN ... END block, and statements outside any block, are run.", flow)}
	case alter != "":
		return ddlVerdict{code: 501, reason: "notImplemented", msg: "Not implemented here: ALTER TABLE. BigQuery runs it, " +
			"but the emulator behind CloudBurrow reports ADD COLUMN, DROP COLUMN, RENAME TO and SET OPTIONS as done and " +
			"leaves the table unchanged (measured), so nothing was run. Change a table's schema or options with " +
			"tables.update or tables.patch (Table.Update in the Go client), which the emulator applies."}
	case unsupported == "RAISE":
		return ddlVerdict{code: 501, reason: "notImplemented", msg: "Not implemented here: RAISE. BigQuery ends the script " +
			"with the error it raises, but the emulator behind CloudBurrow ignores RAISE and reports the script done " +
			"(measured), so nothing was run."}
	case unsupported != "":
		return ddlVerdict{code: 501, reason: "notImplemented", msg: "Not implemented here: " + unsupported + ". BigQuery " +
			"runs it, but the emulator behind CloudBurrow does not support it (measured: 400 \"not supported\"), so " +
			"nothing was run. A view without a column list (CREATE VIEW ... AS SELECT a AS name) is supported."}
	}
	return v
}

// stripControlFlow returns a statement without the control-flow text in
// front of its first statement, and the block it opens, if any: after a
// split at semicolons, the statement at the head of a block carries the
// block's opening ("IF c THEN CREATE ...", "LOOP CREATE ...", "EXCEPTION
// WHEN ERROR THEN CREATE ...").
func stripControlFlow(t []token) (body []token, block string, handler bool) {
	for len(t) > 0 {
		// A label: "name: BEGIN", "name: LOOP".
		if len(t) > 1 && (t[0].kind == tokWord || t[0].kind == tokQuoted) && t[1].punct(":") {
			t = t[2:]
			continue
		}
		switch w := strings.ToUpper(t[0].text); {
		case t[0].kind != tokWord:
			return t, block, handler
		case w == "BEGIN" && !(len(t) > 1 && (t[1].is("TRANSACTION") || t[1].is("TRAN"))), w == "ELSE":
			t = t[1:]
		case w == "LOOP" || w == "REPEAT":
			block, t = orBlock(block, w), t[1:]
		case w == "EXCEPTION" && len(t) > 1 && t[1].is("WHEN"):
			handler, t = true, afterKeyword(t, 2, "THEN")
		case w == "IF" || w == "ELSEIF" || w == "CASE" || w == "WHEN":
			if w != "ELSEIF" && w != "WHEN" {
				block = orBlock(block, w)
			}
			t = afterKeyword(t, 1, "THEN")
		case w == "WHILE" || w == "FOR":
			block, t = orBlock(block, w), afterKeyword(t, 1, "DO")
		default:
			return t, block, handler
		}
	}
	return t, block, handler
}

func orBlock(block, w string) string {
	if block != "" {
		return block
	}
	return w
}

// afterKeyword returns t after the first kw at the top level from i:
// outside parentheses and brackets and outside a CASE ... END expression,
// whose own THEN is not the block's.
func afterKeyword(t []token, i int, kw string) []token {
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
		case depth == 0 && cases == 0 && t[i].is(kw):
			return t[i+1:]
		}
	}
	return nil
}

type tokKind int

const (
	tokWord   tokKind = iota // an unquoted identifier, keyword or number
	tokQuoted                // a `quoted identifier`, unescaped
	tokString                // a string or bytes literal
	tokPunct                 // one punctuation character
)

type token struct {
	kind tokKind
	text string
	// pos and end are the token's byte offsets in the statement text.
	pos, end int
}

func (t token) is(word string) bool { return t.kind == tokWord && strings.EqualFold(t.text, word) }

func (t token) punct(p string) bool { return t.kind == tokPunct && t.text == p }

// lex splits GoogleSQL into tokens, dropping comments. ok is false for text
// it cannot lex, such as an unterminated string.
// https://cloud.google.com/bigquery/docs/reference/standard-sql/lexical
func lex(s string) (toks []token, ok bool) {
	isWord := func(c byte) bool {
		return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '#' || c == '-' && strings.HasPrefix(s[i:], "--"):
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '/' && strings.HasPrefix(s[i:], "/*"):
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return nil, false
			}
			i += end + 4
		case c == '`':
			var b strings.Builder
			j := i + 1
			for ; j < len(s) && s[j] != '`'; j++ {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				b.WriteByte(s[j])
			}
			if j >= len(s) {
				return nil, false
			}
			toks = append(toks, token{tokQuoted, b.String(), i, j + 1})
			i = j + 1
		case c == '\'' || c == '"':
			n, ok := stringEnd(s[i:], false)
			if !ok {
				return nil, false
			}
			toks = append(toks, token{tokString, s[i : i+n], i, i + n})
			i += n
		case isWord(c):
			j := i
			for j < len(s) && isWord(s[j]) {
				j++
			}
			// r'…', b'…', rb'…' and br'…' are string and bytes literals.
			if j < len(s) && (s[j] == '\'' || s[j] == '"') && j-i <= 2 && strings.Trim(strings.ToLower(s[i:j]), "rb") == "" {
				n, ok := stringEnd(s[j:], strings.ContainsAny(s[i:j], "rR"))
				if !ok {
					return nil, false
				}
				toks = append(toks, token{tokString, s[i : j+n], i, j + n})
				i = j + n
				continue
			}
			toks = append(toks, token{tokWord, s[i:j], i, j})
			i = j
		default:
			toks = append(toks, token{tokPunct, string(c), i, i + 1})
			i++
		}
	}
	return toks, true
}

// stringEnd returns the length of the quoted literal s starts with,
// triple-quoted or not; raw literals have no escapes.
func stringEnd(s string, raw bool) (int, bool) {
	q := s[:1]
	if strings.HasPrefix(s, q+q+q) {
		q = q + q + q
	}
	for i := len(q); i < len(s); i++ {
		if s[i] == '\\' && !raw {
			i++
			continue
		}
		if len(q) == 1 && s[i] == '\n' {
			return 0, false
		}
		if strings.HasPrefix(s[i:], q) {
			return i + len(q), true
		}
	}
	return 0, false
}

// splitStatements splits a script at its semicolons.
func splitStatements(toks []token) [][]token {
	var out [][]token
	start := 0
	for i, t := range toks {
		if t.punct(";") {
			out = append(out, toks[start:i])
			start = i + 1
		}
	}
	return append(out, toks[start:])
}

// checkCreate checks a CREATE TABLE, CREATE VIEW, CREATE MATERIALIZED VIEW
// or CREATE SCHEMA statement; others pass. kind is what it creates: TABLE,
// VIEW, MATERIALIZED VIEW, SCHEMA or "". c describes a TABLE or VIEW, with
// offsets into the statement's source (the caller makes them relative):
// its query is the one after AS, whose result's columns the table or view
// takes when it has no column list.
func checkCreate(t []token) (msg, kind string, c createStmt) {
	i := 1
	if i+1 < len(t) && t[i].is("OR") && t[i+1].is("REPLACE") {
		c.replace = true
		i += 2
	}
	for i < len(t) && (t[i].is("TEMP") || t[i].is("TEMPORARY") || t[i].is("SNAPSHOT") || t[i].is("EXTERNAL")) {
		if !t[i].is("SNAPSHOT") && !t[i].is("EXTERNAL") {
			c.temp = true
		} else {
			// A snapshot or external table is not one the checks
			// below know.
			kind = "-"
		}
		i++
	}
	if i >= len(t) {
		return "", "", c
	}
	switch {
	case t[i].is("SCHEMA"):
		kind = "SCHEMA"
	case t[i].is("TABLE"):
		if kind == "" {
			kind = "TABLE"
		}
	case t[i].is("VIEW"):
		kind = "VIEW"
		c.view = true
	case t[i].is("MATERIALIZED") && i+1 < len(t) && t[i+1].is("VIEW"):
		kind = "MATERIALIZED VIEW"
		i++
	default:
		return "", "", c
	}
	i++
	if i < len(t) && t[i].is("FUNCTION") {
		return "", "", c
	}
	if i+2 < len(t) && t[i].is("IF") && t[i+1].is("NOT") && t[i+2].is("EXISTS") {
		i += 3
	}
	start := i
	parts, i := path(t, i)
	if len(parts) == 0 {
		return "", "", c
	}
	c.path, c.pathPos, c.pathEnd = parts, t[start].pos, t[i-1].end
	name := parts[len(parts)-1]
	if kind == "SCHEMA" {
		return checkDatasetID(name), kind, c
	}
	if msg := checkTableID(name); msg != "" {
		return msg, kind, c
	}
	if kind == "-" {
		return "", "", c
	}
	if i < len(t) && t[i].punct("(") {
		c.columnList = true
		if msg := checkColumns(t, i+1); msg != "" {
			return msg, kind, c
		}
		i = skipTo(t, i+1, ")") + 1
	}
	// The options (PARTITION BY, CLUSTER BY, OPTIONS(...)) run to AS and
	// the query. A LIKE, COPY or CLONE takes an existing table's columns,
	// which were held to the rules when it was made.
	for ; i < len(t); i++ {
		switch {
		case t[i].is("LIKE") || t[i].is("COPY") || t[i].is("CLONE"):
			return "", "", c
		case t[i].punct("("):
			i = skipTo(t, i+1, ")")
		case t[i].is("AS"):
			if i+1 < len(t) {
				c.query = "-"
				c.queryPos, c.queryEnd = t[i+1].pos, t[len(t)-1].end
			}
			return "", kind, c
		}
	}
	return "", kind, c
}

// checkAlter checks the names an ALTER TABLE statement gives: each ADD
// COLUMN's name and STRUCT fields, RENAME TO's table ID and RENAME
// COLUMN's new name. Its other actions name nothing new.
func checkAlter(t []token) string {
	i := 2
	if i+1 < len(t) && t[i].is("IF") && t[i+1].is("EXISTS") {
		i += 2
	}
	if _, i = path(t, i); i >= len(t) {
		return ""
	}
	for i < len(t) {
		switch {
		case t[i].is("ADD") && i+1 < len(t) && t[i+1].is("COLUMN"):
			i += 2
			if i+2 < len(t) && t[i].is("IF") && t[i+1].is("NOT") && t[i+2].is("EXISTS") {
				i += 3
			}
			if i < len(t) && (t[i].kind == tokWord || t[i].kind == tokQuoted) {
				if msg := checkColumnName(t[i].text); msg != "" {
					return msg
				}
				var msg string
				if i, msg = checkType(t, i+1, t[i].text+"."); msg != "" {
					return msg
				}
			}
		case t[i].is("RENAME") && i+1 < len(t) && t[i+1].is("TO"):
			if parts, _ := path(t, i+2); len(parts) > 0 {
				if msg := checkTableID(parts[len(parts)-1]); msg != "" {
					return msg
				}
			}
		case t[i].is("RENAME") && i+1 < len(t) && t[i+1].is("COLUMN"):
			// RENAME COLUMN [IF EXISTS] old TO new.
			j := i + 2
			if j+1 < len(t) && t[j].is("IF") && t[j+1].is("EXISTS") {
				j += 2
			}
			if j+2 < len(t) && t[j+1].is("TO") && (t[j+2].kind == tokWord || t[j+2].kind == tokQuoted) {
				if msg := checkColumnName(t[j+2].text); msg != "" {
					return msg
				}
			}
		}
		i = skipTo(t, i, ",")
		if i < len(t) {
			i++
		}
	}
	return ""
}

// path reads a table or dataset path: names joined by dots, a quoted name
// holding dots of its own, and a project with dashes in it
// (my-project.ds.t).
func path(t []token, i int) (parts []string, next int) {
	for i < len(t) {
		switch t[i].kind {
		case tokQuoted:
			parts = append(parts, strings.Split(t[i].text, ".")...)
			i++
		case tokWord:
			name := t[i].text
			i++
			for len(parts) == 0 && i+1 < len(t) && t[i].punct("-") && t[i+1].kind == tokWord {
				name += "-" + t[i+1].text
				i += 2
			}
			parts = append(parts, name)
		default:
			return parts, i
		}
		if i < len(t) && t[i].punct(".") {
			i++
			continue
		}
		return parts, i
	}
	return parts, i
}

// checkColumns checks the column list that starts at t[i], after its "(".
func checkColumns(t []token, i int) string {
	for i < len(t) {
		if t[i].punct(")") {
			return ""
		}
		// A table constraint, not a column.
		if t[i].is("PRIMARY") || t[i].is("FOREIGN") || t[i].is("CONSTRAINT") {
			i = skipTo(t, i, ",", ")")
		} else {
			if t[i].kind != tokWord && t[i].kind != tokQuoted {
				return ""
			}
			if msg := checkColumnName(t[i].text); msg != "" {
				return msg
			}
			var msg string
			if i, msg = checkType(t, i+1, t[i].text+"."); msg != "" {
				return msg
			}
			i = skipTo(t, i, ",", ")")
		}
		if i < len(t) && t[i].punct(",") {
			i++
		}
	}
	return ""
}

// checkType reads the column type at t[i] and checks the name of each
// STRUCT field in it. It returns the index after the type.
func checkType(t []token, i int, prefix string) (int, string) {
	if i >= len(t) {
		return i, ""
	}
	if i+1 < len(t) && t[i+1].punct("<") {
		if t[i].is("STRUCT") {
			i += 2
			for i < len(t) && !t[i].punct(">") {
				// STRUCT<name type, ...>; a field with no name
				// (STRUCT<INT64>) is a type followed by , or >.
				named := i+1 < len(t) && !t[i+1].punct(",") && !t[i+1].punct(">") && !t[i+1].punct("<") && !t[i+1].punct("(")
				if named {
					if msg := checkColumnName(t[i].text); msg != "" {
						return i, strings.Replace(msg, fmt.Sprintf("%q", t[i].text), fmt.Sprintf("%q", prefix+t[i].text), 1)
					}
					var msg string
					if i, msg = checkType(t, i+1, prefix+t[i].text+"."); msg != "" {
						return i, msg
					}
				} else {
					i, _ = checkType(t, i, prefix)
				}
				i = skipTo(t, i, ",", ">")
				if i < len(t) && t[i].punct(",") {
					i++
				}
			}
			return i + 1, ""
		}
		// ARRAY<type>, RANGE<type>.
		i, msg := checkType(t, i+2, prefix)
		if msg != "" {
			return i, msg
		}
		return skipTo(t, i, ">") + 1, ""
	}
	i++
	if i < len(t) && t[i].punct("(") {
		// STRING(10), NUMERIC(10, 2).
		i = skipTo(t, i+1, ")") + 1
	}
	return i, ""
}

// skipTo returns the index of the first of stops at the current nesting
// level, skipping anything inside (), [] and <>.
func skipTo(t []token, i int, stops ...string) int {
	depth := 0
	for ; i < len(t); i++ {
		if depth == 0 {
			for _, s := range stops {
				if t[i].punct(s) {
					return i
				}
			}
		}
		switch {
		case t[i].punct("(") || t[i].punct("[") || t[i].punct("<"):
			depth++
		case t[i].punct(")") || t[i].punct("]") || t[i].punct(">"):
			depth--
			if depth < 0 {
				return i
			}
		}
	}
	return i
}
