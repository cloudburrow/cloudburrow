package bigqueryfront

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Values the emulator stores as other data (#1065, #1066).
//
// BYTES. BigQuery reads a BYTES value in a JSON load, a CSV load and a
// streamed row (tabledata.insertAll) as base64: "BYTES ... must be
// base64-encoded" (https://cloud.google.com/bigquery/docs/loading-data-cloud-storage-json,
// https://cloud.google.com/bigquery/docs/reference/rest/v2/tabledata/insertAll).
// The emulator stores the base64 text itself: its load and insertAll bind
// each value to its engine as the Go value it decoded
// (internal/contentdata/repository.go, AddTableData), and the engine casts
// a string to BYTES as the string's bytes. Measured through the front with
// the official Go client, on the pinned image: "/w==" loaded by
// NEWLINE_DELIMITED_JSON and by CSV, and []byte{0xff} put with
// Inserter.Put, each read back TO_HEX 2f773d3d where BigQuery reads ff. A
// string is carried to the engine exactly (measured: "a\u0000b", "☃" and
// "\r\n" in a JSON load read back 610062, e29883 and 0d0a, at the top level,
// in a RECORD and in a REPEATED column), so the front decodes each BYTES
// value and sends its bytes as that string, which the engine stores as
// those bytes. That is exact only for bytes that are UTF-8 text: the
// emulator reads the data as JSON (or CSV) text, whose strings are UTF-8,
// so no string it reads carries other bytes (a byte 0xff reads back EF BF
// BD, as its Parquet load showed, #988). A value whose bytes are not
// UTF-8 is 501, and nothing is written; in a CSV load so is one with a
// carriage return, which the emulator's CSV reader drops before a newline
// (encoding/csv). A value that is not base64 is refused as BigQuery
// refuses it.
//
// NaN. The emulator's engine keeps FLOAT64 values as SQLite REALs, and
// SQLite stores a NaN as NULL, so every NaN written reads back NULL
// (measured: a JSON load's "NaN", a CSV load's NaN and nan, a query
// parameter NaN, and IEEE_DIVIDE(0, 0) in an INSERT, all read back NULL,
// IS_NAN NULL). BigQuery keeps it: FLOAT64 "includes NaN"
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-types#floating_point_types).
// The front refuses (501) each write whose values it reads that carries a
// NaN into a FLOAT64 column: a JSON or CSV load's value, a streamed row's,
// and a query parameter's (whose NaN the engine computes as NULL whatever
// the statement is). A NaN a statement computes (IEEE_DIVIDE(0, 0),
// CAST('nan' AS FLOAT64)) is the engine's, which the front does not see:
// that is the patched emulator's to fix (#1061). ±Infinity is kept
// (measured: read back ±Inf through every path).

// valueProblem is why a value cannot be written as BigQuery writes it.
type valueProblem struct {
	code   int
	reason string
	msg    string
}

func (p *valueProblem) loadError() *loadDataError {
	return &loadDataError{code: p.code, reason: p.reason, msg: p.msg}
}

// storedValues reports whether fields, at any depth, have a column whose
// values the front must read: BYTES or FLOAT64.
func storedValues(fields []field) bool {
	for _, f := range fields {
		switch strings.ToUpper(f.Type) {
		case "BYTES", "FLOAT", "FLOAT64":
			return true
		case "RECORD", "STRUCT":
			if storedValues(f.Fields) {
				return true
			}
		}
	}
	return false
}

// isNaNText reports whether s is a NaN as the engine reads a FLOAT64 from
// text (strconv.ParseFloat: "NaN" in any case).
func isNaNText(s string) bool {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return err == nil && math.IsNaN(v)
}

// nanProblem is the 501 for a NaN written to loc, in what (a load, a
// streamed row, a query parameter).
func nanProblem(what, loc string) *valueProblem {
	return &valueProblem{code: http.StatusNotImplemented, reason: "notImplemented", msg: fmt.Sprintf(
		"Not implemented here: a NaN in %s (%s). BigQuery keeps a FLOAT64 NaN, but the emulator's engine behind "+
			"CloudBurrow stores it as NULL (measured, #1066). Nothing was written.", what, loc)}
}

// decodeBytes returns the bytes of a BYTES value v, base64 as BigQuery
// reads it, as the string that carries them to the engine; or why it
// cannot. csv is whether the string goes into a CSV load.
func decodeBytes(v, what, loc string, csv bool) (string, *valueProblem) {
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		if b, err = base64.URLEncoding.DecodeString(v); err != nil {
			return "", &valueProblem{code: http.StatusBadRequest, reason: "invalid", msg: fmt.Sprintf(
				"Error while reading data, error message: Could not decode base64 string to bytes. Field: %s; Value: %s",
				loc, v)}
		}
	}
	why := ""
	switch {
	case !utf8.Valid(b):
		why = "whose bytes are not UTF-8 text"
	case csv && strings.ContainsRune(string(b), '\r'):
		why = "with a carriage return, which the emulator's CSV reader drops before a newline"
	}
	if why != "" {
		return "", &valueProblem{code: http.StatusNotImplemented, reason: "notImplemented", msg: fmt.Sprintf(
			"Not implemented here: a BYTES value in %s (%s) %s. The emulator behind CloudBurrow stores a BYTES "+
				"value it reads as text, so CloudBurrow can hand it only bytes that are UTF-8 text (#1065). Nothing "+
				"was written.", what, loc, why)}
	}
	return string(b), nil
}

