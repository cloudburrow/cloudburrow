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
// for the request that made it: its column list stays 501 (checkDDL).

// viewColumnList is a lone CREATE VIEW statement with a column list.
type viewColumnList struct {
	// head is the statement's text up to the column list (CREATE [OR
	// REPLACE] VIEW [IF NOT EXISTS] path), options what follows the list
	// before AS, and query the AS query.
	head, options, query string
	path                 []string
	columns              []string
	ifNotExists          bool
	// descriptions are the listed columns' OPTIONS(description = ...), by
	// position (nil for none); badOption names a column option the front
	// does not set.
	descriptions []*string
	badOption    string
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
				case o.name != "description" && v.badOption == "":
					v.badOption = "the option " + o.name
				case o.name == "description" && !lit && v.badOption == "":
					v.badOption = "a description that is not a string literal"
				case o.name == "description" && lit:
					v.descriptions[len(v.descriptions)-1] = &s
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

// createViewWithColumns carries out v, the lone statement of q (above).
func (f front) createViewWithColumns(w http.ResponseWriter, r *http.Request, q queryOptions, v viewColumnList, insert bool) {
	if v.badOption != "" {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: "+v.badOption+" on a column "+
			"in the column list of CREATE VIEW. CloudBurrow sets a listed column's OPTIONS(description = 'literal'), "+
			"the one column option BigQuery takes for a view, and nothing else, so nothing was run.")
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
	if !failed && ok && !existed && v.hasDescriptions() {
		if msg := f.setViewDescriptions(r, ds, table, v.descriptions); msg != "" {
			writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: the view was made, but its "+
				"columns' descriptions could not be set: "+msg)
			return
		}
	}
	if !failed && ok && !existed && f.views != nil {
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
	f.clientText(w, rec, client)
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
