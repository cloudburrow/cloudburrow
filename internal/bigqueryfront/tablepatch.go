package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// tables.patch's labels and description (#1009), and the one path of
// tables.patch and tables.update (#1054).
//
// Measured against the pinned image through the front: a table with
// labels a=1, b=2 patched with {"labels": {"a": "9", "b": null, "c": "3"}}
// (what the Go client's TableMetadataToUpdate.DeleteLabel sends) read back
// a=9, b="", c=3, and patched with {"a": "9", "c": "3"} alone lost b: the
// emulator replaces the map with the one it is sent, a null read as "".
// {"description": ""} left the description as it was, and so would an
// empty labels map: the emulator reads the patch into its Table type,
// which drops empty fields, and keeps what it is not given. BigQuery's
// patch "replaces fields that are provided in the submitted table
// resource" (https://cloud.google.com/bigquery/docs/reference/rest/v2/tables/patch),
// merging the labels, a label given null removed
// (https://cloud.google.com/bigquery/docs/deleting-labels#table), and an
// empty description is one given. So a tables.patch that gives labels or
// a description is carried out by the front as the emulator's
// tables.update (which replaces the whole resource, measured: a
// resource without a description or labels cleared them) of the table
// as it is, with the patch applied as BigQuery applies it (patchTable).
//
// The emulator also took, through tables.patch and tables.update, a
// schema that drops a column or changes a column's type, and a REQUIRED
// column added to the table (measured: a patch whose schema was one
// STRING column `id` left a table of three columns with that one), and
// it changes only a table's metadata with either, never its columns
// (#1010, schemaupdate.go). What BigQuery refuses is 400, what it applies
// and the front cannot is 501, and columns added at the table's end are
// added, and fields added to a RECORD since #1036 (schemaChange,
// addColumns).
//
// updateTable is the one path of both methods (#1054): the table is read
// once; a schema is checked against it and, when it adds columns, the
// table is made again with them; then a tables.patch that gives labels or
// a description is sent as the tables.update that carries it out
// (patchTable), and any other request as it came.

// updateTable carries out a tables.patch or tables.update of
// dataset.table whose body, raw, insertTable read (schema is its schema,
// or nil), and answers w.
func (f front) updateTable(w http.ResponseWriter, r *http.Request, dataset, table string, raw []byte, schema *tableSchema) {
	var patch map[string]json.RawMessage
	if json.Unmarshal(raw, &patch) != nil {
		f.next.ServeHTTP(w, r)
		return
	}
	_, hasLabels := patch["labels"]
	_, hasDesc := patch["description"]
	patching := r.Method == http.MethodPatch && (hasLabels || hasDesc)
	if schema == nil && !patching {
		f.next.ServeHTTP(w, r)
		return
	}
	status, got := f.get(r, tablePath(dataset, table))
	var current map[string]json.RawMessage
	var meta struct {
		Type   string      `json:"type"`
		Schema tableSchema `json:"schema"`
	}
	if status != http.StatusOK || json.Unmarshal(got, &current) != nil || json.Unmarshal(got, &meta) != nil {
		f.next.ServeHTTP(w, r) // the emulator's own answer
		return
	}
	if schema != nil {
		name := tableName(projectOf(f.base), tableRef{DatasetID: dataset, TableID: table})
		if isView(meta.Type) || meta.Type != "" && !strings.EqualFold(meta.Type, "TABLE") {
			// A view's columns are its query's.
			if !sameSchema(meta.Schema.Fields, schema.Fields) {
				writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a schema update of "+name+
					", a "+meta.Type+", other than of its descriptions. CloudBurrow does not carry it out. Nothing was changed.")
				return
			}
		} else {
			adds, code, msg := schemaChange(name, meta.Schema.Fields, schema.Fields)
			switch {
			case code == http.StatusBadRequest:
				writeError(w, code, "invalid", msg)
				return
			case code != 0:
				writeError(w, code, "notImplemented", msg)
				return
			}
			// The schema in the table's order (#1036, schemaupdate.go).
			if ordered, ok := schemaInTableOrder(meta.Schema.Fields, patch["schema"]); ok {
				patch["schema"] = ordered
				if b, err := json.Marshal(patch); err == nil {
					setBody(r, b)
				}
			}
			if adds && !f.addColumns(w, r, dataset, table, got, meta.Schema.Fields, patch["schema"]) {
				return
			}
		}
	}
	if patching {
		if body, ok := patchTable(current, patch); ok {
			setBody(r, body)
			r.Method = http.MethodPut
		}
	}
	f.next.ServeHTTP(w, r)
}

// patchTable applies a tables.patch to a table as BigQuery does (above),
// and returns the whole table to send the emulator's tables.update.
func patchTable(current, patch map[string]json.RawMessage) ([]byte, bool) {
	out := map[string]json.RawMessage{}
	for k, v := range current {
		out[k] = v
	}
	for k, v := range patch {
		switch {
		case k == "labels":
			var have map[string]string
			_ = json.Unmarshal(current["labels"], &have)
			var give map[string]*string
			if json.Unmarshal(v, &give) != nil {
				return nil, false
			}
			if give == nil && bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				// labels: null removes them all.
				delete(out, "labels")
				continue
			}
			merged := map[string]string{}
			for name, value := range have {
				merged[name] = value
			}
			for name, value := range give {
				if value == nil {
					delete(merged, name)
					continue
				}
				merged[name] = *value
			}
			if len(merged) == 0 {
				delete(out, "labels")
				continue
			}
			b, err := json.Marshal(merged)
			if err != nil {
				return nil, false
			}
			out["labels"] = b
		case bytes.Equal(bytes.TrimSpace(v), []byte("null")) || k == "description" && bytes.Equal(bytes.TrimSpace(v), []byte(`""`)):
			delete(out, k)
		default:
			out[k] = v
		}
	}
	b, err := json.Marshal(out)
	return b, err == nil
}

// schemaInTableOrder returns a TableSchema's JSON, raw, with its fields in
// the order of the table's, old (inTableOrder), when that is another
// order.
func schemaInTableOrder(old []field, raw json.RawMessage) (json.RawMessage, bool) {
	s, ok := decodeMap(raw)
	if !ok {
		return nil, false
	}
	fields, _ := s["fields"].([]any)
	ordered, moved := inTableOrder(old, fields)
	if !moved {
		return nil, false
	}
	s["fields"] = ordered
	b, err := json.Marshal(s)
	return b, err == nil
}
