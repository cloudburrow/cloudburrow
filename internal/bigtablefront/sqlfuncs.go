package bigtablefront

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
)

// The functions of GoogleSQL for Bigtable the emulator's SQL runs (#1511):
// conversions, string and bytes, math, conditional, timestamp, map, array
// and JSON functions, and the aggregates. A documented function it does
// not run is refused UNIMPLEMENTED by name (documented below); any other
// name is "Function not found", INVALID_ARGUMENT, as Google answers.

// documented are the reference's function names (functions-all), so that a
// documented function not built here is UNIMPLEMENTED, not "not found".
var documented = strings.Fields(`ARRAY_AGG ARRAY_CONCAT_AGG AVG BIT_AND BIT_OR BIT_XOR COUNT COUNTIF GROUPING
LOGICAL_AND LOGICAL_OR MAX MAX_BY MIN MIN_BY STRING_AGG SUM APPROX_COUNT_DISTINCT APPROX_QUANTILES
APPROX_TOP_COUNT APPROX_TOP_SUM CORR COVAR_POP COVAR_SAMP STDDEV STDDEV_POP STDDEV_SAMP VAR_POP VAR_SAMP VARIANCE
MAP_CONTAINS_KEY MAP_EMPTY MAP_ENTRIES MAP_KEYS MAP_VALUES ASCII BYTE_LENGTH CHAR_LENGTH CHARACTER_LENGTH CHR
CODE_POINTS_TO_BYTES CODE_POINTS_TO_STRING CONCAT ENDS_WITH FORMAT FROM_BASE32 FROM_BASE64 FROM_HEX INITCAP INSTR
LEFT LENGTH LOWER LPAD LTRIM NORMALIZE NORMALIZE_AND_CASEFOLD OCTET_LENGTH REGEXP_CONTAINS REGEXP_EXTRACT
REGEXP_EXTRACT_ALL REGEXP_INSTR REGEXP_REPLACE REPEAT REPLACE REVERSE RIGHT RPAD RTRIM
SAFE_CONVERT_BYTES_TO_STRING SOUNDEX SPLIT STARTS_WITH STRPOS SUBSTR SUBSTRING TO_BASE32 TO_BASE64
TO_CODE_POINTS TO_FLOAT32 TO_FLOAT64 TO_HEX TO_INT64 TO_VECTOR32 TO_VECTOR64 TRANSLATE TRIM UNICODE UPPER ABS
ACOS ACOSH ASIN ASINH ATAN ATAN2 ATANH CEIL CEILING COS COSH COSINE_DISTANCE COT COTH CSC CSCH DIV EXP
EUCLIDEAN_DISTANCE FLOOR GREATEST IEEE_DIVIDE IS_INF IS_NAN LEAST LN LOG LOG10 MOD POW POWER RAND ROUND SAFE_ADD
SAFE_DIVIDE SAFE_MULTIPLY SAFE_NEGATE SAFE_SUBTRACT SEC SECH SIGN SIN SINH SQRT TAN TANH TRUNC
CURRENT_TIMESTAMP EXTRACT FORMAT_TIMESTAMP PARSE_TIMESTAMP STRING TIMESTAMP TIMESTAMP_ADD TIMESTAMP_DIFF
TIMESTAMP_FROM_UNIX_MICROS TIMESTAMP_FROM_UNIX_MILLIS TIMESTAMP_FROM_UNIX_SECONDS TIMESTAMP_MICROS
TIMESTAMP_MILLIS TIMESTAMP_SECONDS TIMESTAMP_SUB TIMESTAMP_TRUNC UNIX_MICROS UNIX_MILLIS UNIX_SECONDS
JSON_EXTRACT JSON_EXTRACT_SCALAR JSON_QUERY JSON_QUERY_ARRAY JSON_VALUE TO_JSON_STRING ARRAY_CONCAT
ARRAY_FILTER ARRAY_FIRST ARRAY_INCLUDES ARRAY_INCLUDES_ALL ARRAY_INCLUDES_ANY ARRAY_IS_DISTINCT ARRAY_LAST
ARRAY_LAST_N ARRAY_LENGTH ARRAY_OFFSET ARRAY_OFFSETS ARRAY_REVERSE ARRAY_SLICE ARRAY_TO_STRING ARRAY_TRANSFORM
GENERATE_ARRAY GENERATE_DATE_ARRAY GENERATE_TIMESTAMP_ARRAY IF IFNULL NULLIF COALESCE CURRENT_DATE DATE
UNIX_DATE DATE_ADD DATE_SUB DATE_DIFF DATE_TRUNC FORMAT_DATE PARSE_DATE DATE_FROM_UNIX_DATE`)

var aggregates = map[string]bool{"COUNT": true, "COUNTIF": true, "SUM": true, "AVG": true, "MIN": true, "MAX": true,
	"ANY_VALUE": true, "LOGICAL_AND": true, "LOGICAL_OR": true, "ARRAY_AGG": true, "STRING_AGG": true,
	"BIT_AND": true, "BIT_OR": true, "BIT_XOR": true, "MIN_BY": true, "MAX_BY": true}

type call struct {
	name        string
	args        []expr
	distinct    bool
	star        bool
	ignoreNulls bool
	t           *btpb.Type
	argTypes    []*btpb.Type
	fn          *fnDef
	agg         int // the aggregate's slot, for an aggregate
	isAgg       bool
	re          *regexp.Regexp // a constant regular expression argument
}

// fnDef is a scalar function: sig types a call, run evaluates it. Unless
// nulls is set, a NULL argument makes the result NULL without run.
type fnDef struct {
	sig   func(c *call, ts []*btpb.Type) (*btpb.Type, error)
	run   func(c *call, vs []any, e *env) (any, error)
	nulls bool
}

func (c *call) String() string {
	var p []string
	for _, a := range c.args {
		p = append(p, a.String())
	}
	return fmt.Sprintf("%s(%v,%v,%s)", c.name, c.distinct, c.star, strings.Join(p, ","))
}

func (c *call) check(s *scope) (*btpb.Type, error) {
	if aggregates[c.name] {
		return c.checkAgg(s)
	}
	if c.distinct {
		return nil, fmt.Errorf("DISTINCT is allowed only in aggregate functions, not %s", c.name)
	}
	fn, ok := functions[c.name]
	if !ok {
		for _, d := range documented {
			if d == c.name {
				return nil, unsupportedErr{"the function " + c.name}
			}
		}
		return nil, fmt.Errorf("Function not found: %s", c.name)
	}
	c.fn = fn
	c.argTypes = nil
	for _, a := range c.args {
		if iv, ok := a.(*interval); ok {
			if _, err := iv.n.check(s); err != nil {
				return nil, err
			}
			c.argTypes = append(c.argTypes, nil)
			continue
		}
		t, err := a.check(s)
		if err != nil {
			return nil, err
		}
		if l, ok := a.(*literal); ok && l.t == nil {
			t = nil
		}
		c.argTypes = append(c.argTypes, t)
	}
	t, err := fn.sig(c, c.argTypes)
	if err != nil {
		return nil, err
	}
	c.t = t
	return t, nil
}

func (c *call) eval(e *env) (any, error) {
	if c.isAgg {
		return e.aggs[c.agg], nil
	}
	vs := make([]any, len(c.args))
	for i, a := range c.args {
		if iv, ok := a.(*interval); ok {
			n, err := iv.n.eval(e)
			if err != nil {
				return nil, err
			}
			if n == nil {
				return nil, nil
			}
			vs[i] = intervalValue{n: n.(int64), unit: iv.unit}
			continue
		}
		v, err := a.eval(e)
		if err != nil {
			return nil, err
		}
		if v == nil && !c.fn.nulls {
			return nil, nil
		}
		vs[i] = v
	}
	return c.fn.run(c, vs, e)
}

