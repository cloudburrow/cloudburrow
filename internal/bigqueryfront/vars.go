package bigqueryfront

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// columnLookup returns the names of the top-level columns of the table a
// statement at offset pos reads through path (a FROM, JOIN, UPDATE or
// MERGE source), and whether they are known. A nil lookup knows none.
type columnLookup func(path []string, pos int) (cols []string, known bool)

// renameVariables gives each script variable a script's DECLAREs make a
// name of its own, unique to this request, and returns the text and the
// names it gave, each mapped to the name as the client wrote it (#933,
// #956). When a statement names a variable where BigQuery may read a
// column, an alias or a table of the same name instead, it returns the
// 501 message to refuse the query with, and the text unchanged.
//
// The emulator's SQL engine (goccy/googlesqlite v0.3.1, in the pinned
// image) keeps a script's variables after the script, on the one
// connection every query of the instance runs on, and replaces each bare
// word that names one, in every later query, with its value: measured,
// after `DECLARE zz INT64 DEFAULT 5; SELECT zz`, a separate `SELECT zz AS
// v` returned 5, and after `DECLARE w INT64 DEFAULT 9`, `SELECT 1 AS w`
// failed "Syntax error: Unexpected integer literal "9"". BigQuery scopes a
// variable to its script: the later query fails "Unrecognized name: zz",
// and the alias is an alias. A variable of a unique name is still kept by
// the engine, but no later query names it.
//
// The engine replaces the same words within the script that declares the
// variable (its substituteScriptVariables replaces the text before the
// statement is analysed): measured, `DECLARE n INT64 DEFAULT 1; SELECT 2
// AS n` failed "Syntax error: Unexpected integer literal "1"", where
// BigQuery returns 2 (#956). So only the words that refer to the variable
// are renamed: the engine replaces those with the value, and leaves the
// others, under the client's name, to be read as the names they are.
//
// The words the engine would replace are the name after DECLARE in a
// statement that starts with it (only the first of a list, as the engine
// records only that one), and after that statement, each bare word equal
// to it, ignoring case, that does not follow a "." and is not followed by
// "(" (spaces or tabs between), outside strings and quoted names. Of
// those, the ones that are not a reference to the variable are left as
// they are:
//
//   - an alias, after AS or in place of one (SELECT 2 n, FROM ds.t n), and
//     a table's, CTE's or function's name (after FROM, JOIN, TABLE, INTO,
//     UPDATE, MERGE, WITH, VIEW, FUNCTION);
//   - a column name in a CREATE TABLE's or INSERT's column list, a
//     function's parameter, a STRUCT field in a type (STRUCT<n INT64>), a
//     column in SELECT * EXCEPT (n) or JOIN ... USING (n), and a column
//     UPDATE ... SET assigns.
//
// BigQuery's documentation does not say which of a variable and a column,
// alias or range variable of the same name a query reads. A statement that
// refers to a variable is therefore refused when the name may also be one
// of those in it: one of the words above that names something (not a
// column list's, a type's or the DECLARE's own), a column after a "."
// (t.n), a column of a table it reads (lookup), or a table it reads whose
// columns are not known.
func renameVariables(sql string, lookup columnLookup) (string, map[string]string, string) {
	if !strings.Contains(strings.ToUpper(sql), "DECLARE") {
		return sql, nil, ""
	}
	toks, ok := lex(sql)
	if !ok {
		return sql, nil, ""
	}
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	tag := hex.EncodeToString(suffix)
	vars := map[string]string{}  // lower-case name → unique name
	names := map[string]string{} // unique name → name as declared
	var edits []varEdit
	for _, stmt := range splitStatements(toks) {
		body := stmt
		if len(stmt) > 1 && stmt[0].is("DECLARE") && stmt[1].kind == tokWord && engineIdent(stmt[1].text) &&
			(stmt[1].pos == 0 || sql[stmt[1].pos-1] != '.') {
			lower := strings.ToLower(stmt[1].text)
			u, ok := vars[lower]
			if !ok {
				u = "cbvar_" + tag + "_" + lower
				names[u] = stmt[1].text
			}
			edits = append(edits, varEdit{stmt[1].pos, stmt[1].end, u})
			// The DEFAULT expression is read with the variables declared
			// before this one.
			body = stmt[2:]
			refs, msg := variableRefs(sql, body, vars, lookup)
			if msg != "" {
				return sql, nil, msg
			}
			edits = append(edits, refs...)
			vars[lower] = u
			continue
		}
		refs, msg := variableRefs(sql, body, vars, lookup)
		if msg != "" {
			return sql, nil, msg
		}
		edits = append(edits, refs...)
	}
	if len(edits) == 0 {
		return sql, nil, ""
	}
	sort.Slice(edits, func(a, b int) bool { return edits[a].pos < edits[b].pos })
	var b strings.Builder
	last := 0
	for _, e := range edits {
		b.WriteString(sql[last:e.pos])
		b.WriteString(e.text)
		last = e.end
	}
	b.WriteString(sql[last:])
	return b.String(), names, ""
}

