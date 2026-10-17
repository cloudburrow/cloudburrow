package bigqueryfront

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// A dataset's default collation (#1459): "When a new table is created in
// the dataset, the table inherits a default collation specification unless
// a collation specification is explicitly specified for a table or a
// column" (DEFAULT COLLATE of CREATE SCHEMA,
// https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#create_schema_statement),
// and a table's default collation is that of its new STRING columns
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/collation-concepts#collate_define).
// The emulator kept the dataset's defaultCollation (as datasets.insert
// gave it) and made the table with neither. The front gives a table made
// in such a dataset, by tables.insert or by a CREATE [OR REPLACE] TABLE,
// lone or (#1516) in a script of several statements, the dataset's
// defaultCollation when it names none, and each of its STRING columns,
// and each STRING inside a STRUCT or an ARRAY column (#1516), the table's
// default collation when it names none; a CREATE TABLE gets the dataset's
// as its own DEFAULT COLLATE before it runs (withDefaultCollation and
// withScriptDefaultCollation, below). The engine compares a collated
// column's values by its collation (#1508).

// withCollation writes collation into each STRING field of fields that
// names none, a REPEATED one and one inside a RECORD at any depth too
// (#1516: "A column that contains a collation-supported type in its
// column dataset is a collation-supported column"), and reports whether
// it wrote any.
func withCollation(fields []any, collation string) bool {
	changed := false
	for _, f := range fields {
		m, ok := f.(map[string]any)
		if !ok {
			continue
		}
		if sub, ok := m["fields"].([]any); ok && withCollation(sub, collation) {
			changed = true
		}
		if m["type"] != "STRING" {
			continue
		}
		if c, _ := m["collation"].(string); c == "" {
			m["collation"] = collation
			changed = true
		}
	}
	return changed
}

// inheritCollation gives f, a new column's TableFieldSchema, and each
// field inside it, the table's default collation when it is a STRING
// that names none; one whose COLLATE is the empty string keeps none (#1516).
func inheritCollation(f map[string]any, collation string) {
	if sub, ok := f["fields"].([]any); ok {
		for _, s := range sub {
			if m, ok := s.(map[string]any); ok {
				inheritCollation(m, collation)
			}
		}
	}
	if _, named := f["collation"]; !named && f["type"] == "STRING" {
		f["collation"] = collation
	}
}

// collated gives table, a Table resource, the dataset's collation when
// it names none, and its STRING columns the table's (above). It reports
// whether it changed table.
func collated(table map[string]any, datasetCollation string) bool {
	changed := false
	c, _ := table["defaultCollation"].(string)
	if _, named := table["defaultCollation"]; !named && datasetCollation != "" {
		table["defaultCollation"], c, changed = datasetCollation, datasetCollation, true
	}
	if c == "" {
		return changed
	}
	if schema, ok := table["schema"].(map[string]any); ok {
		if fields, ok := schema["fields"].([]any); ok && withCollation(fields, c) {
			changed = true
		}
	}
	return changed
}

// datasetCollation returns the defaultCollation of dataset, or "".
func (f front) datasetCollation(r *http.Request, dataset string) string {
	st, got := f.get(r, "/datasets/"+url.PathEscape(dataset))
	var ds struct {
		DefaultCollation string `json:"defaultCollation"`
	}
	if st != http.StatusOK || json.Unmarshal(got, &ds) != nil {
		return ""
	}
	return ds.DefaultCollation
}

// defaultTableCollation returns tables.insert's body, raw, collated
// (above), and sets it as r's.
func (f front) defaultTableCollation(r *http.Request, dataset string, raw []byte) []byte {
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&body) != nil || body == nil {
		return raw
	}
	// The dataset is read only for a table that a collation could change.
	if _, named := body["defaultCollation"]; named || !bytes.Contains(raw, []byte(`"STRING"`)) {
		if !collated(body, "") {
			return raw
		}
	} else if !collated(body, f.datasetCollation(r, dataset)) {
		return raw
	}
	out, err := json.Marshal(body)
	if err != nil {
		return raw
	}
	setBody(r, out)
	return out
}

// defaultCollationKey marks a request whose CREATE TABLE is being
// collated, so that runQuery runs it as it would otherwise.
type defaultCollationKey struct{}

