package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Tables of one ID in two datasets (#1015).
//
// In BigQuery a table is named by its project, dataset and table ID
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/tabledata/list),
// and a query's table name without a dataset is in its default dataset
// (QueryRequest.defaultDataset), or refused when it has none.
//
// The emulator's engine (goccy/googlesqlite v0.3.1, in the pinned image)
// does not read a name so. Its catalog registers each table under every
// suffix of its path (`project.dataset.table`, `dataset.table` and the bare
// `table`, catalog.go's addTableSpecRecursiveImpl), and only when no table
// holds that name yet (existsTable): so the bare ID of a table names the
// first table of that ID made in any dataset, whatever dataset the query
// runs in, and the engine's analyser, which resolves names in that
// catalog, never reads the default dataset for a table. The emulator then
// reads tables by the bare ID in three places: tabledata.list sends
// "SELECT * FROM `<tableId>`" (server/handler.go, tabledataListHandler),
// and so do an extract job (exportToGCS) and the Storage Read API.
//
// Measured against the pinned image through the front, with b1.same (s
// STRING) made first and holding 'from b1', then b2.same (a INT64) holding
// 2: tabledata.list of b2.same answered b1's row; with b2 the default
// dataset, `SELECT * FROM same` read b1's row, `INSERT INTO same (a)`
// failed "Column a is not present in table w1015-local_b1_same",
// `DELETE FROM same WHERE true` deleted b1's rows and `TRUNCATE TABLE same`
// emptied b1.same; with no default dataset, `SELECT * FROM same` read b1
// where BigQuery refuses the name. `SELECT * FROM b2.same` read b2's, and
// `CREATE TABLE same2` with b2 the default dataset made b2.same2.
//
// So the front sends every table name a query gives without a dataset
// with its default dataset (qualifyTables), refuses
// one in a query with no default dataset as BigQuery does, and answers
// tabledata.list itself, from a query of the whole name when another
// dataset has a table of the ID (listTableData, sharedID). Measured: with
// every tabledata.list sent as such a query, the emulator's engine crashed
// (SIGSEGV in its WebAssembly GoogleSQL) in three runs of three of the
// BigQuery compat suite, where it did in none with its own read; so its
// own read is kept where it reads the right table.

// nameRef is a table name a statement gives without a dataset, at byte
// offsets pos to end in the query.
type nameRef struct {
	pos, end int
	name     string
	// first is whether it is in the query's first statement.
	first bool
}

// unqualifiedTables returns the table names the statements in sql give
// with no dataset, where a query reads or writes a table: after FROM and
// JOIN, after a comma in a FROM clause, MERGE's USING, the tables of
// INSERT, UPDATE, DELETE, MERGE and TRUNCATE TABLE, and those CREATE
// TABLE, CREATE VIEW, DROP TABLE and DROP VIEW name. A CTE's name in its
// statement and a TEMP table a statement of the script makes are not
// tables of a dataset, and are left out. ok is false for text lex cannot
// read, which the engine refuses itself.
func unqualifiedTables(sql string) (refs []nameRef, ok bool) {
	toks, ok := lex(sql)
	if !ok {
		return nil, false
	}
	stmts := splitStatements(toks)
	temps := map[string]bool{}
	for _, stmt := range stmts {
		body, _, _ := stripControlFlow(stmt)
		if name, ok := createsTemp(body); ok {
			temps[strings.ToLower(name)] = true
		}
	}
	n := 0
	for _, stmt := range stmts {
		if len(stmt) == 0 {
			continue
		}
		body, _, _ := stripControlFlow(stmt)
		for _, ref := range statementTables(body, temps) {
			ref.first = n == 0
			refs = append(refs, ref)
		}
		n++
	}
	return refs, true
}