type varEdit struct {
	pos, end int
	text     string
}

// reservedWords are GoogleSQL's reserved keywords
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/lexical#reserved_keywords),
// and the others after which an expression, not an alias, follows. A word
// after any other word, a literal, a quoted name or a closing bracket is an
// alias.
var reservedWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`ALL AND ANY ARRAY AS ASC ASSERT_ROWS_MODIFIED AT BETWEEN BY CASE CAST COLLATE
		CONTAINS CREATE CROSS CUBE CURRENT DEFAULT DEFINE DESC DISTINCT ELSE END ENUM ESCAPE EXCEPT EXCLUDE EXISTS
		EXTRACT FALSE FETCH FOLLOWING FOR FROM FULL GRAPH_TABLE GROUP GROUPING GROUPS HASH HAVING IF IGNORE IN INNER
		INTERSECT INTERVAL INTO IS JOIN LATERAL LEFT LIKE LIMIT LOOKUP MERGE NATURAL NEW NO NOT NULL NULLS OF ON OR
		ORDER OUTER OVER PARTITION PRECEDING PROTO QUALIFY RANGE RECURSIVE RESPECT RIGHT ROLLUP ROWS SELECT SET SOME
		STRUCT TABLESAMPLE THEN TO TREAT TRUE UNBOUNDED UNION UNNEST USING WHEN WHERE WINDOW WITH WITHIN
		OFFSET VALUE VALUES ASSERT IMMEDIATE ELSEIF UNTIL WHILE RETURN DO LOOP REPEAT BEGIN EXCEPTION CALL DECLARE
		EXECUTE RAISE ZONE UPDATE DELETE INSERT TRUNCATE TABLE VIEW SCHEMA FUNCTION DROP ALTER`) {
		reservedWords[w] = true
	}
}

// nameAfter are the words after which a name is a table's, a CTE's or a
// function's, not a variable's.
var nameAfter = map[string]bool{"FROM": true, "JOIN": true, "TABLE": true, "INTO": true, "UPDATE": true,
	"MERGE": true, "WITH": true, "VIEW": true, "FUNCTION": true, "SCHEMA": true, "EXISTS": true, "PROCEDURE": true}