// fixValues changes each BYTES value in obj, a row or RECORD value of
// fields read from JSON, to the string of its bytes (decodeBytes), and
// returns whether it changed one, or the first value that cannot be
// written: a BYTES value decodeBytes refuses, or a NaN in a FLOAT64
// column. Fields are matched without regard to case, as the emulator
// matches them; a value of a field fields lacks, or of the wrong JSON
// kind, is left for the emulator (or checkRow) to answer. what names the
// write, for messages; prefix is obj's location.
func fixValues(fields []field, obj map[string]any, what, prefix string) (bool, *valueProblem) {
	changed := false
	for k, v := range obj {
		f, ok := fieldNamed(fields, k)
		if !ok || v == nil {
			continue
		}
		loc := prefix + f.Name
		if strings.EqualFold(f.Mode, "REPEATED") {
			arr, ok := v.([]any)
			if !ok {
				continue
			}
			for i, e := range arr {
				out, c, p := fixValue(f, e, what, fmt.Sprintf("%s[%d]", loc, i))
				if p != nil {
					return false, p
				}
				if c {
					arr[i], changed = out, true
				}
			}
			continue
		}
		out, c, p := fixValue(f, v, what, loc)
		if p != nil {
			return false, p
		}
		if c {
			obj[k], changed = out, true
		}
	}
	return changed, nil
}

// fixValue is fixValues for one value (not an array) of f.
func fixValue(f field, v any, what, loc string) (any, bool, *valueProblem) {
	switch strings.ToUpper(f.Type) {
	case "RECORD", "STRUCT":
		if m, ok := v.(map[string]any); ok {
			c, p := fixValues(f.Fields, m, what, loc+".")
			return m, c, p
		}
	case "BYTES":
		if s, ok := v.(string); ok {
			out, p := decodeBytes(s, what, loc, false)
			return out, p == nil, p
		}
	case "FLOAT", "FLOAT64":
		if s, ok := v.(string); ok && isNaNText(s) {
			return nil, false, nanProblem(what, loc)
		}
	}
	return v, false, nil
}

// fieldNamed returns the field of fields named name, without regard to
// case.
func fieldNamed(fields []field, name string) (field, bool) {
	for _, f := range fields {
		if strings.EqualFold(f.Name, name) {
			return f, true
		}
	}
	return field{}, false
}

// nanParameter returns the 501 for a query whose parameters, a
// QueryParameter list as JSON, carry a FLOAT64 NaN, at any depth; or "".
// The engine computes such a parameter as NULL (measured: an INSERT of a
// NaN parameter read back NULL), in any statement.
func nanParameter(params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	type paramType struct {
		Type        string          `json:"type"`
		ArrayType   json.RawMessage `json:"arrayType"`
		StructTypes []struct {
			Name string          `json:"name"`
			Type json.RawMessage `json:"type"`
		} `json:"structTypes"`
	}
	type paramValue struct {
		Value        *string                    `json:"value"`
		ArrayValues  []json.RawMessage          `json:"arrayValues"`
		StructValues map[string]json.RawMessage `json:"structValues"`
	}
	var nan func(t, v json.RawMessage) bool
	nan = func(t, v json.RawMessage) bool {
		var pt paramType
		var pv paramValue
		if json.Unmarshal(t, &pt) != nil || len(v) == 0 || json.Unmarshal(v, &pv) != nil {
			return false
		}
		switch strings.ToUpper(pt.Type) {
		case "FLOAT64", "FLOAT":
			return pv.Value != nil && isNaNText(*pv.Value)
		case "ARRAY":
			for _, e := range pv.ArrayValues {
				if nan(pt.ArrayType, e) {
					return true
				}
			}
		case "STRUCT":
			for _, st := range pt.StructTypes {
				if sv, ok := pv.StructValues[st.Name]; ok && nan(st.Type, sv) {
					return true
				}
			}
		}
		return false
	}
	var list []struct {
		Name           string          `json:"name"`
		ParameterType  json.RawMessage `json:"parameterType"`
		ParameterValue json.RawMessage `json:"parameterValue"`
	}
	if json.Unmarshal(params, &list) != nil {
		return ""
	}
	for i, p := range list {
		if nan(p.ParameterType, p.ParameterValue) {
			name := p.Name
			if name == "" {
				name = fmt.Sprintf("at position %d", i+1)
			}
			return nanProblem("a query parameter", "parameter "+name).msg
		}
	}
	return ""
}
