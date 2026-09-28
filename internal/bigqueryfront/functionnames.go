package bigqueryfront

import (
	"bytes"
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

// Functions of another dataset than the default one (#1107).
//
// In BigQuery each call of dataset.name calls that dataset's function,
// whatever the default dataset. The emulator's engine (goccy/googlesqlite
// v0.3.1) resolves such a call to the right function, but before each
// query it collects the SQL functions it may inline from the query's name
// path only (analyzer.go, Analyze: a.catalog.getFunctions(a.namePath)),
// which keeps a function when the name path's key, project_dataset, is a
// substring of the function's own (catalog.go, getFunctions); the emulator
// sets that path to the project and the default dataset
// (internal/connection/manager.go). So with the default dataset two, the
// function one.fn is not collected, and the engine sends its call to
// SQLite as the bare storage name, <project>_one_fn(...) (formatter.go),
// which SQLite reads as <project's first part> - ...: measured through
// the front with the official Go client, on the pinned image, SELECT
// one.fn(0) with the default dataset two failed 400 "sqlite3: SQL logic
// error: no such column: w1108", and so did SELECT two.fn(1), one.fn(2)
// with the default dataset one or two. With no default dataset the key is
// the project's, which every function of the project holds. How the call
// is written (quoted, with the project) does not change the storage name.
//
// So a query with a default dataset that calls a function the front knows
// (knownFunctions) of another dataset of its project is sent with no
// default dataset, after its table names and its calls of the default
// dataset's functions have been given the dataset (qualifyTables,
// qualifyFunctions); jobs.get and jobs.list show the client's default
// dataset (jobText.defaultDataset). That is done only for one statement
// that is a query or DML and names no INFORMATION_SCHEMA and no
// @@dataset_id or @@dataset_project_id, which the default dataset could
// still name; any other such query is 501, naming the function, before
// anything runs (#1123). A call by one quoted path (`p.ds.fn`) the engine
// does not find at all (#1122).

// otherDatasetCall returns the name (dataset.name) of the first call in
// sql of a function the front knows of a dataset of the project other
// than dataset, or "".
func (f front) otherDatasetCall(r *http.Request, sql, dataset string) string {
	toks, ok := lex(sql)
	if !ok {
		return ""
	}
	project := projectOf(f.base)
	known := map[string]map[string]bool{} // dataset -> lower-case names
	for i := 0; i+1 < len(toks); i++ {
		if !toks[i+1].punct("(") || toks[i].kind != tokWord && toks[i].kind != tokQuoted {
			continue
		}
		parts, from := callPath(toks, i)
		if from > 0 && (toks[from-1].punct("@") || toks[from-1].punct(".")) {
			continue
		}
		switch {
		case len(parts) == 3 && parts[0] == project:
			parts = parts[1:]
		case len(parts) != 2:
			continue
		}
		if parts[0] == dataset {
			continue
		}
		names, ok := known[parts[0]]
		if !ok {
			names = map[string]bool{}
			for _, p := range f.functionsIn(r, project, parts[0]) {
				names[strings.ToLower(p[len(p)-1])] = true
			}
			known[parts[0]] = names
		}
		if names[strings.ToLower(parts[1])] {
			return parts[0] + "." + parts[1]
		}
	}
	return ""
}

// callPath returns the dotted path whose last part is toks[i] (a quoted
// part may hold dots), and the index of its first token.
func callPath(toks []token, i int) ([]string, int) {
	split := func(t token) []string {
		if t.kind == tokQuoted {
			return strings.Split(t.text, ".")
		}
		return []string{t.text}
	}
	parts := split(toks[i])
	for i >= 2 && toks[i-1].punct(".") && (toks[i-2].kind == tokWord || toks[i-2].kind == tokQuoted) {
		parts = append(split(toks[i-2]), parts...)
		i -= 2
	}
	return parts, i
}

// needsDefaultDataset returns why sql cannot be sent with no default
// dataset (above), or "".
func needsDefaultDataset(sql string) string {
	toks, ok := lex(sql)
	if !ok {
		return "a query CloudBurrow cannot read"
	}
	if stmts := splitStatements(toks); len(stmts) != 1 {
		return "a script"
	}
	body, block, _ := stripControlFlow(toks)
	if block != "" || len(body) == 0 {
		return "a script"
	}
	switch strings.ToUpper(body[0].text) {
	case "SELECT", "WITH", "(", "INSERT", "UPDATE", "DELETE", "MERGE":
	default:
		return "a " + strings.ToUpper(body[0].text) + " statement"
	}
	for i, t := range toks {
		if t.is("INFORMATION_SCHEMA") || t.kind == tokQuoted && strings.Contains(strings.ToUpper(t.text), "INFORMATION_SCHEMA") {
			return "INFORMATION_SCHEMA"
		}
		if i >= 2 && toks[i-1].punct("@") && toks[i-2].punct("@") && (t.is("dataset_id") || t.is("dataset_project_id")) {
			return "@@" + strings.ToLower(t.text)
		}
	}
	return ""
}

// withoutDefaultDataset removes the default dataset from r's body:
// jobs.query's, or (insert) a query job's configuration.query.
func withoutDefaultDataset(r *http.Request, insert bool) bool {
	b, err := readBody(r)
	if err != nil {
		return false
	}
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if dec.Decode(&body) != nil {
		return false
	}
	target := body
	if insert {
		conf, _ := body["configuration"].(map[string]any)
		target, _ = conf["query"].(map[string]any)
		if target == nil {
			return false
		}
	}
	delete(target, "defaultDataset")
	out, err := json.Marshal(body)
	if err != nil {
		return false
	}
	setBody(r, out)
	return true
}
