package bigqueryfront

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// EXECUTE IMMEDIATE (#1011).
//
// BigQuery runs the statement EXECUTE IMMEDIATE gives, with the values
// its USING clause binds to the statement's query parameters
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/procedural-language#execute_immediate).
// The emulator runs nothing: measured against the pinned image through
// the front, `EXECUTE IMMEDIATE 'SELECT 1 AS a'` through jobs.query
// answered 200 with no rows and no schema, `EXECUTE IMMEDIATE 'SELECT @a +
// 1' USING 1 AS a` the same; `EXECUTE IMMEDIATE 'CREATE TABLE ds.t (x
// INT64)'`, through jobs.query and as a query job that ended DONE with no
// error, made no table; and `EXECUTE IMMEDIATE 'CREATE FUNCTION ...'`,
// alone, in BEGIN ... END and with the string in a DECLAREd variable, made
// no function.
//
// So the front carries out what it can before the query is sent: an
// EXECUTE IMMEDIATE whose SQL is a string literal, or a script variable
// whose value is one (its DECLARE's DEFAULT, or the last SET of it before
// the statement), is replaced by that statement, each of its query
// parameters by the USING clause's expression for it, in parentheses. The
// rest is 501 before anything runs (expandExecuteImmediate), as the
// emulator would report it done without running it.

// expandExecuteImmediate returns sql with each EXECUTE IMMEDIATE statement
// replaced by the statement it runs (above), and whether it replaced any;
// or the status (400 or 501) and message to answer the query with, when a
// statement cannot be carried out so.
func expandExecuteImmediate(sql string) (text string, changed bool, code int, msg string) {
	if !strings.Contains(strings.ToUpper(sql), "IMMEDIATE") {
		return sql, false, 0, ""
	}
	toks, ok := lex(sql)
	if !ok {
		return sql, false, 0, ""
	}
	notImplemented := func(why string) (string, bool, int, string) {
		return sql, false, http.StatusNotImplemented, "Not implemented here: EXECUTE IMMEDIATE " + why + " BigQuery runs " +
			"the statement it gives, but the emulator behind CloudBurrow reports every EXECUTE IMMEDIATE done and runs " +
			"nothing (measured), and CloudBurrow carries one out itself only when its SQL is a string literal, a " +
			"CONCAT or || of string literals and such variables, or a script variable set to one by its DECLARE's " +
			"DEFAULT or a SET, run with no INTO and with literals, " +
			"variables or query parameters in USING. Nothing was run."
	}
	// vars are the script's STRING variables whose value the text gives,
	// by lower-case name; unknown ones are those it does not give.
	vars := map[string]string{}
	unknown := map[string]bool{}
	var edits []varEdit
	for _, stmt := range splitStatements(toks) {
		body, _, _ := stripControlFlow(stmt)
		switch {
		case len(body) > 1 && body[0].is("DECLARE"):
			names, rest := declaredNames(body[1:])
			value, known := "", false
			for i := 0; i+1 < len(rest); i++ {
				if rest[i].is("DEFAULT") {
					value, known = constantString(rest[i+1:], vars)
					break
				}
			}
			for _, n := range names {
				n = strings.ToLower(n)
				if known {
					vars[n], unknown[n] = value, false
				} else {
					delete(vars, n)
					unknown[n] = true
				}
			}
		case len(body) > 1 && body[0].is("SET"):
			if body[1].punct("(") {
				for _, t := range body[2:skipTo(body, 2, ")")] {
					if t.kind == tokWord || t.kind == tokQuoted {
						delete(vars, strings.ToLower(t.text))
						unknown[strings.ToLower(t.text)] = true
					}
				}
				break
			}
			n := strings.ToLower(body[1].text)
			if len(body) > 2 && body[2].punct("=") {
				if v, ok := constantString(body[3:], vars); ok {
					vars[n], unknown[n] = v, false
					break
				}
			}
			delete(vars, n)
			unknown[n] = true
		case len(body) > 1 && body[0].is("EXECUTE") && body[1].is("IMMEDIATE"):
			// The SQL, up to INTO or USING at the top level.
			end := len(body)
			depth := 0
			into, using := -1, -1
			for i := 2; i < len(body); i++ {
				switch {
				case body[i].punct("(") || body[i].punct("["):
					depth++
				case body[i].punct(")") || body[i].punct("]"):
					depth--
				case depth == 0 && body[i].is("INTO") && into < 0 && using < 0:
					into = i
				case depth == 0 && body[i].is("USING") && using < 0:
					using = i
				}
			}
			if into >= 0 {
				return notImplemented("... INTO, which sets script variables from the statement's result.")
			}
			exprEnd := end
			if using >= 0 {
				exprEnd = using
			}
			expr := body[2:exprEnd]
			var inner string
			switch {
			case len(expr) == 1 && expr[0].kind == tokWord && vars[strings.ToLower(expr[0].text)] != "":
				inner = vars[strings.ToLower(expr[0].text)]
			case len(expr) == 1 && expr[0].kind == tokWord && unknown[strings.ToLower(expr[0].text)]:
				return notImplemented(fmt.Sprintf("of the variable %s, whose value the script sets with an expression "+
					"other than a string literal.", expr[0].text))
			default:
				s, ok := constantString(expr, vars)
				if !ok {
					return notImplemented(fmt.Sprintf("of %s, an expression other than a string literal, a CONCAT or "+
						"|| of them, or a script variable set to one.", statementText(sql, expr)))
				}
				inner = s
			}
			var items []usingItem
			if using >= 0 {
				var why string
				if items, why = usingItems(sql, body[using+1:end]); why != "" {
					return notImplemented(why)
				}
			}
			out, code, why := bindImmediate(inner, items)
			if code == http.StatusNotImplemented {
				return notImplemented(why)
			}
			if code != 0 {
				return sql, false, code, why
			}
			edits = append(edits, varEdit{body[0].pos, body[len(body)-1].end, out})
		}
	}
	if len(edits) == 0 {
		return sql, false, 0, ""
	}
	var b strings.Builder
	last := 0
	for _, e := range edits {
		b.WriteString(sql[last:e.pos])
		b.WriteString(e.text)
		last = e.end
	}
	b.WriteString(sql[last:])
	return b.String(), true, 0, ""
}

