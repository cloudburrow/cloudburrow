package bigqueryfront

import (
	"encoding/json"
	"net/http"
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
//
// STRING(L) and BYTES(L) are read the same way (#1493): measured, CREATE
// TABLE (ps STRING(10)) and CREATE TABLE AS SELECT made the column with
// no maxLength, and STRUCT<a STRING(3)> and ARRAY<STRING(4)> failed 400
// "GOOGLESQL_RET_CHECK failure ... annotations != nullptr", as a nested
// NUMERIC(P) did. The length is cut from a nested type the engine is
// sent and written to the table's resource, which tabledata.insertAll
// checks a value against (overMaxLength, rows.go).

// decimalColumn is one such column, or a STRUCT's field: precision and
// scale as the REST TableFieldSchema gives them (scale "" when the
// statement gives none), rounding its rounding_mode, or "", and fields a
// STRUCT's (or ARRAY<STRUCT>'s) fields that have one (#1409).
//
// A column's (or field's) OPTIONS(description = ...) is read here too
// (#1438): measured on the pinned image, CREATE TABLE ds.t (a INT64
// OPTIONS (description = "col")) made the table and tables.get read no
// description for a, and a STRUCT field's OPTIONS failed 400 "Nested
// column options are unsupported". BigQuery keeps it as the field's
// description (#column_option_list), a STRING literal or a constant
// expression, which the front evaluates before the statement runs
// (descExpr, evalColumnDescriptions). The column OPTIONS are cut from the
// statement the engine is sent (withoutFieldRounding) and written to the
// table's resource once it is made; an option other than description and
// rounding_mode (data_policies) is 501 and nothing is run.
type decimalColumn struct {
	name, precision, scale, rounding string
	maxLength                        string // STRING(L), BYTES(L): #1493
	description                      *string
	descExpr                         string
	dataPolicies                     []any // #1456, datapolicies.go
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

// decimalsOf is the decimalColumn of f, a TableFieldSchema fieldTypeOf
// read from sql with exprs, named name, and whether it has a parameter,
// rounding mode or description at any depth.
func decimalsOf(sql, name string, f map[string]any) (decimalColumn, bool) {
	d := decimalColumn{name: name, precision: str(f, "precision"), scale: str(f, "scale"), rounding: str(f, "roundingMode"),
		maxLength: str(f, "maxLength")}
	if s, ok := f["description"].(string); ok {
		d.description = &s
	}
	if span, ok := f[descExprKey].([2]int); ok { // #1438
		d.descExpr = sql[span[0]:span[1]]
		delete(f, descExprKey)
	}
	d.dataPolicies, _ = f["dataPolicies"].([]any)
	subs, _ := f["fields"].([]any)
	for _, sub := range subs {
		if m, ok := sub.(map[string]any); ok {
			if sd, ok := decimalsOf(sql, str(m, "name"), m); ok {
				d.fields = append(d.fields, sd)
			}
		}
	}
	return d, d.precision != "" || d.maxLength != "" || d.rounding != "" || d.description != nil || d.descExpr != "" || len(d.fields) > 0 ||
		len(d.dataPolicies) > 0
}

// readDecimalColumns reads cols, the tokens of sql's CREATE TABLE column
// list (after its "("), for its parameterized NUMERIC and BIGNUMERIC
// columns and STRUCT fields, at any depth (#1409), the rounding_mode of
// any NUMERIC or BIGNUMERIC column or field, and the description of any
// column or field (#1438). invalid is a 400's message for a rounding_mode
// BigQuery refuses, or ""; unsupported names a column option the front
// does not set (501), or "".
func readDecimalColumns(sql string, cols []token) (out []decimalColumn, invalid, unsupported string) {
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
		f, j, what := fieldTypeOf(c, 1, true)
		if what != "" {
			switch {
			case strings.HasPrefix(what, "a STRUCT field's option rounding_mode") && invalid == "":
				invalid = roundingModeInvalid
			case unsupported == "" && hasToken(c, "OPTIONS"):
				// A column whose type is not read here may still have
				// OPTIONS, which the engine would drop (#1438).
				unsupported = "the OPTIONS of column " + c[0].text + " (" + what + ")"
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
				if unsupported == "" {
					unsupported = "the OPTIONS of column " + c[0].text + " as written"
				}
				break
			}
			for _, o := range opts {
				switch o.name {
				case "description": // #1438
					isNull := len(o.val) == 1 && o.val[0].is("NULL")
					if s, ok := stringLiteral(singleToken(o.val)); ok {
						f["description"] = s
					} else if !isNull && len(o.val) > 0 {
						f[descExprKey] = [2]int{o.val[0].pos, o.val[len(o.val)-1].end}
					}
				case "rounding_mode":
					if kind != "NUMERIC" && kind != "BIGNUMERIC" {
						f["roundingMode"] = "" // refused below, by roundingModeRefused
						if s, ok := stringLiteral(singleToken(o.val)); ok {
							f["roundingMode"] = s
						}
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
				case "data_policies": // #1456, datapolicies.go
					if _, msg := dataPolicyNames(o.val); msg != "" {
						if invalid == "" {
							invalid = msg
						}
						continue
					}
					setDataPolicies(f, o)
				default:
					if unsupported == "" {
						unsupported = "the column option " + o.name + " (of column " + c[0].text + ")"
					}
				}
			}
			break
		}
		if msg := roundingModeRefused(f); msg != "" {
			if invalid == "" {
				invalid = msg
			}
			continue
		}
		if d, ok := decimalsOf(sql, c[0].text, f); ok {
			out = append(out, d)
		}
	}
	return out, invalid, unsupported
}

// hasToken reports whether t holds the word w.
func hasToken(t []token, w string) bool {
	for _, tok := range t {
		if tok.is(w) {
			return true
		}
	}
	return false
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
			if c.maxLength != "" { // #1493
				m["maxLength"] = c.maxLength
			}
			if c.rounding != "" {
				m["roundingMode"] = c.rounding
			}
			if c.description != nil { // #1438
				m["description"] = *c.description
			}
			if len(c.dataPolicies) > 0 { // #1456
				m["dataPolicies"] = c.dataPolicies
			}
			if subs, ok := m["fields"].([]any); ok && len(c.fields) > 0 {
				putDecimalFields(subs, c.fields)
			}
		}
	}
}

