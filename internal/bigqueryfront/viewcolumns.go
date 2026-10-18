package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// A view's column list (#1240): CREATE VIEW v (c, d) AS SELECT ...
//
// BigQuery names a view's columns by the list, in order, whatever the
// query calls them ("view_column_name_list: Lets you explicitly specify
// the column names of the view, which may be aliases to the column names
// in the underlying SQL query",
// https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#create_view_statement),
// keeps the query as written in view.query and says so with
// view.useExplicitColumnNames ("True if the column names are explicitly
// specified. For example by using the 'CREATE VIEW v(c1, c2) AS ...'
// syntax", ViewDefinition). The emulator refuses the statement (measured,
// #918: 400 "CREATE VIEW with explicit column list is not supported").
//
// So a query that is one such statement is sent on as the CREATE VIEW it
// amounts to, the query's columns aliased in order. The query is run alone
// first (lone, a LIMIT 0 of it) for its columns. When their names are
// distinct and none is one the engine made up for an unnamed column
// ($col1 ...), the view's query is SELECT `q`.`a` AS `c`, ... FROM (query)
// AS `q`; otherwise, since such a column cannot be named from outside the
// query, it is an empty row of the listed names and the query's types,
// UNION ALL the query (measured on the pinned image: SELECT * FROM (SELECT
// CAST(NULL AS INT64) AS c, CAST(NULL AS STRING) AS d FROM (SELECT 1)
// WHERE FALSE UNION ALL (SELECT 1, "x")) read back the row 1, "x" as c and
// d). A list whose length is not the query's is 400 invalidQuery, as
// BigQuery refuses it, and so is a listed name BigQuery refuses for a
// column, or one listed twice. After the view is made, tables.get answers
// the client's query as view.query and useExplicitColumnNames true
// (viewTexts), and the job keeps the client's text. OR REPLACE, IF NOT
// EXISTS and OPTIONS(...) are kept.
//
// A column's OPTIONS in the list (#1323): the one option BigQuery's CREATE
// VIEW reference lists for a view's column is description (same page,
// view_column_name_list). The emulator keeps no column description from DDL, so
// once the view is made its schema is read, each listed column given its
// description literal, and written back with the emulator's tables.update,
// which keeps a view's field descriptions (measured). Another option, or a
// description that is not a string literal, is 501, nothing run.
//
// A column list inside a multi-statement script (#1323) is carried out by
// the front's run of the script (scriptflow.go, hasControlFlow): each
// statement is a request of its own, so such a CREATE VIEW is one alone,
// carried out as above. Its listed names are checked, with the script's
// other statements, before anything runs (checkStatements). A script with
// a TEMP table or function cannot be split, as a TEMP table lasts only
// for the request that made it: its column lists are put in its text
// (#1335, viewcolumnstemp.go), as is the dry run of a lone one.

// viewColumnList is a lone CREATE VIEW statement with a column list.
type viewColumnList struct {
	// head is the statement's text up to the column list (CREATE [OR
	// REPLACE] VIEW [IF NOT EXISTS] path), options what follows the list
	// before AS, and query the AS query.
	head, options, query string
	path                 []string
	columns              []string
	ifNotExists          bool
	orReplace            bool
	// descriptions are the listed columns' OPTIONS(description = ...), by
	// position (nil for none), and descExprs the text of one that is not
	// a string literal, a constant expression evaluated before the view
	// is made (resolveDescriptions, #1416); badOption names OPTIONS the
	// front cannot read, and refused why BigQuery refuses an option.
	descriptions []*string
	descExprs    []string
	badOption    string
	refused      string
}

// isViewColumnList reports whether body, one statement of a script, is a
// CREATE VIEW with a column list.
func isViewColumnList(sql string, body []token) bool {
	if len(body) < 8 || !body[0].is("CREATE") {
		return false
	}
	_, ok := parseViewColumnList(sql[body[0].pos:body[len(body)-1].end])
	return ok
}

// checkNames is why BigQuery refuses the listed names before running the
// statement, or "".
func (v viewColumnList) checkNames() string {
	if v.refused != "" {
		return v.refused
	}
	for _, e := range v.descExprs {
		if msg := constantOptionRefused(e); msg != "" {
			return msg
		}
	}
	seen := map[string]bool{}
	for _, c := range v.columns {
		if msg := checkColumnName(c); msg != "" {
			return msg
		}
		key := strings.ToLower(c)
		if seen[key] {
			return fmt.Sprintf("Duplicate column name %s in the column list of CREATE VIEW", c)
		}
		seen[key] = true
	}
	return ""
}

