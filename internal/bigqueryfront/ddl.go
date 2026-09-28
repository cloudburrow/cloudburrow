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
	// selects are the queries of the CREATE TABLE ... AS SELECT statements
	// with no column list, whose result's column names become the table's.
	// The front runs each alone, before the statement, to read those names
	// (ctasColumns).
	selects []string
}

// checkDDL reads the statements in sql and returns what the front makes of
// them (#881, #901).
//
// The emulator's SQL engine refuses a column named twice (measured, 400)
// and a name that does not lex, but a quoted name is taken whatever it
// holds: CREATE TABLE ds.`t!`, a column `a b!`, CREATE SCHEMA `bad-name`,
// and CREATE TABLE ds.c AS SELECT 1 AS `x!` all succeeded. Those names are
// held here to the rules tables.insert and datasets.insert are held to: the
// table ID, each column in the column list and each field of a STRUCT
// column type, and the dataset ID of CREATE SCHEMA; in ALTER TABLE, each
// ADD COLUMN's name and STRUCT fields, RENAME TO's table ID and RENAME
// COLUMN's new name. The columns of CREATE TABLE ... AS SELECT come from
// its query, which the caller runs (selects).
//
// Every statement of a script is read, including those inside a
// control-flow block (IF, LOOP, WHILE, REPEAT, FOR, CASE) and a BEGIN ...
// EXCEPTION block's handler, whether or not the branch would run.
//
// Two kinds of statement the emulator answers as done and does nothing
// with (measured against the pinned image, #901) are then 501:
//
//   - ALTER TABLE, whatever it does: ADD COLUMN, DROP COLUMN, RENAME TO,
//     SET OPTIONS and ALTER COLUMN ... SET OPTIONS each returned success,
//     and the first four left the table as it was; RENAME COLUMN it
//     refuses itself.
//   - A script with a control-flow block: the statements inside IF (either
//     branch), LOOP, WHILE, REPEAT, FOR and a script CASE were not run, and
//     the script still returned success. A BEGIN ... END block is run.
//
// It reads the statements with a lexer, not a parser. A statement in a
// string, such as EXECUTE IMMEDIATE's, is not read.
func checkDDL(sql string) ddlVerdict {
	upper := strings.ToUpper(sql)
	if !strings.Contains(upper, "CREATE") && !strings.Contains(upper, "ALTER") &&
		!strings.Contains(upper, "IF") && !strings.Contains(upper, "LOOP") && !strings.Contains(upper, "WHILE") &&
		!strings.Contains(upper, "REPEAT") && !strings.Contains(upper, "FOR") && !strings.Contains(upper, "CASE") {
		return ddlVerdict{}
	}
	toks, ok := lex(sql)
	if !ok {
		// The engine reports its own syntax error.
		return ddlVerdict{}
	}
	var v ddlVerdict
	var flow, alter string
	for _, stmt := range splitStatements(toks) {
		body, block := stripControlFlow(stmt)
		if block != "" && flow == "" {
			flow = block
		}
		var msg string
		switch {
		case len(body) > 0 && body[0].is("CREATE"):
			var sel []token
			msg, sel = checkCreate(body)
			if msg == "" && len(sel) > 0 {
				v.selects = append(v.selects, sql[sel[0].pos:sel[len(sel)-1].end])
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
	}
	return v
}

// stripControlFlow returns a statement without the control-flow text in
// front of its first statement, and the block it opens, if any: after a
// split at semicolons, the statement at the head of a block carries the
// block's opening ("IF c THEN CREATE ...", "LOOP CREATE ...", "EXCEPTION
// WHEN ERROR THEN CREATE ...").
func stripControlFlow(t []token) (body []token, block string) {
	for len(t) > 0 {
		// A label: "name: BEGIN", "name: LOOP".
		if len(t) > 1 && (t[0].kind == tokWord || t[0].kind == tokQuoted) && t[1].punct(":") {
			t = t[2:]
			continue
		}
		switch w := strings.ToUpper(t[0].text); {
		case t[0].kind != tokWord:
			return t, block
		case w == "BEGIN" && !(len(t) > 1 && (t[1].is("TRANSACTION") || t[1].is("TRAN"))), w == "ELSE":
			t = t[1:]
		case w == "LOOP" || w == "REPEAT":
			block, t = orBlock(block, w), t[1:]
		case w == "EXCEPTION" && len(t) > 1 && t[1].is("WHEN"):
			t = afterKeyword(t, 2, "THEN")
		case w == "IF" || w == "ELSEIF" || w == "CASE" || w == "WHEN":
			if w != "ELSEIF" && w != "WHEN" {
				block = orBlock(block, w)
			}
			t = afterKeyword(t, 1, "THEN")
		case w == "WHILE" || w == "FOR":
			block, t = orBlock(block, w), afterKeyword(t, 1, "DO")
		default:
			return t, block
		}
	}
	return t, block
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

// checkCreate checks a CREATE TABLE or CREATE SCHEMA statement; others
// pass. sel is the query of a CREATE TABLE ... AS with no column list,
// whose result's columns the table takes.
func checkCreate(t []token) (msg string, sel []token) {
	i := 1
	if i+1 < len(t) && t[i].is("OR") && t[i+1].is("REPLACE") {
		i += 2
	}
	for i < len(t) && (t[i].is("TEMP") || t[i].is("TEMPORARY") || t[i].is("SNAPSHOT") || t[i].is("EXTERNAL")) {
		i++
	}
	if i >= len(t) {
		return "", nil
	}
	schema := t[i].is("SCHEMA")
	if !schema && !t[i].is("TABLE") {
		return "", nil
	}
	i++
	if i < len(t) && t[i].is("FUNCTION") {
		return "", nil
	}
	if i+2 < len(t) && t[i].is("IF") && t[i+1].is("NOT") && t[i+2].is("EXISTS") {
		i += 3
	}
	parts, i := path(t, i)
	if len(parts) == 0 {
		return "", nil
	}
	name := parts[len(parts)-1]
	if schema {
		return checkDatasetID(name), nil
	}
	if msg := checkTableID(name); msg != "" {
		return msg, nil
	}
	if i < len(t) && t[i].punct("(") {
		return checkColumns(t, i+1), nil
	}
	// No column list: the options (PARTITION BY, CLUSTER BY, OPTIONS(...))
	// run to AS and the query. A LIKE, COPY or CLONE takes an existing
	// table's columns, which were held to the rules when it was made.
	for ; i < len(t); i++ {
		switch {
		case t[i].is("LIKE") || t[i].is("COPY") || t[i].is("CLONE"):
			return "", nil
		case t[i].punct("("):
			i = skipTo(t, i+1, ")")
		case t[i].is("AS"):
			if i+1 < len(t) {
				return "", t[i+1:]
			}
			return "", nil
		}
	}
	return "", nil
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