type intervalValue struct {
	n    int64
	unit string
}

func sigErr(c *call, ts []*btpb.Type) error {
	var ks []string
	for _, t := range ts {
		ks = append(ks, kind(t))
	}
	return fmt.Errorf("No matching signature for function %s for argument types: %s", c.name, strings.Join(ks, ", "))
}

// fixed is a signature of exactly the argument kinds given ("" for any;
// "STR|BYTES" for alternatives); ret nil returns the first argument's type.
func fixed(ret *btpb.Type, kinds ...string) func(*call, []*btpb.Type) (*btpb.Type, error) {
	return func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
		if len(ts) != len(kinds) {
			return nil, sigErr(c, ts)
		}
		for i, k := range kinds {
			if !argOK(ts[i], k) {
				return nil, sigErr(c, ts)
			}
		}
		if ret == nil {
			return firstType(ts), nil
		}
		return ret, nil
	}
}

// ranged accepts min to max arguments of the kinds given (the last repeated).
func ranged(ret *btpb.Type, min int, kinds ...string) func(*call, []*btpb.Type) (*btpb.Type, error) {
	return func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
		if len(ts) < min || len(ts) > len(kinds) && kinds[len(kinds)-1] != "..." {
			return nil, sigErr(c, ts)
		}
		for i, t := range ts {
			k := ""
			if i < len(kinds) && kinds[i] != "..." {
				k = kinds[i]
			} else if kinds[len(kinds)-1] == "..." {
				k = kinds[len(kinds)-2]
			}
			if !argOK(t, k) {
				return nil, sigErr(c, ts)
			}
		}
		if ret == nil {
			return firstType(ts), nil
		}
		return ret, nil
	}
}

func firstType(ts []*btpb.Type) *btpb.Type {
	for _, t := range ts {
		if t != nil {
			return t
		}
	}
	return int64Type
}

func argOK(t *btpb.Type, k string) bool {
	if t == nil || k == "" {
		return true
	}
	for _, alt := range strings.Split(k, "|") {
		switch alt {
		case "NUM":
			if isNumeric(t) {
				return true
			}
		case "ARRAY", "MAP", "STRUCT":
			if isKind(t, alt+"<") {
				return true
			}
		case "FLOAT64":
			if isNumeric(t) {
				return true
			}
		default:
			if isKind(t, alt) {
				return true
			}
		}
	}
	return false
}

// textual is the result type of a STRING-or-BYTES function: the type of
// its first argument.
func textual(n int, rest ...string) func(*call, []*btpb.Type) (*btpb.Type, error) {
	kinds := append([]string{"STRING|BYTES"}, rest...)
	return func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
		if len(ts) < n || len(ts) > len(kinds) {
			return nil, sigErr(c, ts)
		}
		for i, t := range ts {
			if !argOK(t, kinds[i]) {
				return nil, sigErr(c, ts)
			}
		}
		t := firstType(ts)
		if !isKind(t, "STRING", "BYTES") {
			t = stringType
		}
		return t, nil
	}
}

// sameText checks that the STRING-or-BYTES arguments agree.
func sameText(ts []*btpb.Type, idx ...int) bool {
	var first *btpb.Type
	for _, i := range idx {
		if i >= len(ts) || ts[i] == nil {
			continue
		}
		if first == nil {
			first = ts[i]
		} else if !sameType(first, ts[i]) && !(isKind(first, "BYTES") && isKind(ts[i], "STRING")) {
			return false
		}
	}
	return true
}

func text(v any, asBytesType bool) any {
	if asBytesType {
		return asBytes(v)
	}
	switch x := v.(type) {
	case []byte:
		return string(x)
	}
	return v
}

var functions map[string]*fnDef

