package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"strings"
)

// An ALTER TABLE of several actions is done whole or not at all (#1612).
//
// The DDL reference refuses a statement of several actions as one: "ALTER
// TABLE mydataset.mytable ADD COLUMN A STRING, ADD COLUMN IF NOT EXISTS B
// GEOGRAPHY, ADD COLUMN C ARRAY<NUMERIC>, ADD COLUMN D DATE OPTIONS(...)
// ... If any of the columns named A, C, or D already exist, the statement
// fails", and so for RENAME COLUMN ("If column A does not exist, then the
// statement fails") and DROP COLUMN
// (https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language,
// docs re-read 2026-10-17). It names no partial result: that the actions
// before a refused one are not done is read from the statement failing
// as a whole, UNVERIFIED by a measurement.
//
// alterTable carries out the actions a run at a time (alterGroups), so it
// checks every run first, as a dry run does (s.dry), and patchRefusal
// makes the checks the tables.patch path (insertTable, insertTableChecked,
// updateTable) would make when the run's patch is sent: a refusal there
// came after the runs before were done, and after the run's own options
// kept by the front (tables.set) and cleared fields (clearTableFields).
// The checks are made on the table as it is (the runs before do not
// change what they check, alterTable). What the engine fails only when
// the table is made again (addColumns) is not checked first.

// patchRefusal returns the status and message with which the tables.patch
// path refuses patch, ALTER TABLE's patch of dataset.table (name), whose
// resource is got; status 0 when it does not.
func (f front) patchRefusal(dataset, table, name string, got []byte, patch map[string]any) (int, string) {
	raw, err := json.Marshal(patch)
	if err != nil {
		return http.StatusInternalServerError, err.Error()
	}
	var body struct {
		Schema json.RawMessage `json:"schema"`
	}
	_ = json.Unmarshal(raw, &body)
	if _, msg, nested := governanceTagsOf(body.Schema); msg != "" { // insertTable, governancetags.go
		return http.StatusBadRequest, msg
	} else if nested != "" {
		return http.StatusNotImplemented, "Not implemented here: " + nested
	}
	if code, _, msg := tableCollationRefusal(raw); code != 0 { // insertTableChecked, collationddl.go
		return code, msg
	}
	if len(body.Schema) == 0 {
		return 0, ""
	}
	var schema tableSchema
	if json.Unmarshal(body.Schema, &schema) != nil {
		return 0, ""
	}
	if hasAlias(schema.Fields) { // legacyTableTypes, typenames.go
		legacyFields(schema.Fields)
	}
	if msg := checkSchema(schema.Fields, ""); msg != "" {
		return http.StatusBadRequest, msg
	}
	var meta struct {
		Type   string      `json:"type"`
		Schema tableSchema `json:"schema"`
	}
	if json.Unmarshal(got, &meta) != nil {
		return 0, ""
	}
	defaults, refused := defaultChange(meta.Schema.Fields, schema.Fields) // updateTable, tablepatch.go
	if snapshotDefinitionOf(got) != nil && (!sameSchema(meta.Schema.Fields, schema.Fields) || defaults || refused != "") {
		return http.StatusForbidden, snapshotWriteMessage(projectOf(f.base), dataset, table, "a change of its schema")
	}
	if meta.Type != "" && !strings.EqualFold(meta.Type, "TABLE") {
		return 0, "" // refused by alterRun before
	}
	_, code, msg := schemaChange(name, meta.Schema.Fields, schema.Fields)
	if code == 0 && refused != "" {
		code, msg = http.StatusBadRequest, refused
	}
	return code, msg
}
