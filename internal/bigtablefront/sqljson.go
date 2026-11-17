package bigtablefront

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
)

// JSON_QUERY, JSON_EXTRACT, JSON_QUERY_ARRAY and TO_JSON_STRING (#1704), as
// https://cloud.google.com/bigtable/docs/reference/sql/json_functions
// documents them: the first three return the value a JSONPath selects as a
// JSON-formatted STRING (compact, its keys in the input's order), a JSON
// null as SQL NULL; TO_JSON_STRING encodes a value with the reference's
// JSON encodings. JSON_QUERY and JSON_QUERY_ARRAY escape a key with double
// quotes ($."a.b"), JSON_EXTRACT with single quotes and brackets
// ($['a.b']). This file sorts after sqlfuncs.go, so its init adds to the
// functions that file made.

func init() {
	query := func(c *call, vs []any, _ *env) (any, error) {
		raw, err := jsonSelect(vs[0].(string), jsonPathArg(vs), c.name == "JSON_EXTRACT")
		if err != nil || raw == nil {
			return nil, err
		}
		return string(raw), nil
	}
	functions["JSON_QUERY"] = &fnDef{sig: fixed(stringType, "STRING", "STRING"), run: query}
	functions["JSON_EXTRACT"] = &fnDef{sig: fixed(stringType, "STRING", "STRING"), run: query}
	functions["JSON_QUERY_ARRAY"] = &fnDef{sig: ranged(arrayType(stringType), 1, "STRING", "STRING"), run: func(c *call, vs []any, _ *env) (any, error) {
		raw, err := jsonSelect(vs[0].(string), jsonPathArg(vs), false)
		if err != nil || raw == nil {
			return nil, err
		}
		var elems []json.RawMessage
		if json.Unmarshal(raw, &elems) != nil {
			return nil, nil // not an array
		}
		out := make([]any, len(elems))
		for i, e := range elems {
			var b bytes.Buffer
			if err := json.Compact(&b, e); err != nil {
				return nil, err
			}
			out[i] = b.String()
		}
		return out, nil
	}}
	functions["TO_JSON_STRING"] = &fnDef{nulls: true, sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
		if len(ts) < 1 || len(ts) > 2 || len(ts) == 2 && !argOK(ts[1], "BOOL") {
			return nil, sigErr(c, ts)
		}
		if ts[0] != nil && hasMap(ts[0]) {
			return nil, fmt.Errorf("TO_JSON_STRING: the MAP type has no JSON encoding")
		}
		return stringType, nil
	}, run: func(c *call, vs []any, _ *env) (any, error) {
		if len(vs) == 2 && vs[1] == nil {
			return nil, nil
		}
		var b strings.Builder
		t := c.argTypes[0]
		if t == nil {
			t = int64Type
		}
		toJSON(&b, vs[0], t)
		if len(vs) == 2 && vs[1] == true {
			var out bytes.Buffer
			if err := json.Indent(&out, []byte(b.String()), "", "  "); err != nil {
				return nil, err
			}
			return out.String(), nil
		}
		return b.String(), nil
	}}
}

func jsonPathArg(vs []any) string {
	if len(vs) > 1 {
		return vs[1].(string)
	}
	return "$"
}

func hasMap(t *btpb.Type) bool {
	switch {
	case isKind(t, "MAP<"):
		return true
	case isKind(t, "ARRAY<"):
		return hasMap(t.GetArrayType().GetElementType())
	case isKind(t, "STRUCT<"):
		for _, f := range t.GetStructType().GetFields() {
			if hasMap(f.GetType()) {
				return true
			}
		}
	}
	return false
}