func init() {
	num1 := func(f func(float64) float64) *fnDef {
		return &fnDef{sig: fixed(float64Type, "NUM"), run: func(c *call, vs []any, _ *env) (any, error) {
			return f(toFloat(vs[0])), nil
		}}
	}
	functions = map[string]*fnDef{
		// conditional
		"IF": {nulls: true, sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) != 3 || !argOK(ts[0], "BOOL") {
				return nil, sigErr(c, ts)
			}
			return unifyArgs(c, ts[1:])
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			if vs[0] == true {
				return coerce(vs[1], c.t), nil
			}
			return coerce(vs[2], c.t), nil
		}},
		"IFNULL": {nulls: true, sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) != 2 {
				return nil, sigErr(c, ts)
			}
			return unifyArgs(c, ts)
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			if vs[0] != nil {
				return coerce(vs[0], c.t), nil
			}
			return coerce(vs[1], c.t), nil
		}},
		"COALESCE": {nulls: true, sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) == 0 {
				return nil, sigErr(c, ts)
			}
			return unifyArgs(c, ts)
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			for _, v := range vs {
				if v != nil {
					return coerce(v, c.t), nil
				}
			}
			return nil, nil
		}},
		"NULLIF": {nulls: true, sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) != 2 {
				return nil, sigErr(c, ts)
			}
			if _, err := unifyArgs(c, ts); err != nil {
				return nil, err
			}
			return firstType(ts), nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			if vs[0] != nil && vs[1] != nil {
				if cmp, ok := compare(vs[0], vs[1]); ok && cmp == 0 {
					return nil, nil
				}
			}
			return vs[0], nil
		}},
		// conversions from Bigtable's bytes
		"TO_INT64": {sig: fixed(int64Type, "BYTES|STRING"), run: func(c *call, vs []any, _ *env) (any, error) {
			b := asBytes(vs[0])
			if len(b) != 8 {
				return nil, fmt.Errorf("TO_INT64: a value of %d bytes is not the big-endian bytes of an INT64", len(b))
			}
			return int64(binary.BigEndian.Uint64(b)), nil
		}},
		"TO_FLOAT64": {sig: fixed(float64Type, "BYTES|STRING"), run: func(c *call, vs []any, _ *env) (any, error) {
			b := asBytes(vs[0])
			if len(b) != 8 {
				return nil, fmt.Errorf("TO_FLOAT64: a value of %d bytes is not the big-endian bytes of a FLOAT64", len(b))
			}
			return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
		}},
		"TO_FLOAT32": {sig: fixed(float32Type, "BYTES|STRING"), run: func(c *call, vs []any, _ *env) (any, error) {
			b := asBytes(vs[0])
			if len(b) != 4 {
				return nil, fmt.Errorf("TO_FLOAT32: a value of %d bytes is not the big-endian bytes of a FLOAT32", len(b))
			}
			return float64(math.Float32frombits(binary.BigEndian.Uint32(b))), nil
		}},
		"SAFE_CONVERT_BYTES_TO_STRING": {sig: fixed(stringType, "BYTES"), run: func(c *call, vs []any, _ *env) (any, error) {
			return strings.ToValidUTF8(string(asBytes(vs[0])), "�"), nil
		}},
		// strings and bytes
		"CONCAT": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) == 0 {
				return nil, sigErr(c, ts)
			}
			out := firstType(ts)
			for _, t := range ts {
				if !argOK(t, "STRING|BYTES") {
					return nil, sigErr(c, ts)
				}
				if t != nil && isKind(t, "BYTES") {
					out = bytesType
				}
			}
			return out, nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			var b []byte
			for _, v := range vs {
				b = append(b, asBytes(v)...)
			}
			return text(b, isKind(c.t, "BYTES")), nil
		}},
		"LENGTH": {sig: fixed(int64Type, "STRING|BYTES"), run: func(c *call, vs []any, _ *env) (any, error) {
			if s, ok := vs[0].(string); ok {
				return int64(utf8.RuneCountInString(s)), nil
			}
			return int64(len(asBytes(vs[0]))), nil
		}},
		"CHAR_LENGTH": {sig: fixed(int64Type, "STRING"), run: func(c *call, vs []any, _ *env) (any, error) {
			return int64(utf8.RuneCountInString(vs[0].(string))), nil
		}},
		"BYTE_LENGTH": {sig: fixed(int64Type, "STRING|BYTES"), run: func(c *call, vs []any, _ *env) (any, error) {
			return int64(len(asBytes(vs[0]))), nil
		}},
		"LOWER": {sig: textual(1), run: func(c *call, vs []any, _ *env) (any, error) {
			if s, ok := vs[0].(string); ok {
				return strings.ToLower(s), nil
			}
			return asciiCase(vs[0].([]byte), false), nil
		}},
		"UPPER": {sig: textual(1), run: func(c *call, vs []any, _ *env) (any, error) {
			if s, ok := vs[0].(string); ok {
				return strings.ToUpper(s), nil
			}
			return asciiCase(vs[0].([]byte), true), nil
		}},
		"STARTS_WITH": {sig: textBool, run: func(c *call, vs []any, _ *env) (any, error) {
			return strings.HasPrefix(string(asBytes(vs[0])), string(asBytes(vs[1]))), nil
		}},
		"ENDS_WITH": {sig: textBool, run: func(c *call, vs []any, _ *env) (any, error) {
			return strings.HasSuffix(string(asBytes(vs[0])), string(asBytes(vs[1]))), nil
		}},
		"STRPOS": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if _, err := textBool(c, ts); err != nil {
				return nil, err
			}
			return int64Type, nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			if s, ok := vs[0].(string); ok {
				i := strings.Index(s, string(asBytes(vs[1])))
				if i < 0 {
					return int64(0), nil
				}
				return int64(utf8.RuneCountInString(s[:i]) + 1), nil
			}
			return int64(strings.Index(string(asBytes(vs[0])), string(asBytes(vs[1]))) + 1), nil
		}},
		"SUBSTR":    {sig: textual(2, "INT64", "INT64"), run: substr},
		"SUBSTRING": {sig: textual(2, "INT64", "INT64"), run: substr},
		"LEFT": {sig: textual(2, "INT64"), run: func(c *call, vs []any, e *env) (any, error) {
			if vs[1].(int64) < 0 {
				return nil, fmt.Errorf("LEFT: the length must not be negative")
			}
			return substr(c, []any{vs[0], int64(1), vs[1]}, e)
		}},
		"RIGHT": {sig: textual(2, "INT64"), run: func(c *call, vs []any, e *env) (any, error) {
			n := vs[1].(int64)
			if n < 0 {
				return nil, fmt.Errorf("RIGHT: the length must not be negative")
			}
			r := units(vs[0])
			if n > int64(len(r)) {
				n = int64(len(r))
			}
			return fromUnits(r[int64(len(r))-n:], vs[0]), nil
		}},
		"REVERSE": {sig: textual(1), run: func(c *call, vs []any, _ *env) (any, error) {
			r := units(vs[0])
			out := make([]rune, len(r))
			for i := range r {
				out[len(r)-1-i] = r[i]
			}
			return fromUnits(out, vs[0]), nil
		}},
		"REPLACE": {sig: textual(3, "STRING|BYTES", "STRING|BYTES"), run: func(c *call, vs []any, _ *env) (any, error) {
			from := string(asBytes(vs[1]))
			if from == "" {
				return vs[0], nil
			}
			return text(strings.ReplaceAll(string(asBytes(vs[0])), from, string(asBytes(vs[2]))), isKind(c.t, "BYTES")), nil
		}},
		"REPEAT": {sig: textual(2, "INT64"), run: func(c *call, vs []any, _ *env) (any, error) {
			n := vs[1].(int64)
			if n < 0 {
				return nil, fmt.Errorf("REPEAT: the count must not be negative")
			}
			return text(strings.Repeat(string(asBytes(vs[0])), int(n)), isKind(c.t, "BYTES")), nil
		}},
		"TRIM":  {sig: textual(1, "STRING|BYTES"), run: trimmer(strings.Trim)},
		"LTRIM": {sig: textual(1, "STRING|BYTES"), run: trimmer(strings.TrimLeft)},
		"RTRIM": {sig: textual(1, "STRING|BYTES"), run: trimmer(strings.TrimRight)},
		"LPAD":  {sig: textual(2, "INT64", "STRING|BYTES"), run: padder(true)},
		"RPAD":  {sig: textual(2, "INT64", "STRING|BYTES"), run: padder(false)},
		"SPLIT": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			t, err := textual(1, "STRING|BYTES")(c, ts)
			if err != nil {
				return nil, err
			}
			return arrayType(t), nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			sep := ","
			if len(vs) > 1 {
				sep = string(asBytes(vs[1]))
			}
			s := string(asBytes(vs[0]))
			isB := isKind(c.t.GetArrayType().GetElementType(), "BYTES")
			var parts []string
			if sep == "" {
				for _, r := range s {
					parts = append(parts, string(r))
				}
				if s == "" {
					parts = []string{""}
				}
			} else {
				parts = strings.Split(s, sep)
			}
			out := make([]any, len(parts))
			for i, p := range parts {
				out[i] = text(p, isB)
			}
			return out, nil
		}},
		"REGEXP_CONTAINS": {sig: textBool, run: func(c *call, vs []any, _ *env) (any, error) {
			re, err := c.regexp(vs[1])
			if err != nil {
				return nil, err
			}
			return re.Match(asBytes(vs[0])), nil
		}},
		"REGEXP_EXTRACT": {sig: textual(2, "STRING|BYTES"), run: func(c *call, vs []any, _ *env) (any, error) {
			re, err := c.regexp(vs[1])
			if err != nil {
				return nil, err
			}
			if re.NumSubexp() > 1 {
				return nil, fmt.Errorf("Regular expressions passed into extraction functions must not have more than 1 capturing group")
			}
			m := re.FindSubmatch(asBytes(vs[0]))
			if m == nil {
				return nil, nil
			}
			return text(m[len(m)-1], isKind(c.t, "BYTES")), nil
		}},
		"REGEXP_EXTRACT_ALL": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			t, err := textual(2, "STRING|BYTES")(c, ts)
			if err != nil {
				return nil, err
			}
			return arrayType(t), nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			re, err := c.regexp(vs[1])
			if err != nil {
				return nil, err
			}
			out := []any{}
			for _, m := range re.FindAllSubmatch(asBytes(vs[0]), -1) {
				out = append(out, text(m[len(m)-1], isKind(c.t.GetArrayType().GetElementType(), "BYTES")))
			}
			return out, nil
		}},
		"REGEXP_REPLACE": {sig: textual(3, "STRING|BYTES", "STRING|BYTES"), run: func(c *call, vs []any, _ *env) (any, error) {
			re, err := c.regexp(vs[1])
			if err != nil {
				return nil, err
			}
			repl := regexp.MustCompile(`\\(\d)`).ReplaceAllString(string(asBytes(vs[2])), "$${$1}")
			return text(re.ReplaceAll(asBytes(vs[0]), []byte(repl)), isKind(c.t, "BYTES")), nil
		}},
		"TO_HEX": {sig: fixed(stringType, "BYTES"), run: func(c *call, vs []any, _ *env) (any, error) {
			return hex.EncodeToString(asBytes(vs[0])), nil
		}},
		"FROM_HEX": {sig: fixed(bytesType, "STRING"), run: func(c *call, vs []any, _ *env) (any, error) {
			s := vs[0].(string)
			if len(s)%2 == 1 {
				s = "0" + s
			}
			b, err := hex.DecodeString(s)
			if err != nil {
				return nil, fmt.Errorf("FROM_HEX: %q is not hexadecimal", vs[0])
			}
			return b, nil
		}},
		"TO_BASE64": {sig: fixed(stringType, "BYTES"), run: func(c *call, vs []any, _ *env) (any, error) {
			return base64.StdEncoding.EncodeToString(asBytes(vs[0])), nil
		}},
		"FROM_BASE64": {sig: fixed(bytesType, "STRING"), run: func(c *call, vs []any, _ *env) (any, error) {
			b, err := base64.StdEncoding.DecodeString(vs[0].(string))
			if err != nil {
				return nil, fmt.Errorf("FROM_BASE64: %v", err)
			}
			return b, nil
		}},
		// math
		"ABS": {sig: fixed(nil, "NUM"), run: func(c *call, vs []any, _ *env) (any, error) {
			if n, ok := vs[0].(int64); ok {
				if n == math.MinInt64 {
					return nil, fmt.Errorf("int64 overflow: ABS(%d)", n)
				}
				if n < 0 {
					return -n, nil
				}
				return n, nil
			}
			return math.Abs(toFloat(vs[0])), nil
		}},
		"SIGN": {sig: fixed(nil, "NUM"), run: func(c *call, vs []any, _ *env) (any, error) {
			if n, ok := vs[0].(int64); ok {
				switch {
				case n > 0:
					return int64(1), nil
				case n < 0:
					return int64(-1), nil
				}
				return int64(0), nil
			}
			f := toFloat(vs[0])
			switch {
			case f > 0:
				return 1.0, nil
			case f < 0:
				return -1.0, nil
			}
			return f, nil
		}},
		"CEIL":    num1(math.Ceil),
		"CEILING": num1(math.Ceil),
		"FLOOR":   num1(math.Floor),
		"SQRT": {sig: fixed(float64Type, "NUM"), run: func(c *call, vs []any, _ *env) (any, error) {
			f := toFloat(vs[0])
			if f < 0 {
				return nil, fmt.Errorf("Argument to SQRT cannot be negative: %v", f)
			}
			return math.Sqrt(f), nil
		}},
		"EXP": num1(math.Exp),
		"LN": {sig: fixed(float64Type, "NUM"), run: func(c *call, vs []any, _ *env) (any, error) {
			f := toFloat(vs[0])
			if f <= 0 {
				return nil, fmt.Errorf("Argument to LN must be positive: %v", f)
			}
			return math.Log(f), nil
		}},
		"LOG10": {sig: fixed(float64Type, "NUM"), run: func(c *call, vs []any, _ *env) (any, error) {
			f := toFloat(vs[0])
			if f <= 0 {
				return nil, fmt.Errorf("Argument to LOG10 must be positive: %v", f)
			}
			return math.Log10(f), nil
		}},
		"LOG": {sig: ranged(float64Type, 1, "NUM", "NUM"), run: func(c *call, vs []any, _ *env) (any, error) {
			f := toFloat(vs[0])
			if f <= 0 {
				return nil, fmt.Errorf("Argument to LOG must be positive: %v", f)
			}
			if len(vs) == 2 {
				return math.Log(f) / math.Log(toFloat(vs[1])), nil
			}
			return math.Log(f), nil
		}},
		"POW":   {sig: fixed(float64Type, "NUM", "NUM"), run: pow},
		"POWER": {sig: fixed(float64Type, "NUM", "NUM"), run: pow},
		"ROUND": {sig: ranged(float64Type, 1, "NUM", "INT64"), run: func(c *call, vs []any, _ *env) (any, error) {
			return roundTo(toFloat(vs[0]), vs, math.Round), nil
		}},
		"TRUNC": {sig: ranged(float64Type, 1, "NUM", "INT64"), run: func(c *call, vs []any, _ *env) (any, error) {
			return roundTo(toFloat(vs[0]), vs, math.Trunc), nil
		}},
		"MOD": {sig: fixed(int64Type, "INT64", "INT64"), run: func(c *call, vs []any, _ *env) (any, error) {
			b := vs[1].(int64)
			if b == 0 {
				return nil, fmt.Errorf("division by zero: MOD(%d, 0)", vs[0])
			}
			if b == -1 {
				return int64(0), nil
			}
			return vs[0].(int64) % b, nil
		}},
		"DIV": {sig: fixed(int64Type, "INT64", "INT64"), run: func(c *call, vs []any, _ *env) (any, error) {
			a, b := vs[0].(int64), vs[1].(int64)
			if b == 0 {
				return nil, fmt.Errorf("division by zero: DIV(%d, 0)", a)
			}
			if a == math.MinInt64 && b == -1 {
				return nil, fmt.Errorf("int64 overflow: DIV(%d, -1)", a)
			}
			return a / b, nil
		}},
		"SAFE_DIVIDE": {sig: fixed(float64Type, "NUM", "NUM"), run: func(c *call, vs []any, _ *env) (any, error) {
			if toFloat(vs[1]) == 0 {
				return nil, nil
			}
			return toFloat(vs[0]) / toFloat(vs[1]), nil
		}},
		"IEEE_DIVIDE": {sig: fixed(float64Type, "NUM", "NUM"), run: func(c *call, vs []any, _ *env) (any, error) {
			a, b := toFloat(vs[0]), toFloat(vs[1])
			if b == 0 {
				switch {
				case a == 0 || math.IsNaN(a):
					return math.NaN(), nil
				case a > 0:
					return math.Inf(1), nil
				}
				return math.Inf(-1), nil
			}
			return a / b, nil
		}},
		"IS_NAN": {sig: fixed(boolType, "NUM"), run: func(c *call, vs []any, _ *env) (any, error) { return math.IsNaN(toFloat(vs[0])), nil }},
		"IS_INF": {sig: fixed(boolType, "NUM"), run: func(c *call, vs []any, _ *env) (any, error) { return math.IsInf(toFloat(vs[0]), 0), nil }},
		"GREATEST": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			return unifyArgs(c, ts)
		}, run: func(c *call, vs []any, _ *env) (any, error) { return extreme(c, vs, 1) }},
		"LEAST": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			return unifyArgs(c, ts)
		}, run: func(c *call, vs []any, _ *env) (any, error) { return extreme(c, vs, -1) }},
		// timestamps
		"CURRENT_TIMESTAMP": {sig: fixed(timestampType), run: func(c *call, vs []any, e *env) (any, error) { return e.now, nil }},
		"TIMESTAMP": {sig: ranged(timestampType, 1, "STRING|TIMESTAMP|DATE", "STRING"), run: func(c *call, vs []any, _ *env) (any, error) {
			loc, err := zone(vs, 1)
			if err != nil {
				return nil, err
			}
			if t, ok := vs[0].(time.Time); ok {
				if isKind(c.argTypes[0], "DATE") {
					return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc).UTC(), nil
				}
				return t, nil
			}
			return parseTimestamp(vs[0].(string), loc)
		}},
		"TIMESTAMP_MICROS":            {sig: fixed(timestampType, "INT64"), run: fromUnix(time.Microsecond)},
		"TIMESTAMP_MILLIS":            {sig: fixed(timestampType, "INT64"), run: fromUnix(time.Millisecond)},
		"TIMESTAMP_SECONDS":           {sig: fixed(timestampType, "INT64"), run: fromUnix(time.Second)},
		"TIMESTAMP_FROM_UNIX_MICROS":  {sig: fixed(timestampType, "INT64|TIMESTAMP"), run: fromUnix(time.Microsecond)},
		"TIMESTAMP_FROM_UNIX_MILLIS":  {sig: fixed(timestampType, "INT64|TIMESTAMP"), run: fromUnix(time.Millisecond)},
		"TIMESTAMP_FROM_UNIX_SECONDS": {sig: fixed(timestampType, "INT64|TIMESTAMP"), run: fromUnix(time.Second)},
		"UNIX_MICROS": {sig: fixed(int64Type, "TIMESTAMP"), run: func(c *call, vs []any, _ *env) (any, error) {
			return vs[0].(time.Time).UnixMicro(), nil
		}},
		"UNIX_MILLIS": {sig: fixed(int64Type, "TIMESTAMP"), run: func(c *call, vs []any, _ *env) (any, error) {
			return floorDiv(vs[0].(time.Time).UnixMicro(), 1000), nil
		}},
		"UNIX_SECONDS": {sig: fixed(int64Type, "TIMESTAMP"), run: func(c *call, vs []any, _ *env) (any, error) {
			return floorDiv(vs[0].(time.Time).UnixMicro(), 1000000), nil
		}},
		"TIMESTAMP_ADD": {sig: intervalSig, run: func(c *call, vs []any, _ *env) (any, error) { return addInterval(vs[0].(time.Time), vs[1].(intervalValue), 1) }},
		"TIMESTAMP_SUB": {sig: intervalSig, run: func(c *call, vs []any, _ *env) (any, error) { return addInterval(vs[0].(time.Time), vs[1].(intervalValue), -1) }},
		"TIMESTAMP_DIFF": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) != 3 || !argOK(ts[0], "TIMESTAMP") || !argOK(ts[1], "TIMESTAMP") || !isPart(c.args[2]) {
				return nil, sigErr(c, ts)
			}
			return int64Type, nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			d := vs[0].(time.Time).Sub(vs[1].(time.Time))
			u, err := unitDuration(vs[2].(string))
			if err != nil {
				return nil, err
			}
			return int64(d / u), nil
		}},
		"TIMESTAMP_TRUNC": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) < 2 || len(ts) > 3 || !argOK(ts[0], "TIMESTAMP") || !isPart(c.args[1]) || len(ts) == 3 && !argOK(ts[2], "STRING") {
				return nil, sigErr(c, ts)
			}
			return timestampType, nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			loc, err := zone(vs, 2)
			if err != nil {
				return nil, err
			}
			return truncTimestamp(vs[0].(time.Time).In(loc), vs[1].(string))
		}},
		"FORMAT_TIMESTAMP": {sig: ranged(stringType, 2, "STRING", "TIMESTAMP", "STRING"), run: func(c *call, vs []any, _ *env) (any, error) {
			loc, err := zone(vs, 2)
			if err != nil {
				return nil, err
			}
			return formatTimestamp(vs[0].(string), vs[1].(time.Time).In(loc))
		}},
		"STRING": {sig: ranged(stringType, 1, "TIMESTAMP", "STRING"), run: func(c *call, vs []any, _ *env) (any, error) {
			loc, err := zone(vs, 1)
			if err != nil {
				return nil, err
			}
			return timestampString(vs[0].(time.Time).In(loc)), nil
		}},
		// maps
		"MAP_KEYS": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) != 1 || !argOK(ts[0], "MAP") {
				return nil, sigErr(c, ts)
			}
			return arrayType(ts[0].GetMapType().GetKeyType()), nil
		}, run: func(c *call, vs []any, _ *env) (any, error) { return append([]any{}, vs[0].(*mapValue).keys...), nil }},
		"MAP_VALUES": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) != 1 || !argOK(ts[0], "MAP") {
				return nil, sigErr(c, ts)
			}
			return arrayType(ts[0].GetMapType().GetValueType()), nil
		}, run: func(c *call, vs []any, _ *env) (any, error) { return append([]any{}, vs[0].(*mapValue).vals...), nil }},
		"MAP_ENTRIES": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) != 1 || !argOK(ts[0], "MAP") {
				return nil, sigErr(c, ts)
			}
			m := ts[0].GetMapType()
			return arrayType(structType([]string{"key", "value"}, []*btpb.Type{m.GetKeyType(), m.GetValueType()})), nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			m := vs[0].(*mapValue)
			out := make([]any, len(m.keys))
			for i := range m.keys {
				out[i] = []any{m.keys[i], m.vals[i]}
			}
			return out, nil
		}},
		"MAP_CONTAINS_KEY": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) != 2 || !argOK(ts[0], "MAP") || ts[0] != nil && !assignable(ts[1], ts[0].GetMapType().GetKeyType()) {
				return nil, sigErr(c, ts)
			}
			return boolType, nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			_, ok := vs[0].(*mapValue).get(vs[1])
			return ok, nil
		}},
		"MAP_EMPTY": {sig: fixed(boolType, "MAP"), run: func(c *call, vs []any, _ *env) (any, error) {
			return len(vs[0].(*mapValue).keys) == 0, nil
		}},
		// arrays
		"ARRAY_LENGTH": {sig: fixed(int64Type, "ARRAY"), run: func(c *call, vs []any, _ *env) (any, error) { return int64(len(vs[0].([]any))), nil }},
		"ARRAY_REVERSE": {sig: fixed(nil, "ARRAY"), run: func(c *call, vs []any, _ *env) (any, error) {
			a := vs[0].([]any)
			out := make([]any, len(a))
			for i := range a {
				out[len(a)-1-i] = a[i]
			}
			return out, nil
		}},
		"ARRAY_FIRST": {sig: arrayElem, run: func(c *call, vs []any, _ *env) (any, error) {
			a := vs[0].([]any)
			if len(a) == 0 {
				return nil, fmt.Errorf("ARRAY_FIRST cannot get the first element of an empty array")
			}
			return a[0], nil
		}},
		"ARRAY_LAST": {sig: arrayElem, run: func(c *call, vs []any, _ *env) (any, error) {
			a := vs[0].([]any)
			if len(a) == 0 {
				return nil, fmt.Errorf("ARRAY_LAST cannot get the last element of an empty array")
			}
			return a[len(a)-1], nil
		}},
		"ARRAY_INCLUDES": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) != 2 || !argOK(ts[0], "ARRAY") {
				return nil, sigErr(c, ts)
			}
			return boolType, nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			for _, v := range vs[0].([]any) {
				if v != nil {
					if cmp, ok := compare(v, vs[1]); ok && cmp == 0 {
						return true, nil
					}
				}
			}
			return false, nil
		}},
		"ARRAY_CONCAT": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			for _, t := range ts {
				if !argOK(t, "ARRAY") {
					return nil, sigErr(c, ts)
				}
			}
			return unifyArgs(c, ts)
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			var out []any
			for _, v := range vs {
				out = append(out, v.([]any)...)
			}
			return append([]any{}, out...), nil
		}},
		"ARRAY_TO_STRING": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) < 2 || len(ts) > 3 || !argOK(ts[0], "ARRAY") {
				return nil, sigErr(c, ts)
			}
			et := stringType
			if ts[0] != nil {
				et = ts[0].GetArrayType().GetElementType()
			}
			if !isKind(et, "STRING", "BYTES") {
				return nil, sigErr(c, ts)
			}
			return et, nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			var parts []string
			for _, v := range vs[0].([]any) {
				if v == nil {
					if len(vs) == 3 && vs[2] != nil {
						parts = append(parts, string(asBytes(vs[2])))
					}
					continue
				}
				parts = append(parts, string(asBytes(v)))
			}
			return text(strings.Join(parts, string(asBytes(vs[1]))), isKind(c.t, "BYTES")), nil
		}},
		// JSON
		"JSON_VALUE":          {sig: ranged(stringType, 1, "STRING", "STRING"), run: jsonValue},
		"JSON_EXTRACT_SCALAR": {sig: ranged(stringType, 1, "STRING", "STRING"), run: jsonValue},
	}
}

