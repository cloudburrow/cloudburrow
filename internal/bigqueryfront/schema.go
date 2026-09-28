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