// jsonSelect is the compact JSON the path selects in doc, nil when it
// selects nothing or a JSON null.
func jsonSelect(doc, path string, brackets bool) (json.RawMessage, error) {
	cur := json.RawMessage(doc)
	if !json.Valid(cur) {
		return nil, nil
	}
	if !strings.HasPrefix(path, "$") {
		return nil, fmt.Errorf("JSONPath must start with '$'")
	}
	rest := path[1:]
	for rest != "" {
		var key string
		index := -1
		switch {
		case strings.HasPrefix(rest, "."):
			rest = rest[1:]
			if !brackets && strings.HasPrefix(rest, `"`) {
				end := strings.IndexByte(rest[1:], '"')
				if end < 0 {
					return nil, fmt.Errorf("Invalid JSONPath: %s", path)
				}
				key, rest = rest[1:end+1], rest[end+2:]
				break
			}
			end := strings.IndexAny(rest, ".[")
			if end < 0 {
				end = len(rest)
			}
			key, rest = rest[:end], rest[end:]
		case strings.HasPrefix(rest, "["):
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return nil, fmt.Errorf("Invalid JSONPath: %s", path)
			}
			k := rest[1:end]
			rest = rest[end+1:]
			if brackets && len(k) >= 2 && (k[0] == '\'' || k[0] == '"') {
				key = k[1 : len(k)-1]
				break
			}
			n, err := strconv.Atoi(k)
			if err != nil {
				return nil, fmt.Errorf("Invalid JSONPath: %s", path)
			}
			index = n
		default:
			return nil, fmt.Errorf("Invalid JSONPath: %s", path)
		}
		if index >= 0 {
			var arr []json.RawMessage
			if json.Unmarshal(cur, &arr) != nil || index >= len(arr) {
				return nil, nil
			}
			cur = arr[index]
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(cur, &obj) != nil {
			return nil, nil
		}
		v, ok := obj[key]
		if !ok {
			return nil, nil
		}
		cur = v
	}
	var b bytes.Buffer
	if err := json.Compact(&b, cur); err != nil {
		return nil, err
	}
	if b.String() == "null" {
		return nil, nil
	}
	return b.Bytes(), nil
}

// toJSON writes v, of type t, with the reference's JSON encodings.
func toJSON(b *strings.Builder, v any, t *btpb.Type) {
	if v == nil {
		b.WriteString("null")
		return
	}
	switch x := v.(type) {
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case int64:
		if x >= -1<<53 && x <= 1<<53 {
			b.WriteString(strconv.FormatInt(x, 10))
		} else {
			b.WriteString(`"` + strconv.FormatInt(x, 10) + `"`)
		}
	case float64:
		switch {
		case math.IsNaN(x):
			b.WriteString(`"NaN"`)
		case math.IsInf(x, 1):
			b.WriteString(`"Infinity"`)
		case math.IsInf(x, -1):
			b.WriteString(`"-Infinity"`)
		case math.Abs(x) < 1e21:
			bits := 64
			if isKind(t, "FLOAT32") {
				bits = 32
			}
			b.WriteString(strconv.FormatFloat(x, 'f', -1, bits))
		default:
			b.WriteString(strconv.FormatFloat(x, 'g', -1, 64))
		}
	case string:
		jsonString(b, x)
	case []byte:
		if isKind(t, "STRING") {
			jsonString(b, string(x))
		} else {
			b.WriteString(`"` + base64.StdEncoding.EncodeToString(x) + `"`)
		}
	case time.Time:
		if isKind(t, "DATE") {
			b.WriteString(`"` + x.Format("2006-01-02") + `"`)
		} else {
			b.WriteString(`"` + x.UTC().Format("2006-01-02T15:04:05.999999Z") + `"`)
		}
	case []any:
		if isKind(t, "ARRAY<") {
			b.WriteByte('[')
			for i, e := range x {
				if i > 0 {
					b.WriteByte(',')
				}
				toJSON(b, e, t.GetArrayType().GetElementType())
			}
			b.WriteByte(']')
			return
		}
		fs := t.GetStructType().GetFields()
		b.WriteByte('{')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			jsonString(b, fs[i].GetFieldName())
			b.WriteByte(':')
			toJSON(b, e, fs[i].GetType())
		}
		b.WriteByte('}')
	default:
		b.WriteString("null")
	}
}

// jsonString writes s as a JSON string, escaping ", \ and the control
// characters U+0000 to U+001F, as the reference's encodings say.
func jsonString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20:
			fmt.Fprintf(b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}