// statementTables is unqualifiedTables for one statement.
func statementTables(stmt []token, temps map[string]bool) []nameRef {
	if len(stmt) == 0 {
		return nil
	}
	ctes := map[string]bool{}
	for i := 0; i+2 < len(stmt); i++ {
		if (stmt[i].kind == tokWord || stmt[i].kind == tokQuoted) && stmt[i+1].is("AS") && stmt[i+2].punct("(") &&
			(i == 0 || !stmt[i-1].is("CAST") && !stmt[i-1].is("SAFE_CAST") && !stmt[i-1].punct("(")) {
			ctes[strings.ToLower(stmt[i].text)] = true
		}
	}
	var refs []nameRef
	seen := map[int]bool{}
	// add notes the name at j; source is whether it is read in a FROM
	// clause, where a "(" after it makes it a table-valued function's.
	add := func(j int, source bool) {
		if j >= len(stmt) || seen[j] || stmt[j].kind != tokWord && stmt[j].kind != tokQuoted {
			return
		}
		if stmt[j].kind == tokWord && reservedWords[strings.ToUpper(stmt[j].text)] {
			return // UNNEST, SELECT, ...
		}
		parts, next := path(stmt, j)
		if len(parts) != 1 || next != j+1 || source && next < len(stmt) && stmt[next].punct("(") {
			return // qualified, or a table-valued function
		}
		lower := strings.ToLower(parts[0])
		if source && ctes[lower] || temps[lower] {
			return
		}
		seen[j] = true
		refs = append(refs, nameRef{pos: stmt[j].pos, end: stmt[j].end, name: parts[0]})
	}

	// The tables DDL and DML name at the head of the statement.
	switch {
	case stmt[0].is("INSERT"):
		j := 1
		if j < len(stmt) && stmt[j].is("INTO") {
			j++
		}
		add(j, false)
	case stmt[0].is("UPDATE"):
		add(1, false)
	case stmt[0].is("DELETE"):
		j := 1
		if j < len(stmt) && stmt[j].is("FROM") {
			j++
		}
		add(j, false)
	case stmt[0].is("MERGE"):
		j := 1
		if j < len(stmt) && stmt[j].is("INTO") {
			j++
		}
		add(j, false)
	case stmt[0].is("TRUNCATE") && len(stmt) > 1 && stmt[1].is("TABLE"):
		add(2, false)
	case stmt[0].is("CREATE") || stmt[0].is("DROP"):
		if _, temp := createsTemp(stmt); temp {
			break
		}
		j := 1
		if stmt[0].is("CREATE") && j+1 < len(stmt) && stmt[j].is("OR") && stmt[j+1].is("REPLACE") {
			j += 2
		}
		for j < len(stmt) && (stmt[j].is("EXTERNAL") || stmt[j].is("SNAPSHOT")) {
			j++
		}
		if j >= len(stmt) || !stmt[j].is("TABLE") && !stmt[j].is("VIEW") || j+1 < len(stmt) && stmt[j+1].is("FUNCTION") {
			break
		}
		j++
		switch {
		case j+2 < len(stmt) && stmt[j].is("IF") && stmt[j+1].is("NOT") && stmt[j+2].is("EXISTS"):
			j += 3
		case j+1 < len(stmt) && stmt[j].is("IF") && stmt[j+1].is("EXISTS"):
			j += 2
		}
		add(j, false)
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
	fromAt := map[int]bool{} // paren depth → inside a FROM clause
	depth := 0
	for i := 0; i < len(stmt); i++ {
		t := stmt[i]
		switch {
		case t.punct("(") || t.punct("["):
			depth++
		case t.punct(")") || t.punct("]"):
			delete(fromAt, depth)
			depth--
		case t.is("FROM") && extract[i]:
		case t.is("FROM") && i >= 2 && stmt[i-1].is("DISTINCT") && (stmt[i-2].is("IS") || stmt[i-2].is("NOT")):
			// a IS [NOT] DISTINCT FROM b.
		case t.is("FROM") || t.is("JOIN"):
			fromAt[depth] = true
			add(i+1, true)
		case t.punct(",") && fromAt[depth]:
			add(i+1, true)
		case t.is("USING") && stmt[0].is("MERGE") && depth == 0:
			add(i+1, true)
			delete(fromAt, depth)
		case t.is("WHERE") || t.is("GROUP") || t.is("HAVING") || t.is("QUALIFY") || t.is("WINDOW") || t.is("ORDER") ||
			t.is("LIMIT") || t.is("UNION") || t.is("INTERSECT") || t.is("EXCEPT") || t.is("ON") || t.is("USING") ||
			t.is("SELECT") || t.is("SET") || t.is("WHEN"):
			delete(fromAt, depth)
		}
	}
	return refs
}

// defaultDatasetOf returns a query's default dataset, or "".
func defaultDatasetOf(q queryOptions) string {
	var def struct {
		DatasetID string `json:"datasetId"`
	}
	if json.Unmarshal(q.DefaultDataset, &def) != nil {
		return ""
	}
	return def.DatasetID
}

// noDefaultDataset is the dataset a table name is sent in when a later
// statement of a script gives it without a dataset and the query has no
// default dataset (qualifyTables): no such dataset exists, so the
// statement fails when it runs, as BigQuery fails it then.
const noDefaultDataset = "_cloudburrow_no_default_dataset"

// qualifyTables returns sql with each table name it gives without a
// dataset (unqualifiedTables) written as dataset.name, and whether it
// changed any. The dataset's project is left out, as the front's own
// queries leave it: the emulator serves one project, and resolves
// dataset.name to that dataset's table.
//
// With no default dataset (dataset ""), BigQuery refuses such a name when
// the statement runs: "Table ... must be qualified with a dataset". One in
// the query's first statement is refused before anything runs, and msg is
// the message; one in a later statement of a script, after statements
// BigQuery runs first, is sent in noDefaultDataset, so that the emulator
// runs the statements before it and fails there.
func qualifyTables(sql, dataset string) (text string, changed bool, msg string) {
	refs, ok := unqualifiedTables(sql)
	if !ok || len(refs) == 0 {
		return sql, false, ""
	}
	sort.Slice(refs, func(a, b int) bool { return refs[a].pos < refs[b].pos })
	if dataset == "" {
		for _, ref := range refs {
			if ref.first {
				return sql, false, fmt.Sprintf("Table %q must be qualified with a dataset (e.g. dataset.table).", ref.name)
			}
		}
		dataset = noDefaultDataset
	}
	var b strings.Builder
	last := 0
	for _, ref := range refs {
		if ref.pos < last {
			continue
		}
		b.WriteString(sql[last:ref.pos])
		b.WriteString(quotePath([]string{dataset, ref.name}))
		last = ref.end
	}
	b.WriteString(sql[last:])
	return b.String(), true, ""
}

// listTableData answers tabledata.list of dataset.table itself (#1015,
// above): when another dataset has a table of the ID (sharedID), from a
// query of the table's whole name, as the emulator's own read would be of
// the first table of the ID made in any dataset; else from the emulator's
// own read, which is then of this table. The rows are as the
// emulator's tabledata.list writes them (the same formatter), and are
// paged as BigQuery pages them: maxResults rows from startIndex, or from
// a pageToken the front gave, which is the index of the next row.
// selectedFields is 501: the emulator ignores it (measured: every column
// came back), and the front does not select columns itself.
func (f front) listTableData(w http.ResponseWriter, r *http.Request, dataset, table string) {
	status, got := f.get(r, tablePath(dataset, table))
	if status != http.StatusOK {
		// Not found, or not readable: the emulator's own answer stands.
		f.next.ServeHTTP(w, r)
		return
	}
	params := r.URL.Query()
	if params.Get("selectedFields") != "" {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: tabledata.list with "+
			"selectedFields. BigQuery returns only the fields named, but the emulator behind CloudBurrow returns every "+
			"column (measured), and CloudBurrow does not select them itself. Nothing was read. List the table without "+
			"selectedFields, or query the columns (SELECT a, b FROM dataset.table).")
		return
	}
	start, err := indexParam(params, "startIndex")
	if err == nil && params.Get("pageToken") != "" {
		start, err = indexParam(params, "pageToken")
		if err != nil {
			err = fmt.Errorf("Invalid page token %q", params.Get("pageToken"))
		}
	}
	max, errMax := indexParam(params, "maxResults")
	if err == nil {
		err = errMax
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	var int64Timestamp *bool
	if v := params.Get("formatOptions.useInt64Timestamp"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			int64Timestamp = &b
		}
	}
	var rows []json.RawMessage
	if f.sharedID(r, dataset, table) {
		rows, status, got = f.tableData(r, dataset, table, int64Timestamp)
	} else {
		// No other dataset has a table of the ID: the emulator's own read
		// is of this table. Its rows are paged here, as it pages none.
		rows, status, got = f.emulatorTableData(r, dataset, table)
	}
	if status != http.StatusOK {
		writeRaw(w, status, got)
		return
	}
	res := struct{ Rows []json.RawMessage }{rows}
	total := len(res.Rows)
	if start > total {
		start = total
	}
	end := total
	if params.Has("maxResults") && start+max < total {
		end = start + max
	}
	if res.Rows == nil {
		res.Rows = []json.RawMessage{}
	}
	out := map[string]any{
		"kind":      "bigquery#tableDataList",
		"totalRows": strconv.Itoa(total),
		"rows":      res.Rows[start:end],
	}
	if end < total {
		out["pageToken"] = strconv.Itoa(end)
	}
	writeJSON(w, http.StatusOK, out)
}