// variableRefs returns the edits that rename the references to the
// variables declared so far (vars) in one statement, or why the statement
// is refused (renameVariables).
func variableRefs(sql string, stmt []token, vars map[string]string, lookup columnLookup) ([]varEdit, string) {
	if len(vars) == 0 || len(stmt) == 0 {
		return nil, ""
	}
	mentioned := false
	for _, t := range stmt {
		if t.kind == tokWord {
			if _, ok := vars[strings.ToLower(t.text)]; ok {
				mentioned = true
				break
			}
		}
	}
	if !mentioned {
		return nil, ""
	}

	// skip marks the tokens that name a column in a list or a type, never
	// a variable and nothing a query can read: a CREATE TABLE's or
	// INSERT's column list, and STRUCT<...> or ARRAY<...> types.
	// namesIn marks the tokens of a list that names columns or parameters
	// a query reads: a function's parameters, EXCEPT (...), USING (...).
	skip := make([]bool, len(stmt))
	namesIn := make([]bool, len(stmt))
	markGroup := func(open int, mark []bool) {
		end := skipTo(stmt, open+1, ")")
		for k := open + 1; k < end && k < len(stmt); k++ {
			mark[k] = true
		}
	}
	isCreate := stmt[0].is("CREATE")
	isInsert := stmt[0].is("INSERT")
	isUpdate := stmt[0].is("UPDATE")
	for i := 0; i < len(stmt); i++ {
		t := stmt[i]
		switch {
		case (t.is("STRUCT") || t.is("ARRAY") || t.is("RANGE")) && i+1 < len(stmt) && stmt[i+1].punct("<"):
			end := skipTo(stmt, i+2, ">")
			for k := i + 2; k < end && k < len(stmt); k++ {
				skip[k] = true
			}
		case (t.is("EXCEPT") || t.is("USING")) && i+1 < len(stmt) && stmt[i+1].punct("("):
			markGroup(i+1, namesIn)
		case i > 0 && t.is("INSERT") && i+1 < len(stmt) && stmt[i+1].punct("("):
			// MERGE ... WHEN NOT MATCHED THEN INSERT (columns).
			markGroup(i+1, skip)
		case isCreate && (t.is("TABLE") || t.is("FUNCTION")) || isInsert && (t.is("INTO") || i == 0):
			j := i + 1
			if j+2 < len(stmt) && stmt[j].is("IF") && stmt[j+1].is("NOT") && stmt[j+2].is("EXISTS") {
				j += 3
			}
			if isInsert && i == 0 && j < len(stmt) && stmt[j].is("INTO") {
				continue
			}
			parts, next := path(stmt, j)
			if len(parts) > 0 && next < len(stmt) && stmt[next].punct("(") {
				if t.is("FUNCTION") {
					markGroup(next, namesIn)
				} else {
					markGroup(next, skip)
				}
			}
		}
	}

	// extract marks the tokens directly inside EXTRACT(part FROM value),
	// whose FROM is not a clause.
	extract := make([]bool, len(stmt))
	var parens []bool
	for i, t := range stmt {
		switch {
		case t.punct("("):
			parens = append(parens, i > 0 && stmt[i-1].is("EXTRACT"))
		case t.punct(")") && len(parens) > 0:
			parens = parens[:len(parens)-1]
		}
		extract[i] = len(parens) > 0 && parens[len(parens)-1]
	}

	// The tables the statement reads, and the names of CTEs it defines.
	ctes := map[string]bool{}
	for i := 0; i+2 < len(stmt); i++ {
		if (stmt[i].kind == tokWord || stmt[i].kind == tokQuoted) && stmt[i+1].is("AS") && stmt[i+2].punct("(") &&
			(i == 0 || !stmt[i-1].is("CAST") && !stmt[i-1].punct("(")) {
			ctes[strings.ToLower(stmt[i].text)] = true
		}
	}
	type source struct {
		path []string
		pos  int
	}
	var sources []source
	unknown := false
	definers := map[string]bool{}
	fromAt := map[int]bool{} // paren depth → inside a FROM clause
	depth := 0
	addSource := func(j int) {
		if j >= len(stmt) || stmt[j].punct("(") || stmt[j].is("UNNEST") {
			return
		}
		parts, next := path(stmt, j)
		if len(parts) == 0 {
			return
		}
		definers[strings.ToLower(parts[len(parts)-1])] = true
		if next < len(stmt) && stmt[next].punct("(") {
			unknown = true // a table-valued function
			return
		}
		if len(parts) == 1 && ctes[strings.ToLower(parts[0])] {
			return
		}
		sources = append(sources, source{parts, stmt[0].pos})
	}
	for i := 0; i < len(stmt); i++ {
		t := stmt[i]
		switch {
		case t.punct("(") || t.punct("["):
			depth++
		case t.punct(")") || t.punct("]"):
			delete(fromAt, depth)
			depth--
		case t.is("FROM") && extract[i]:
		case t.is("FROM") || t.is("JOIN"):
			fromAt[depth] = true
			addSource(i + 1)
		case t.punct(",") && fromAt[depth]:
			addSource(i + 1)
		case t.is("WHERE") || t.is("GROUP") || t.is("HAVING") || t.is("QUALIFY") || t.is("WINDOW") || t.is("ORDER") ||
			t.is("LIMIT") || t.is("UNION") || t.is("INTERSECT") || t.is("EXCEPT") || t.is("ON") || t.is("USING") ||
			t.is("SELECT") || t.is("SET") || t.is("WHEN"):
			delete(fromAt, depth)
			if t.is("USING") && stmt[0].is("MERGE") && depth == 0 {
				addSource(i + 1)
			}
		case i == 0 && t.is("UPDATE"):
			addSource(1)
		case i == 0 && t.is("MERGE"):
			j := 1
			if j < len(stmt) && stmt[j].is("INTO") {
				j++
			}
			addSource(j)
		}
	}

	// Each word the engine would replace, and whether it refers to the
	// variable.
	var edits []varEdit
	refs := map[string]string{} // lower-case name of a variable referred to → the word
	setDepth := -1              // the depth of UPDATE ... SET's assignments
	depth = 0
	for i, t := range stmt {
		switch {
		case t.punct("(") || t.punct("["):
			depth++
		case t.punct(")") || t.punct("]"):
			depth--
		case isUpdate && t.is("SET"):
			setDepth = depth
		case isUpdate && t.is("WHERE") && depth == setDepth:
			setDepth = -1
		}
		if t.kind == tokQuoted && i > 0 && (stmt[i-1].is("AS") || nameAfter[strings.ToUpper(stmt[i-1].text)] && stmt[i-1].kind == tokWord) {
			definers[strings.ToLower(t.text)] = true
		}
		if t.kind != tokWord || skip[i] {
			continue
		}
		lower := strings.ToLower(t.text)
		u, ok := vars[lower]
		if !ok || !engineIdent(t.text) {
			continue
		}
		if t.pos > 0 && sql[t.pos-1] == '.' {
			// t.n: a column or field, never replaced by the engine.
			definers[lower] = true
			continue
		}
		k := t.end
		for k < len(sql) && (sql[k] == ' ' || sql[k] == '\t') {
			k++
		}
		if k < len(sql) && sql[k] == '(' {
			continue // a call
		}
		if namesIn[i] {
			definers[lower] = true
			continue
		}
		if i > 0 {
			prev := stmt[i-1]
			pw := strings.ToUpper(prev.text)
			switch {
			case prev.is("FROM") && extract[i-1]:
				// EXTRACT(part FROM n): the value.
			case prev.is("AS"), prev.kind == tokWord && nameAfter[pw]:
				definers[lower] = true
				continue
			case isUpdate && depth == setDepth && (prev.is("SET") || prev.punct(",")) && i+1 < len(stmt) && stmt[i+1].punct("="):
				definers[lower] = true
				continue
			case prev.kind == tokString || prev.kind == tokQuoted || prev.punct(")") || prev.punct("]") ||
				prev.kind == tokWord && !reservedWords[pw]:
				// An alias in place of AS.
				definers[lower] = true
				continue
			}
		}
		refs[lower] = t.text
		edits = append(edits, varEdit{t.pos, t.end, u})
	}
	if len(refs) == 0 {
		return nil, ""
	}
	cols := map[string]bool{}
	for _, s := range sources {
		var names []string
		known := false
		if lookup != nil {
			names, known = lookup(s.path, s.pos)
		}
		if !known {
			unknown = true
			continue
		}
		for _, n := range names {
			cols[strings.ToLower(n)] = true
		}
	}
	var clash []string
	for lower := range refs {
		if definers[lower] || cols[lower] || unknown {
			clash = append(clash, refs[lower])
		}
	}
	if len(clash) == 0 {
		return edits, ""
	}
	sort.Strings(clash)
	why := "a column, an alias, a table or a function parameter of the same name"
	if unknown && !definers[strings.ToLower(clash[0])] && !cols[strings.ToLower(clash[0])] {
		why = "a table whose columns CloudBurrow could not read, which may have a column of the same name"
	}
	return nil, fmt.Sprintf("Not implemented here: the script's variable %s is named in a statement that also has %s "+
		"(%s). BigQuery reads each name by its scoping rules, but the emulator behind CloudBurrow replaces every bare "+
		"word naming a script variable with its value before it reads the statement (measured: DECLARE n INT64 "+
		"DEFAULT 1; SELECT 2 AS n failed \"Unexpected integer literal\"), so it could read the variable where BigQuery "+
		"reads another name, and BigQuery's documentation does not say which one it reads. Nothing was run. Give the "+
		"variable a name that nothing else in the statement has.", clash[0], why, statementText(sql, stmt))
}

