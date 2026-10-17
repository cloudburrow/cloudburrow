package bigqueryfront

import (
	"strings"
)

// Logical bytes: PARTITIONS' total_logical_bytes and total_billable_bytes,
// and region TABLE_STORAGE's logical byte columns (#1458).
//
// The engine keeps no stored size (its tables.get answers numBytes 0,
// measured), but BigQuery's logical size is not a stored size: it is
// defined from the values, by the data type sizes
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-types#data_type_sizes,
// which "Data size calculation" on the pricing page links to, read
// 2026-10-16):
// INT64, FLOAT64, DATE, DATETIME, TIME and TIMESTAMP 8 bytes; NUMERIC and
// INTERVAL 16; BIGNUMERIC 32; BOOL 1; STRING 2 + its UTF-8 bytes; BYTES 2
// + its bytes; GEOGRAPHY 16 + 24 per vertex; JSON the UTF-8 bytes of its
// JSON text; RANGE 16; a STRUCT the sum of its fields; an ARRAY (a
// repeated column) the sum of its elements; "A NULL value for any data
// type is calculated as 0 logical bytes". So the front computes it from the rows
// with one SQL expression per column, summed: the documented logical
// bytes, which is faithful. Physical (compressed) bytes are not defined
// by the values and stay left out.

// logicalBytesExpr is the SQL of the logical bytes of the row's columns
// fields.
func logicalBytesExpr(fields []field) string {
	var parts []string
	for _, fl := range fields {
		parts = append(parts, fieldBytesExpr(fl, quoteName(fl.Name)))
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, " + ")
}

// fieldBytesExpr is the SQL of the logical bytes of fl's value v.
func fieldBytesExpr(fl field, v string) string {
	if strings.EqualFold(fl.Mode, "REPEATED") {
		one := fl
		one.Mode = "NULLABLE"
		return "IFNULL((SELECT SUM(" + valueBytesExpr(one, "e_") + ") FROM UNNEST(" + v + ") AS e_), 0)"
	}
	return valueBytesExpr(fl, v)
}

// valueBytesExpr is the SQL of the logical bytes of one value v of fl's
// type (not repeated): 0 when v is NULL.
func valueBytesExpr(fl field, v string) string {
	var size string
	switch strings.ToUpper(fl.Type) {
	case "INTEGER", "INT64", "FLOAT", "FLOAT64", "DATE", "DATETIME", "TIME", "TIMESTAMP":
		size = "8"
	case "NUMERIC", "INTERVAL", "RANGE":
		size = "16"
	case "BIGNUMERIC":
		size = "32"
	case "BOOLEAN", "BOOL":
		size = "1"
	case "STRING", "BYTES":
		size = "2 + BYTE_LENGTH(" + v + ")"
	case "GEOGRAPHY":
		size = "16 + 24 * ST_NUMPOINTS(" + v + ")"
	case "JSON":
		size = "BYTE_LENGTH(TO_JSON_STRING(" + v + "))"
	case "RECORD", "STRUCT":
		var parts []string
		for _, sub := range fl.Fields {
			parts = append(parts, fieldBytesExpr(sub, v+"."+quoteName(sub.Name)))
		}
		if len(parts) == 0 {
			return "0"
		}
		size = strings.Join(parts, " + ")
	default:
		size = "0"
	}
	return "IF(" + v + " IS NULL, 0, " + size + ")"
}