// indexParam reads a non-negative integer query parameter; 0 when absent.
func indexParam(params url.Values, name string) (int, error) {
	v := params.Get(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("Invalid value for %s: %q is not a non-negative integer", name, v)
	}
	return n, nil
}

// tableData reads the rows of dataset.table in f's project, each as
// tabledata.list writes it ({"f":[{"v":...}]}), from a query of the
// table's whole name (listTableData); on failure it returns the
// emulator's status and body. int64Timestamp is the request's
// formatOptions.useInt64Timestamp, or nil.
func (f front) tableData(r *http.Request, dataset, table string, int64Timestamp *bool) ([]json.RawMessage, int, []byte) {
	legacy := false
	q := map[string]any{
		"query":        "SELECT * FROM " + quotePath([]string{dataset, table}),
		"useLegacySql": &legacy,
	}
	if int64Timestamp != nil {
		q["formatOptions"] = map[string]bool{"useInt64Timestamp": *int64Timestamp}
	}
	body, err := json.Marshal(q)
	if err != nil {
		return nil, http.StatusInternalServerError, nil
	}
	status, got := f.send(r, http.MethodPost, "/queries", body)
	var res struct {
		Rows []json.RawMessage `json:"rows"`
	}
	if status != http.StatusOK || json.Unmarshal(got, &res) != nil {
		if status == http.StatusOK {
			status = http.StatusBadGateway
		}
		return nil, status, got
	}
	return res.Rows, http.StatusOK, nil
}

