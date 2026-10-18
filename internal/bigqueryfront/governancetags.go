package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"strings"
)

// A column's data governance tags (#1456).
//
// The DDL reference's column option data_governance_tags,
// ARRAY<STRUCT<STRING, STRING>>, "Applies data governance tags to a column
// in a table. Example: data_governance_tags = [("myproject/tag_key",
// "tag_value")]", in a CREATE TABLE's column_option_list and in ALTER
// COLUMN SET OPTIONS, where "Setting the VALUE to NULL clears the column's
// value for that option"
// (https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language,
// read 2026-10-17). The table resource keeps them as the field's
// dataGovernanceTagsInfo.dataGovernanceTags, a map of tag key to tag value
// (TableFieldSchema): "Only one data governance tag is currently supported
// on a field", the key "in the namespaced format, for example
// "parent-id/pii""; and a tables.patch that omits dataGovernanceTagsInfo
// keeps the column's tags, an empty object clears them, and one with tags
// replaces them.
//
// The engine's TableFieldSchema (google.golang.org/api v0.279.0) has no
// dataGovernanceTagsInfo, so it drops the field from everything it keeps:
// the front keeps each top-level column's tags in the table's settings
// (tableoptions.go), and puts them in the table's schema in tables.get,
// tables.insert, tables.patch and tables.update answers. INFORMATION_SCHEMA
// documents no column for them (COLUMNS, COLUMN_FIELD_PATHS, read
// 2026-10-17). A tag is not looked up in Resource Manager, and no access
// rule of it is applied to the column's reads.

// governanceTagsKey is the field of a TableFieldSchema.
const governanceTagsKey = "dataGovernanceTagsInfo"

// governanceTagsOption reads val, data_governance_tags's value (an array
// of (key, value) STRUCT literals, or NULL), as the field's
// dataGovernanceTagsInfo; msg is why it cannot be read (400).
func governanceTagsOption(val []token) (info map[string]any, msg string) {
	if len(val) == 1 && val[0].is("NULL") {
		return map[string]any{}, ""
	}
	bad := `The option data_governance_tags must be an array of (key, value) string literals such as [("myproject/tag_key", "tag_value")]`
	if len(val) > 0 && val[0].is("ARRAY") {
		val = val[1:]
	}
	if len(val) < 2 || !val[0].punct("[") || !val[len(val)-1].punct("]") {
		return nil, bad
	}
	tags := map[string]any{}
	in := val[1 : len(val)-1]
	for i := 0; i < len(in); {
		if i > 0 {
			if !in[i].punct(",") {
				return nil, bad
			}
			i++
		}
		if i < len(in) && in[i].is("STRUCT") {
			i++
		}
		if i+4 >= len(in) || !in[i].punct("(") || !in[i+2].punct(",") || !in[i+4].punct(")") {
			return nil, bad
		}
		k, ok1 := stringLiteral(in[i+1])
		v, ok2 := stringLiteral(in[i+3])
		if !ok1 || !ok2 {
			return nil, bad
		}
		tags[k] = v
		i += 5
	}
	info = map[string]any{"dataGovernanceTags": tags}
	if msg := governanceTagsRefused(info); msg != "" {
		return nil, msg
	}
	return info, ""
}

// governanceTagsRefused is why BigQuery refuses info, a field's
// dataGovernanceTagsInfo (400), or "": more than one tag, a key not
// "parent-id/key", an empty value.
func governanceTagsRefused(info map[string]any) string {
	tags, _ := info["dataGovernanceTags"].(map[string]any)
	if len(tags) > 1 {
		return "Only one data governance tag is supported on a field"
	}
	for k, v := range tags {
		s, _ := v.(string)
		parent, short, ok := strings.Cut(k, "/")
		if !ok || parent == "" || short == "" || strings.Contains(short, "/") {
			return "The data governance tag key " + k + " must be in the namespaced format parent-id/tag_key"
		}
		if s == "" {
			return "The data governance tag " + k + " must have a value"
		}
	}
	return ""
}

