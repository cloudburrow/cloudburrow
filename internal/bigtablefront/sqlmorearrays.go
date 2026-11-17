package bigtablefront

import (
	"encoding/base32"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
)

// More of the documented functions #1704 adds (this file sorts after
// sqlfuncs.go, so its init adds to the functions that file made): the array functions
// ARRAY_INCLUDES_ALL, ARRAY_INCLUDES_ANY, ARRAY_IS_DISTINCT, ARRAY_LAST_N,
// ARRAY_SLICE, GENERATE_ARRAY, GENERATE_DATE_ARRAY and
// GENERATE_TIMESTAMP_ARRAY (https://cloud.google.com/bigtable/docs/reference/sql/array_functions),
// and the string functions OCTET_LENGTH, CHARACTER_LENGTH, TO_CODE_POINTS,
// CODE_POINTS_TO_BYTES, CODE_POINTS_TO_STRING, TO_BASE32 and FROM_BASE32
// (https://cloud.google.com/bigtable/docs/reference/sql/string_functions).
// IFERROR, ISERROR and NULLIFERROR
// (https://cloud.google.com/bigtable/docs/reference/sql/debugging_functions)
// catch an evaluation error of their try_expression; an error the
// emulator's SQL raises for what it does not build (UNIMPLEMENTED) is not
// one, and passes through as the reference's "system error" does.
// A generated array is held to GoogleSQL's default limit of 16,000
// elements, which the reference names.

const maxGenerated = 16000