// declaredNames returns the names a DECLARE gives (a, b, c), and the
// tokens after them.
func declaredNames(t []token) ([]string, []token) {
	var names []string
	i := 0
	for i < len(t) && (t[i].kind == tokWord || t[i].kind == tokQuoted) {
		names = append(names, t[i].text)
		i++
		if i < len(t) && t[i].punct(",") {
			i++
			continue
		}
		break
	}
	return names, t[i:]
}

// usingItem is one value of EXECUTE IMMEDIATE's USING clause: its
// expression's text and, for a named parameter, its name.
type usingItem struct {
	expr, name string
}

// usingItems reads EXECUTE IMMEDIATE's USING clause, t. Each value must be
// a literal (a string, a number, TRUE, FALSE or NULL, a negative number),
// a script variable or a query parameter, so that putting it in place of
// each reference to its parameter reads it as BigQuery reads the value
// once. It returns why the clause cannot be read so, or "".
func usingItems(sql string, t []token) ([]usingItem, string) {
	var items []usingItem
	for len(t) > 0 {
		end := skipTo(t, 0, ",")
		item := t[:end]
		var it usingItem
		if n := len(item); n >= 3 && item[n-2].is("AS") && (item[n-1].kind == tokWord || item[n-1].kind == tokQuoted) {
			it.name = item[n-1].text
			item = item[:n-2]
		}
		if !simpleValue(sql, item) {
			if len(item) == 0 {
				return nil, "with an empty value in USING."
			}
			return nil, fmt.Sprintf("with USING %s, an expression CloudBurrow would put in place of the parameter "+
				"each time the statement names it; a literal, a script variable or a query parameter is supported.",
				statementText(sql, item))
		}
		it.expr = sql[item[0].pos:item[len(item)-1].end]
		items = append(items, it)
		if end >= len(t) {
			break
		}
		t = t[end+1:]
	}
	return items, ""
}

