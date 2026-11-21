package bigqueryfront

// ALTER VIEW ... ALTER COLUMN ... SET OPTIONS (#2104).
//
// The DDL reference documents
//
//	ALTER { TABLE | VIEW } [IF EXISTS] name
//	ALTER COLUMN [IF EXISTS] column_name
//	SET OPTIONS({ column_set_options_list | view_column_set_options_list })
//
// whose one option, description, "is equivalent to the
// schema.fields[].description table resource property"
// (https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#alter_column_set_options_statement,
// re-read 2026-11-21): "The following example sets a new description on a
// view column called total". The emulator refuses ALTER VIEW (measured:
// 400 "Statement not supported: AlterViewStatement"), so the front sets
// the column's description itself, as tables.patch of the view's schema
// does (tablepatch.go), and answers the emulator's job of a statement
// that changes nothing (ddloptions.go's ALTER VIEW SET OPTIONS does the
// same), with the client's text. Only top-level columns: "Modifying
// subfields, such as nested columns in a STRUCT, is not supported."

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// viewColumnOptions is one ALTER VIEW ... ALTER COLUMN ... SET OPTIONS.
type viewColumnOptions struct {
	path                 []string
	ifExists, colIfExist bool
	column               string
	description          *string // nil: NULL
}

// parseViewColumnOptions reads sql as one ALTER VIEW ... ALTER COLUMN ...
// SET OPTIONS; ok is false for any other query. msg is a syntax error.
func parseViewColumnOptions(sql string) (v viewColumnOptions, msg string, ok bool) {
	t, lexed := lex(sql)
	if !lexed {
		return v, "", false
	}
	for len(t) > 0 && t[len(t)-1].punct(";") {
		t = t[:len(t)-1]
	}
	for _, x := range t {
		if x.punct(";") {
			return v, "", false
		}
	}
	if len(t) < 6 || !t[0].is("ALTER") || !t[1].is("VIEW") {
		return v, "", false
	}
	i := 2
	if i+1 < len(t) && t[i].is("IF") && t[i+1].is("EXISTS") {
		v.ifExists, i = true, i+2
	}
	v.path, i = path(t, i)
	if len(v.path) == 0 || i+1 >= len(t) || !t[i].is("ALTER") || !t[i+1].is("COLUMN") {
		return v, "", false
	}
	i += 2
	if i+1 < len(t) && t[i].is("IF") && t[i+1].is("EXISTS") {
		v.colIfExist, i = true, i+2
	}
	if i >= len(t) || t[i].kind != tokWord && t[i].kind != tokQuoted {
		return v, "Syntax error: expected the column's name after ALTER COLUMN", true
	}
	v.column = t[i].text
	i++
	if i+2 >= len(t) || !t[i].is("SET") || !t[i+1].is("OPTIONS") || !t[i+2].punct("(") {
		return v, "", false // DROP NOT NULL, SET DATA TYPE ...: the emulator's answer
	}
	end := closing(t, i+2)
	if end != len(t)-1 {
		return v, "Syntax error: expected the end of the statement after SET OPTIONS (...)", true
	}
	for _, o := range splitTop(t[i+3 : end]) {
		if len(o) == 0 {
			continue
		}
		if len(o) != 3 || !o[1].punct("=") {
			return v, "Syntax error in SET OPTIONS: expected NAME = VALUE", true
		}
		if !o[0].is("description") {
			return v, fmt.Sprintf("Invalid option %s for a view column: a view column takes description", o[0].text), true
		}
		switch {
		case o[2].is("NULL"):
			v.description = nil
		case o[2].kind == tokString:
			d := unquoteLiteral(o[2].text)
			v.description = &d
		default:
			return v, "Invalid value for the column option description: it is a STRING literal or NULL", true
		}
	}
	return v, "", true
}

// alterViewColumn carries out v, q's statement.
func (f front) alterViewColumn(w http.ResponseWriter, r *http.Request, q queryOptions, v viewColumnOptions, insert bool) {
	ds, table, ok := tableOf(q, v.path)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalidQuery", "Table name "+strings.Join(v.path, ".")+
			" is missing a dataset while no default dataset is set in the request.")
		return
	}
	status, got := f.get(r, tablePath(ds, table))
	name := projectOf(f.base) + ":" + ds + "." + table
	var meta map[string]json.RawMessage
	var view struct {
		Type   string `json:"type"`
		Schema struct {
			Fields []map[string]any `json:"fields"`
		} `json:"schema"`
	}
	if status == http.StatusOK && (json.Unmarshal(got, &meta) != nil || json.Unmarshal(got, &view) != nil) {
		status = http.StatusInternalServerError
	}
	patch := false
	switch {
	case status == http.StatusNotFound && v.ifExists:
	case status == http.StatusNotFound:
		writeError(w, http.StatusNotFound, "notFound", "Not found: Table "+name)
		return
	case status != http.StatusOK:
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not read "+name)
		return
	case !isView(view.Type):
		writeError(w, http.StatusBadRequest, "invalidQuery", name+" is not a view; ALTER VIEW alters views (use ALTER TABLE)")
		return
	default:
		found := false
		for _, fld := range view.Schema.Fields {
			if n, _ := fld["name"].(string); strings.EqualFold(n, v.column) {
				found = true
				if v.description == nil {
					delete(fld, "description")
				} else {
					fld["description"] = *v.description
				}
			}
		}
		if !found && !v.colIfExist {
			writeError(w, http.StatusBadRequest, "invalidQuery", "Column "+v.column+" not found in "+name)
			return
		}
		patch = found
	}
	sent := "ALTER TABLE " + quotePath([]string{ds, table}) + " SET OPTIONS(description = NULL)"
	if status == http.StatusNotFound {
		sent = "SELECT 1"
	}
	if !setQueryText(r, insert, sent) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return
	}
	rec := newRecorder()
	f.next.ServeHTTP(rec, r)
	var job map[string]any
	if insert && rec.status == http.StatusOK {
		_ = json.Unmarshal(rec.body.Bytes(), &job)
	}
	if _, failed := queryFailure(rec, job); failed || !patch {
		f.clientText(w, rec, jobText{query: q.Query})
		return
	}
	body, err := json.Marshal(map[string]any{"schema": map[string]any{"fields": view.Schema.Fields}})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internalError", err.Error())
		return
	}
	if st, b := f.send(r, http.MethodPatch, tablePath(ds, table), body); st != http.StatusOK {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: the column's description could not be set: "+string(b))
		return
	}
	f.clientText(w, rec, jobText{query: q.Query})
}