// governanceTagsOf reads schema, a TableSchema, for the dataGovernanceTagsInfo
// of each top-level field that has one: tags by the field's name in lower
// case, an empty map clearing them. msg is a 400's message, nested a 501's.
func governanceTagsOf(schema json.RawMessage) (tags map[string]map[string]string, msg, nested string) {
	var s struct {
		Fields []map[string]any `json:"fields"`
	}
	if len(schema) == 0 || json.Unmarshal(schema, &s) != nil {
		return nil, "", ""
	}
	var walk func(fields []any, path string) string
	walk = func(fields []any, path string) string {
		for _, x := range fields {
			m, _ := x.(map[string]any)
			if m == nil {
				continue
			}
			if _, ok := m[governanceTagsKey]; ok {
				return path + str(m, "name")
			}
			if sub, ok := m["fields"].([]any); ok {
				if n := walk(sub, path+str(m, "name")+"."); n != "" {
					return n
				}
			}
		}
		return ""
	}
	for _, f := range s.Fields {
		if sub, ok := f["fields"].([]any); ok && nested == "" {
			nested = walk(sub, str(f, "name")+".")
		}
		raw, ok := f[governanceTagsKey]
		if !ok {
			continue
		}
		info, _ := raw.(map[string]any)
		if raw != nil && info == nil {
			return nil, "dataGovernanceTagsInfo of field " + str(f, "name") + " must be an object", ""
		}
		if m := governanceTagsRefused(info); m != "" {
			return nil, m, ""
		}
		if tags == nil {
			tags = map[string]map[string]string{}
		}
		t := map[string]string{}
		g, _ := info["dataGovernanceTags"].(map[string]any)
		for k, v := range g {
			t[k], _ = v.(string)
		}
		tags[strings.ToLower(str(f, "name"))] = t
	}
	if nested != "" {
		nested = "dataGovernanceTagsInfo of the field " + nested + ", inside a RECORD: the DDL reference applies data " +
			"governance tags to a column in a table"
	}
	return tags, "", nested
}

// setGovernanceTags keeps tags, governanceTagsOf's, for the table made at
// created: with replace (tables.insert, tables.update), the table's tags
// are those alone; else (tables.patch) each column named is set, an empty
// map clearing it. columns, when not nil, are the table's columns in lower
// case: a column it lacks keeps no tags (DROP COLUMN, RENAME COLUMN).
func (s *tableSettings) setGovernanceTags(key, created string, tags map[string]map[string]string, replace bool, columns map[string]bool) {
	if s == nil || tags == nil && !replace && columns == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.m[key]
	if t.Created != created {
		t = tableSetting{Created: created}
	}
	if replace {
		t.GovernanceTags = nil
	}
	for col, g := range tags {
		if len(g) == 0 {
			delete(t.GovernanceTags, col)
			continue
		}
		if t.GovernanceTags == nil {
			t.GovernanceTags = map[string]map[string]string{}
		}
		t.GovernanceTags[col] = g
	}
	if columns != nil {
		for col := range t.GovernanceTags {
			if !columns[col] {
				delete(t.GovernanceTags, col)
			}
		}
	}
	if len(t.GovernanceTags) == 0 {
		t.GovernanceTags = nil
	}
	if s.m == nil {
		if t.zero() {
			return
		}
		s.m = map[string]tableSetting{}
	}
	s.m[key] = t
	s.saveLocked()
}

// withGovernanceTags puts the kept tags of meta, a table resource of
// project.dataset, in its schema's fields, and reports whether it put any.
func (s *tableSettings) withGovernanceTags(project, dataset string, meta map[string]any) bool {
	ref, _ := meta["tableReference"].(map[string]any)
	st := s.get(streamKey(project, dataset, str(ref, "tableId")), str(meta, "creationTime"))
	if len(st.GovernanceTags) == 0 {
		return false
	}
	schema, _ := meta["schema"].(map[string]any)
	fields, _ := schema["fields"].([]any)
	put := false
	for _, x := range fields {
		m, _ := x.(map[string]any)
		g := st.GovernanceTags[strings.ToLower(str(m, "name"))]
		if m == nil || len(g) == 0 {
			continue
		}
		tags := map[string]any{}
		for k, v := range g {
			tags[k] = v
		}
		m[governanceTagsKey] = map[string]any{"dataGovernanceTags": tags}
		put = true
	}
	return put
}

