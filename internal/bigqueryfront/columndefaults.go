package bigqueryfront

import (
	"fmt"
	"strings"
)

// Column defaults (#1340).
//
// A column's defaultValueExpression is "a SQL expression to specify the
// default value for this field"; "the default value is used when the
// value for a column is not specified" in an INSERT, when the DEFAULT
// keyword is given, by the Storage Write API and tabledata.insertAll for a
// row that leaves the column out, and by a load whose data has fewer
// columns ("NULL values in the loaded data are not converted to default
// values"), https://cloud.google.com/bigquery/docs/default-values.
//
// The engine applies a column's DEFAULT (engine patches bigquery-emulator
// 0014 and googlesqlite 0027): tables.insert makes the engine's table with
// each top-level column's defaultValueExpression as its DEFAULT, CREATE
// TABLE ... DEFAULT reads back as the column's defaultValueExpression, and
// a row that does not give a column with a default gets it.
//
// The emulator's tables.patch and tables.update change only a table's
// metadata (#1010, schemaupdate.go), so a schema that sets, changes or
// clears a top-level column's defaultValueExpression makes the table again
// with the new DEFAULT, its rows copied as they are (addColumns): "Setting
// the default value for a column only affects future inserts to the
// table. It does not change any existing table data." ALTER COLUMN SET
// DEFAULT and DROP DEFAULT go through that path (altertable.go). A column
// added with a default is refused, 400, as BigQuery refuses it: "You
// cannot add a new column with a default value to an existing table.
// However, you can add the column without a default value, then change its
// default value". The wording of the refusal is CloudBurrow's
// (UNVERIFIED).
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/default-values

// addDefaultRefused is the 400 for a column added with a default.
func addDefaultRefused(column string) string {
	return fmt.Sprintf("Cannot add column %s with a default value to an existing table. Add the column without a "+
		"default value, then set its default with ALTER COLUMN SET DEFAULT.", column)
}

// nestedDefaultRefused is the 400 for a default given a field of a
// STRUCT, path, rather than the STRUCT itself: "you must set the default
// value for the entire STRUCT field. You cannot set the default value for
// a subset of the fields" (https://cloud.google.com/bigquery/docs/default-values,
// #1456). The wording is CloudBurrow's (UNVERIFIED).
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/default-values
func nestedDefaultRefused(path, name string) string {
	return fmt.Sprintf("Cannot set a default value on field %s.%s: the default value of a STRUCT is set for the "+
		"entire STRUCT field %s, not for a subset of its fields.", path, name, path)
}

// nestedDefault is the 400 for a field below the top level of fields
// that has a default, or "".
func nestedDefault(fields []field) string {
	var walk func(fs []field, path string) string
	walk = func(fs []field, path string) string {
		for _, f := range fs {
			if path != "" && f.DefaultValueExpression != "" {
				return nestedDefaultRefused(path, f.Name)
			}
			p := f.Name
			if path != "" {
				p = path + "." + f.Name
			}
			if msg := walk(f.Fields, p); msg != "" {
				return msg
			}
		}
		return ""
	}
	return walk(fields, "")
}

// defaultChange compares the top-level defaults of a table's columns, old,
// with those of a schema update's, next. It reports whether a column old
// has gets another default (or loses it), or the message to refuse a new
// column with a default with (400).
func defaultChange(old, next []field) (changed bool, refused string) {
	if msg := nestedDefault(next); msg != "" {
		return false, msg
	}
	had := byLowerName(old)
	for _, n := range next {
		o, ok := had[strings.ToLower(n.Name)]
		if !ok {
			if n.DefaultValueExpression != "" {
				return false, addDefaultRefused(n.Name)
			}
			continue
		}
		if o.DefaultValueExpression != n.DefaultValueExpression {
			changed = true
		}
	}
	return changed, ""
}
