package bigqueryfront

import (
	"encoding/json"
	"strings"
)

// A CREATE TABLE column list's parameterized NUMERIC and BIGNUMERIC
// columns (#1401).
//
// Measured against the pinned image (2026-10-16): a table made by CREATE
// TABLE (x NUMERIC(5, 2) OPTIONS (rounding_mode = ...)) is made, but the
// emulator's table resource has the column as a plain NUMERIC, with no
// precision, scale or roundingMode, so the engine's rounding triggers
// (bigquery-emulator patch 0020), which read the resource, are not made.
// BigQuery keeps them
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#column_name_and_column_schema,
// #column_option_list).
//
// The front reads them from the statement's column list (a column, and a
// STRUCT's field at any depth, #1409: the triggers round both) and, once the statement has run, writes
// them to the table's resource with its OPTIONS (ddlWithOptions,
// setTableOptions): the emulator's tables.update makes the triggers, which
// round by the column's roundingMode, else the table's
// default_rounding_mode, else ROUND_HALF_AWAY_FROM_ZERO.

// decimalColumn is one such column, or a STRUCT's field: precision and
// scale as the REST TableFieldSchema gives them (scale "" when the
// statement gives none), rounding its rounding_mode, or "", and fields a
// STRUCT's (or ARRAY<STRUCT>'s) fields that have one (#1409).
type decimalColumn struct {
	name, precision, scale, rounding string
	fields                           []decimalColumn
}

// roundingModeInvalid is BigQuery's 400 for a rounding_mode it does not
// take.
const roundingModeInvalid = "Invalid value for rounding_mode: expected 'ROUND_HALF_AWAY_FROM_ZERO' or 'ROUND_HALF_EVEN'"

// roundingModeRefused is the 400 of f, a TableFieldSchema fieldType read,
// for a roundingMode BigQuery refuses, at any depth: one not of its two
// modes, or one on a field that is not NUMERIC or BIGNUMERIC ("You cannot
// set the rounding_mode option for a column that is not a NUMERIC or
// BIGNUMERIC type", https://cloud.google.com/bigquery/docs/schemas#rounding_mode),
// or "".
func roundingModeRefused(f map[string]any) string {
	if m, ok := f["roundingMode"].(string); ok {
		if m != "ROUND_HALF_AWAY_FROM_ZERO" && m != "ROUND_HALF_EVEN" {
			return roundingModeInvalid
		}
		if t := str(f, "type"); t != "NUMERIC" && t != "BIGNUMERIC" {
			name := str(f, "name")
			return "You cannot set the rounding_mode option for a column that is not a NUMERIC or BIGNUMERIC type: " + name + " is " + t + "."
		}
	}
	subs, _ := f["fields"].([]any)
	for _, sub := range subs {
		if m, ok := sub.(map[string]any); ok {
			if msg := roundingModeRefused(m); msg != "" {
				return msg
			}
		}
	}
	return ""
}

// decimalsOf is the decimalColumn of f, a TableFieldSchema fieldType read,
// named name, and whether it has a parameter or rounding mode at any depth.
func decimalsOf(name string, f map[string]any) (decimalColumn, bool) {
	d := decimalColumn{name: name, precision: str(f, "precision"), scale: str(f, "scale"), rounding: str(f, "roundingMode")}
	subs, _ := f["fields"].([]any)
	for _, sub := range subs {
		if m, ok := sub.(map[string]any); ok {
			if sd, ok := decimalsOf(str(m, "name"), m); ok {
				d.fields = append(d.fields, sd)
			}
		}
	}
	return d, d.precision != "" || d.rounding != "" || len(d.fields) > 0
}

