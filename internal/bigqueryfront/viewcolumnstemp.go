package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// A view's column list in a script with a TEMP table or function, and in
// a dry run (#1335).
//
// A TEMP table or function lasts only for the request that made it, so
// such a script is sent to the emulator as one text, not run a statement
// at a time (#1323's path). Each CREATE VIEW with a column list in it is
// put in that text as the CREATE VIEW it amounts to (viewcolumns.go): its
// query aliased by the list when the query can be run alone for its
// columns, and otherwise, as when it reads a table an earlier statement of
// the script makes, an empty row of the listed names whose values are
// untyped NULLs, UNION ALL the query, which takes the query's types
// (measured on the pinned image: CREATE VIEW m.v AS SELECT * FROM (SELECT
// NULL AS c, NULL AS d, NULL AS e, NULL AS f FROM (SELECT 1) WHERE FALSE
// UNION ALL (SELECT a, b, arr, s FROM m.t)) gave c INTEGER, d STRING, e
// REPEATED FLOAT and f a RECORD with its field z, and read back the row).
// Once the script is done, each such view is given its columns'
// descriptions and keeps its query as written (madeViewWithColumns).
//
// A dry run of a lone such CREATE VIEW runs nothing and makes nothing:
// the view's dataset must exist (404 notFound "Not found: Dataset
// project:ds", as for any query), its query is checked with a dry run of
// it alone, the list with it (400 invalidQuery, as for the statement
// itself), and the aliased query is given to the emulator as a dry run,
// whose answer is the client's with statistics.query.statementType
// CREATE_VIEW and totalBytesProcessed 0 (a view reads no data when it is
// made), and no schema or rows: the statement gives no result. The
// emulator's own dry run of a CREATE VIEW answered statementType SELECT
// (measured). A dry run of CREATE VIEW, without OR REPLACE or IF NOT
// EXISTS, of a view that exists is 501: what BigQuery's dry run answers
// for it is not known here. A script with a column list in a dry run stays
// 501 (checkDDL).

// scriptViewList is a CREATE VIEW with a column list in a script, at
// sql[pos:end].
type scriptViewList struct {
	pos, end int
	v        viewColumnList
}

// tempScriptViewLists returns the CREATE VIEW statements with a column
// list of sql, a script of several statements with a TEMP table or
// function, or false for any other query.
func tempScriptViewLists(sql string) ([]scriptViewList, bool) {
	upper := strings.ToUpper(sql)
	if !strings.Contains(upper, "VIEW") || !strings.Contains(upper, "TEMP") || !strings.Contains(sql, ";") {
		return nil, false
	}
	toks, ok := lex(sql)
	if !ok || !tokensHaveTemp(toks) {
		return nil, false
	}
	stmts := 0
	var out []scriptViewList
	for _, stmt := range splitStatements(toks) {
		if len(stmt) > 0 {
			stmts++
		}
		body, _, _ := stripControlFlow(stmt)
		if len(body) < 8 || !body[0].is("CREATE") {
			continue
		}
		pos, end := body[0].pos, body[len(body)-1].end
		if v, ok := parseViewColumnList(sql[pos:end]); ok {
			out = append(out, scriptViewList{pos, end, v})
		}
	}
	return out, stmts > 1 && len(out) > 0
}

// untypedQuery is the view's query with the columns of the client's named
// by the list, when the query's columns cannot be read beforehand (above).
func (v viewColumnList) untypedQuery() string {
	cols := make([]string, len(v.columns))
	for i, c := range v.columns {
		cols[i] = "NULL AS " + quoteName(c)
	}
	return "SELECT * FROM (SELECT " + strings.Join(cols, ", ") + " FROM (SELECT 1) WHERE FALSE UNION ALL (\n" +
		v.query + "\n))"
}