// withoutFieldRounding is sql, a CREATE TABLE whose column list's tokens
// are cols, with the parameters of each NUMERIC or BIGNUMERIC inside a
// STRUCT<...> or ARRAY<...>, and each column's or STRUCT field's OPTIONS
// list, cut; or "" when it has none. The engine refuses the first two
// (measured: 400 "GOOGLESQL_RET_CHECK failure ... annotations != nullptr"
// for STRUCT<a NUMERIC(2)> and ARRAY<NUMERIC(2)>, and "Nested column
// options are unsupported") and drops a column's OPTIONS (#1438); the
// front reads every option they hold (readDecimalColumns, which refuses
// the rest), writes them to the table's resource once it is made
// (putDecimalColumns), and the engine's triggers round by them (#1409).
func withoutFieldRounding(sql string, cols []token) string {
	var b strings.Builder
	last, depth := 0, 0
	for j := 0; j < len(cols); j++ {
		switch {
		case cols[j].punct("<"):
			depth++
		case cols[j].punct(">"):
			depth--
		case depth > 0 && j+1 < len(cols) && cols[j+1].punct("(") && (decimalTypeName(cols[j]) || lengthTypeName(cols[j])):
			close := closing(cols, j+1)
			if close < 0 {
				return ""
			}
			b.WriteString(sql[last:cols[j].end])
			last = cols[close].end
			j = close
		case cols[j].is("OPTIONS") && j+1 < len(cols) && cols[j+1].punct("("):
			close := closing(cols, j+1)
			if close < 0 {
				return ""
			}
			if _, ok := readOptions(cols[j : close+1]); !ok {
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

// lengthTypeName reports whether t names STRING or BYTES, whose
// parameter (a maximum length) the engine refuses inside a STRUCT<...> or
// ARRAY<...> as it does a NUMERIC's (measured: 400 "GOOGLESQL_RET_CHECK
// failure ... annotations != nullptr" for STRUCT<a STRING(3)>, #1493).
func lengthTypeName(t token) bool {
	if t.kind != tokWord {
		return false
	}
	kind := strings.ToUpper(t.text)
	if alias, ok := typeAliases[kind]; ok {
		kind = alias
	}
	return kind == "STRING" || kind == "BYTES"
}

// evalColumnDescriptions evaluates each description of cols that is a
// constant expression, at any depth, as the statement's query would (its
// default dataset and parameters), and returns why BigQuery refuses one
// (400), or "" (#1438). It runs before the statement, so a refused one
// makes nothing.
func (f front) evalColumnDescriptions(r *http.Request, q queryOptions, cols []decimalColumn) string {
	for i := range cols {
		c := &cols[i]
		if c.descExpr != "" {
			if msg := constantOptionRefused(c.descExpr); msg != "" {
				return msg
			}
			v, msg := f.evalString(r, q, "CAST(("+c.descExpr+") AS STRING)")
			if msg != "" {
				return "Could not evaluate the description of column " + c.name + ": " + msg
			}
			c.description, c.descExpr = v, ""
		}
		if msg := f.evalColumnDescriptions(r, q, c.fields); msg != "" {
			return msg
		}
	}
	return ""
}
