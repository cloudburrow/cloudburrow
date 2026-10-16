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
// The front reads them from the statement's column list (top-level
// columns, as the triggers are) and, once the statement has run, writes
// them to the table's resource with its OPTIONS (ddlWithOptions,
// setTableOptions): the emulator's tables.update makes the triggers, which
// round by the column's roundingMode, else the table's
// default_rounding_mode, else ROUND_HALF_AWAY_FROM_ZERO.

// decimalColumn is one such column: precision and scale as the REST
// TableFieldSchema gives them (scale "" when the statement gives none),
// rounding its rounding_mode, or "".
type decimalColumn struct {
	name, precision, scale, rounding string
}

// readDecimalColumns reads cols, a CREATE TABLE's column list's tokens
// (after its "("), for its parameterized NUMERIC and BIGNUMERIC columns
// and the rounding_mode of any NUMERIC or BIGNUMERIC column. invalid is a
// 400's message for a rounding_mode BigQuery refuses, or "".
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
		kind := strings.ToUpper(c[1].text)
		if alias, ok := typeAliases[kind]; ok {
			kind = alias
		}
		if kind != "NUMERIC" && kind != "BIGNUMERIC" {
			continue
		}
		d := decimalColumn{name: c[0].text}
		j := 2
		if j < len(c) && c[j].punct("(") {
			close := closing(c, j)
			if close < 0 {
				continue
			}
			var nums []string
			for _, p := range c[j+1 : close] {
				if !p.punct(",") {
					nums = append(nums, p.text)
				}
			}
			if len(nums) >= 1 && len(nums) <= 2 {
				d.precision = nums[0]
				if len(nums) == 2 {
					d.scale = nums[1]
				}
			}
			j = close + 1
		}
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
				if o.name != "rounding_mode" {
					continue
				}
				s, ok := stringLiteral(singleToken(o.val))
				if !ok || s != "ROUND_HALF_AWAY_FROM_ZERO" && s != "ROUND_HALF_EVEN" {
					if invalid == "" {
						invalid = "Invalid value for rounding_mode: expected 'ROUND_HALF_AWAY_FROM_ZERO' or 'ROUND_HALF_EVEN'"
					}
					continue
				}
				d.rounding = s
			}
			break
		}
		if d.precision != "" || d.rounding != "" {
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
		}
	}
	if b, err := json.Marshal(schema); err == nil {
		meta["schema"] = b
	}
}
