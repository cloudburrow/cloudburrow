package bigqueryfront

import (
	"fmt"
	"strings"
)

// Rows of one schema read as another's (#1036, #1083).
//
// A schema update that adds a field to a RECORD (schemaupdate.go) and a
// query job's WRITE_TRUNCATE_DATA into a table whose columns are not the
// result's (querywrite.go) both copy rows into columns that are not the
// ones they were read from. The engine assigns a STRUCT to a RECORD column
// by position, not by name, and refuses one of other fields, so each
// column is sent as an expression that builds the RECORD of the target's
// fields, by name, in its order: a field the source has is read from it,
// one it does not is NULL (a REPEATED one the empty array), and the
// RECORD is NULL where the source's is. A REPEATED RECORD is the ARRAY of
// its elements so rebuilt, in their order. Measured against the pinned
// image through the front with the official Go client: INSERT ... SELECT
// id, IF(r IS NULL, NULL, STRUCT(r.x AS x, IF(r.n IS NULL, NULL, STRUCT(r.n.z
// AS z, CAST(NULL AS BYTES) AS w)) AS n, CAST(NULL AS INT64) AS y)),
// ARRAY(SELECT STRUCT(e.x AS x, CAST(NULL AS FLOAT64) AS y) FROM UNNEST(rr)
// AS e WITH OFFSET AS o ORDER BY o) read back every row as it was, with the
// new fields NULL: a NULL RECORD NULL, a RECORD of NULLs a RECORD, and a
// REPEATED RECORD's elements in order.

// convProblem is why rows of one schema are not read as another's: a
// difference BigQuery refuses (invalid), or one CloudBurrow does not map.
type convProblem struct {
	invalid bool
	msg     string
}

// convertColumns returns, for each column of dst, the expression that
// reads it from a row of the columns src, each read as prefix and its name
// (prefix "" for a table's columns), and whether every expression is the
// column itself; or why it cannot. A column of dst that src does not have
// is NULL; one of src that dst does not have is a problem.
func convertColumns(src, dst []field, prefix string) (exprs []string, same bool, p *convProblem) {
	return convertFields(src, dst, prefix, "", 0)
}

func convertFields(src, dst []field, prefix, path string, depth int) ([]string, bool, *convProblem) {
	for _, s := range src {
		if _, ok := fieldNamed(dst, s.Name); !ok {
			return nil, false, &convProblem{invalid: true, msg: fmt.Sprintf("the field %s%s is not in the table's schema",
				path, s.Name)}
		}
	}
	same := len(src) == len(dst)
	exprs := make([]string, len(dst))
	for i, d := range dst {
		s, ok := fieldNamed(src, d.Name)
		if !ok {
			if modeOf(d) == "REQUIRED" {
				return nil, false, &convProblem{invalid: true, msg: fmt.Sprintf("the REQUIRED field %s%s is missing", path,
					d.Name)}
			}
			e, why := nullOf(d)
			if why != "" {
				return nil, false, &convProblem{msg: fmt.Sprintf("the field %s%s is missing, and %s", path, d.Name, why)}
			}
			exprs[i], same = e, false
			continue
		}
		e, sameValue, p := convertValue(s, d, prefix+quoteName(s.Name), path+d.Name, depth)
		if p != nil {
			return nil, false, p
		}
		if i >= len(src) || !strings.EqualFold(src[i].Name, d.Name) || !sameValue {
			same = false
		}
		exprs[i] = e
	}
	return exprs, same, nil
}