func unifyArgs(c *call, ts []*btpb.Type) (*btpb.Type, error) {
	var out *btpb.Type
	for _, t := range ts {
		if t == nil {
			continue
		}
		if out == nil {
			out = t
			continue
		}
		u, ok := unify(out, t)
		if !ok {
			return nil, sigErr(c, ts)
		}
		out = u
	}
	if out == nil {
		out = int64Type
	}
	return out, nil
}

func textBool(c *call, ts []*btpb.Type) (*btpb.Type, error) {
	if len(ts) != 2 || !argOK(ts[0], "STRING|BYTES") || !argOK(ts[1], "STRING|BYTES") || !sameText(ts, 0, 1) {
		return nil, sigErr(c, ts)
	}
	return boolType, nil
}

func arrayElem(c *call, ts []*btpb.Type) (*btpb.Type, error) {
	if len(ts) != 1 || !argOK(ts[0], "ARRAY") {
		return nil, sigErr(c, ts)
	}
	if ts[0] == nil {
		return int64Type, nil
	}
	return ts[0].GetArrayType().GetElementType(), nil
}

func (c *call) regexp(pat any) (*regexp.Regexp, error) {
	if c.re != nil {
		return c.re, nil
	}
	re, err := regexp.Compile(string(asBytes(pat)))
	if err != nil {
		return nil, fmt.Errorf("Cannot parse regular expression: %v", err)
	}
	if _, ok := c.args[1].(*literal); ok {
		c.re = re
	}
	return re, nil
}

