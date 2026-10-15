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
// EXISTS and OPTIONS(...) are kept. A column's own OPTIONS in the list,
// and a column list inside a multi-statement script, stay 501 (checkDDL).

// viewColumnList is a lone CREATE VIEW statement with a column list.
type viewColumnList struct {
	// head is the statement's text up to the column list (CREATE [OR
	// REPLACE] VIEW [IF NOT EXISTS] path), options what follows the list
	// before AS, and query the AS query.
	head, options, query string
	path                 []string
	columns              []string
	ifNotExists          bool
	// columnOptions is whether a listed column has OPTIONS(...).
	columnOptions bool
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
		i++
		if i < end && t[i].is("OPTIONS") {
			v.columnOptions = true
			depth := 0
			for i++; i < end; i++ {
				if t[i].punct("(") {
					depth++
				} else if t[i].punct(")") {
					if depth--; depth == 0 {
						i++
						break
					}
				}
			}
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
	seen := map[string]bool{}
	for _, c := range v.columns {
		if msg := checkColumnName(c); msg != "" {
			return "", msg
		}
		key := strings.ToLower(c)
		if seen[key] {
			return "", fmt.Sprintf("Duplicate column name %s in the column list of CREATE VIEW", c)
		}
		seen[key] = true
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
	if v.columnOptions {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: OPTIONS on a column in the "+
			"column list of CREATE VIEW. CloudBurrow names a view's columns by its column list, but does not set a "+
			"column's options from it, so nothing was run. Set a view column's description with tables.patch "+
			"(Table.Update in the Go client).")
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
	if _, failed := queryFailure(rec, job); !failed && ok && !existed && f.views != nil {
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
