package bigqueryfront

import (
	"fmt"
	"strings"
)

// checkDDL returns why a CREATE TABLE or CREATE SCHEMA statement in sql
// names something BigQuery refuses, or "" (#881).
//
// The emulator's SQL engine refuses a column named twice (measured, 400)
// and a name that does not lex, but a quoted name is taken whatever it
// holds: CREATE TABLE ds.`t!`, a column `a b!`, and CREATE SCHEMA
// `bad-name` all succeeded. Those are the names checked here, with the
// rules tables.insert and datasets.insert are held to: the table ID, each
// column in the column list and each field of a STRUCT column type, and
// the dataset ID of CREATE SCHEMA.
//
// It reads the statement with a lexer, not a parser. What is not checked:
// the columns of CREATE TABLE ... AS SELECT (they come from the query),
// ALTER TABLE (ADD COLUMN, RENAME), and a statement inside a control-flow
// block of a script (IF, LOOP, BEGIN ... EXCEPTION); a script's top-level
// statements are checked each.
func checkDDL(sql string) string {
	if !strings.Contains(strings.ToUpper(sql), "CREATE") {
		return ""
	}
	toks, ok := lex(sql)
	if !ok {
		// The engine reports its own syntax error.
		return ""
	}
	for _, stmt := range splitStatements(toks) {
		if msg := checkCreate(stmt); msg != "" {
			return msg
		}
	}
	return ""
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
			toks = append(toks, token{tokQuoted, b.String()})
			i = j + 1
		case c == '\'' || c == '"':
			n, ok := stringEnd(s[i:], false)
			if !ok {
				return nil, false
			}
			toks = append(toks, token{tokString, s[i : i+n]})
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
				toks = append(toks, token{tokString, s[i : j+n]})
				i = j + n
				continue
			}
			toks = append(toks, token{tokWord, s[i:j]})
			i = j
		default:
			toks = append(toks, token{tokPunct, string(c)})
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

// checkCreate checks one statement if it is CREATE TABLE or CREATE SCHEMA.
func checkCreate(t []token) string {
	i := 0
	for i < len(t) && t[i].is("BEGIN") {
		i++
	}
	if i >= len(t) || !t[i].is("CREATE") {
		return ""
	}
	i++
	if i+1 < len(t) && t[i].is("OR") && t[i+1].is("REPLACE") {
		i += 2
	}
	for i < len(t) && (t[i].is("TEMP") || t[i].is("TEMPORARY") || t[i].is("SNAPSHOT") || t[i].is("EXTERNAL")) {
		i++
	}
	if i >= len(t) {
		return ""
	}
	schema := t[i].is("SCHEMA")
	if !schema && !t[i].is("TABLE") {
		return ""
	}
	i++
	if i < len(t) && t[i].is("FUNCTION") {
		return ""
	}
	if i+2 < len(t) && t[i].is("IF") && t[i+1].is("NOT") && t[i+2].is("EXISTS") {
		i += 3
	}
	parts, i := path(t, i)
	if len(parts) == 0 {
		return ""
	}
	name := parts[len(parts)-1]
	if schema {
		return checkDatasetID(name)
	}
	if msg := checkTableID(name); msg != "" {
		return msg
	}
	if i < len(t) && t[i].punct("(") {
		return checkColumns(t, i+1)
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