func asciiCase(b []byte, upper bool) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		switch {
		case upper && c >= 'a' && c <= 'z':
			c -= 32
		case !upper && c >= 'A' && c <= 'Z':
			c += 32
		}
		out[i] = c
	}
	return out
}

// units is a STRING's characters or a BYTES' bytes.
func units(v any) []rune {
	if s, ok := v.(string); ok {
		return []rune(s)
	}
	b := asBytes(v)
	out := make([]rune, len(b))
	for i, c := range b {
		out[i] = rune(c)
	}
	return out
}

func fromUnits(r []rune, like any) any {
	if _, ok := like.(string); ok {
		return string(r)
	}
	out := make([]byte, len(r))
	for i, c := range r {
		out[i] = byte(c)
	}
	return out
}

func substr(c *call, vs []any, _ *env) (any, error) {
	r := units(vs[0])
	pos := vs[1].(int64)
	n := int64(len(r))
	switch {
	case pos > 0:
		pos--
	case pos < 0:
		pos = n + pos
		if pos < 0 {
			pos = 0
		}
	}
	if pos > n {
		pos = n
	}
	end := n
	if len(vs) > 2 {
		if vs[2] == nil {
			return nil, nil
		}
		l := vs[2].(int64)
		if l < 0 {
			return nil, fmt.Errorf("Third argument in SUBSTR() cannot be negative")
		}
		if pos+l < end {
			end = pos + l
		}
	}
	return fromUnits(r[pos:end], vs[0]), nil
}

