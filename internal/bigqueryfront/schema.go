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

// unreadableNesting returns a message naming the first RECORD the emulator
// cannot read back, or "". Measured against the pinned image (#874): a
// RECORD nested in a RECORD, where either one or any RECORD above them is
// REPEATED, is created and takes rows, and then every read of the table
// fails with 500 "failed to scan rows: failed to convert struct from
// array", which the Go client retries until its deadline. So a RECORD in a
// REPEATED RECORD, and a REPEATED RECORD in a RECORD, cannot be stored
// here, and the schema is refused as not implemented rather than accepted
// into a table that breaks at its first such row. RECORDs nested with none
// REPEATED read back, as do REPEATED scalars in a RECORD and a REPEATED
// RECORD of scalars.
func unreadableNesting(fields []field, prefix string, inRecord, repeatedAbove bool) string {
	for _, f := range fields {
		typ := strings.ToUpper(f.Type)
		if typ != "RECORD" && typ != "STRUCT" {
			continue
		}
		repeated := strings.ToUpper(f.Mode) == "REPEATED"
		if inRecord && (repeatedAbove || repeated) {
			return fmt.Sprintf("Not implemented here: field %s%s is a RECORD nested in a RECORD with a REPEATED one among them. "+
				"BigQuery accepts it, but the emulator behind CloudBurrow cannot read such a table back once a row "+
				"holds a value there (\"failed to scan rows\"). Nest RECORDs only where none of them is REPEATED.", prefix, f.Name)
		}
		if msg := unreadableNesting(f.Fields, prefix+f.Name+".", true, repeatedAbove || repeated); msg != "" {
			return msg
		}
	}
	return ""
}
