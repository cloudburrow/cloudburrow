package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// tables.patch's labels and description, and the schema changes
// tables.patch and tables.update refuse (#1009).
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
// STRING column `id` left a table of three columns with that one).
// BigQuery refuses each ("Provided Schema does not match Table"): a
// column is changed with ALTER TABLE or by overwriting the table, and a
// column added to a table must be NULLABLE or REPEATED
// (https://cloud.google.com/bigquery/docs/managing-table-schemas). Only
// a REQUIRED column may be relaxed to NULLABLE. So such a schema is 400
// invalid, before the emulator sees it (schemaChange). Adding NULLABLE or
// REPEATED columns is left as it was (#1010, #1013).

// checkTableUpdate checks a tables.update or tables.patch of dataset.table
// and, for a patch that gives labels or a description, turns it into the
// update that carries it out. It reports whether it answered w.
func (f front) checkTableUpdate(w http.ResponseWriter, r *http.Request, dataset, table string) bool {
	var patch map[string]json.RawMessage
	if _, ok := decode(r, &patch); !ok {
		return false
	}
	_, hasSchema := patch["schema"]
	_, hasLabels := patch["labels"]
	_, hasDesc := patch["description"]
	if !hasSchema && !(r.Method == http.MethodPatch && (hasLabels || hasDesc)) {
		return false
	}
	status, got := f.get(r, tablePath(dataset, table))
	if status != http.StatusOK {
		return false
	}
	var current map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(got))
	dec.UseNumber()
	if dec.Decode(&current) != nil {
		return false
	}
	if hasSchema {
		var have, want struct {
			Schema *tableSchema `json:"schema"`
			Type   string       `json:"type"`
		}
		_ = json.Unmarshal(got, &have)
		if json.Unmarshal(patch["schema"], &want.Schema) == nil && want.Schema != nil && have.Schema != nil &&
			(have.Type == "" || strings.EqualFold(have.Type, "TABLE")) {
			if msg := schemaChange(have.Schema.Fields, want.Schema.Fields, ""); msg != "" {
				writeError(w, http.StatusBadRequest, "invalid", fmt.Sprintf("Provided Schema does not match Table %s:%s.%s. %s",
					projectOf(f.base), dataset, table, msg))
				return true
			}
		}
	}
	if r.Method != http.MethodPatch || !hasLabels && !hasDesc {
		return false
	}
	body, ok := patchTable(current, patch)
	if !ok {
		return false
	}
	setBody(r, body)
	r.Method = http.MethodPut
	return false
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

// schemaChange returns why BigQuery refuses to change a table's fields
// (have) to want, or "": a field left out, a field whose type or mode
// changed (other than REQUIRED to NULLABLE), or a REQUIRED field added.
// Names are compared as BigQuery compares them, without case.
func schemaChange(have, want []field, prefix string) string {
	byName := map[string]field{}
	for _, f := range want {
		byName[strings.ToLower(f.Name)] = f
	}
	old := map[string]bool{}
	for _, h := range have {
		old[strings.ToLower(h.Name)] = true
		w, ok := byName[strings.ToLower(h.Name)]
		if !ok {
			return fmt.Sprintf("Field %s%s is missing in new schema", prefix, h.Name)
		}
		if ht, wt := canonicalType(h.Type), canonicalType(w.Type); ht != wt {
			return fmt.Sprintf("Field %s%s has changed type from %s to %s", prefix, h.Name, strings.ToUpper(h.Type), strings.ToUpper(w.Type))
		}
		hm, wm := modeOf(h.Mode), modeOf(w.Mode)
		if hm != wm && !(hm == "REQUIRED" && wm == "NULLABLE") {
			return fmt.Sprintf("Field %s%s has changed mode from %s to %s", prefix, h.Name, hm, wm)
		}
		if canonicalType(h.Type) == "RECORD" {
			if msg := schemaChange(h.Fields, w.Fields, prefix+h.Name+"."); msg != "" {
				return msg
			}
		}
	}
	for _, w := range want {
		if !old[strings.ToLower(w.Name)] && modeOf(w.Mode) == "REQUIRED" {
			return fmt.Sprintf("Cannot add required fields to an existing schema. (field: %s%s)", prefix, w.Name)
		}
	}
	return ""
}

func modeOf(m string) string {
	if m == "" {
		return "NULLABLE"
	}
	return strings.ToUpper(m)
}
