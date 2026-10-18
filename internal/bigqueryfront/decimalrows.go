package bigqueryfront

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// A row streamed into a parameterized NUMERIC(P, S) or BIGNUMERIC(P, S)
// column (#1401): BigQuery rounds the value to S digits by the column's
// roundingMode, else the table's defaultRoundingMode, else
// ROUND_HALF_AWAY_FROM_ZERO, and refuses one with more than P digits once
// rounded
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-types#parameterized_decimal_type).
// The engine rounds (its triggers, bigquery-emulator patch 0020), but its
// refusal fails the whole tabledata.insertAll with a 500, which a client
// retries. The front checks each row first, and a row whose value is out
// of range is an invalid row of the response's insertErrors, as any
// invalid row is ("reason": "invalid", the column its location; the
// message, which BigQuery does not document, is the engine's: UNVERIFIED).
//
// A table streamed through a scratch table (streamingbuffer.go, #1370)
// makes it with no precision or scale (scratchSchema), so the rows are
// rounded once, by the table's own triggers, with its own modes.
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/data-types

// decimalOverflow is the errors of the parameterized NUMERIC and BIGNUMERIC
// values of obj, a row checkRow passed, that are out of range once
// rounded; tableMode is the table's defaultRoundingMode. A STRUCT's fields
// and an ARRAY's elements, at any depth, are checked as a top-level
// column's value is (#1409): the engine rounds them too (bigquery-emulator
// patch 0021). A nested value's location is its field's path, a.b
// (UNVERIFIED).
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/data-types
func decimalOverflow(fields []field, obj map[string]any, tableMode string) []rowError {
	var errs []rowError
	for k, v := range obj {
		if f, ok := fieldNamed(fields, k); ok {
			errs = append(errs, decimalOverflowIn(f, v, f.Name, tableMode)...)
		}
	}
	return errs
}

// decimalOverflowIn is decimalOverflow of v, a value of f, at path.
func decimalOverflowIn(f field, v any, path, tableMode string) []rowError {
	if v == nil {
		return nil
	}
	if strings.EqualFold(f.Mode, "REPEATED") {
		list, _ := v.([]any)
		elem := f
		elem.Mode = "NULLABLE"
		var errs []rowError
		for _, e := range list {
			if errs = append(errs, decimalOverflowIn(elem, e, path, tableMode)...); len(errs) > 0 {
				return errs
			}
		}
		return nil
	}
	typ := strings.ToUpper(f.Type)
	if alias, ok := typeAliases[typ]; ok {
		typ = alias
	}
	if typ == "RECORD" || typ == "STRUCT" {
		m, _ := v.(map[string]any)
		var errs []rowError
		for k, sv := range m {
			if sub, ok := fieldNamed(f.Fields, k); ok {
				errs = append(errs, decimalOverflowIn(sub, sv, path+"."+sub.Name, tableMode)...)
			}
		}
		return errs
	}
	if f.Precision == "" || typ != "NUMERIC" && typ != "BIGNUMERIC" {
		return nil
	}
	precision, err := strconv.ParseInt(string(f.Precision), 10, 64)
	if err != nil {
		return nil
	}
	var scale int64
	if f.Scale != "" {
		if scale, err = strconv.ParseInt(string(f.Scale), 10, 64); err != nil {
			return nil
		}
	}
	text := strings.TrimSpace(fmt.Sprint(v))
	r, ok := new(big.Rat).SetString(text)
	if !ok {
		return nil
	}
	mode := f.RoundingMode
	if mode == "" || mode == "ROUNDING_MODE_UNSPECIFIED" {
		mode = tableMode
	}
	if roundedFits(roundDecimal(r, scale, mode), precision, scale) {
		return nil
	}
	return []rowError{{Reason: "invalid", Location: path,
		Message: fmt.Sprintf("OUT_OF_RANGE: Value %s is out of range for column %s of type %s(%d, %d)",
			text, path, typ, precision, scale)}}
}

// roundDecimal rounds r to scale digits after the point: halves to even
// under ROUND_HALF_EVEN, else away from zero.
func roundDecimal(r *big.Rat, scale int64, mode string) *big.Rat {
	p := new(big.Int).Exp(big.NewInt(10), big.NewInt(scale), nil)
	m := new(big.Rat).Mul(r, new(big.Rat).SetInt(p))
	abs := new(big.Rat).Abs(m)
	q, rem := new(big.Int).QuoRem(abs.Num(), abs.Denom(), new(big.Int))
	switch c := new(big.Int).Mul(rem, big.NewInt(2)).Cmp(abs.Denom()); {
	case c > 0, c == 0 && (mode != "ROUND_HALF_EVEN" || q.Bit(0) == 1):
		q.Add(q, big.NewInt(1))
	}
	if m.Sign() < 0 {
		q.Neg(q)
	}
	return new(big.Rat).SetFrac(q, p)
}

// roundedFits reports whether r, already rounded to scale digits, has at
// most precision digits.
func roundedFits(r *big.Rat, precision, scale int64) bool {
	p := new(big.Int).Exp(big.NewInt(10), big.NewInt(scale), nil)
	n := new(big.Rat).Mul(new(big.Rat).Abs(r), new(big.Rat).SetInt(p))
	limit := new(big.Int).Exp(big.NewInt(10), big.NewInt(precision), nil)
	return n.Num().Cmp(new(big.Int).Mul(limit, n.Denom())) < 0
}

// scratchSchema is fields with no precision, scale or roundingMode on a
// NUMERIC or BIGNUMERIC field, at any depth (#1409), for a scratch table
// the rows pass through on their way to the table, which rounds them
// (above).
func scratchSchema(fields []field) tableSchema {
	return tableSchema{Fields: unparameterized(fields)}
}

func unparameterized(fields []field) []field {
	out := make([]field, len(fields))
	for i, f := range fields {
		typ := strings.ToUpper(f.Type)
		if alias, ok := typeAliases[typ]; ok {
			typ = alias
		}
		switch typ {
		case "NUMERIC", "BIGNUMERIC":
			f.Precision, f.Scale, f.RoundingMode = "", "", ""
		case "RECORD", "STRUCT":
			f.Fields = unparameterized(f.Fields)
		}
		out[i] = f
	}
	return out
}
