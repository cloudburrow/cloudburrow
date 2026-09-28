package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// Function names without a dataset (#1033).
//
// In BigQuery a query's default dataset is "the default dataset to use
// for unqualified table names in the query ... Setting the system
// variable @@dataset_id achieves the same behavior"
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobConfigurationQuery.FIELDS.default_dataset),
// and @@dataset_id "is used when a dataset is not specified for a project
// in the query" (https://cloud.google.com/bigquery/docs/reference/system-variables);
// a persistent function must be named with its dataset only "from another
// persistent UDF or a logical view"
// (https://cloud.google.com/bigquery/docs/user-defined-functions, limitations
// of persistent UDFs). So a query's call of a persistent function with no
// dataset calls the one in its default dataset.
//
// The emulator's engine registers each function, as it does a table, under
// every suffix of its path, the bare name only when no function holds it
// yet (#1015, qualify.go), and never reads the default dataset for one.
// Measured against the pinned image through the front with the official Go
// client: with b1.fn(x) AS (x + 1) made first and b2.fn(x) AS (x + 2),
// SELECT fn(0) with the default dataset b2 failed 400 "failed to query
// SELECT fn(0): sqlite3: SQL logic error: no such column: w1082"; with a
// function of a name no other dataset had, SELECT myfn(1) read 2; SELECT
// b2.fn(0) read 2.
//
// So the front sends a call of a function without a dataset as
// dataset.name, the query's default dataset, when that dataset has a
// function of the name that the front knows (knownFunctions: made by a
// CREATE FUNCTION it sent on, or routines.insert through it) or a CREATE
// FUNCTION of the script makes it; and when the name is not a built-in
// function's. The engine tells a built-in by SAFE., which it takes only for
// one (measured: SELECT SAFE.upper() failed "No matching signature for
// function UPPER", SAFE.fn() of a function made by CREATE FUNCTION "No
// matching signature for function :<PROJECT>_<DATASET>_FN", and
// SAFE.nosuchthing() "Function not found"); a built-in's name is left as
// it is, which the engine calls. A call in a CREATE FUNCTION, CREATE
// PROCEDURE or CREATE VIEW statement is left as it is: its body must name
// a persistent function with its dataset. A TEMP function the script makes
// is left as it is, and so is a name after FROM or JOIN (a table function:
// CREATE TABLE FUNCTION is 501, #1043). Not qualified: a function the
// front does not know, one routines.insert made before the front started
// (knownFunctions); the engine resolves its bare name itself.

// qualifyFunctions returns sql with each call of a function that
// dataset has (above) given without a dataset written as dataset.name,
// and whether it changed any.
func (f front) qualifyFunctions(r *http.Request, sql, dataset string) (string, bool) {
	if dataset == "" {
		return sql, false
	}
	toks, ok := lex(sql)
	if !ok {
		return sql, false
	}
	stmts := splitStatements(toks)
	names := map[string]bool{} // lower-case names of the dataset's functions
	temps := map[string]bool{}
	for _, stmt := range stmts {
		body, _, _ := stripControlFlow(stmt)
		s, ok := functionStatement(body)
		if !ok || s.drop {
			continue
		}
		name := strings.ToLower(s.path[len(s.path)-1])
		switch {
		case s.temp:
			temps[name] = true
		case len(s.path) == 1 || len(s.path) == 2 && strings.EqualFold(s.path[0], dataset):
			names[name] = true
		}
	}
	for _, p := range f.functionsIn(r, projectOf(f.base), dataset) {
		names[strings.ToLower(p[len(p)-1])] = true
	}
	if len(names) == 0 {
		return sql, false
	}
	type call struct{ pos, end int }
	var calls []call
	builtin := map[string]bool{}
	for _, stmt := range stmts {
		body, _, _ := stripControlFlow(stmt)
		if definesBody(body) {
			continue
		}
		for i, t := range body {
			if t.kind != tokWord && t.kind != tokQuoted || i+1 >= len(body) || !body[i+1].punct("(") {
				continue
			}
			if t.kind == tokWord && reservedWords[strings.ToUpper(t.text)] || strings.Contains(t.text, ".") {
				continue
			}
			if i > 0 {
				prev := body[i-1]
				if prev.punct(".") || prev.punct("@") || prev.is("FROM") || prev.is("JOIN") || prev.is("FUNCTION") ||
					prev.is("PROCEDURE") || prev.is("CALL") {
					continue
				}
			}
			lower := strings.ToLower(t.text)
			if !names[lower] || temps[lower] {
				continue
			}
			isBuiltin, seen := builtin[lower]
			if !seen {
				isBuiltin = f.builtinFunction(r, t.text)
				builtin[lower] = isBuiltin
			}
			if !isBuiltin {
				calls = append(calls, call{t.pos, t.end})
			}
		}
	}
	if len(calls) == 0 {
		return sql, false
	}
	sort.Slice(calls, func(a, b int) bool { return calls[a].pos < calls[b].pos })
	var b strings.Builder
	last := 0
	for _, c := range calls {
		b.WriteString(sql[last:c.pos])
		name := sql[c.pos:c.end]
		if !strings.HasPrefix(name, "`") {
			name = quoteName(name)
		}
		b.WriteString(quoteName(dataset) + "." + name)
		last = c.end
	}
	b.WriteString(sql[last:])
	return b.String(), true
}

// definesBody reports whether a statement is a CREATE FUNCTION, CREATE
// PROCEDURE or CREATE VIEW, whose body names persistent functions with
// their datasets (above).
func definesBody(stmt []token) bool {
	if len(stmt) == 0 || !stmt[0].is("CREATE") {
		return false
	}
	for _, t := range stmt[1:] {
		switch {
		case t.is("FUNCTION") || t.is("PROCEDURE") || t.is("VIEW"):
			return true
		case t.is("OR") || t.is("REPLACE") || t.is("TEMP") || t.is("TEMPORARY") || t.is("AGGREGATE") || t.is("TABLE") ||
			t.is("MATERIALIZED"):
		default:
			return false
		}
	}
	return false
}

// builtinFunction reports whether the engine has a built-in function of
// the name, by calling SAFE.name() (above). One that cannot be told is
// taken to be built in, so that its call is left as it is.
func (f front) builtinFunction(r *http.Request, name string) bool {
	legacy := false
	if !plainName(name) {
		name = quoteName(name)
	}
	body, err := json.Marshal(queryOptions{Query: "SELECT SAFE." + name + "()", UseLegacySQL: &legacy})
	if err != nil {
		return true
	}
	status, got := f.send(r, http.MethodPost, "/queries", body)
	if status == http.StatusOK {
		return true
	}
	msg := errorMessage(got, status)
	return !strings.Contains(msg, "Function not found") && !strings.Contains(msg, "No matching signature for function :")
}

// plainName reports whether name is an identifier that needs no quotes.
func plainName(name string) bool {
	for i, c := range name {
		if c != '_' && !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return name != ""
}
