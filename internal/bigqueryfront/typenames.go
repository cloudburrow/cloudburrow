package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"strings"
)

// GoogleSQL type names in a schema (#1034).
//
// BigQuery's REST reference lists a field's type by its legacy name, with
// the GoogleSQL name as an alias it accepts: "INTEGER (or INT64)", "FLOAT
// (or FLOAT64)", "BOOLEAN (or BOOL)", "RECORD (or STRUCT)"
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/tables#TableFieldSchema.FIELDS.type).
// The official Go client says the same ("The API will accept alias names
// for the types based on the Standard SQL type names", schema.go's
// fieldAliases, which adds DECIMAL for NUMERIC and BIGDECIMAL for
// BIGNUMERIC) and reads only the legacy names back: its Table.Read fails
// "unrecognized type: INT64" on any other (value.go, convertBasicType).
//
// The emulator keeps the name it was sent. Measured against the pinned
// image through the front: tables.insert of fields typed INT64 and BOOL
// read back INT64 and BOOL from tables.get, and Table.Read of the table
// failed "unrecognized type: INT64"; a table made by `CREATE TABLE ... (n
// INT64, b BOOL, f FLOAT64, r STRUCT<x INT64>)` read back INTEGER,
// BOOLEAN, FLOAT and RECORD, and so did a query's result schema.
//
// So the front sends each schema a client gives, in tables.insert,
// tables.update, tables.patch and a load job, with every alias written as
// its legacy name (legacyTypeNames). The engine's types are the same for
// both names (INTEGER is INT64, BOOLEAN BOOL, RECORD STRUCT: floattype.go),
// except FLOAT, which floattype.go then makes FLOAT64 in the engine as it
// does for a client that sent FLOAT (#1000). Whether BigQuery's own
// tables.get of a field made as INT64 reads INTEGER is UNVERIFIED here:
// it is inferred from the official client, which could not read it
// otherwise, and from the emulator's own DDL, which reads so.

// legacyTypes maps each alias TableFieldSchema.type accepts to the name
// BigQuery reports.
var legacyTypes = map[string]string{
	"INT64":      "INTEGER",
	"FLOAT64":    "FLOAT",
	"BOOL":       "BOOLEAN",
	"STRUCT":     "RECORD",
	"DECIMAL":    "NUMERIC",
	"BIGDECIMAL": "BIGNUMERIC",
}

// legacyTypeName returns the legacy name of typ, a field type, or "" when
// it is not an alias.
func legacyTypeName(typ string) string {
	return legacyTypes[strings.ToUpper(typ)]
}

// legacySchema writes each alias in a TableSchema, at any depth, as its
// legacy name, and reports whether there was one.
func legacySchema(schema map[string]any) bool {
	fields, _ := schema["fields"].([]any)
	changed := false
	for _, fl := range fields {
		m, ok := fl.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); legacyTypeName(t) != "" {
			m["type"] = legacyTypeName(t)
			changed = true
		}
		if legacySchema(m) {
			changed = true
		}
	}
	return changed
}

// legacyFields is legacySchema for the fields the checks read.
func legacyFields(fields []field) {
	for i := range fields {
		if n := legacyTypeName(fields[i].Type); n != "" {
			fields[i].Type = n
		}
		legacyFields(fields[i].Fields)
	}
}

// legacyTableTypes sends r, whose body b is a Table resource
// (tables.insert, tables.update, tables.patch), with its schema's aliases
// as legacy names, and returns the body now sent; schema, the checks'
// reading of it, is changed to match. A body with no alias is left as it
// is.
func legacyTableTypes(r *http.Request, b []byte, schema *tableSchema) []byte {
	if schema == nil || !hasAlias(schema.Fields) {
		return b
	}
	table, ok := decodeMap(b)
	if !ok {
		return b
	}
	s, _ := table["schema"].(map[string]any)
	if s == nil || !legacySchema(s) {
		return b
	}
	out, err := json.Marshal(table)
	if err != nil {
		return b
	}
	setBody(r, out)
	legacyFields(schema.Fields)
	return out
}

// legacyLoadTypes sends a load job, r, with its schema's aliases as legacy
// names (editJob), and changes schema, the checks' reading of it, to
// match.
func legacyLoadTypes(r *http.Request, schema *tableSchema) {
	if schema == nil || !hasAlias(schema.Fields) {
		return
	}
	if editJob(r, func(job map[string]any) bool {
		conf, _ := job["configuration"].(map[string]any)
		load, _ := conf["load"].(map[string]any)
		s, _ := load["schema"].(map[string]any)
		return s != nil && legacySchema(s)
	}) {
		legacyFields(schema.Fields)
	}
}

// hasAlias reports whether fields have a type that is an alias.
func hasAlias(fields []field) bool {
	for _, fl := range fields {
		if legacyTypeName(fl.Type) != "" || hasAlias(fl.Fields) {
			return true
		}
	}
	return false
}