// withDefaultCollation runs q, a lone CREATE TABLE (a CREATE TABLE AS
// SELECT too) with a default collation, its own DEFAULT COLLATE or else
// its dataset's. With the dataset's, it adds `DEFAULT COLLATE '...'` to
// the statement before running it, so the engine makes each STRING
// column that names no COLLATE with that collation, and the emulator
// records it (bigquery-emulator patch 0049); a table made first and
// collated after would compare case-sensitively (#1508). It then gives
// the table it made the default collation, with tables.update. It
// reports false, having done nothing, otherwise.
func (f front) withDefaultCollation(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	if r.Context().Value(defaultCollationKey{}) != nil || !strings.Contains(strings.ToUpper(q.Query), "CREATE") {
		return false
	}
	parts, replace, _, ok := createdTableOf(q.Query, false)
	if !ok {
		return false
	}
	own, named, at, ok := tableDefaultCollate(q.Query)
	if !ok {
		return false
	}
	project := projectOf(f.base)
	dataset, table := "", parts[len(parts)-1]
	switch len(parts) {
	case 1:
		dataset = defaultDatasetOf(q)
	case 2:
		dataset = parts[0]
	case 3:
		if parts[0] != project {
			return false
		}
		dataset = parts[1]
	}
	if dataset == "" {
		return false
	}
	c, client := own, jobText{query: q.Query}
	if !named {
		if c = f.datasetCollation(r, dataset); c == "" {
			return false
		}
		sql := q.Query[:at] + " DEFAULT COLLATE " + sqlString(c) + q.Query[at:]
		if !setQueryText(r, insert, sql) {
			return false
		}
		q.Query = sql
	} else if c == "" {
		return false
	}
	existed, _ := f.get(r, tablePath(dataset, table))
	rec := newRecorder()
	f.runQuery(rec, r.WithContext(context.WithValue(r.Context(), defaultCollationKey{}, true)), q, insert)
	if rec.status == http.StatusOK && (existed != http.StatusOK || replace) {
		if st, got := f.get(r, tablePath(dataset, table)); st == http.StatusOK {
			var tb map[string]any
			dec := json.NewDecoder(bytes.NewReader(got))
			dec.UseNumber()
			if dec.Decode(&tb) == nil && tb != nil {
				if _, has := tb["defaultCollation"]; !has {
					tb["defaultCollation"] = c
					if body, err := json.Marshal(tb); err == nil {
						f.send(r, http.MethodPut, tablePath(dataset, table), body) // tables.update keeps it, as projectdefaults.go's
					}
				}
			}
		}
	}
	f.clientText(w, rec, client) // the job reads back the query as the client sent it
	return true
}

// scriptCreatedTable is a CREATE TABLE of a script that
// withScriptDefaultCollation collates: its dataset and table, and the
// default collation it is given.
type scriptCreatedTable struct {
	dataset, table, collation string
	existed                   bool
}