// sharedID reports whether a dataset other than dataset has a table (or
// view) of the ID table, or whether that cannot be told: then the
// emulator's reads by the bare ID may be of another table (#1015). It
// reads only the emulator's metadata: datasets.list, and tables.get of
// the ID in each other dataset.
func (f front) sharedID(r *http.Request, dataset, table string) bool {
	token := ""
	for {
		p := "/datasets?all=true"
		if token != "" {
			p += "&pageToken=" + url.QueryEscape(token)
		}
		status, got := f.get(r, p)
		var list struct {
			NextPageToken string `json:"nextPageToken"`
			Datasets      []struct {
				DatasetReference struct {
					DatasetID string `json:"datasetId"`
				} `json:"datasetReference"`
			} `json:"datasets"`
		}
		if status != http.StatusOK || json.Unmarshal(got, &list) != nil {
			return true
		}
		for _, d := range list.Datasets {
			id := d.DatasetReference.DatasetID
			if id == dataset || id == "" {
				continue
			}
			if st, _ := f.get(r, tablePath(id, table)); st != http.StatusNotFound {
				return true
			}
		}
		if list.NextPageToken == "" || list.NextPageToken == token {
			return false
		}
		token = list.NextPageToken
	}
}

// emulatorTableData reads dataset.table's rows through the emulator's own
// tabledata.list, which gives every row in one answer (measured: it reads
// no maxResults or pageToken), following a page token should it give one.
func (f front) emulatorTableData(r *http.Request, dataset, table string) ([]json.RawMessage, int, []byte) {
	var rows []json.RawMessage
	path := tablePath(dataset, table) + "/data"
	if q := r.URL.Query().Get("formatOptions.useInt64Timestamp"); q != "" {
		path += "?formatOptions.useInt64Timestamp=" + url.QueryEscape(q)
	}
	token := ""
	for {
		p := path
		if token != "" {
			sep := "?"
			if strings.Contains(p, "?") {
				sep = "&"
			}
			p += sep + "pageToken=" + url.QueryEscape(token)
		}
		status, got := f.get(r, p)
		var page struct {
			PageToken string            `json:"pageToken"`
			Rows      []json.RawMessage `json:"rows"`
		}
		if status != http.StatusOK || json.Unmarshal(got, &page) != nil {
			if status == http.StatusOK {
				status = http.StatusBadGateway
			}
			return nil, status, got
		}
		rows = append(rows, page.Rows...)
		if page.PageToken == "" || page.PageToken == token {
			return rows, http.StatusOK, nil
		}
		token = page.PageToken
	}
}