// columnsOf is the lower-case names of meta's top-level columns, or nil
// when it has no schema.
func columnsOf(meta map[string]any) map[string]bool {
	schema, _ := meta["schema"].(map[string]any)
	fields, ok := schema["fields"].([]any)
	if !ok {
		return nil
	}
	out := map[string]bool{}
	for _, x := range fields {
		if m, _ := x.(map[string]any); m != nil {
			out[strings.ToLower(str(m, "name"))] = true
		}
	}
	return out
}

// insertTable is insertTableChecked (front.go) keeping the
// dataGovernanceTagsInfo of the body's top-level fields and answering with
// the table's kept tags.
func (f front) insertTable(w http.ResponseWriter, r *http.Request, dataset, table string, update bool) {
	var body struct {
		Schema json.RawMessage `json:"schema"`
	}
	raw, ok := decode(r, &body)
	if !ok || f.tables == nil {
		f.insertTableChecked(w, r, dataset, table, update)
		return
	}
	tags, msg, nested := governanceTagsOf(body.Schema)
	switch {
	case msg != "":
		writeError(w, http.StatusBadRequest, "invalid", msg)
		return
	case nested != "":
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: "+nested)
		return
	}
	setBody(r, raw)
	if tags == nil && !hasGovernanceTags(f, dataset, table) {
		f.insertTableChecked(w, r, dataset, table, update)
		return
	}
	rec := newRecorder()
	f.insertTableChecked(rec, r, dataset, table, update)
	var meta map[string]any
	if (rec.status != 0 && rec.status != http.StatusOK) || json.Unmarshal(rec.body.Bytes(), &meta) != nil || meta == nil {
		rec.copyTo(w)
		return
	}
	ref, _ := meta["tableReference"].(map[string]any)
	key := streamKey(projectOf(f.base), dataset, str(ref, "tableId"))
	replace := !update || r.Method == http.MethodPut
	if len(body.Schema) == 0 {
		replace = !update
	}
	f.tables.setGovernanceTags(key, str(meta, "creationTime"), tags, replace, columnsOf(meta))
	if f.tables.withGovernanceTags(projectOf(f.base), dataset, meta) {
		writeRecorded(w, rec, meta)
		return
	}
	rec.copyTo(w)
}

// hasGovernanceTags reports whether dataset's table keeps tags,
// which an update without any may change (a column dropped or renamed).
func hasGovernanceTags(f front, dataset, table string) bool {
	if table == "" {
		return false
	}
	f.tables.mu.Lock()
	defer f.tables.mu.Unlock()
	return len(f.tables.m[streamKey(projectOf(f.base), dataset, table)].GovernanceTags) > 0
}

// getTable is getTableAnswered (views.go) with the table's kept data
// governance tags in its schema.
func (f front) getTable(w http.ResponseWriter, r *http.Request, dataset, table string) {
	if f.tables == nil || !hasGovernanceTags(f, dataset, table) {
		f.getTableAnswered(w, r, dataset, table)
		return
	}
	rec := newRecorder()
	f.getTableAnswered(rec, r, dataset, table)
	var meta map[string]any
	if (rec.status != 0 && rec.status != http.StatusOK) || json.Unmarshal(rec.body.Bytes(), &meta) != nil || meta == nil {
		rec.copyTo(w)
		return
	}
	if f.tables.withGovernanceTags(projectOf(f.base), dataset, meta) {
		writeRecorded(w, rec, meta)
		return
	}
	rec.copyTo(w)
}

// renameGovernanceTags keeps the tags of the table made at created of the
// columns of renamed (old name in lower case to new name) under their new
// names, and drops the others' (ALTER TABLE's RENAME COLUMN, DROP COLUMN
// and SET DATA TYPE, which make the table again: alterremake.go).
func (s *tableSettings) renameGovernanceTags(key, created string, renamed map[string]string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.m[key]
	if !ok || t.Created != created || len(t.GovernanceTags) == 0 {
		return
	}
	out := map[string]map[string]string{}
	for old, g := range t.GovernanceTags {
		if n, ok := renamed[old]; ok {
			out[strings.ToLower(n)] = g
		}
	}
	if len(out) == 0 {
		out = nil
	}
	t.GovernanceTags = out
	s.m[key] = t
	s.saveLocked()
}
