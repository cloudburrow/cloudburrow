package bigqueryfront

import (
	"fmt"
	"strings"
)

// field is the part of a TableFieldSchema the checks read.
// https://cloud.google.com/bigquery/docs/reference/rest/v2/tables#TableFieldSchema
type field struct {
	Name   string  `json:"name"`
	Type   string  `json:"type"`
	Mode   string  `json:"mode"`
	Fields []field `json:"fields"`
}

// tableSchema is a Table's schema.
type tableSchema struct {
	Fields []field `json:"fields"`
}

// fieldTypes are the TableFieldSchema.type values the REST reference lists,
// legacy names and their GoogleSQL aliases alike.
var fieldTypes = map[string]bool{
	"STRING": true, "BYTES": true, "INTEGER": true, "INT64": true, "FLOAT": true, "FLOAT64": true,
	"NUMERIC": true, "BIGNUMERIC": true, "BOOLEAN": true, "BOOL": true, "TIMESTAMP": true,
	"DATE": true, "TIME": true, "DATETIME": true, "GEOGRAPHY": true, "RECORD": true, "STRUCT": true,
	"JSON": true, "INTERVAL": true, "RANGE": true,
}

// fieldModes are TableFieldSchema.mode's values; empty means NULLABLE.
var fieldModes = map[string]bool{"": true, "NULLABLE": true, "REQUIRED": true, "REPEATED": true}

// checkSchema returns why a schema BigQuery would refuse is invalid, or "".
// It checks each name, that no two fields at one level share a name
// ("Duplicate column names are not allowed even if the case differs",
// https://cloud.google.com/bigquery/docs/schemas), and each type and mode.
//
// The emulator checks only duplicates, and answers them 500 (measured,
// #861); a 500 is retried by the Go client until its deadline.
func checkSchema(fields []field, prefix string) string {
	seen := map[string]string{}
	for _, f := range fields {
		if msg := checkColumnName(f.Name); msg != "" {
			return msg
		}
		key := strings.ToLower(f.Name)
		if prev, dup := seen[key]; dup {
			return fmt.Sprintf("Field %s%s already exists in schema (as %s%s); field names are case-insensitive", prefix, f.Name, prefix, prev)
		}
		seen[key] = f.Name
		typ := strings.ToUpper(f.Type)
		if !fieldTypes[typ] {
			return fmt.Sprintf("Invalid field type %q for field %s%s", f.Type, prefix, f.Name)
		}
		if !fieldModes[strings.ToUpper(f.Mode)] {
			return fmt.Sprintf("Invalid field mode %q for field %s%s", f.Mode, prefix, f.Name)
		}
		isRecord := typ == "RECORD" || typ == "STRUCT"
		if isRecord && len(f.Fields) == 0 {
			return fmt.Sprintf("Field %s%s is type %s but has no schema", prefix, f.Name, typ)
		}
		if !isRecord && len(f.Fields) > 0 {
			return fmt.Sprintf("Field %s%s is type %s and cannot have subfields", prefix, f.Name, typ)
		}
		if isRecord {
			if msg := checkSchema(f.Fields, prefix+f.Name+"."); msg != "" {
				return msg
			}
		}
	}
	return ""
}

// unstorableNesting returns the location of the first value in a streamed
// row that the emulator would store so that the table can no longer be
// read, or "".
//
// Measured against the pinned image (#874, #881): a RECORD value nested in
// a RECORD, where either one or any RECORD above them is REPEATED, is
// stored by tabledata.insertAll in a shape its SQL engine then cannot
// read, and from then on every read of the table fails with 500 "failed
// to scan rows: failed to convert struct from array", which the Go client
// retries until its deadline. Any object there does it, even an empty one;
// a null there, or an empty array for a REPEATED RECORD, does not. The
// table itself is sound: the same values written by a DML INSERT or a load
// job, or a table made by DDL, read back. The fault is in the emulator's
// SQL engine, googlesqlite, which does not reshape a STRUCT below an ARRAY
// from the form the emulator binds it in; the fix is upstream, not yet
// released (goccy/googlesqlite#76).
//
// So only the values are refused, not the schema: RECORDs nested with none
// REPEATED, a REPEATED RECORD of scalars and REPEATED scalars in a RECORD
// are stored as sent.
func unstorableNesting(fields []field, obj map[string]any, prefix string, inRecord, repeatedAbove bool) string {
	for _, f := range fields {
		typ := strings.ToUpper(f.Type)
		if typ != "RECORD" && typ != "STRUCT" {
			continue
		}
		v, _ := lookup(obj, f.Name)
		if v == nil {
			continue
		}
		repeated := strings.ToUpper(f.Mode) == "REPEATED"
		var elems []map[string]any
		if repeated {
			arr, _ := v.([]any)
			for _, e := range arr {
				if m, ok := e.(map[string]any); ok {
					elems = append(elems, m)
				}
			}
		} else if m, ok := v.(map[string]any); ok {
			elems = append(elems, m)
		}
		if len(elems) == 0 {
			continue
		}
		if inRecord && (repeatedAbove || repeated) {
			return prefix + f.Name
		}
		for i, m := range elems {
			sub := prefix + f.Name + "."
			if repeated {
				sub = fmt.Sprintf("%s%s[%d].", prefix, f.Name, i)
			}
			if loc := unstorableNesting(f.Fields, m, sub, true, repeatedAbove || repeated); loc != "" {
				return loc
			}
		}
	}
	return ""
}
