package bigqueryfront

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// rowError is one ErrorProto in an insertErrors entry.
// https://cloud.google.com/bigquery/docs/reference/rest/v2/ErrorProto
type rowError struct {
	Reason   string `json:"reason"`
	Location string `json:"location"`
	// DebugInfo is always empty; BigQuery marks it internal-only.
	DebugInfo string `json:"debugInfo"`
	Message   string `json:"message"`
}

// checkRow checks one row's JSON object against a table's fields. It
// returns the row to store, which is obj without its unknown fields when
// ignoreUnknown is set, and every reason the row is invalid.
//
// What is checked is whether BigQuery could convert each value to its
// column's type from the JSON insertAll takes: a missing or null REQUIRED
// value, a field the table does not have, a scalar where an array or record
// belongs and the reverse, and a value that does not parse as its type. The
// emulator checks unknown fields only (measured, #861): it stored "x" in a
// NUMERIC column as 0, and a string in a REPEATED INTEGER column, after
// which reading the table failed.
func checkRow(fields []field, obj map[string]any, ignoreUnknown bool) (map[string]any, []rowError) {
	var errs []rowError
	out := checkRecord(fields, obj, "", ignoreUnknown, &errs)
	return out, errs
}

func checkRecord(fields []field, obj map[string]any, prefix string, ignoreUnknown bool, errs *[]rowError) map[string]any {
	byName := make(map[string]field, len(fields))
	for _, f := range fields {
		byName[strings.ToLower(f.Name)] = f
	}
	out := make(map[string]any, len(obj))
	// Sorted, so a row's errors come back in the same order every time.
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		f, known := byName[strings.ToLower(k)]
		if !known {
			if !ignoreUnknown {
				*errs = append(*errs, rowError{Reason: "invalid", Location: prefix + k,
					Message: "no such field: " + prefix + k + "."})
			}
			continue
		}
		out[k] = checkField(f, obj[k], prefix, ignoreUnknown, errs)
	}
	for _, f := range fields {
		if strings.ToUpper(f.Mode) != "REQUIRED" {
			continue
		}
		v, present := lookup(obj, f.Name)
		if !present || v == nil {
			*errs = append(*errs, rowError{Reason: "invalid", Location: prefix + f.Name,
				Message: "Missing required field: " + prefix + f.Name + "."})
		}
	}
	return out
}