// withoutList is the statement without its column list, for the checks
// of the rest of it.
func (v viewColumnList) withoutList() string {
	return v.head + v.options + " AS " + v.query
}

// parseViewColumnList reads a query that is one CREATE VIEW statement with
// a column list, and reports false for any other query.
func parseViewColumnList(sql string) (viewColumnList, bool) {
	var v viewColumnList
	t, ok := lex(sql)
	if !ok || len(t) < 8 || !t[0].is("CREATE") {
		return v, false
	}
	end := len(t)
	if t[end-1].punct(";") {
		end--
	}
	for _, tok := range t[:end] {
		if tok.punct(";") {
			return v, false
		}
	}
	i := 1
	if i+1 < end && t[i].is("OR") && t[i+1].is("REPLACE") {
		v.orReplace = true
		i += 2
	}
	if i >= end || !t[i].is("VIEW") {
		return v, false
	}
	i++
	if i+2 < end && t[i].is("IF") && t[i+1].is("NOT") && t[i+2].is("EXISTS") {
		v.ifNotExists = true
		i += 3
	}
	start := i
	i, ok = pathEnd(t, i, end)
	if !ok || i >= end || !t[i].punct("(") {
		return v, false
	}
	for j := start; j < i; j++ {
		if t[j].kind != tokPunct {
			v.path = append(v.path, t[j].text)
		}
	}
	if len(v.path) == 1 {
		// A path written as one quoted name: `p.ds.v`.
		v.path = strings.Split(v.path[0], ".")
	}
	v.head = sql[:t[i].pos]
	i++
	for {
		if i >= end || t[i].kind != tokWord && t[i].kind != tokQuoted {
			return v, false
		}
		v.columns = append(v.columns, t[i].text)
		v.descriptions = append(v.descriptions, nil)
		v.descExprs = append(v.descExprs, "")
		i++
		if i < end && t[i].is("OPTIONS") {
			close := -1
			if i+1 < end && t[i+1].punct("(") {
				close = closing(t[:end], i+1)
			}
			if close < 0 {
				return v, false
			}
			opts, ok := readOptions(t[i : close+1])
			if !ok && v.badOption == "" {
				v.badOption = "OPTIONS as written"
			}
			for _, o := range opts {
				s, lit := stringLiteral(singleToken(o.val))
				switch {
				case o.name != "description":
					// "Column options for a view have the same syntax and
					// requirements as for a table, but with a different
					// list of NAME and VALUE fields": description alone
					// (#1416; the wording is CloudBurrow's, UNVERIFIED).
					// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language
					if v.refused == "" {
						v.refused = "The column option " + o.name + " is not supported for a column of a view: " +
							"description is the one column option of a view."
					}
				case lit:
					v.descriptions[len(v.descriptions)-1] = &s
				case len(o.val) == 1 && o.val[0].is("NULL"):
				case len(o.val) == 0:
					if v.badOption == "" {
						v.badOption = "OPTIONS as written"
					}
				default:
					v.descExprs[len(v.descExprs)-1] = sql[o.val[0].pos:o.val[len(o.val)-1].end]
				}
			}
			i = close + 1
		}
		if i < end && t[i].punct(",") {
			i++
			continue
		}
		if i < end && t[i].punct(")") {
			i++
			break
		}
		return v, false
	}
	// OPTIONS(...) of the view, then AS and the query.
	optStart, depth := i, 0
	for ; i < end; i++ {
		switch {
		case t[i].punct("("):
			depth++
		case t[i].punct(")"):
			depth--
		case depth == 0 && t[i].is("AS"):
			if i > optStart {
				v.options = " " + sql[t[optStart].pos:t[i-1].end]
			}
			if i+1 >= end {
				return v, false
			}
			v.query = sql[t[i+1].pos:t[end-1].end]
			return v, true
		}
	}
	return v, false
}

