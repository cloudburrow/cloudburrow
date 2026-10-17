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
// in such a dataset, by tables.insert or by a lone CREATE [OR REPLACE]
// TABLE, the dataset's defaultCollation when it names none, and each of
// its STRING columns, a RECORD's fields too, the table's default
// collation when the column names none. The collation is recorded, as
// tables.insert of a collated column records its own; the engine compares
// the column's values as it does any STRING's (#1508).

// withCollation writes collation into each STRING field of fields that
// names none, a RECORD's fields too, and reports whether it wrote any.
func withCollation(fields []any, collation string) bool {
	changed := false
	for _, f := range fields {
		m, ok := f.(map[string]any)
		if !ok {
			continue
		}
		switch m["type"] {
		case "STRING":
			if c, _ := m["collation"].(string); c == "" {
				m["collation"] = collation
				changed = true
			}
		case "RECORD", "STRUCT":
			if sub, ok := m["fields"].([]any); ok && withCollation(sub, collation) {
				changed = true
			}
		}
	}
	return changed
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

// withDefaultCollation runs q, a lone CREATE TABLE in a dataset with a
// default collation, and collates the table it made (above) with
// tables.update. It reports false, having done nothing, otherwise.
func (f front) withDefaultCollation(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	if r.Context().Value(defaultCollationKey{}) != nil || !strings.Contains(strings.ToUpper(q.Query), "CREATE") {
		return false
	}
	parts, replace, _, ok := createdTableOf(q.Query, false)
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
	c := f.datasetCollation(r, dataset)
	if c == "" {
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
			if dec.Decode(&tb) == nil && tb != nil && collated(tb, c) {
				if body, err := json.Marshal(tb); err == nil {
					f.send(r, http.MethodPut, tablePath(dataset, table), body) // tables.update keeps it, as projectdefaults.go's
				}
			}
		}
	}
	rec.copyTo(w)
	return true
}