// convertValue returns the expression that reads expr, a value of field
// src, as a value of field dst, and whether it is expr itself.
func convertValue(src, dst field, expr, path string, depth int) (string, bool, *convProblem) {
	sm, dm := modeOf(src), modeOf(dst)
	if (sm == "REPEATED") != (dm == "REPEATED") {
		return "", false, &convProblem{msg: fmt.Sprintf("the field %s is %s in the result and %s in the table", path, sm, dm)}
	}
	st, dt := canonicalType(src.Type), canonicalType(dst.Type)
	if st != dt {
		return "", false, &convProblem{msg: fmt.Sprintf("the field %s is %s in the result and %s in the table", path,
			strings.ToUpper(src.Type), strings.ToUpper(dst.Type))}
	}
	if dt != "RECORD" {
		return expr, true, nil
	}
	if dm == "REPEATED" {
		e := fmt.Sprintf("_cloudburrow_e%d", depth)
		inner, same, p := convertRecord(src.Fields, dst.Fields, e, path, depth+1)
		if p != nil || same {
			return expr, same, p
		}
		o := fmt.Sprintf("_cloudburrow_o%d", depth)
		return "ARRAY(SELECT " + inner + " FROM UNNEST(" + expr + ") AS " + e + " WITH OFFSET AS " + o + " ORDER BY " + o + ")",
			false, nil
	}
	inner, same, p := convertRecord(src.Fields, dst.Fields, expr, path, depth+1)
	if p != nil || same {
		return expr, same, p
	}
	return "IF(" + expr + " IS NULL, NULL, " + inner + ")", false, nil
}

// convertRecord returns the STRUCT of dst's fields read from expr, a
// RECORD of src's, and whether it is expr itself.
func convertRecord(src, dst []field, expr, path string, depth int) (string, bool, *convProblem) {
	exprs, same, p := convertFields(src, dst, expr+".", path+".", depth)
	if p != nil || same {
		return expr, same, p
	}
	parts := make([]string, len(dst))
	for i, d := range dst {
		parts[i] = exprs[i] + " AS " + quoteName(d.Name)
	}
	return "STRUCT(" + strings.Join(parts, ", ") + ")", false, nil
}

// nullOf returns a NULL of field f's type (the empty array for a REPEATED
// one), or why the front cannot write one.
func nullOf(f field) (string, string) {
	t, why := fieldTypeSQL(f, false)
	if why != "" {
		return "", why
	}
	if modeOf(f) == "REPEATED" {
		return t + "[]", ""
	}
	return "CAST(NULL AS " + t + ")", ""
}

// fieldTypeSQL returns field f's GoogleSQL type, an ARRAY for a REPEATED
// field unless element, or why the front does not write it.
func fieldTypeSQL(f field, element bool) (string, string) {
	var t string
	switch u := canonicalType(f.Type); {
	case u == "RECORD":
		if len(f.Fields) == 0 {
			return "", "a RECORD with no fields has no type"
		}
		parts := make([]string, len(f.Fields))
		for i, sub := range f.Fields {
			s, why := fieldTypeSQL(sub, false)
			if why != "" {
				return "", why
			}
			parts[i] = quoteName(sub.Name) + " " + s
		}
		t = "STRUCT<" + strings.Join(parts, ", ") + ">"
	case ddlTypes[u] != "":
		t = ddlTypes[u]
	default:
		return "", "CloudBurrow does not write a NULL of type " + strings.ToUpper(f.Type)
	}
	if !element && modeOf(f) == "REPEATED" {
		t = "ARRAY<" + t + ">"
	}
	return t, ""
}

// nullViolations returns a condition true of a row of the columns fields
// (each read as prefix and its name) that has NULL where a REQUIRED field
// is, at any depth, or "" when there is no REQUIRED field.
func nullViolations(fields []field, prefix string, depth int) string {
	var parts []string
	for _, f := range fields {
		if v := valueViolation(f, prefix+quoteName(f.Name), depth); v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, " OR ")
}

func valueViolation(f field, expr string, depth int) string {
	if modeOf(f) == "REPEATED" {
		if canonicalType(f.Type) != "RECORD" {
			return "" // an ARRAY holds no NULL
		}
		e := fmt.Sprintf("_cloudburrow_v%d", depth)
		inner := nullViolations(f.Fields, e+".", depth+1)
		if inner == "" {
			return ""
		}
		return "EXISTS(SELECT 1 FROM UNNEST(" + expr + ") AS " + e + " WHERE " + inner + ")"
	}
	var sub string
	if canonicalType(f.Type) == "RECORD" {
		sub = nullViolations(f.Fields, expr+".", depth+1)
	}
	switch {
	case modeOf(f) == "REQUIRED" && sub != "":
		return "(" + expr + " IS NULL OR " + sub + ")"
	case modeOf(f) == "REQUIRED":
		return expr + " IS NULL"
	case sub != "":
		return "(" + expr + " IS NOT NULL AND (" + sub + "))"
	}
	return ""
}