// aliasedQuery returns the view's query with the columns of the client's,
// fields, named by the list (above), or why the list is refused.
func (v viewColumnList) aliasedQuery(fields []field) (string, string) {
	if len(v.columns) != len(fields) {
		return "", fmt.Sprintf("The number of columns in the column list (%d) does not match the number of columns in "+
			"the query (%d)", len(v.columns), len(fields))
	}
	if msg := v.checkNames(); msg != "" {
		return "", msg
	}
	named := true
	names := map[string]bool{}
	for _, fl := range fields {
		key := strings.ToLower(fl.Name)
		if fl.Name == "" || anonymousColumn.MatchString(fl.Name) || names[key] {
			named = false
		}
		names[key] = true
	}
	cols := make([]string, len(fields))
	if named {
		for i, fl := range fields {
			cols[i] = "`_cloudburrow_q`." + quoteName(fl.Name) + " AS " + quoteName(v.columns[i])
		}
		return "SELECT " + strings.Join(cols, ", ") + " FROM (\n" + v.query + "\n) AS `_cloudburrow_q`", ""
	}
	for i, fl := range fields {
		cols[i] = "CAST(NULL AS " + sqlType(fl) + ") AS " + quoteName(v.columns[i])
	}
	return "SELECT * FROM (SELECT " + strings.Join(cols, ", ") + " FROM (SELECT 1) WHERE FALSE UNION ALL (\n" +
		v.query + "\n))", ""
}

// badOptionMessage is the 501 for a column's OPTIONS the front cannot read.
func (v viewColumnList) badOptionMessage() string {
	return "Not implemented here: " + v.badOption + " on a column in the column list of CREATE VIEW. CloudBurrow " +
		"sets a listed column's OPTIONS(description = ...), the one column option BigQuery takes for a view, " +
		"and nothing else, so nothing was run."
}

// constantFunctions are the scalar functions an option's constant
// expression "cannot contain" (data-definition-language, column_set_options_list).
var constantFunctions = map[string]bool{"ARRAY_TO_STRING": true, "REPLACE": true, "REGEXP_REPLACE": true, "RAND": true,
	"FORMAT": true, "LPAD": true, "RPAD": true, "REPEAT": true, "SESSION_USER": true, "GENERATE_ARRAY": true,
	"GENERATE_DATE_ARRAY": true}

// constantOptionRefused is why BigQuery refuses expr as an option's value,
// "a constant expression containing only literals, query parameters, and
// scalar functions" with no table, subquery or statement and none of
// constantFunctions, or "" (#1416). The engine refuses the rest (a column
// or a table it cannot resolve) when it evaluates it. The wording is
// CloudBurrow's, UNVERIFIED.
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language
func constantOptionRefused(expr string) string {
	t, ok := lex(expr)
	if !ok {
		return ""
	}
	for i, tok := range t {
		switch {
		case tok.kind != tokWord:
		case tok.is("SELECT") || tok.is("FROM") || tok.is("WITH"):
			return "An option's value must be a constant expression, without a subquery or a table: " + expr
		case constantFunctions[strings.ToUpper(tok.text)] && i+1 < len(t) && t[i+1].punct("("):
			return "An option's value must be a constant expression, which cannot call " + strings.ToUpper(tok.text) + ": " + expr
		}
	}
	return ""
}

// resolveDescriptions evaluates v's descriptions that are constant
// expressions (descExprs) with q's query parameters, each the STRING it
// is (NULL for none), and returns the status and message of a refusal, or
// 0 (#1416).
func (f front) resolveDescriptions(r *http.Request, q queryOptions, v *viewColumnList) (int, string) {
	for i, e := range v.descExprs {
		if e == "" {
			continue
		}
		if msg := constantOptionRefused(e); msg != "" {
			return http.StatusBadRequest, msg
		}
		legacy := false
		opts := queryOptions{Query: "SELECT CAST((" + e + ") AS STRING)", UseLegacySQL: &legacy, DefaultDataset: q.DefaultDataset}
		if strings.ContainsAny(e, "@?") {
			// The engine refuses parameters a query does not use
			// (measured: "expected 0 arguments, got 1").
			opts.ParameterMode, opts.QueryParameters = q.ParameterMode, q.QueryParameters
		}
		req, err := json.Marshal(opts)
		if err != nil {
			return http.StatusInternalServerError, err.Error()
		}
		status, got := f.send(r, http.MethodPost, "/queries", req)
		var res struct {
			Rows []struct {
				F []struct {
					V *string `json:"v"`
				} `json:"f"`
			} `json:"rows"`
		}
		if status != http.StatusOK || json.Unmarshal(got, &res) != nil || len(res.Rows) != 1 || len(res.Rows[0].F) != 1 {
			return http.StatusBadRequest, "Could not evaluate the description of column " + v.columns[i] + ": " +
				errorMessage(got, status)
		}
		v.descriptions[i] = res.Rows[0].F[0].V
	}
	return 0, ""
}

