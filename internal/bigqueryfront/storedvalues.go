package bigqueryfront

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Values the pinned emulator stored as other data (#1065, #1066, #1075).
//
// BYTES. BigQuery reads a BYTES value in a JSON load, a CSV load and a
// streamed row (tabledata.insertAll) as base64: "BYTES ... must be
// base64-encoded" (https://cloud.google.com/bigquery/docs/loading-data-cloud-storage-json,
// https://cloud.google.com/bigquery/docs/reference/rest/v2/tabledata/insertAll).
// The pinned emulator stored the base64 text itself (measured through the
// front with the official Go client: "/w==" loaded by NEWLINE_DELIMITED_JSON
// and by CSV, and []byte{0xff} put with Inserter.Put, each read back TO_HEX
// 2f773d3d where BigQuery reads ff), and #1074 had the front decode each
// value and send its bytes as a string, which could carry only bytes that
// are UTF-8 text, so others were 501. The emulator CloudBurrow builds
// (#1061, third_party/bigquery-emulator, bigquery-emulator patch 0001)
// decodes the base64 itself, so the front now only checks each value: one
// that is not base64 is refused as BigQuery refuses it, and one in the
// URL-safe alphabet, which BigQuery also reads, is sent in the standard
// one, the only one the emulator decodes. Any bytes are written.
//
// NaN. The pinned emulator's engine stored and computed every FLOAT64 NaN
// as NULL (SQLite makes a NaN NULL), and #1074 had the front refuse (501)
// the NaNs it could see. The engine CloudBurrow builds keeps a NaN
// (googlesqlite patch 0004, #1066), so a NaN in a load, a streamed row or
// a query parameter is sent on as it came. ±Infinity is kept (measured:
// read back ±Inf through every path).

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
// values the front must read: BYTES (above).
func storedValues(fields []field) bool {
	for _, f := range fields {
		switch strings.ToUpper(f.Type) {
		case "BYTES":
			return true
		case "RECORD", "STRUCT":
			if storedValues(f.Fields) {
				return true
			}
		}
	}
	return false
}

// decodeBytes checks a BYTES value v, base64 as BigQuery reads it, and
// returns it in the standard base64 alphabet, which the emulator decodes
// (above); or, when it is not base64, BigQuery's error. csv is whether the
// value is in a CSV load; it no longer changes anything, since the value
// stays base64 text.
func decodeBytes(v, what, loc string, csv bool) (string, *valueProblem) {
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		if b, err = base64.URLEncoding.DecodeString(v); err != nil {
			return "", &valueProblem{code: http.StatusBadRequest, reason: "invalid", msg: fmt.Sprintf(
				"Error while reading data, error message: Could not decode base64 string to bytes. Field: %s; Value: %s",
				loc, v)}
		}
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// fixValues changes each BYTES value in obj, a row or RECORD value of
// fields read from JSON, to its standard base64 (decodeBytes), and returns
// whether it changed one, or the first value that cannot be written: a
// BYTES value that is not base64. Fields are matched without regard to case, as the emulator
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
			return out, p == nil && out != s, p
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

// exactNumbers writes each INT64, NUMERIC and BIGNUMERIC value in obj, a
// streamed row or RECORD value of fields read with UseNumber, that came as
// a JSON number as the string of its digits (#1129). BigQuery takes such a
// value as either ("a number or its string", scalarProblem), and keeps its
// every digit. The emulator decodes a streamed row's JSON number as a
// float64: measured through the front, an INT64 9007199254740993 sent by
// tabledata.insertAll as a number read back 9007199254740992, a NUMERIC
// 12345678901234567890.123456789 read back 12345678901234567168 and a
// BIGNUMERIC lost its digits past the 17th alike, where the same values
// sent as strings, or loaded as numbers from NEWLINE_DELIMITED_JSON, were
// kept. The official Go client's Inserter sends an int64 as a JSON number.
// A value checkRow refused is not here; one of the wrong JSON kind is left
// as it is.
func exactNumbers(fields []field, obj map[string]any) {
	for k, v := range obj {
		f, ok := fieldNamed(fields, k)
		if !ok || v == nil {
			continue
		}
		if strings.EqualFold(f.Mode, "REPEATED") {
			if arr, ok := v.([]any); ok {
				for i, e := range arr {
					arr[i] = exactNumber(f, e)
				}
			}
			continue
		}
		obj[k] = exactNumber(f, v)
	}
}

func exactNumber(f field, v any) any {
	switch strings.ToUpper(f.Type) {
	case "RECORD", "STRUCT":
		if m, ok := v.(map[string]any); ok {
			exactNumbers(f.Fields, m)
		}
	case "INTEGER", "INT64", "NUMERIC", "BIGNUMERIC", "DECIMAL", "BIGDECIMAL":
		if n, ok := v.(json.Number); ok {
			return n.String()
		}
	}
	return v
}