// statementText is a statement's text, shortened for an error.
func statementText(sql string, stmt []token) string {
	s := strings.Join(strings.Fields(sql[stmt[0].pos:stmt[len(stmt)-1].end]), " ")
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
}

// engineIdent reports whether the engine reads word as one identifier: an
// ASCII letter or underscore, then ASCII letters, digits and underscores.
func engineIdent(word string) bool {
	for i := 0; i < len(word); i++ {
		c := word[i]
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return word != ""
}

// unname puts back the names renameVariables gave in an answer: an error
// that names a variable names it as the client wrote it.
func unname(b []byte, names map[string]string) []byte {
	for u, orig := range names {
		b = bytes.ReplaceAll(b, []byte(u), []byte(orig))
	}
	return b
}

// selectColumns returns the names of the columns a query's result has, as
// its text gives them, and whether they are known: each item of its first
// SELECT's list, by its alias, or the last name of a path. A * or an item
// whose name is not in the text makes them unknown; an expression with no
// alias has no name.
func selectColumns(query string) ([]string, bool) {
	toks, ok := lex(query)
	if !ok {
		return nil, false
	}
	i := 0
	if len(toks) > 0 && toks[0].is("WITH") {
		// The main query follows the CTEs: the first SELECT at the top
		// level.
		depth := 0
		for i = 1; i < len(toks); i++ {
			switch {
			case toks[i].punct("("):
				depth++
			case toks[i].punct(")"):
				depth--
			}
			if depth == 0 && toks[i].is("SELECT") {
				break
			}
		}
	}
	for i < len(toks) && toks[i].punct("(") {
		i++
	}
	if i >= len(toks) || !toks[i].is("SELECT") {
		return nil, false
	}
	i++
	for i < len(toks) && (toks[i].is("DISTINCT") || toks[i].is("ALL")) {
		i++
	}
	if i < len(toks) && toks[i].is("AS") {
		return nil, false // AS STRUCT, AS VALUE
	}
	var names []string
	for i < len(toks) {
		end := i
		depth := 0
		for ; end < len(toks); end++ {
			t := toks[end]
			if depth == 0 && (t.punct(",") || t.punct(")") || t.is("FROM") || t.is("WHERE") || t.is("UNION") ||
				t.is("INTERSECT") || t.is("EXCEPT") || t.is("GROUP") || t.is("ORDER") || t.is("LIMIT") || t.is("WINDOW") ||
				t.is("QUALIFY") || t.is("HAVING")) {
				break
			}
			switch {
			case t.punct("(") || t.punct("["):
				depth++
			case t.punct(")") || t.punct("]"):
				depth--
			case depth == 0 && t.punct("*"):
				return nil, false
			}
		}
		item := toks[i:end]
		n := len(item)
		switch {
		case n == 0:
			return nil, false
		case n >= 2 && item[n-2].is("AS"):
			names = append(names, item[n-1].text)
		case n >= 2 && (item[n-1].kind == tokWord && !reservedWords[strings.ToUpper(item[n-1].text)] || item[n-1].kind == tokQuoted) &&
			(item[n-2].kind == tokWord && !reservedWords[strings.ToUpper(item[n-2].text)] || item[n-2].kind == tokQuoted ||
				item[n-2].kind == tokString || item[n-2].punct(")") || item[n-2].punct("]")):
			names = append(names, item[n-1].text) // an alias without AS
		default:
			if parts, next := path(item, 0); next == n && len(parts) > 0 {
				names = append(names, parts[len(parts)-1])
			}
		}
		if end >= len(toks) || !toks[end].punct(",") {
			break
		}
		i = end + 1
	}
	return names, true
}

// columnListNames returns the names of the columns in a CREATE TABLE's
// column list, the statement's tokens.
func columnListNames(stmt []token) []string {
	i := 0
	for ; i < len(stmt) && !stmt[i].is("TABLE"); i++ {
	}
	i++
	if i+2 < len(stmt) && stmt[i].is("IF") && stmt[i+1].is("NOT") && stmt[i+2].is("EXISTS") {
		i += 3
	}
	_, i = path(stmt, i)
	if i >= len(stmt) || !stmt[i].punct("(") {
		return nil
	}
	var names []string
	for i++; i < len(stmt) && !stmt[i].punct(")"); {
		if stmt[i].is("PRIMARY") || stmt[i].is("FOREIGN") || stmt[i].is("CONSTRAINT") {
			i = skipTo(stmt, i, ",", ")")
		} else {
			names = append(names, stmt[i].text)
			i = skipTo(stmt, i+1, ",", ")")
		}
		if i < len(stmt) && stmt[i].punct(",") {
			i++
		}
	}
	return names
}
