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
// the statement), or an expression it evaluates first (evalImmediate,
// #1037), is replaced by that statement, each of its query
// parameters by the USING clause's expression for it, in parentheses. The
// rest is carried out in its turn by the front's own run of the script
// where that is faithful (immediateInTurn), or 501 before anything runs
// (expandExecuteImmediate), as the emulator would report it done without
// running it.

// immediateInTurn is the code expandExecuteImmediate gives for an EXECUTE
// IMMEDIATE it does not put in the script's text but the front's own run
// of the script carries out (runScript, scriptRun.executeImmediate), as
// BigQuery does, in its turn (#1037): SQL whose value the front cannot
// evaluate first faithfully (it calls CURRENT_TIMESTAMP or RAND, it reads
// a table after DML), a USING value that is an expression, a statement
// that names a script variable (sent as a request of its own, it does not
// see them, as BigQuery hides them: "all other local variables and query
// parameters aren't exposed"). In its turn each expression is evaluated once, when the
// statements before it have run. The caller runs the script so
// (rewriteQuery), or answers 501 with the message given when it cannot: a
// TEMP table or function lasts only for the request that made it.
const immediateInTurn = 1

// expandExecuteImmediate returns sql with each EXECUTE IMMEDIATE statement
// replaced by the statement it runs (above), and whether it replaced any;
// or the status (400 or 501) and message to answer the query with, when a
// statement cannot be carried out so; or immediateInTurn, with the 501
// message to answer when the script cannot be run in turn.
//
// eval, when not nil, evaluates SQL given by an expression the front does
// not read itself (evalImmediate); with nil such SQL is 501.
func expandExecuteImmediate(sql string, eval immediateEval) (text string, changed bool, code int, msg string) {
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
			"DEFAULT or a SET, or an expression it can evaluate first (evalImmediate: after DECLARE, SET, queries, DML " +
			"and DDL, in BEGIN ... END blocks but no other control flow, with no function whose value changes from one run " +
			"to the next), run with INTO only " +
			"of a query, and with literals, " +
			"variables or query parameters in USING. Nothing was run."
	}
	// inTurn is a refusal the front's own run of the script, statement by
	// statement, carries out instead (immediateInTurn).
	inTurn := func(why string) (string, bool, int, string) {
		text, changed, _, msg := notImplemented(why)
		return text, changed, immediateInTurn, msg
	}
	// vars are the script's STRING variables whose value the text gives,
	// by lower-case name; unknown ones are those it does not give.
	vars := map[string]string{}
	unknown := map[string]bool{}
	// declared are the script's variables so far, by lower-case name,
	// which BigQuery hides from the statement (hiddenVariable).
	declared := map[string]bool{}
	var edits []varEdit
	stmts := splitStatements(toks)
	for si, stmt := range stmts {
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
				declared[n] = true
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
			exprEnd := end
			if into >= 0 {
				exprEnd = into
			} else if using >= 0 {
				exprEnd = using
			}
			// INTO's variables, up to USING.
			var intoVars []string
			if into >= 0 {
				stop := end
				if using > into {
					stop = using
				}
				names, rest := declaredNames(body[into+1 : stop])
				switch {
				case len(names) == 0 || len(rest) > 0:
					return notImplemented(fmt.Sprintf("... INTO %s, which is not one script variable.",
						statementText(sql, body[into+1:stop])))
				}
				for _, t := range body[into+1 : stop] {
					if t.kind == tokQuoted && !plainIdentifier(t.text) {
						return notImplemented(fmt.Sprintf("... INTO %s, a quoted name that must be quoted. CloudBurrow "+
							"runs INTO as a SET of each variable, and the emulator behind it does not run a SET of a "+
							"quoted name (measured).", statementText(sql, []token{t})))
					}
				}
				// A quoted name that need not be quoted names the same
				// variable unquoted, which the emulator can SET.
				for _, n := range names {
					intoVars = append(intoVars, n)
					delete(vars, strings.ToLower(n))
					unknown[strings.ToLower(n)] = true
				}
			}
			expr := body[2:exprEnd]
			var inner string
			switch {
			case len(expr) == 1 && expr[0].kind == tokWord && vars[strings.ToLower(expr[0].text)] != "":
				inner = vars[strings.ToLower(expr[0].text)]
			default:
				s, ok := constantString(expr, vars)
				if !ok && len(expr) > 0 {
					// The SQL is computed: evaluated first, with the
					// variables as the statements before it leave them,
					// when that can be done faithfully (#1037).
					var code int
					var why string
					s, code, why = evalImmediate(sql, stmts, si, statementText(sql, expr), expr, eval)
					switch code {
					case 0:
						ok = true
					case http.StatusNotImplemented:
						return inTurn(why)
					default:
						return sql, false, code, why
					}
				}
				if !ok {
					return notImplemented(fmt.Sprintf("of %s, an expression other than a string literal, a CONCAT or "+
						"|| of them, or a script variable set to one.", statementText(sql, expr)))
				}
				inner = s
			}
			if name := hiddenVariable(inner, declared); name != "" {
				return inTurn(fmt.Sprintf("of %q, which names %s, a variable of the script. BigQuery does not "+
					"show the script's variables to the statement (\"all other local variables and query parameters "+
					"aren't exposed\"), so it reads a column or alias of that name or fails, but CloudBurrow runs the "+
					"statement in the script, where the name reads the variable; pass its value with USING.", inner, name))
			}
			var items []usingItem
			if using >= 0 {
				var why string
				var expression bool
				if items, expression, why = usingItems(sql, body[using+1:end]); why != "" {
					if expression {
						return inTurn(why)
					}
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
			if into >= 0 {
				if cols, known := selectItemCount(out); known && cols != len(intoVars) {
					return notImplemented(fmt.Sprintf("... INTO %d variables of a query of %d columns. BigQuery's "+
						"documentation assigns the columns to the variables by position, and does not say what it "+
						"does when their numbers differ.", len(intoVars), cols))
				}
				bound := out
				later := laterResult(stmts[si+1:])
				if out, why = immediateInto(out, intoVars, later, len(edits)+1); why != "" {
					return notImplemented(why)
				}
				if !later && eval != nil {
					// No later statement to put the guard before: the
					// rows are counted first (countRows, #1037).
					many, code, why := countRows(sql, stmts, si, bound, eval)
					switch {
					case code == http.StatusNotImplemented:
						return inTurn(why)
					case code != 0:
						return sql, false, code, why
					case many:
						out = "SELECT ERROR('EXECUTE IMMEDIATE ... INTO: the query returned more than one row')"
					}
				}
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

// immediateInto returns the statements that run EXECUTE IMMEDIATE's
// query, inner, INTO the variables vars (#1037). BigQuery sets the
// variables from the query's one row, the first column into the first
// variable and so on, and fails the script at the statement when the query
// returns more than one row
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/procedural-language#execute_immediate).
//
// One variable is SET v = (query). The emulator does not run SET (a, b) =
// ... (measured: a parse error), so several are one SET per variable, each
// taking its column of the same row by position: (SELECT AS STRUCT * FROM
// (query))[OFFSET(i)], which the emulator runs (measured: SET a = ...
// [OFFSET(0)] and SET b = ...[OFFSET(1)] of SELECT 5, 'five' set 5 and
// 'five', named columns or not). A scalar subquery of several rows gives
// the first there (measured: SET x = (SELECT n FROM UNNEST([1, 2]) n) set
// 1), so each value is IF(the query has more than one row, ERROR(...),
// value) ("more than one row"; BigQuery's wording UNVERIFIED).
//
// The emulator evaluates a SET's value only when the variable is read
// (measured: a script ending in the SET of two rows succeeded), so when
// guard is true the statements begin with a CREATE TEMP TABLE of that IF,
// which the emulator runs at once (measured: it failed the script there,
// and its TEMP table, _cloudburrow_into_n, is dropped at the script's
// end). A DDL statement is a result of its own in the emulator's answer
// (measured: SELECT 3; CREATE TEMP TABLE ... answered no rows), so guard
// is given only when a later statement of the script answers instead
// (laterResult). Otherwise the caller counts the query's rows first
// (countRows), and when there are several puts SELECT ERROR(...) in the
// statement's place, so the script still fails at the statement and its
// answer is unchanged when it does not. No rows set the variables NULL, as BigQuery
// documents ("If the query returns zero rows, all variables in the INTO
// clause are set to NULL"). A number of variables other than the query's
// columns, when its text gives them (selectItemCount), is 501 before this:
// BigQuery documents only that the columns are assigned by position.
//
// inner must be a query (SELECT or WITH); it returns why it cannot be run
// so, or "".
func immediateInto(inner string, vars []string, guard bool, n int) (string, string) {
	toks, ok := lex(inner)
	if !ok || len(toks) == 0 {
		return "", "... INTO of a statement CloudBurrow cannot read."
	}
	first := 0
	for first < len(toks) && toks[first].punct("(") {
		first++
	}
	if first >= len(toks) || !toks[first].is("SELECT") && !toks[first].is("WITH") {
		return "", "... INTO of " + statementText(inner, toks) + ", which is not a query (SELECT or WITH)."
	}
	tooMany := "(SELECT COUNT(*) FROM (" + inner + ")) > 1, ERROR('EXECUTE IMMEDIATE ... INTO: the query returned more than one row')"
	var b strings.Builder
	if guard {
		fmt.Fprintf(&b, "CREATE TEMP TABLE _cloudburrow_into_%d AS SELECT IF(%s, 0) AS ok; ", n, tooMany)
	}
	if len(vars) == 1 {
		b.WriteString("SET " + vars[0] + " = IF(" + tooMany + ", (" + inner + "))")
		return b.String(), ""
	}
	for i, v := range vars {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "SET %s = IF(%s, (SELECT AS STRUCT * FROM (%s))[OFFSET(%d)])", v, tooMany, inner, i)
	}
	return b.String(), ""
}

// laterResult reports whether one of stmts, the statements after an
// EXECUTE IMMEDIATE ... INTO, gives the script's answer in the emulator
// (a query, DML or DDL), so that a statement put before them does not
// change it (immediateInto).
func laterResult(stmts [][]token) bool {
	for _, s := range stmts {
		body, _, _ := stripControlFlow(s)
		if len(body) == 0 {
			continue
		}
		for _, w := range []string{"SELECT", "WITH", "INSERT", "UPDATE", "DELETE", "MERGE", "CREATE", "DROP", "ALTER", "TRUNCATE"} {
			if body[0].is(w) {
				return true
			}
		}
		if body[0].punct("(") {
			return true
		}
	}
	return false
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

// hiddenVariable returns the first name in inner, the statement an
// EXECUTE IMMEDIATE runs, that is one of declared, the script's variables
// by lower-case name; or "". BigQuery runs the statement apart from the
// script: "The SQL statement can access query parameters that are passed
// using the USING clause; all other local variables and query parameters
// aren't exposed" (https://cloud.google.com/bigquery/docs/reference/standard-sql/procedural-language#execute_immediate).
// The front puts the statement in the script, where such a name would
// read the variable, so a statement that names one is not run (#1037). A
// name after AS (an alias it gives), after a dot (a field), after @ (a
// query parameter) or before ( (a function) is not a reference to a
// variable.
func hiddenVariable(inner string, declared map[string]bool) string {
	if len(declared) == 0 {
		return ""
	}
	toks, ok := lex(inner)
	if !ok {
		return ""
	}
	for i, t := range toks {
		if t.kind != tokWord && t.kind != tokQuoted || !declared[strings.ToLower(t.text)] {
			continue
		}
		if i > 0 && (toks[i-1].is("AS") || toks[i-1].punct(".") || toks[i-1].punct("@")) {
			continue
		}
		if i+1 < len(toks) && toks[i+1].punct("(") {
			continue
		}
		return t.text
	}
	return ""
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
//
// expression reports that the refusal is of a value that is an expression,
// which the front's own run of the script evaluates once (immediateInTurn).
func usingItems(sql string, t []token) (items []usingItem, expression bool, why string) {
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
				return nil, false, "with an empty value in USING."
			}
			return nil, true, fmt.Sprintf("with USING %s, an expression CloudBurrow would put in place of the parameter "+
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
	return items, false, ""
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

// immediateEval runs probe, a script, on the emulator and returns the
// value of the one column of its result's first row (evalImmediate): the
// column's name ("" when the script answered no columns) and type, the
// value, and whether it is NULL; or, when the script failed, the status
// and message to answer the query with.
type immediateEval func(probe string) (name, typ, value string, null bool, code int, msg string)

// unstableFunctions are the functions whose value changes from one run to
// the next, so that evaluating an expression that calls one first, and
// then running the script, would not give the value BigQuery gives
// (evalImmediate). TABLESAMPLE reads rows at random.
var unstableFunctions = map[string]bool{
	"RAND": true, "GENERATE_UUID": true, "CURRENT_DATE": true, "CURRENT_TIME": true, "CURRENT_DATETIME": true,
	"CURRENT_TIMESTAMP": true, "SESSION_USER": true, "TABLESAMPLE": true,
}

// evalImmediate returns the SQL that EXECUTE IMMEDIATE's expression, expr
// (its text, text), gives, when the front does not read it from the text
// (#1037): FORMAT, CAST and any other expression. stmts are the script's
// statements, split at semicolons, and the EXECUTE IMMEDIATE is stmts[si].
// BigQuery evaluates the expression when the script reaches the statement,
// with the variables as the statements before it left them, and then runs
// the string
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/procedural-language#execute_immediate).
// The emulator runs no EXECUTE IMMEDIATE, so the front evaluates it first,
// with a probe (probeScript): a script of its own that runs the statements
// before it, then SELECT (expr), and puts the string in the statement's
// place. When that cannot be faithful it is 501.
//
// The expression's value must be a STRING that is not NULL; BigQuery fails
// the script otherwise (its message UNVERIFIED), and the front answers 400.
// When the evaluation fails, its error is the answer, 400: the script
// fails there in BigQuery too, before the statement runs. When the probe
// left out DML or DDL, a failure could be the probe's own (a function it
// did not create), so it is 501.
func evalImmediate(sql string, stmts [][]token, si int, text string, expr []token, eval immediateEval) (string, int, string) {
	typ, value, null, code, msg := runProbe(sql, stmts, si, text, expr, eval)
	switch {
	case code != 0:
		return "", code, msg
	case null:
		// Before the type: the emulator types a NULL variable INTEGER
		// whatever its DECLARE gives (measured: DECLARE s STRING; SELECT
		// (s) answered a NULL INTEGER).
		return "", http.StatusBadRequest, fmt.Sprintf("EXECUTE IMMEDIATE's SQL cannot be NULL; %s is NULL.", text)
	case !strings.EqualFold(typ, "STRING"):
		return "", http.StatusBadRequest, fmt.Sprintf("EXECUTE IMMEDIATE's SQL must be a STRING; %s is %s.", text, typ)
	}
	return value, 0, ""
}

// countRows reports whether bound, the query EXECUTE IMMEDIATE stmts[si]
// runs INTO variables, its USING values bound, returns more than one row,
// counted by a probe (probeScript) before the script runs (#1037). It is
// used when no later statement gives the script's answer, so that the
// guard immediateInto puts before a later one cannot be used (it would be
// the answer): when it does, the front puts SELECT ERROR(...) in the
// statement's place, so the script fails there, after the statements
// before it, as BigQuery's does; otherwise nothing changes, and the
// answer is the same. When the count cannot be faithful it is 501.
func countRows(sql string, stmts [][]token, si int, bound string, eval immediateEval) (bool, int, string) {
	text := "(SELECT COUNT(*) FROM (" + bound + "))"
	toks, ok := lex(bound) // the query's own tokens, for probeScript's checks
	if !ok {
		return false, http.StatusNotImplemented, "... INTO of a query CloudBurrow cannot read to count its rows."
	}
	_, value, _, code, msg := runProbe(sql, stmts, si, text, toks, eval)
	if code != 0 {
		return false, code, strings.Replace(msg, "of "+text+",", "... INTO of "+bound+", whose rows CloudBurrow counts first,", 1)
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return false, http.StatusNotImplemented, "... INTO of " + bound + ", whose rows CloudBurrow could not count first."
	}
	return n > 1, 0, ""
}

// runProbe runs the probe of the expression expr (its text, text) for the
// EXECUTE IMMEDIATE stmts[si] (probeScript) and returns its value's type,
// the value and whether it is NULL; or the status and message to answer
// with.
func runProbe(sql string, stmts [][]token, si int, text string, expr []token, eval immediateEval) (string, string, bool, int, string) {
	if eval == nil {
		return "", "", false, http.StatusNotImplemented, fmt.Sprintf("of %s, an expression CloudBurrow does not evaluate here.", text)
	}
	probe, skipped, why := probeScript(sql, stmts, si, text, expr)
	if why != "" {
		return "", "", false, http.StatusNotImplemented, fmt.Sprintf("of %s, which CloudBurrow evaluates first by running "+
			"the statements before it, which it cannot do faithfully here: %s", text, why)
	}
	name, typ, value, null, code, msg := eval(probe)
	switch {
	case code != 0 && skipped && code != http.StatusNotImplemented:
		return "", "", false, http.StatusNotImplemented, fmt.Sprintf("of %s, which CloudBurrow evaluates first by running "+
			"the statements before it without their DML and DDL, which failed (%s), maybe because of what it left out.", text, msg)
	case code != 0:
		return "", "", false, code, msg
	case !strings.EqualFold(name, "_cloudburrow_sql"):
		return "", "", false, http.StatusNotImplemented, fmt.Sprintf("of %s, which CloudBurrow evaluates first, and which "+
			"gave no value.", text)
	}
	return typ, value, null, 0, ""
}

// probeScript returns the probe for the expression expr (its text, text)
// of the EXECUTE IMMEDIATE stmts[si], and whether it leaves out DML or
// DDL; or why it cannot be faithful. The probe runs statements of the
// script a second time, so it is built only when that gives the value
// BigQuery would:
//
//   - DECLARE and SET run as they are;
//   - a query, and DML and DDL, are not run (each is SET _cloudburrow_nop =
//     0, a variable the probe declares first, so that its own SELECT is its
//     answer): a query changes nothing, and after DML or DDL no statement
//     the probe runs, nor the expression, may read a table (FROM), so what
//     the DML or DDL changed is not read;
//   - BEGIN ... END blocks are run as their statements, without BEGIN and
//     END: the emulator runs a block as its statements, and not a block
//     sent to it as such (measured: "Statement not supported:
//     BeginEndBlock"; the front's own run of a script flattens it), and it
//     runs no DECLARE in one, so a block has no variables of its own.
//     IF, LOOP, WHILE, REPEAT, FOR and CASE are not followed: the
//     emulator does not run the statements in them (checkDDL, measured),
//     so neither the script nor a probe of it could; nor an EXCEPTION
//     handler (the probe does not raise the errors of the queries it does
//     not run), CALL, RAISE, a transaction or another EXECUTE IMMEDIATE;
//   - no function whose value changes from one run to the next
//     (unstableFunctions), function of a dataset (it could be one) or system
//     variable (@@) in what the probe runs.
//
// The probe ends with SELECT (expr) AS _cloudburrow_sql.
func probeScript(sql string, stmts [][]token, si int, text string, expr []token) (string, bool, string) {
	var b strings.Builder
	b.WriteString("DECLARE _cloudburrow_nop INT64")
	skipped := false
	open := 0
	for i := 0; i <= si; i++ {
		seg := stmts[i]
		if len(seg) == 0 {
			continue
		}
		// The BEGINs in front of the statement, and the END that is one.
		head := 0
		for head < len(seg) && seg[head].is("BEGIN") && !(head+1 < len(seg) && (seg[head+1].is("TRANSACTION") || seg[head+1].is("TRAN"))) {
			head++
		}
		body := seg[head:]
		if len(body) > 0 && body[0].is("END") {
			if len(body) > 1 || open == 0 {
				return "", false, "the script has a control-flow block (" + statementText(sql, body) + "), which the " +
					"emulator behind CloudBurrow does not run."
			}
			open--
			continue
		}
		open += head
		b.WriteString("; ")
		kept := expr
		if i == si {
			if !body[0].is("EXECUTE") {
				return "", false, "the statement is in a control-flow block (" + strings.ToUpper(body[0].text) + "), which the " +
					"emulator behind CloudBurrow does not run."
			}
			b.WriteString("SELECT (" + text + ") AS _cloudburrow_sql")
		} else {
			switch k := statementKind(body); k {
			case "run":
				b.WriteString(statementText(sql, body))
				kept = body
			case "query", "effect":
				b.WriteString("SET _cloudburrow_nop = 0")
				skipped = skipped || k == "effect"
				kept = nil
			default:
				return "", false, "the script has " + k + ", which the probe cannot run again or leave out."
			}
		}
		for j, t := range kept {
			switch {
			case t.is("FROM") && skipped:
				return "", false, "it reads a table after DML or DDL, which the probe would read without what they changed."
			case t.kind == tokWord && unstableFunctions[strings.ToUpper(t.text)]:
				return "", false, fmt.Sprintf("it calls %s, whose value differs from one run to the next.", strings.ToUpper(t.text))
			case t.punct("@") && j+1 < len(kept) && kept[j+1].punct("@"):
				return "", false, "it reads a system variable, which the probe would not read as the script does."
			case t.punct(".") && j+2 < len(kept) && (kept[j+1].kind == tokWord || kept[j+1].kind == tokQuoted) && kept[j+2].punct("("):
				name := kept[j+1].text
				if j > 0 {
					name = kept[j-1].text + "." + name
				}
				return "", false, fmt.Sprintf("it calls %s, a function of a dataset, whose value could differ from one "+
					"run to the next.", name)
			}
		}
	}
	// A block around the statement with an EXCEPTION handler, after it.
	for i := si + 1; i < len(stmts) && open > 0; i++ {
		for _, t := range stmts[i] {
			if t.is("EXCEPTION") {
				return "", false, "the statement may be in a block with an EXCEPTION handler."
			}
			if !t.is("BEGIN") {
				break
			}
		}
	}
	return b.String(), skipped, ""
}

// statementKind is how probeScript treats the statement t: "run" (DECLARE,
// SET), "query", "effect" (DML, DDL), or what it is when it can be neither
// run again nor left out.
func statementKind(t []token) string {
	if len(t) == 0 {
		return "an empty statement"
	}
	if t[0].punct("(") {
		return "query"
	}
	if t[0].kind != tokWord {
		return "a statement CloudBurrow cannot read"
	}
	switch strings.ToUpper(t[0].text) {
	case "DECLARE", "SET":
		return "run"
	case "SELECT", "WITH":
		return "query"
	case "INSERT", "UPDATE", "DELETE", "MERGE", "TRUNCATE", "CREATE", "DROP", "ALTER":
		return "effect"
	case "IF", "LOOP", "WHILE", "REPEAT", "FOR", "CASE", "ELSE", "ELSEIF", "EXCEPTION", "UNTIL":
		return "a control-flow block (" + strings.ToUpper(t[0].text) + "), which the emulator behind CloudBurrow does not run"
	}
	return "a statement " + strings.ToUpper(t[0].text)
}

// plainIdentifier reports whether s, a quoted name's text, is a name that
// need not be quoted: letters, digits and underscores, not first a digit,
// and not a reserved word.
func plainIdentifier(s string) bool {
	if s == "" || s[0] >= '0' && s[0] <= '9' || reservedWords[strings.ToUpper(s)] {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// selectItemCount returns the number of columns a query's text gives, the
// items of its first SELECT's list, and whether it is known: a *, or
// SELECT AS STRUCT or AS VALUE, makes it unknown.
func selectItemCount(query string) (int, bool) {
	toks, ok := lex(query)
	if !ok {
		return 0, false
	}
	i, depth := 0, 0
	if len(toks) > 0 && toks[0].is("WITH") {
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
		return 0, false
	}
	i++
	for i < len(toks) && (toks[i].is("DISTINCT") || toks[i].is("ALL")) {
		i++
	}
	if i >= len(toks) || toks[i].is("AS") {
		return 0, false
	}
	n, depth := 1, 0
	for ; i < len(toks); i++ {
		t := toks[i]
		if depth == 0 && (t.punct(")") || t.punct(";") || t.is("FROM") || t.is("WHERE") || t.is("UNION") ||
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
			return 0, false
		case depth == 0 && t.punct(","):
			n++
		}
	}
	return n, true
}