// simpleValue reports whether t is one literal, script variable or query
// parameter (usingItems).
func simpleValue(sql string, t []token) bool {
	if len(t) > 1 && t[0].punct("-") {
		t = t[1:]
		if t[0].kind != tokWord || t[0].text[0] < '0' || t[0].text[0] > '9' {
			return false
		}
	}
	switch {
	case len(t) == 1:
		return t[0].kind == tokString || t[0].kind == tokWord && !reservedWords[strings.ToUpper(t[0].text)] ||
			t[0].is("TRUE") || t[0].is("FALSE") || t[0].is("NULL")
	case len(t) == 2 && t[0].punct("@") && t[1].kind == tokWord && t[0].end == t[1].pos:
		return true // a query parameter
	case len(t) == 3 && t[0].kind == tokWord && t[1].punct(".") && t[2].kind == tokWord && t[0].end == t[1].pos &&
		t[1].end == t[2].pos && t[0].text[0] >= '0' && t[0].text[0] <= '9':
		return true // 1.5
	}
	return false
}

// bindImmediate returns the statement EXECUTE IMMEDIATE runs, inner, with
// each query parameter in it replaced by its USING value, in parentheses;
// or the status and message to refuse it with: 400 as BigQuery refuses a
// parameter with no value, 501 for what CloudBurrow does not carry out.
func bindImmediate(inner string, items []usingItem) (string, int, string) {
	toks, ok := lex(inner)
	if !ok {
		if len(items) == 0 {
			return inner, 0, "" // the engine reports the syntax error
		}
		return "", http.StatusNotImplemented, "of a statement CloudBurrow cannot read to put the USING values in."
	}
	// The statement, without the semicolons it may end with.
	for len(toks) > 0 && toks[len(toks)-1].punct(";") {
		inner = inner[:toks[len(toks)-1].pos]
		toks = toks[:len(toks)-1]
	}
	if len(toks) == 0 {
		return "", http.StatusBadRequest, "EXECUTE IMMEDIATE was given an empty statement."
	}
	for i, t := range toks {
		if t.punct(";") {
			return "", http.StatusNotImplemented, "of several statements (" + statementText(inner, toks[i+1:]) + " after the " +
				"first). BigQuery's documentation gives it one statement."
		}
		if t.is("EXECUTE") && i+1 < len(toks) && toks[i+1].is("IMMEDIATE") {
			return "", http.StatusNotImplemented, "of a statement that has an EXECUTE IMMEDIATE of its own."
		}
	}
	named := map[string]string{}
	var positional []string
	for _, it := range items {
		if it.name != "" {
			named[strings.ToLower(it.name)] = it.expr
		} else {
			positional = append(positional, it.expr)
		}
	}
	if len(named) > 0 && len(positional) > 0 {
		return "", http.StatusBadRequest, "EXECUTE IMMEDIATE's USING clause mixes named and positional parameters."
	}
	var b strings.Builder
	last, next := 0, 0
	usedNamed, usedPositional := false, false
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch {
		case t.punct("@") && i+1 < len(toks) && toks[i+1].punct("@"):
			i++ // @@system_variable
			if i+1 < len(toks) {
				i++
			}
		case t.punct("@") && i+1 < len(toks) && toks[i+1].pos == t.end &&
			(toks[i+1].kind == tokWord || toks[i+1].kind == tokQuoted):
			name := toks[i+1].text
			v, ok := named[strings.ToLower(name)]
			if !ok {
				return "", http.StatusBadRequest, fmt.Sprintf("Query parameter '%s' not found in EXECUTE IMMEDIATE's USING clause.", name)
			}
			usedNamed = true
			b.WriteString(inner[last:t.pos])
			b.WriteString("(" + v + ")")
			last = toks[i+1].end
			i++
		case t.punct("?"):
			if next >= len(positional) {
				return "", http.StatusBadRequest, "EXECUTE IMMEDIATE's statement has more positional parameters (?) than its USING clause has values."
			}
			usedPositional = true
			b.WriteString(inner[last:t.pos])
			b.WriteString("(" + positional[next] + ")")
			next++
			last = t.end
		}
	}
	if usedNamed && usedPositional {
		return "", http.StatusBadRequest, "EXECUTE IMMEDIATE's statement mixes named and positional parameters."
	}
	if next < len(positional) {
		return "", http.StatusBadRequest, "EXECUTE IMMEDIATE's USING clause has more values than its statement has positional parameters (?)."
	}
	b.WriteString(inner[last:])
	return b.String(), 0, ""
}