func trimmer(f func(string, string) string) func(*call, []any, *env) (any, error) {
	return func(c *call, vs []any, _ *env) (any, error) {
		cut := " \t\n\r\v\f"
		if len(vs) > 1 {
			cut = string(asBytes(vs[1]))
		}
		if _, ok := vs[0].(string); ok && len(vs) == 1 {
			return strings.TrimFunc(vs[0].(string), func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f' }), nil
		}
		return text(f(string(asBytes(vs[0])), cut), isKind(c.t, "BYTES")), nil
	}
}

func padder(left bool) func(*call, []any, *env) (any, error) {
	return func(c *call, vs []any, _ *env) (any, error) {
		n := vs[1].(int64)
		if n < 0 {
			return nil, fmt.Errorf("the pad length must not be negative")
		}
		r := units(vs[0])
		pad := []rune(" ")
		if len(vs) > 2 {
			pad = units(vs[2])
		}
		if int64(len(r)) >= n {
			return fromUnits(r[:n], vs[0]), nil
		}
		if len(pad) == 0 {
			return nil, fmt.Errorf("the pattern must not be empty")
		}
		var fill []rune
		for int64(len(fill)+len(r)) < n {
			fill = append(fill, pad[len(fill)%len(pad)])
		}
		if left {
			return fromUnits(append(fill, r...), vs[0]), nil
		}
		return fromUnits(append(r, fill...), vs[0]), nil
	}
}

func pow(c *call, vs []any, _ *env) (any, error) {
	return math.Pow(toFloat(vs[0]), toFloat(vs[1])), nil
}

func roundTo(f float64, vs []any, how func(float64) float64) float64 {
	if len(vs) < 2 {
		return how(f)
	}
	p := math.Pow(10, float64(vs[1].(int64)))
	return how(f*p) / p
}

func extreme(c *call, vs []any, sign int) (any, error) {
	var best any
	for _, v := range vs {
		if v == nil {
			return nil, nil
		}
		if f, ok := v.(float64); ok && math.IsNaN(f) {
			return math.NaN(), nil
		}
		if best == nil {
			best = v
			continue
		}
		if cmp, _ := compare(v, best); cmp*sign > 0 {
			best = v
		}
	}
	return coerce(best, c.t), nil
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

func fromUnix(unit time.Duration) func(*call, []any, *env) (any, error) {
	return func(c *call, vs []any, _ *env) (any, error) {
		if t, ok := vs[0].(time.Time); ok {
			return t, nil
		}
		n := vs[0].(int64)
		per := int64(unit / time.Microsecond)
		if n > math.MaxInt64/per || n < math.MinInt64/per {
			return nil, fmt.Errorf("timestamp out of range: %d", n)
		}
		return time.UnixMicro(n * per).UTC(), nil
	}
}

func isPart(e expr) bool {
	l, ok := e.(*literal)
	return ok && l.t == datePartType
}

func intervalSig(c *call, ts []*btpb.Type) (*btpb.Type, error) {
	if len(ts) != 2 || !argOK(ts[0], "TIMESTAMP") {
		return nil, sigErr(c, ts)
	}
	if _, ok := c.args[1].(*interval); !ok {
		return nil, fmt.Errorf("%s takes an INTERVAL as its second argument", c.name)
	}
	return timestampType, nil
}

func unitDuration(u string) (time.Duration, error) {
	switch u {
	case "NANOSECOND":
		return time.Nanosecond, nil
	case "MICROSECOND":
		return time.Microsecond, nil
	case "MILLISECOND":
		return time.Millisecond, nil
	case "SECOND":
		return time.Second, nil
	case "MINUTE":
		return time.Minute, nil
	case "HOUR":
		return time.Hour, nil
	case "DAY":
		return 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("Unsupported date part %s for a TIMESTAMP", u)
}

func addInterval(t time.Time, iv intervalValue, sign int64) (any, error) {
	u, err := unitDuration(iv.unit)
	if err != nil {
		return nil, err
	}
	return t.Add(time.Duration(sign*iv.n) * u), nil
}

func truncTimestamp(t time.Time, part string) (any, error) {
	y, m, d := t.Date()
	loc := t.Location()
	switch part {
	case "MICROSECOND":
		return t.Truncate(time.Microsecond).UTC(), nil
	case "MILLISECOND", "SECOND", "MINUTE", "HOUR":
		u, _ := unitDuration(part)
		// Truncate in the zone, so an hour boundary is the zone's.
		_, off := t.Zone()
		shifted := t.Add(time.Duration(off) * time.Second).Truncate(u)
		return shifted.Add(-time.Duration(off) * time.Second).UTC(), nil
	case "DAY":
		return time.Date(y, m, d, 0, 0, 0, 0, loc).UTC(), nil
	case "WEEK":
		return time.Date(y, m, d-int(t.Weekday()), 0, 0, 0, 0, loc).UTC(), nil
	case "MONTH":
		return time.Date(y, m, 1, 0, 0, 0, 0, loc).UTC(), nil
	case "QUARTER":
		return time.Date(y, m-(m-1)%3, 1, 0, 0, 0, 0, loc).UTC(), nil
	case "YEAR":
		return time.Date(y, 1, 1, 0, 0, 0, 0, loc).UTC(), nil
	}
	return nil, unsupportedErr{"TIMESTAMP_TRUNC to " + part}
}

func zone(vs []any, i int) (*time.Location, error) {
	if len(vs) <= i || vs[i] == nil {
		return time.UTC, nil
	}
	name := vs[i].(string)
	if strings.HasPrefix(name, "+") || strings.HasPrefix(name, "-") || strings.HasPrefix(name, "UTC+") || strings.HasPrefix(name, "UTC-") {
		s := strings.TrimPrefix(name, "UTC")
		sign := 1
		if s[0] == '-' {
			sign = -1
		}
		hm := strings.SplitN(s[1:], ":", 2)
		h, err := strconv.Atoi(hm[0])
		if err != nil {
			return nil, fmt.Errorf("Invalid time zone: %s", name)
		}
		m := 0
		if len(hm) == 2 {
			if m, err = strconv.Atoi(hm[1]); err != nil {
				return nil, fmt.Errorf("Invalid time zone: %s", name)
			}
		}
		return time.FixedZone(name, sign*(h*3600+m*60)), nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("Invalid time zone: %s", name)
	}
	return loc, nil
}

// parseTimestamp reads a canonical timestamp literal.
func parseTimestamp(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999Z07", "2006-01-02T15:04:05.999999999Z07"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	if i := strings.LastIndexByte(s, ' '); i > 0 {
		if l, err := zone([]any{s[i+1:]}, 0); err == nil {
			if t, err := parseTimestamp(s[:i], l); err == nil {
				return t, nil
			}
		}
	}
	for _, layout := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("Invalid timestamp: '%s'", s)
}

func timestampString(t time.Time) string {
	s := t.Format("2006-01-02 15:04:05.999999")
	_, off := t.Zone()
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	z := fmt.Sprintf("%s%02d", sign, off/3600)
	if off%3600 != 0 {
		z += fmt.Sprintf(":%02d", off%3600/60)
	}
	return s + z
}

// formatTimestamp is FORMAT_TIMESTAMP's format elements, the common ones.
func formatTimestamp(f string, t time.Time) (any, error) {
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		if f[i] != '%' || i+1 == len(f) {
			b.WriteByte(f[i])
			continue
		}
		i++
		switch f[i] {
		case 'Y':
			fmt.Fprintf(&b, "%04d", t.Year())
		case 'y':
			fmt.Fprintf(&b, "%02d", t.Year()%100)
		case 'm':
			fmt.Fprintf(&b, "%02d", int(t.Month()))
		case 'd':
			fmt.Fprintf(&b, "%02d", t.Day())
		case 'e':
			fmt.Fprintf(&b, "%2d", t.Day())
		case 'H':
			fmt.Fprintf(&b, "%02d", t.Hour())
		case 'I':
			fmt.Fprintf(&b, "%02d", (t.Hour()+11)%12+1)
		case 'M':
			fmt.Fprintf(&b, "%02d", t.Minute())
		case 'S':
			fmt.Fprintf(&b, "%02d", t.Second())
		case 'p':
			b.WriteString(t.Format("PM"))
		case 'j':
			fmt.Fprintf(&b, "%03d", t.YearDay())
		case 'a':
			b.WriteString(t.Format("Mon"))
		case 'A':
			b.WriteString(t.Format("Monday"))
		case 'b', 'h':
			b.WriteString(t.Format("Jan"))
		case 'B':
			b.WriteString(t.Format("January"))
		case 'F':
			b.WriteString(t.Format("2006-01-02"))
		case 'D':
			b.WriteString(t.Format("01/02/06"))
		case 'T':
			b.WriteString(t.Format("15:04:05"))
		case 'R':
			b.WriteString(t.Format("15:04"))
		case 'z':
			b.WriteString(t.Format("-0700"))
		case 'Z':
			b.WriteString(t.Format("MST"))
		case 's':
			fmt.Fprintf(&b, "%d", t.Unix())
		case 'u':
			wd := int(t.Weekday())
			if wd == 0 {
				wd = 7
			}
			fmt.Fprintf(&b, "%d", wd)
		case 'w':
			fmt.Fprintf(&b, "%d", int(t.Weekday()))
		case 'c':
			b.WriteString(t.Format("Mon Jan  2 15:04:05 2006"))
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case '%':
			b.WriteByte('%')
		case 'E':
			// %E<n>S, %E*S: seconds with fractional digits.
			j := i + 1
			for j < len(f) && f[j] != 'S' {
				j++
			}
			if j == len(f) {
				return nil, unsupportedErr{"the FORMAT_TIMESTAMP element %E" + f[i+1:]}
			}
			spec := f[i+1 : j]
			digits := 6
			if spec != "*" {
				n, err := strconv.Atoi(spec)
				if err != nil {
					return nil, unsupportedErr{"the FORMAT_TIMESTAMP element %E" + spec}
				}
				digits = n
			}
			fmt.Fprintf(&b, "%02d", t.Second())
			if digits > 0 {
				frac := fmt.Sprintf("%09d", t.Nanosecond())[:min(digits, 9)]
				if spec == "*" {
					frac = strings.TrimRight(frac, "0")
				}
				if frac != "" {
					b.WriteString("." + frac)
				}
			}
			i = j
		default:
			return nil, unsupportedErr{"the FORMAT_TIMESTAMP element %" + string(f[i])}
		}
	}
	return b.String(), nil
}

// jsonValue is JSON_VALUE's scalar at a JSONPath of member and [n] steps.
func jsonValue(c *call, vs []any, _ *env) (any, error) {
	var doc any
	if err := json.Unmarshal([]byte(vs[0].(string)), &doc); err != nil {
		return nil, nil
	}
	path := "$"
	if len(vs) > 1 {
		path = vs[1].(string)
	}
	if !strings.HasPrefix(path, "$") {
		return nil, fmt.Errorf("JSONPath must start with '$'")
	}
	rest := path[1:]
	for rest != "" {
		switch {
		case strings.HasPrefix(rest, "."):
			rest = rest[1:]
			end := strings.IndexAny(rest, ".[")
			if end < 0 {
				end = len(rest)
			}
			name := strings.Trim(rest[:end], `"`)
			rest = rest[end:]
			m, ok := doc.(map[string]any)
			if !ok {
				return nil, nil
			}
			doc = m[name]
		case strings.HasPrefix(rest, "["):
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return nil, fmt.Errorf("Invalid JSONPath: %s", path)
			}
			key := strings.Trim(rest[1:end], `'"`)
			rest = rest[end+1:]
			switch d := doc.(type) {
			case []any:
				n, err := strconv.Atoi(key)
				if err != nil || n < 0 || n >= len(d) {
					return nil, nil
				}
				doc = d[n]
			case map[string]any:
				doc = d[key]
			default:
				return nil, nil
			}
		default:
			return nil, fmt.Errorf("Invalid JSONPath: %s", path)
		}
	}
	switch d := doc.(type) {
	case string:
		return d, nil
	case float64:
		return strconv.FormatFloat(d, 'g', -1, 64), nil
	case bool:
		return strconv.FormatBool(d), nil
	}
	return nil, nil
}