// withScriptDefaultCollation runs q, a script of several statements, with
// each CREATE [OR REPLACE] TABLE in it (not a TEMP one, a CLONE, COPY or
// LIKE) that names no DEFAULT COLLATE given its dataset's as its own,
// before the script runs, as withDefaultCollation does a lone one
// (#1516). Measured before: such a table was made case-sensitive, and
// tables.get read no defaultCollation. It then gives each table the
// script made its default collation, with tables.update. It reports
// false, having done nothing, when no CREATE TABLE of q takes one.
func (f front) withScriptDefaultCollation(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	if r.Context().Value(defaultCollationKey{}) != nil || !strings.Contains(q.Query, ";") ||
		!strings.Contains(strings.ToUpper(q.Query), "CREATE") {
		return false
	}
	toks, ok := lex(q.Query)
	if !ok {
		return false
	}
	stmts := 0
	for _, st := range splitStatements(toks) {
		if len(st) > 0 {
			stmts++
		}
	}
	if stmts < 2 {
		return false
	}
	project := projectOf(f.base)
	collations := map[string]string{}
	type edit struct {
		at   int
		text string
	}
	var edits []edit
	var made []scriptCreatedTable
	for _, st := range splitStatements(toks) {
		for len(st) > 0 && (st[0].is("BEGIN") || st[0].is("THEN") || st[0].is("ELSE") || st[0].is("DO") || st[0].is("LOOP")) {
			st = st[1:] // a statement that opens a block
		}
		if len(st) < 3 || !st[0].is("CREATE") {
			continue
		}
		if ds, c, ok := schemaDefaultCollate(st); ok {
			// The script's CREATE SCHEMA makes the dataset, with that
			// default, before its tables are made.
			if _, seen := collations[ds]; !seen {
				collations[ds] = c
			}
			continue
		}
		sql := q.Query[st[0].pos:st[len(st)-1].end]
		parts, _, _, ok := createdTableOf(sql, false)
		if !ok {
			continue
		}
		own, named, at, ok := tableDefaultCollate(sql)
		if !ok {
			continue
		}
		dataset, table := "", parts[len(parts)-1]
		switch len(parts) {
		case 1:
			dataset = defaultDatasetOf(q)
		case 2:
			dataset = parts[0]
		case 3:
			if parts[0] == project {
				dataset = parts[1]
			}
		}
		if dataset == "" {
			continue
		}
		c := own
		if !named {
			var seen bool
			if c, seen = collations[dataset]; !seen {
				c = f.datasetCollation(r, dataset)
				collations[dataset] = c
			}
			if c == "" {
				continue
			}
			edits = append(edits, edit{at: st[0].pos + at, text: " DEFAULT COLLATE " + sqlString(c)})
		} else if c == "" {
			continue
		}
		existed, _ := f.get(r, tablePath(dataset, table))
		made = append(made, scriptCreatedTable{dataset: dataset, table: table, collation: c, existed: existed == http.StatusOK})
	}
	if len(made) == 0 {
		return false
	}
	client := jobText{query: q.Query}
	if len(edits) > 0 {
		sql := q.Query
		for i := len(edits) - 1; i >= 0; i-- {
			sql = sql[:edits[i].at] + edits[i].text + sql[edits[i].at:]
		}
		if !setQueryText(r, insert, sql) {
			return false
		}
		q.Query = sql
	}
	rec := newRecorder()
	f.runQuery(rec, r.WithContext(context.WithValue(r.Context(), defaultCollationKey{}, true)), q, insert)
	if rec.status == http.StatusOK {
		for _, m := range made {
			if !m.existed {
				f.setTableDefaultCollation(r, m.dataset, m.table, m.collation)
			}
		}
	}
	f.clientText(w, rec, client) // the job reads back the query as the client sent it
	return true
}

// setTableDefaultCollation gives dataset.table, when it has none, the
// default collation c, with tables.update, which keeps it.
func (f front) setTableDefaultCollation(r *http.Request, dataset, table, c string) {
	st, got := f.get(r, tablePath(dataset, table))
	if st != http.StatusOK {
		return
	}
	var tb map[string]any
	dec := json.NewDecoder(bytes.NewReader(got))
	dec.UseNumber()
	if dec.Decode(&tb) != nil || tb == nil {
		return
	}
	if _, has := tb["defaultCollation"]; has {
		return
	}
	tb["defaultCollation"] = c
	if body, err := json.Marshal(tb); err == nil {
		f.send(r, http.MethodPut, tablePath(dataset, table), body)
	}
}

// schemaDefaultCollate reads st, a CREATE SCHEMA statement's tokens, for
// its dataset and DEFAULT COLLATE (empty when it names none); ok is false
// for any other statement.
func schemaDefaultCollate(st []token) (dataset, collation string, ok bool) {
	if len(st) < 3 || !st[0].is("CREATE") || !st[1].is("SCHEMA") {
		return "", "", false
	}
	i := 2
	if i+2 < len(st) && st[i].is("IF") && st[i+1].is("NOT") && st[i+2].is("EXISTS") {
		i += 3
	}
	parts, next := path(st, i)
	if len(parts) == 0 {
		return "", "", false
	}
	for j := next; j+2 < len(st); j++ {
		if st[j].is("OPTIONS") {
			break
		}
		if st[j].is("DEFAULT") && st[j+1].is("COLLATE") {
			collation, _ = stringLiteral(st[j+2])
			break
		}
	}
	return parts[len(parts)-1], collation, true
}