// constantString returns the value of t when it is a constant STRING
// expression (#1037): a string literal, a script variable whose value vars
// gives, or a CONCAT of such values or a || of them, in parentheses or not.
// A variable that is not in vars (its value is not known) is not constant.
func constantString(t []token, vars map[string]string) (string, bool) {
	for len(t) >= 2 && t[0].punct("(") && skipTo(t, 1, ")") == len(t)-1 {
		t = t[1 : len(t)-1]
	}
	// a || b || ..., at the top level.
	var parts [][]token
	depth, from := 0, 0
	for i := 0; i < len(t); i++ {
		switch {
		case t[i].punct("(") || t[i].punct("["):
			depth++
		case t[i].punct(")") || t[i].punct("]"):
			depth--
		case depth == 0 && i+1 < len(t) && t[i].punct("|") && t[i+1].punct("|") && t[i].end == t[i+1].pos:
			parts = append(parts, t[from:i])
			from = i + 2
			i++
		}
	}
	if len(parts) > 0 {
		parts = append(parts, t[from:])
		var b strings.Builder
		for _, p := range parts {
			v, ok := constantString(p, vars)
			if !ok {
				return "", false
			}
			b.WriteString(v)
		}
		return b.String(), true
	}
	switch {
	case len(t) == 1 && t[0].kind == tokString:
		return literalString(t)
	case len(t) == 1 && t[0].kind == tokWord:
		v, ok := vars[strings.ToLower(t[0].text)]
		return v, ok
	case len(t) >= 3 && t[0].is("CONCAT") && t[1].punct("(") && t[len(t)-1].punct(")") && skipTo(t, 2, ")") == len(t)-1:
		args := t[2 : len(t)-1]
		var b strings.Builder
		for len(args) > 0 {
			end := skipTo(args, 0, ",")
			v, ok := constantString(args[:end], vars)
			if !ok {
				return "", false
			}
			b.WriteString(v)
			if end >= len(args) {
				break
			}
			args = args[end+1:]
		}
		return b.String(), true
	}
	return "", false
}

// literalString returns the value of t when it is one string literal (not
// a bytes literal), with its escapes read as GoogleSQL reads them
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/lexical#string_and_bytes_literals).
func literalString(t []token) (string, bool) {
	if len(t) != 1 || t[0].kind != tokString {
		return "", false
	}
	return decodeString(t[0].text)
}

// decodeString reads a GoogleSQL string literal as lex gave it.
func decodeString(lit string) (string, bool) {
	raw := false
	for len(lit) > 0 && lit[0] != '\'' && lit[0] != '"' {
		switch lit[0] {
		case 'r', 'R':
			raw = true
		default:
			return "", false // a bytes literal
		}
		lit = lit[1:]
	}
	q := lit[:1]
	if strings.HasPrefix(lit, q+q+q) && len(lit) >= 6 {
		q = q + q + q
	}
	if len(lit) < 2*len(q) {
		return "", false
	}
	body := lit[len(q) : len(lit)-len(q)]
	if raw {
		return body, true
	}
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(body) {
			return "", false
		}
		switch c = body[i]; c {
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte('\v')
		case '\\', '?', '"', '\'', '`':
			b.WriteByte(c)
		case 'x', 'X', 'u', 'U':
			n := map[byte]int{'x': 2, 'X': 2, 'u': 4, 'U': 8}[c]
			if i+1+n > len(body) {
				return "", false
			}
			v, err := strconv.ParseUint(body[i+1:i+1+n], 16, 32)
			if err != nil {
				return "", false
			}
			if n == 2 {
				b.WriteByte(byte(v))
			} else {
				if !utf8.ValidRune(rune(v)) {
					return "", false
				}
				b.WriteRune(rune(v))
			}
			i += n
		case '0', '1', '2', '3', '4', '5', '6', '7':
			if i+3 > len(body) {
				return "", false
			}
			v, err := strconv.ParseUint(body[i:i+3], 8, 8)
			if err != nil {
				return "", false
			}
			b.WriteByte(byte(v))
			i += 2
		default:
			return "", false
		}
	}
	return b.String(), true
}