// tempScriptViews carries out q, a script with a TEMP table or function
// and the CREATE VIEW statements with a column list views (above).
func (f front) tempScriptViews(w http.ResponseWriter, r *http.Request, q queryOptions, views []scriptViewList, insert bool) {
	type made struct {
		ds, table string
		v         viewColumnList
	}
	var after []made
	text := q.Query
	// From the last, so that the positions of the earlier stand.
	sort.Slice(views, func(i, j int) bool { return views[i].pos > views[j].pos })
	for _, sv := range views {
		v := sv.v
		if v.badOption != "" {
			writeError(w, http.StatusNotImplemented, "notImplemented", v.badOptionMessage())
			return
		}
		var sel string
		if fields, status, _ := f.lone(r, q, v.query); status == http.StatusOK {
			var msg string
			if sel, msg = v.aliasedQuery(fields); msg != "" {
				writeError(w, http.StatusBadRequest, "invalidQuery", msg)
				return
			}
		} else {
			// It cannot run alone: it may read what the script makes
			// first. The engine reports a query that cannot run, or of
			// another number of columns, when the script reaches it.
			if msg := v.checkNames(); msg != "" {
				writeError(w, http.StatusBadRequest, "invalidQuery", msg)
				return
			}
			sel = v.untypedQuery()
		}
		if ds, table, ok := tableOf(q, v.path); ok {
			existed := false
			if v.ifNotExists {
				status, _ := f.get(r, tablePath(ds, table))
				existed = status == http.StatusOK
			}
			if !existed {
				after = append(after, made{ds, table, v})
			}
		}
		text = text[:sv.pos] + v.head + v.options + " AS " + sel + text[sv.end:]
	}
	if !setQueryText(r, insert, text) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return
	}
	client := jobText{query: q.Query}
	q.Query = text
	rec := newRecorder()
	f.runQuery(rec, r, q, insert)
	var job map[string]any
	if insert && rec.status == http.StatusOK {
		_ = json.Unmarshal(rec.body.Bytes(), &job)
	}
	if _, failed := queryFailure(rec, job); !failed {
		for _, m := range after {
			if status, _ := f.get(r, tablePath(m.ds, m.table)); status != http.StatusOK {
				continue // dropped by a later statement of the script
			}
			if msg := f.madeViewWithColumns(r, m.ds, m.table, m.v); msg != "" {
				writeError(w, http.StatusInternalServerError, "internalError", msg)
				return
			}
		}
	}
	f.clientText(w, rec, client)
}

// dryRunViewWithColumns answers a dry run of v, the lone statement of q
// (above).
func (f front) dryRunViewWithColumns(w http.ResponseWriter, r *http.Request, q queryOptions, v viewColumnList, insert bool) {
	if ds, table, ok := tableOf(q, v.path); ok {
		if status, _ := f.get(r, "/datasets/"+url.PathEscape(ds)); status == http.StatusNotFound {
			writeError(w, http.StatusNotFound, "notFound", "Not found: Dataset "+projectOf(f.base)+":"+ds)
			return
		}
		if !v.orReplace && !v.ifNotExists {
			if status, _ := f.get(r, tablePath(ds, table)); status == http.StatusOK {
				writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a dry run of CREATE "+
					"VIEW with a column list, without OR REPLACE or IF NOT EXISTS, of "+ds+"."+table+", which exists. "+
					"CloudBurrow does not know what BigQuery's dry run answers for it. Nothing was run.")
				return
			}
		}
	}
	fields, status, body := f.loneDry(r, q, v.query)
	if status != http.StatusOK {
		writeError(w, http.StatusBadRequest, "invalidQuery", errorMessage(body, status))
		return
	}
	sel, msg := v.aliasedQuery(fields)
	if msg != "" {
		writeError(w, http.StatusBadRequest, "invalidQuery", msg)
		return
	}
	if !setQueryText(r, insert, sel) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return
	}
	rec := newRecorder()
	f.next.ServeHTTP(rec, r)
	if rec.status == http.StatusOK {
		b := dryViewAnswer(rec.body.Bytes())
		rec.body.Reset()
		rec.body.Write(b)
	}
	f.clientText(w, rec, jobText{query: q.Query})
}

// loneDry is lone's dry run: the columns sel gives, read with nothing run
// or kept.
func (f front) loneDry(r *http.Request, q queryOptions, sel string) (fields []field, status int, body []byte) {
	legacy := false
	req, err := json.Marshal(struct {
		queryOptions
		DryRun bool `json:"dryRun"`
	}{queryOptions{Query: "SELECT * FROM (\n" + sel + "\n) LIMIT 0", UseLegacySQL: &legacy,
		DefaultDataset: q.DefaultDataset, ParameterMode: q.ParameterMode, QueryParameters: q.QueryParameters}, true})
	if err != nil {
		return nil, http.StatusInternalServerError, nil
	}
	status, got := f.send(r, http.MethodPost, "/queries", req)
	var res struct {
		Schema tableSchema `json:"schema"`
	}
	if status != http.StatusOK || json.Unmarshal(got, &res) != nil {
		if status == http.StatusOK {
			status = http.StatusInternalServerError
		}
		return nil, status, got
	}
	return res.Schema.Fields, http.StatusOK, nil
}

// dryViewAnswer is the emulator's answer b to the dry run of a view's
// query, as BigQuery's to the dry run of the CREATE VIEW (above): a job
// (jobs.insert) or a jobs.query response.
func dryViewAnswer(b []byte) []byte {
	var resp map[string]any
	if json.Unmarshal(b, &resp) != nil {
		return b
	}
	if st, ok := resp["statistics"].(map[string]any); ok {
		st["totalBytesProcessed"] = "0"
		qs, _ := st["query"].(map[string]any)
		if qs == nil {
			qs = map[string]any{}
			st["query"] = qs
		}
		qs["statementType"] = "CREATE_VIEW"
		qs["totalBytesProcessed"] = "0"
		delete(qs, "schema")
	} else {
		resp["totalBytesProcessed"] = "0"
		for _, k := range []string{"schema", "rows", "totalRows", "pageToken"} {
			delete(resp, k)
		}
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return b
	}
	return out
}