// --- CAST and EXTRACT ---

type cast struct {
	x       expr
	to      string
	safe    bool
	literal bool // TIMESTAMP '...', DATE '...'
	from, t *btpb.Type
}

func (c *cast) String() string { return fmt.Sprintf("cast(%s,%s,%v)", c.x, c.to, c.safe) }

func (c *cast) check(s *scope) (*btpb.Type, error) {
	ft, err := c.x.check(s)
	if err != nil {
		return nil, err
	}
	tt, err := parseTypeName(c.to)
	if err != nil {
		return nil, err
	}
	c.from, c.t = ft, tt
	if l, ok := c.x.(*literal); ok && l.t == nil {
		return tt, nil
	}
	f, t := kind(ft), kind(tt)
	allowed := f == t || map[string]bool{
		"INT64>FLOAT64": true, "INT64>FLOAT32": true, "INT64>STRING": true, "INT64>BOOL": true,
		"FLOAT64>INT64": true, "FLOAT64>FLOAT32": true, "FLOAT64>STRING": true,
		"FLOAT32>INT64": true, "FLOAT32>FLOAT64": true, "FLOAT32>STRING": true,
		"BOOL>INT64": true, "BOOL>STRING": true,
		"STRING>INT64": true, "STRING>FLOAT64": true, "STRING>FLOAT32": true, "STRING>BOOL": true,
		"STRING>BYTES": true, "STRING>TIMESTAMP": true, "STRING>DATE": true,
		"BYTES>STRING": true, "TIMESTAMP>STRING": true, "DATE>STRING": true, "TIMESTAMP>DATE": true, "DATE>TIMESTAMP": true,
	}[f+">"+t]
	if !allowed {
		if f == "BYTES" && (t == "INT64" || t == "FLOAT64" || t == "FLOAT32") {
			return nil, fmt.Errorf("Invalid cast from BYTES to %s: use TO_%s to read big-endian bytes", t, t)
		}
		return nil, fmt.Errorf("Invalid cast from %s to %s", f, t)
	}
	return tt, nil
}