func init() {
	sameArrays := func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
		if len(ts) != 2 || !argOK(ts[0], "ARRAY") || !argOK(ts[1], "ARRAY") {
			return nil, sigErr(c, ts)
		}
		if ts[0] != nil && ts[1] != nil && !sameType(ts[0], ts[1]) {
			return nil, sigErr(c, ts)
		}
		return boolType, nil
	}
	has := func(arr []any, v any) bool {
		for _, x := range arr {
			if x != nil && v != nil {
				if c, ok := compare(x, v); ok && c == 0 {
					return true
				}
			}
		}
		return false
	}
	add := map[string]*fnDef{
		"OCTET_LENGTH":     functions["BYTE_LENGTH"],
		"CHARACTER_LENGTH": functions["CHAR_LENGTH"],
		"ARRAY_INCLUDES_ALL": {sig: sameArrays, run: func(c *call, vs []any, _ *env) (any, error) {
			for _, v := range vs[1].([]any) {
				if !has(vs[0].([]any), v) {
					return false, nil
				}
			}
			return true, nil
		}},
		"ARRAY_INCLUDES_ANY": {sig: sameArrays, run: func(c *call, vs []any, _ *env) (any, error) {
			for _, v := range vs[1].([]any) {
				if has(vs[0].([]any), v) {
					return true, nil
				}
			}
			return false, nil
		}},
		"ARRAY_IS_DISTINCT": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) != 1 || !argOK(ts[0], "ARRAY") || ts[0] != nil && !groupable(ts[0].GetArrayType().GetElementType()) {
				return nil, sigErr(c, ts)
			}
			return boolType, nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			seen := map[string]bool{}
			for _, v := range vs[0].([]any) {
				k := groupKey(v)
				if seen[k] {
					return false, nil
				}
				seen[k] = true
			}
			return true, nil
		}},
		"ARRAY_LAST_N": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) != 2 || !argOK(ts[0], "ARRAY") || !argOK(ts[1], "INT64") {
				return nil, sigErr(c, ts)
			}
			return firstType(ts), nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			a, n := vs[0].([]any), vs[1].(int64)
			if n < 0 {
				return nil, fmt.Errorf("The n argument to ARRAY_LAST_N must not be negative, but is %d", n)
			}
			if n > int64(len(a)) {
				n = int64(len(a))
			}
			return append([]any{}, a[int64(len(a))-n:]...), nil
		}},
		"ARRAY_SLICE": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) != 3 || !argOK(ts[0], "ARRAY") || !argOK(ts[1], "INT64") || !argOK(ts[2], "INT64") {
				return nil, sigErr(c, ts)
			}
			return firstType(ts), nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			a := vs[0].([]any)
			n := int64(len(a))
			s, e := vs[1].(int64), vs[2].(int64)
			if s < 0 {
				s += n
			}
			if e < 0 {
				e += n
			}
			if s < 0 {
				s = 0
			}
			if e > n-1 {
				e = n - 1
			}
			if s > e || s >= n || e < 0 {
				return []any{}, nil
			}
			return append([]any{}, a[s:e+1]...), nil
		}},
		"GENERATE_ARRAY": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) < 2 || len(ts) > 3 {
				return nil, sigErr(c, ts)
			}
			for _, t := range ts {
				if !argOK(t, "INT64|FLOAT64") || t != nil && isKind(t, "FLOAT32") {
					return nil, sigErr(c, ts)
				}
			}
			et, err := unifyArgs(c, ts)
			if err != nil {
				return nil, err
			}
			return arrayType(et), nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			integer := isKind(c.t.GetArrayType().GetElementType(), "INT64")
			start, end, step := toFloat(vs[0]), toFloat(vs[1]), 1.0
			if len(vs) == 3 {
				step = toFloat(vs[2])
			}
			if math.IsNaN(start) || math.IsNaN(end) || math.IsNaN(step) {
				return nil, fmt.Errorf("GENERATE_ARRAY does not take NaN")
			}
			if step == 0 {
				return nil, fmt.Errorf("Sequence step cannot be 0.")
			}
			out := []any{}
			if integer {
				s, e, st := vs[0].(int64), vs[1].(int64), int64(1)
				if len(vs) == 3 {
					st = vs[2].(int64)
				}
				for x := s; st > 0 && x <= e || st < 0 && x >= e; x += st {
					if len(out) == maxGenerated {
						return nil, fmt.Errorf("GENERATE_ARRAY(%d, %d, %d) produces more than %d elements", s, e, st, maxGenerated)
					}
					out = append(out, x)
					if st > 0 && x > math.MaxInt64-st || st < 0 && x < math.MinInt64-st {
						break
					}
				}
				return out, nil
			}
			for i := 0; ; i++ {
				x := start + float64(i)*step
				if step > 0 && x > end || step < 0 && x < end {
					break
				}
				if len(out) == maxGenerated {
					return nil, fmt.Errorf("GENERATE_ARRAY produces more than %d elements", maxGenerated)
				}
				out = append(out, x)
			}
			return out, nil
		}},
		"GENERATE_DATE_ARRAY": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) < 2 || len(ts) > 3 || !argOK(ts[0], "DATE") || !argOK(ts[1], "DATE") {
				return nil, sigErr(c, ts)
			}
			if len(ts) == 3 {
				if _, ok := c.args[2].(*interval); !ok {
					return nil, fmt.Errorf("GENERATE_DATE_ARRAY takes an INTERVAL as its third argument")
				}
			}
			return arrayType(dateType), nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			iv := intervalValue{n: 1, unit: "DAY"}
			if len(vs) == 3 {
				iv = vs[2].(intervalValue)
			}
			if iv.n == 0 {
				return nil, fmt.Errorf("GENERATE_DATE_ARRAY: the step must not be 0")
			}
			start, end := vs[0].(time.Time), vs[1].(time.Time)
			out := []any{}
			for i := int64(0); ; i++ {
				v, err := addDate(start, intervalValue{n: i * iv.n, unit: iv.unit}, 1)
				if err != nil {
					return nil, err
				}
				d := v.(time.Time)
				if iv.n > 0 && d.After(end) || iv.n < 0 && d.Before(end) {
					break
				}
				if len(out) == maxGenerated {
					return nil, fmt.Errorf("GENERATE_DATE_ARRAY produces more than %d elements", maxGenerated)
				}
				out = append(out, d)
			}
			return out, nil
		}},
		"GENERATE_TIMESTAMP_ARRAY": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) != 3 || !argOK(ts[0], "TIMESTAMP") || !argOK(ts[1], "TIMESTAMP") {
				return nil, sigErr(c, ts)
			}
			if _, ok := c.args[2].(*interval); !ok {
				return nil, fmt.Errorf("GENERATE_TIMESTAMP_ARRAY takes an INTERVAL as its third argument")
			}
			return arrayType(timestampType), nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			iv := vs[2].(intervalValue)
			u, err := unitDuration(iv.unit)
			if err != nil || u == time.Nanosecond {
				return nil, fmt.Errorf("GENERATE_TIMESTAMP_ARRAY takes MICROSECOND, MILLISECOND, SECOND, MINUTE, HOUR or DAY, not %s", iv.unit)
			}
			if iv.n == 0 {
				return nil, fmt.Errorf("GENERATE_TIMESTAMP_ARRAY: the step must not be 0")
			}
			start, end := vs[0].(time.Time), vs[1].(time.Time)
			step := time.Duration(iv.n) * u
			out := []any{}
			for x := start; iv.n > 0 && !x.After(end) || iv.n < 0 && !x.Before(end); x = x.Add(step) {
				if len(out) == maxGenerated {
					return nil, fmt.Errorf("GENERATE_TIMESTAMP_ARRAY produces more than %d elements", maxGenerated)
				}
				out = append(out, x)
			}
			return out, nil
		}},
		"TO_CODE_POINTS": {sig: fixed(arrayType(int64Type), "STRING|BYTES"), run: func(c *call, vs []any, _ *env) (any, error) {
			out := []any{}
			if b, ok := vs[0].([]byte); ok {
				for _, x := range b {
					out = append(out, int64(x))
				}
				return out, nil
			}
			for _, r := range vs[0].(string) {
				out = append(out, int64(r))
			}
			return out, nil
		}},
		"CODE_POINTS_TO_BYTES": {sig: fixed(bytesType, "ARRAY"), run: func(c *call, vs []any, _ *env) (any, error) {
			var out []byte
			for _, v := range vs[0].([]any) {
				if v == nil {
					return nil, nil
				}
				n, ok := v.(int64)
				if !ok || n < 0 || n > 255 {
					return nil, fmt.Errorf("Invalid input to CODE_POINTS_TO_BYTES: %v", v)
				}
				out = append(out, byte(n))
			}
			return append([]byte{}, out...), nil
		}},
		"CODE_POINTS_TO_STRING": {sig: fixed(stringType, "ARRAY"), run: func(c *call, vs []any, _ *env) (any, error) {
			var b strings.Builder
			for _, v := range vs[0].([]any) {
				if v == nil {
					return nil, nil
				}
				n, ok := v.(int64)
				if !ok || n < 0 || n > utf8.MaxRune || n >= 0xD800 && n <= 0xDFFF {
					return nil, fmt.Errorf("Invalid codepoint %v", v)
				}
				b.WriteRune(rune(n))
			}
			return b.String(), nil
		}},
		"TO_BASE32": {sig: fixed(stringType, "BYTES"), run: func(c *call, vs []any, _ *env) (any, error) {
			return base32.StdEncoding.EncodeToString(vs[0].([]byte)), nil
		}},
		"FROM_BASE32": {sig: fixed(bytesType, "STRING|BYTES"), run: func(c *call, vs []any, _ *env) (any, error) {
			b, err := base32.StdEncoding.DecodeString(string(asBytes(vs[0])))
			if err != nil {
				return nil, fmt.Errorf("Failed to decode invalid base32 string %q", asBytes(vs[0]))
			}
			return b, nil
		}},
	}
	try := func(c *call, e *env) (any, bool, error) {
		v, err := c.args[0].eval(e)
		if err != nil {
			var u unsupportedErr
			if errors.As(err, &u) {
				return nil, false, err
			}
			return nil, true, nil
		}
		return v, false, nil
	}
	add["IFERROR"] = &fnDef{sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
		if len(ts) != 2 {
			return nil, sigErr(c, ts)
		}
		return unifyArgs(c, ts)
	}, lazy: func(c *call, e *env) (any, error) {
		v, failed, err := try(c, e)
		if err != nil {
			return nil, err
		}
		if failed {
			if v, err = c.args[1].eval(e); err != nil {
				return nil, err
			}
		}
		return coerce(v, c.t), nil
	}}
	add["ISERROR"] = &fnDef{sig: fixed(boolType, ""), lazy: func(c *call, e *env) (any, error) {
		_, failed, err := try(c, e)
		if err != nil {
			return nil, err
		}
		return failed, nil
	}}
	add["NULLIFERROR"] = &fnDef{sig: fixed(nil, ""), lazy: func(c *call, e *env) (any, error) {
		v, _, err := try(c, e)
		return v, err
	}}
	for k, v := range add {
		functions[k] = v
	}
}