// lookup finds a field by name without case, as BigQuery matches them.
func lookup(obj map[string]any, name string) (any, bool) {
	if v, ok := obj[name]; ok {
		return v, true
	}
	for k, v := range obj {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return nil, false
}

// checkField checks one field's value and returns what to store for it.
func checkField(f field, v any, prefix string, ignoreUnknown bool, errs *[]rowError) any {
	loc := prefix + f.Name
	if v == nil {
		// A null REQUIRED value is reported by checkRecord; a null REPEATED
		// one is an empty array, and a null NULLABLE one is NULL.
		return nil
	}
	if strings.ToUpper(f.Mode) == "REPEATED" {
		arr, ok := v.([]any)
		if !ok {
			*errs = append(*errs, invalid(loc, "Field %s is REPEATED and must be a JSON array; found %s", loc, jsonKind(v)))
			return v
		}
		out := make([]any, len(arr))
		for i, e := range arr {
			eloc := fmt.Sprintf("%s[%d]", loc, i)
			if e == nil {
				*errs = append(*errs, invalid(eloc, "Field %s: an array cannot contain NULL", eloc))
				continue
			}
			out[i] = checkValue(f, e, eloc, ignoreUnknown, errs)
		}
		return out
	}
	return checkValue(f, v, loc, ignoreUnknown, errs)
}

func invalid(loc, format string, args ...any) rowError {
	return rowError{Reason: "invalid", Location: loc, Message: fmt.Sprintf(format, args...)}
}

// checkValue checks one non-null value, not an array, against f's type.
func checkValue(f field, v any, loc string, ignoreUnknown bool, errs *[]rowError) any {
	typ := strings.ToUpper(f.Type)
	if typ == "RECORD" || typ == "STRUCT" {
		obj, ok := v.(map[string]any)
		if !ok {
			*errs = append(*errs, invalid(loc, "Field %s is a RECORD and must be a JSON object; found %s", loc, jsonKind(v)))
			return v
		}
		return checkRecord(f.Fields, obj, loc+".", ignoreUnknown, errs)
	}
	if _, isArray := v.([]any); isArray {
		*errs = append(*errs, invalid(loc, "Field %s is not REPEATED; found a JSON array", loc))
		return v
	}
	if _, isObject := v.(map[string]any); isObject && typ != "JSON" {
		*errs = append(*errs, invalid(loc, "Field %s is %s; found a JSON object", loc, typ))
		return v
	}
	if why := scalarProblem(typ, v); why != "" {
		shown, _ := json.Marshal(v)
		*errs = append(*errs, invalid(loc, "Cannot convert value %s of field %s to %s: %s", shown, loc, typ, why))
	}
	return v
}

// jsonKind names the JSON type of a decoded value, for messages.
func jsonKind(v any) string {
	switch v.(type) {
	case map[string]any:
		return "a JSON object"
	case []any:
		return "a JSON array"
	case string:
		return "a string"
	case json.Number:
		return "a number"
	case bool:
		return "a boolean"
	}
	return "null"
}

// numericMax and bigNumericMax bound NUMERIC and BIGNUMERIC: "-9.9999999999999999999999999999999999999E+28
// to 9.9999999999999999999999999999999999999E+28" and "-5.7896044618658097711785492504343953926634992332820282019728792003956564819968E+38
// to 5.7896044618658097711785492504343953926634992332820282019728792003956564819967E+38".
// https://cloud.google.com/bigquery/docs/reference/standard-sql/data-types
var (
	numericMax, _    = new(big.Rat).SetString("9.9999999999999999999999999999999999999E+28")
	bigNumericMax, _ = new(big.Rat).SetString("5.7896044618658097711785492504343953926634992332820282019728792003956564819967E+38")
	bigNumericMin, _ = new(big.Rat).SetString("-5.7896044618658097711785492504343953926634992332820282019728792003956564819968E+38")
)

// Canonical formats, from the GoogleSQL data types reference: DATE
// "YYYY-[M]M-[D]D"; TIME "[H]H:[M]M:[S]S[.F]", seconds "from 00 to 60";
// DATETIME "civil_date_part [time_part]" with time_part "{ |T|t}[H]H:[M]M:[S]S[.F]";
// TIMESTAMP the same followed by a time zone: "Offset from Coordinated
// Universal Time (UTC), or the letter Z or z for UTC", or a "Time zone name
// from the tz database". [.F] is documented as "Up to six fractional digits";
// up to nine are accepted here, so a client sending nanoseconds is not
// refused for precision BigQuery may only truncate.
// https://cloud.google.com/bigquery/docs/reference/standard-sql/data-types
var (
	datePattern      = regexp.MustCompile(`^(\d{4})-(\d{1,2})-(\d{1,2})$`)
	timePattern      = regexp.MustCompile(`^(\d{1,2}):(\d{1,2}):(\d{1,2})(\.\d{1,9})?$`)
	dateTimePattern  = regexp.MustCompile(`^(\d{4})-(\d{1,2})-(\d{1,2})(?:[ Tt](\d{1,2}):(\d{1,2}):(\d{1,2})(\.\d{1,9})?)?$`)
	timestampPattern = regexp.MustCompile(`^(\d{4})-(\d{1,2})-(\d{1,2})(?:[ Tt](\d{1,2}):(\d{1,2}):(\d{1,2})(\.\d{1,9})?` +
		`(?:\s*(?:[Zz]|UTC|[+-]\d{1,2}(?::?\d{2})?|[A-Za-z][A-Za-z0-9_+\-]*(?:/[A-Za-z0-9_+\-]+)*))?)?$`)
)

// scalarProblem returns why v cannot be converted to typ, or "". A value
// BigQuery takes in two JSON forms (a number or its string, for example) is
// accepted in either.
func scalarProblem(typ string, v any) string {
	s, isString := v.(string)
	num, isNumber := v.(json.Number)
	_, isBool := v.(bool)
	text := s
	if isNumber {
		text = num.String()
	}
	switch typ {
	case "STRING":
		// A number or boolean is converted to its text.
		return ""
	case "INTEGER", "INT64":
		if isBool {
			return "not an integer"
		}
		if _, err := strconv.ParseInt(text, 10, 64); err != nil {
			return "not a 64-bit integer"
		}
	case "FLOAT", "FLOAT64":
		if isBool {
			return "not a number"
		}
		// ParseFloat also takes "NaN", "Infinity" and "-Infinity", which a
		// FLOAT column holds and JSON can carry only as strings.
		if _, err := strconv.ParseFloat(text, 64); err != nil && !isRangeErr(err) {
			return "not a number"
		}
	case "NUMERIC", "BIGNUMERIC":
		if isBool || strings.ContainsAny(text, "/") {
			return "not a decimal number"
		}
		r, ok := new(big.Rat).SetString(strings.TrimSpace(text))
		if !ok {
			return "not a decimal number"
		}
		if typ == "NUMERIC" {
			if new(big.Rat).Abs(r).Cmp(numericMax) > 0 {
				return "out of NUMERIC's range"
			}
		} else if r.Cmp(bigNumericMax) > 0 || r.Cmp(bigNumericMin) < 0 {
			return "out of BIGNUMERIC's range"
		}
	case "BOOLEAN", "BOOL":
		if isBool {
			return ""
		}
		if isString && (strings.EqualFold(s, "true") || strings.EqualFold(s, "false")) {
			return ""
		}
		return "not true or false"
	case "BYTES":
		if !isString {
			return "not a base64-encoded string"
		}
		if _, err := base64.StdEncoding.DecodeString(s); err != nil {
			if _, err := base64.URLEncoding.DecodeString(s); err != nil {
				return "not a base64-encoded string"
			}
		}
	case "DATE":
		if !isString || !validDate(datePattern.FindStringSubmatch(s)) {
			return "not a date (YYYY-MM-DD)"
		}
	case "TIME":
		if !isString || !validTime(timePattern.FindStringSubmatch(s)) {
			return "not a time (HH:MM:SS[.DDDDDD])"
		}
	case "DATETIME":
		if !isString || !validDateTime(dateTimePattern.FindStringSubmatch(s)) {
			return "not a datetime (YYYY-MM-DD[ HH:MM:SS[.DDDDDD]])"
		}
	case "TIMESTAMP":
		if isNumber {
			// Seconds since the Unix epoch.
			if _, err := strconv.ParseFloat(text, 64); err != nil {
				return "not a timestamp"
			}
			return ""
		}
		if !isString || !validTimestamp(s) {
			return "not a timestamp (YYYY-MM-DD HH:MM:SS[.DDDDDD][time zone])"
		}
	case "GEOGRAPHY":
		// WKT or GeoJSON text; its geometry is not checked (the emulator
		// does not check it either), only that it is text.
		if !isString {
			return "not a WKT or GeoJSON string"
		}
	case "JSON":
		// A JSON column's value is sent as JSON text in a string, which is
		// how the Go client sends it; that text must parse. A value sent as
		// a JSON object or number is passed as it is.
		if isString && !json.Valid([]byte(s)) {
			return "not valid JSON text"
		}
	}
	// INTERVAL and RANGE are passed as they are: their text formats are not
	// checked here, so the emulator's reading of them stands.
	return ""
}

func isRangeErr(err error) bool {
	ne, ok := err.(*strconv.NumError)
	return ok && ne.Err == strconv.ErrRange
}

// validDate checks a datePattern match: year 1 to 9999, a real day.
func validDate(m []string) bool {
	if m == nil {
		return false
	}
	return realDate(m[1], m[2], m[3])
}

func realDate(ys, ms, ds string) bool {
	y, _ := strconv.Atoi(ys)
	mo, _ := strconv.Atoi(ms)
	d, _ := strconv.Atoi(ds)
	if y < 1 || mo < 1 || mo > 12 || d < 1 {
		return false
	}
	t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	return t.Day() == d && int(t.Month()) == mo
}

func realClock(hs, ms, ss string) bool {
	h, _ := strconv.Atoi(hs)
	mi, _ := strconv.Atoi(ms)
	s, _ := strconv.Atoi(ss)
	return h <= 23 && mi <= 59 && s <= 60
}

func validTime(m []string) bool {
	return m != nil && realClock(m[1], m[2], m[3])
}

func validDateTime(m []string) bool {
	if m == nil || !realDate(m[1], m[2], m[3]) {
		return false
	}
	return m[4] == "" || realClock(m[4], m[5], m[6])
}

// validTimestamp checks a TIMESTAMP string: a DATETIME, then an optional
// time zone after the time.
func validTimestamp(s string) bool {
	m := timestampPattern.FindStringSubmatch(s)
	return m != nil && validDateTime(m[:8])
}