func (c *cast) eval(e *env) (any, error) {
	v, err := c.x.eval(e)
	if err != nil || v == nil {
		return nil, err
	}
	out, err := castValue(v, c.from, c.t)
	if err != nil && c.safe {
		return nil, nil
	}
	return out, err
}

func castValue(v any, from, to *btpb.Type) (any, error) {
	t := kind(to)
	switch x := v.(type) {
	case int64:
		switch t {
		case "INT64":
			return x, nil
		case "FLOAT64":
			return float64(x), nil
		case "FLOAT32":
			return float64(float32(x)), nil
		case "STRING":
			return strconv.FormatInt(x, 10), nil
		case "BOOL":
			return x != 0, nil
		}
	case float64:
		switch t {
		case "INT64":
			if math.IsNaN(x) || math.IsInf(x, 0) || x >= 9.223372036854775807e18 || x < -9.223372036854775808e18 {
				return nil, fmt.Errorf("Illegal conversion of non-finite or out-of-range floating point number to an integer: %v", x)
			}
			return int64(math.RoundToEven(x)), nil
		case "FLOAT64":
			return x, nil
		case "FLOAT32":
			return float64(float32(x)), nil
		case "STRING":
			bits := 64
			if isKind(from, "FLOAT32") {
				bits = 32
			}
			switch {
			case math.IsInf(x, 1):
				return "inf", nil
			case math.IsInf(x, -1):
				return "-inf", nil
			case math.IsNaN(x):
				return "nan", nil
			}
			return strconv.FormatFloat(x, 'g', -1, bits), nil
		}
	case bool:
		switch t {
		case "BOOL":
			return x, nil
		case "INT64":
			if x {
				return int64(1), nil
			}
			return int64(0), nil
		case "STRING":
			return strconv.FormatBool(x), nil
		}
	case string:
		s := strings.TrimSpace(x)
		switch t {
		case "STRING":
			return x, nil
		case "BYTES":
			return []byte(x), nil
		case "INT64":
			n, err := strconv.ParseInt(s, 0, 64)
			if err != nil {
				return nil, fmt.Errorf("Bad int64 value: %s", x)
			}
			return n, nil
		case "FLOAT64", "FLOAT32":
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return nil, fmt.Errorf("Bad double value: %s", x)
			}
			if t == "FLOAT32" {
				f = float64(float32(f))
			}
			return f, nil
		case "BOOL":
			switch strings.ToLower(s) {
			case "true":
				return true, nil
			case "false":
				return false, nil
			}
			return nil, fmt.Errorf("Bad bool value: %s", x)
		case "TIMESTAMP":
			return parseTimestamp(x, time.UTC)
		case "DATE":
			d, err := time.Parse("2006-01-02", s)
			if err != nil {
				return nil, fmt.Errorf("Invalid date: '%s'", x)
			}
			return d, nil
		}
	case []byte:
		switch t {
		case "BYTES":
			return x, nil
		case "STRING":
			if !utf8.Valid(x) {
				return nil, fmt.Errorf("Invalid cast of BYTES to STRING: the bytes are not valid UTF-8")
			}
			return string(x), nil
		}
	case time.Time:
		switch t {
		case "TIMESTAMP":
			return x, nil
		case "DATE":
			y, m, d := x.UTC().Date()
			return time.Date(y, m, d, 0, 0, 0, 0, time.UTC), nil
		case "STRING":
			if isKind(from, "DATE") {
				return x.Format("2006-01-02"), nil
			}
			return timestampString(x.UTC()), nil
		}
	case []any:
		if isKind(to, "ARRAY<") {
			et := to.GetArrayType().GetElementType()
			var ft *btpb.Type
			if from != nil {
				ft = from.GetArrayType().GetElementType()
			}
			out := make([]any, len(x))
			for i, el := range x {
				if el == nil {
					continue
				}
				cv, err := castValue(el, ft, et)
				if err != nil {
					return nil, err
				}
				out[i] = cv
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("Invalid cast from %s to %s", kind(from), t)
}

type extract struct {
	part string
	x    expr
	tz   expr
}

func (x *extract) String() string { return "extract(" + x.part + "," + x.x.String() + ")" }

func (x *extract) check(s *scope) (*btpb.Type, error) {
	t, err := x.x.check(s)
	if err != nil {
		return nil, err
	}
	if !isKind(t, "TIMESTAMP", "DATE") {
		return nil, fmt.Errorf("EXTRACT takes a TIMESTAMP or a DATE, not %s", kind(t))
	}
	if x.tz != nil {
		if zt, err := x.tz.check(s); err != nil || !isKind(zt, "STRING") {
			return nil, fmt.Errorf("AT TIME ZONE takes a STRING")
		}
	}
	if x.part == "DATE" {
		return dateType, nil
	}
	if !datePart[x.part] {
		return nil, fmt.Errorf("A valid date part name is required but found %s", x.part)
	}
	return int64Type, nil
}

func (x *extract) eval(e *env) (any, error) {
	v, err := x.x.eval(e)
	if err != nil || v == nil {
		return nil, err
	}
	t := v.(time.Time)
	if x.tz != nil {
		z, err := x.tz.eval(e)
		if err != nil {
			return nil, err
		}
		loc, err := zone([]any{z}, 0)
		if err != nil {
			return nil, err
		}
		t = t.In(loc)
	}
	switch x.part {
	case "NANOSECOND":
		return int64(t.Nanosecond()), nil
	case "MICROSECOND":
		return int64(t.Nanosecond() / 1000), nil
	case "MILLISECOND":
		return int64(t.Nanosecond() / 1000000), nil
	case "SECOND":
		return int64(t.Second()), nil
	case "MINUTE":
		return int64(t.Minute()), nil
	case "HOUR":
		return int64(t.Hour()), nil
	case "DAY":
		return int64(t.Day()), nil
	case "DAYOFWEEK":
		return int64(t.Weekday()) + 1, nil
	case "DAYOFYEAR":
		return int64(t.YearDay()), nil
	case "MONTH":
		return int64(t.Month()), nil
	case "QUARTER":
		return int64(t.Month()-1)/3 + 1, nil
	case "YEAR":
		return int64(t.Year()), nil
	case "ISOWEEK":
		_, w := t.ISOWeek()
		return int64(w), nil
	case "ISOYEAR":
		y, _ := t.ISOWeek()
		return int64(y), nil
	case "WEEK":
		jan1 := time.Date(t.Year(), 1, 1, 0, 0, 0, 0, t.Location())
		firstSunday := (7 - int(jan1.Weekday())) % 7
		return int64((t.YearDay() - 1 - firstSunday + 7) / 7), nil
	case "DATE":
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC), nil
	}
	return nil, unsupportedErr{"EXTRACT(" + x.part + ")"}
}