// createViewWithColumns carries out v, the lone statement of q (above).
func (f front) createViewWithColumns(w http.ResponseWriter, r *http.Request, q queryOptions, v viewColumnList, insert bool) {
	if v.badOption != "" {
		writeError(w, http.StatusNotImplemented, "notImplemented", v.badOptionMessage())
		return
	}
	if code, msg := f.resolveDescriptions(r, q, &v); code != 0 { // #1416
		writeError(w, code, "invalidQuery", msg)
		return
	}
	if dryRun(r, insert) { // #1335
		f.dryRunViewWithColumns(w, r, q, v, insert)
		return
	}
	ds, table, ok := tableOf(q, v.path)
	existed := false
	if ok && v.ifNotExists {
		status, _ := f.get(r, tablePath(ds, table))
		existed = status == http.StatusOK
	}
	fields, status, body := f.lone(r, q, v.query)
	if status != http.StatusOK {
		msg := errorMessage(body, status)
		writeError(w, http.StatusBadRequest, "invalidQuery", msg)
		return
	}
	sel, msg := v.aliasedQuery(fields)
	if msg != "" {
		writeError(w, http.StatusBadRequest, "invalidQuery", msg)
		return
	}
	sql := v.head + v.options + " AS " + sel
	if !setQueryText(r, insert, sql) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return
	}
	client := jobText{query: q.Query}
	q.Query = sql
	rec := newRecorder()
	f.runQuery(rec, r, q, insert)
	var job map[string]any
	if insert && rec.status == http.StatusOK {
		_ = json.Unmarshal(rec.body.Bytes(), &job)
	}
	_, failed := queryFailure(rec, job)
	if !failed && ok && !existed {
		if msg := f.madeViewWithColumns(r, ds, table, v); msg != "" {
			writeError(w, http.StatusInternalServerError, "internalError", msg)
			return
		}
	}
	f.clientText(w, rec, client)
}

// madeViewWithColumns finishes the view dataset.table that v made: its
// columns' descriptions set, and its query as written kept (viewTexts). It
// returns why it could not, or "".
func (f front) madeViewWithColumns(r *http.Request, ds, table string, v viewColumnList) string {
	if v.hasDescriptions() {
		if msg := f.setViewDescriptions(r, ds, table, v.descriptions); msg != "" {
			return "cloudburrow: the view was made, but its columns' descriptions could not be set: " + msg
		}
	}
	if f.views != nil {
		status, got := f.get(r, tablePath(ds, table))
		var meta struct {
			View *struct {
				Query string `json:"query"`
			} `json:"view"`
		}
		if status == http.StatusOK && json.Unmarshal(got, &meta) == nil && meta.View != nil && meta.View.Query != "" {
			f.views.add(viewKey(projectOf(f.base), ds, table),
				viewText{engine: meta.View.Query, client: strings.TrimSpace(v.query), explicit: true})
		}
	}
	return ""
}

func (v viewColumnList) hasDescriptions() bool {
	for _, d := range v.descriptions {
		if d != nil {
			return true
		}
	}
	return false
}

// setViewDescriptions gives the view dataset.table's columns their
// descriptions, by position, with the emulator's tables.update of the view
// as it is, and returns why it could not, or "".
func (f front) setViewDescriptions(r *http.Request, dataset, table string, descriptions []*string) string {
	status, got := f.get(r, tablePath(dataset, table))
	var meta map[string]json.RawMessage
	if status != http.StatusOK || json.Unmarshal(got, &meta) != nil {
		return errorMessage(got, status)
	}
	var schema struct {
		Fields []map[string]any `json:"fields"`
	}
	if json.Unmarshal(meta["schema"], &schema) != nil || len(schema.Fields) != len(descriptions) {
		return "the view's schema does not have the listed columns"
	}
	for i, d := range descriptions {
		if d != nil {
			schema.Fields[i]["description"] = *d
		}
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return err.Error()
	}
	meta["schema"] = b
	body, err := json.Marshal(meta)
	if err != nil {
		return err.Error()
	}
	if status, got := f.send(r, http.MethodPut, tablePath(dataset, table), body); status != http.StatusOK {
		return errorMessage(got, status)
	}
	return ""
}