// readDecimalColumns reads cols, a CREATE TABLE's column list's tokens
// (after its "("), for its parameterized NUMERIC and BIGNUMERIC columns
// and STRUCT fields, at any depth (#1409), and the rounding_mode of any
// NUMERIC or BIGNUMERIC column or field. invalid is a 400's message for a
// rounding_mode BigQuery refuses, or "".
func readDecimalColumns(cols []token) (out []decimalColumn, invalid string) {
	for i := 0; i < len(cols); {
		end := skipTo(cols, i, ",")
		if end > len(cols) {
			end = len(cols)
		}
		c := cols[i:end]
		i = end + 1
		if len(c) < 2 || !isName(c[0]) || c[1].kind != tokWord {
			continue
		}
		f, j, what := fieldType(c, 1)
		if what != "" {
			if strings.Contains(what, "rounding_mode") && invalid == "" {
				invalid = roundingModeInvalid
			}
			continue
		}
		kind := str(f, "type")
		for ; j < len(c); j++ {
			if !c[j].is("OPTIONS") || j+1 >= len(c) || !c[j+1].punct("(") {
				continue
			}
			close := closing(c, j+1)
			if close < 0 {
				break
			}
			opts, ok := readOptions(c[j : close+1])
			if !ok {
				break
			}
			for _, o := range opts {
				if o.name != "rounding_mode" || kind != "NUMERIC" && kind != "BIGNUMERIC" {
					continue
				}
				s, ok := stringLiteral(singleToken(o.val))
				if !ok || s != "ROUND_HALF_AWAY_FROM_ZERO" && s != "ROUND_HALF_EVEN" {
					if invalid == "" {
						invalid = roundingModeInvalid
					}
					continue
				}
				f["roundingMode"] = s
			}
			break
		}
		if msg := roundingModeRefused(f); msg != "" {
			if invalid == "" {
				invalid = msg
			}
			continue
		}
		if d, ok := decimalsOf(c[0].text, f); ok {
			out = append(out, d)
		}
	}
	return out, invalid
}

// putDecimalColumns writes cols to meta's schema, a table resource's.
func putDecimalColumns(meta map[string]json.RawMessage, cols []decimalColumn) {
	if len(cols) == 0 {
		return
	}
	var schema map[string]any
	if json.Unmarshal(meta["schema"], &schema) != nil || schema == nil {
		return
	}
	fields, _ := schema["fields"].([]any)
	putDecimalFields(fields, cols)
	if b, err := json.Marshal(schema); err == nil {
		meta["schema"] = b
	}
}

// putDecimalFields writes cols to fields, a schema's or a RECORD's.
func putDecimalFields(fields []any, cols []decimalColumn) {
	for _, c := range cols {
		for _, fl := range fields {
			m, ok := fl.(map[string]any)
			if !ok || !strings.EqualFold(str(m, "name"), c.name) {
				continue
			}
			if c.precision != "" {
				m["precision"] = c.precision
				if c.scale != "" {
					m["scale"] = c.scale
				}
			}
			if c.rounding != "" {
				m["roundingMode"] = c.rounding
			}
			if subs, ok := m["fields"].([]any); ok && len(c.fields) > 0 {
				putDecimalFields(subs, c.fields)
			}
		}
	}
}

// withoutFieldRounding is sql, a CREATE TABLE whose column list's tokens
// are cols, with the parameters of each NUMERIC or BIGNUMERIC inside a
// STRUCT<...> or ARRAY<...>, and each OPTIONS list of a STRUCT's field that
// holds rounding_mode alone, cut; or "" when it has none. The engine
// refuses both (measured: 400 "GOOGLESQL_RET_CHECK failure ...
// annotations != nullptr" for STRUCT<a NUMERIC(2)> and ARRAY<NUMERIC(2)>,
// and "Nested column options are unsupported"); the front writes them to
// the table's resource once it is made (putDecimalColumns), and the
// engine's triggers round by them (#1409).
func withoutFieldRounding(sql string, cols []token) string {
	var b strings.Builder
	last, depth := 0, 0
	for j := 0; j < len(cols); j++ {
		switch {
		case cols[j].punct("<"):
			depth++
		case cols[j].punct(">"):
			depth--
		case depth > 0 && j+1 < len(cols) && cols[j+1].punct("(") && decimalTypeName(cols[j]):
			close := closing(cols, j+1)
			if close < 0 {
				return ""
			}
			b.WriteString(sql[last:cols[j].end])
			last = cols[close].end
			j = close
		case depth > 0 && cols[j].is("OPTIONS") && j+1 < len(cols) && cols[j+1].punct("("):
			close := closing(cols, j+1)
			if close < 0 {
				return ""
			}
			opts, ok := readOptions(cols[j : close+1])
			if !ok || len(opts) == 0 {
				continue
			}
			only := true
			for _, o := range opts {
				only = only && o.name == "rounding_mode"
			}
			if !only {
				continue
			}
			b.WriteString(sql[last:cols[j].pos])
			last = cols[close].end
			j = close
		}
	}
	if last == 0 {
		return ""
	}
	b.WriteString(sql[last:])
	return b.String()
}

// decimalTypeName reports whether t names NUMERIC or BIGNUMERIC, or an
// alias of one.
func decimalTypeName(t token) bool {
	if t.kind != tokWord {
		return false
	}
	kind := strings.ToUpper(t.text)
	if alias, ok := typeAliases[kind]; ok {
		kind = alias
	}
	return kind == "NUMERIC" || kind == "BIGNUMERIC"
}
