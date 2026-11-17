package bigtablefront

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
)

// The scalar functions #1704 adds to GoogleSQL for Bigtable's: FORMAT and
// SOUNDEX and the small string functions
// (https://cloud.google.com/bigtable/docs/reference/sql/string_functions),
// the date functions
// (https://cloud.google.com/bigtable/docs/reference/sql/date_functions)
// and PARSE_TIMESTAMP
// (https://cloud.google.com/bigtable/docs/reference/sql/timestamp_functions),
// the vector functions TO_VECTOR32, TO_VECTOR64, COSINE_DISTANCE and
// EUCLIDEAN_DISTANCE, and the trigonometric and SAFE_ arithmetic functions
// (https://cloud.google.com/bigtable/docs/reference/sql/mathematical_functions).
//
// A DATE is held as a time.Time at midnight UTC, as sqlexpr.go holds it.
// This file sorts after sqlfuncs.go, so its init adds to the functions
// that file's init made.

func init() {
	trig := func(f func(float64) float64) *fnDef {
		return &fnDef{sig: fixed(float64Type, "NUM"), run: func(c *call, vs []any, _ *env) (any, error) {
			return f(toFloat(vs[0])), nil
		}}
	}
	// ACOS, ASIN and ATANH of an argument outside their domain are an
	// error, as the reference says; the rest return NaN or infinities.
	domain := func(f func(float64) float64, ok func(float64) bool) *fnDef {
		return &fnDef{sig: fixed(float64Type, "NUM"), run: func(c *call, vs []any, _ *env) (any, error) {
			x := toFloat(vs[0])
			if !math.IsNaN(x) && !ok(x) {
				return nil, fmt.Errorf("Argument to %s is out of range: %v", c.name, x)
			}
			return f(x), nil
		}}
	}
	inUnit := func(x float64) bool { return x >= -1 && x <= 1 }
	safe := func(op string) *fnDef {
		return &fnDef{sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) != 2 || !argOK(ts[0], "NUM") || !argOK(ts[1], "NUM") {
				return nil, sigErr(c, ts)
			}
			return unifyArgs(c, ts)
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			r, err := arith(op, coerce(vs[0], c.t), coerce(vs[1], c.t), isKind(c.t, "INT64"))
			if err != nil {
				return nil, nil // the SAFE_ functions return NULL instead of an error
			}
			if f, ok := r.(float64); ok && math.IsInf(f, 0) && !math.IsInf(toFloat(vs[0]), 0) && !math.IsInf(toFloat(vs[1]), 0) {
				return nil, nil // overflow
			}
			return r, nil
		}}
	}
	add := map[string]*fnDef{
		"SIN":   trig(math.Sin),
		"COS":   trig(math.Cos),
		"TAN":   trig(math.Tan),
		"SINH":  trig(math.Sinh),
		"COSH":  trig(math.Cosh),
		"TANH":  trig(math.Tanh),
		"ATAN":  trig(math.Atan),
		"ASIN":  domain(math.Asin, inUnit),
		"ACOS":  domain(math.Acos, inUnit),
		"ASINH": trig(math.Asinh),
		"ACOSH": domain(math.Acosh, func(x float64) bool { return x >= 1 }),
		"ATANH": domain(math.Atanh, func(x float64) bool { return x > -1 && x < 1 }),
		"COT":   domain(func(x float64) float64 { return 1 / math.Tan(x) }, func(x float64) bool { return x != 0 }),
		"CSC":   domain(func(x float64) float64 { return 1 / math.Sin(x) }, func(x float64) bool { return x != 0 }),
		"SEC":   trig(func(x float64) float64 { return 1 / math.Cos(x) }),
		"COTH":  domain(func(x float64) float64 { return 1 / math.Tanh(x) }, func(x float64) bool { return x != 0 }),
		"CSCH":  domain(func(x float64) float64 { return 1 / math.Sinh(x) }, func(x float64) bool { return x != 0 }),
		"SECH":  trig(func(x float64) float64 { return 1 / math.Cosh(x) }),
		"ATAN2": {sig: fixed(float64Type, "NUM", "NUM"), run: func(c *call, vs []any, _ *env) (any, error) {
			return math.Atan2(toFloat(vs[0]), toFloat(vs[1])), nil
		}},
		// RAND is "a pseudo-random value of type FLOAT64 in the range of
		// [0, 1)": its value differs between runs, as Google's does.
		"RAND":          {sig: fixed(float64Type), run: func(c *call, vs []any, _ *env) (any, error) { return rand.Float64(), nil }},
		"SAFE_ADD":      safe("+"),
		"SAFE_SUBTRACT": safe("-"),
		"SAFE_MULTIPLY": safe("*"),
		"SAFE_NEGATE": {sig: fixed(nil, "NUM"), run: func(c *call, vs []any, _ *env) (any, error) {
			if n, ok := vs[0].(int64); ok {
				if n == math.MinInt64 {
					return nil, nil
				}
				return -n, nil
			}
			return -toFloat(vs[0]), nil
		}},
		// strings
		"ASCII": {sig: fixed(int64Type, "STRING|BYTES"), run: func(c *call, vs []any, _ *env) (any, error) {
			b := asBytes(vs[0])
			if len(b) == 0 {
				return int64(0), nil
			}
			if _, ok := vs[0].(string); ok && b[0] >= utf8.RuneSelf {
				return nil, fmt.Errorf("ASCII: the first character of %q is not ASCII", vs[0])
			}
			return int64(b[0]), nil
		}},
		"UNICODE": {sig: fixed(int64Type, "STRING"), run: func(c *call, vs []any, _ *env) (any, error) {
			s := vs[0].(string)
			if s == "" {
				return int64(0), nil
			}
			r, _ := utf8.DecodeRuneInString(s)
			return int64(r), nil
		}},
		"CHR": {sig: fixed(stringType, "INT64"), run: func(c *call, vs []any, _ *env) (any, error) {
			n := vs[0].(int64)
			if n == 0 {
				return "", nil
			}
			if n < 0 || n > 0x10FFFF || n >= 0xD800 && n <= 0xDFFF {
				return nil, fmt.Errorf("Invalid code point %d", n)
			}
			return string(rune(n)), nil
		}},
		"INITCAP": {sig: ranged(stringType, 1, "STRING", "STRING"), run: func(c *call, vs []any, _ *env) (any, error) {
			delims := " \t\n\r\v\f[](){}/|\\<>!?@\"^#$&~_,.:;*%+-"
			if len(vs) == 2 {
				delims = vs[1].(string)
			}
			var b strings.Builder
			start := true
			for _, r := range vs[0].(string) {
				if strings.ContainsRune(delims, r) || len(vs) == 1 && unicode.IsSpace(r) {
					b.WriteRune(r)
					start = true
					continue
				}
				if start {
					b.WriteRune(unicode.ToUpper(r))
				} else {
					b.WriteRune(unicode.ToLower(r))
				}
				start = false
			}
			return b.String(), nil
		}},
		"INSTR": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) < 2 || len(ts) > 4 || !argOK(ts[0], "STRING|BYTES") || !argOK(ts[1], "STRING|BYTES") || !sameText(ts, 0, 1) {
				return nil, sigErr(c, ts)
			}
			for _, t := range ts[2:] {
				if !argOK(t, "INT64") {
					return nil, sigErr(c, ts)
				}
			}
			return int64Type, nil
		}, run: instr},
		"TRANSLATE": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) != 3 || !argOK(ts[0], "STRING|BYTES") || !sameText(ts, 0, 1, 2) {
				return nil, sigErr(c, ts)
			}
			return textual(3, "STRING|BYTES", "STRING|BYTES")(c, ts)
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			from, to := units(vs[1]), units(vs[2])
			m := map[rune]int{}
			for i, r := range from {
				if _, dup := m[r]; dup {
					return nil, fmt.Errorf("Duplicate character %q in TRANSLATE source characters", string(r))
				}
				m[r] = i
			}
			var out []rune
			for _, r := range units(vs[0]) {
				i, ok := m[r]
				switch {
				case !ok:
					out = append(out, r)
				case i < len(to):
					out = append(out, to[i])
				}
			}
			return fromUnits(out, vs[0]), nil
		}},
		"SOUNDEX": {sig: fixed(stringType, "STRING"), run: func(c *call, vs []any, _ *env) (any, error) {
			return soundex(vs[0].(string)), nil
		}},
		"FORMAT": {nulls: true, sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) < 1 || !argOK(ts[0], "STRING") {
				return nil, sigErr(c, ts)
			}
			return stringType, nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			if vs[0] == nil {
				return nil, nil
			}
			return format(vs[0].(string), vs[1:], c.argTypes[1:])
		}},
		// vectors
		"TO_VECTOR32": {sig: fixed(arrayType(float32Type), "BYTES"), run: func(c *call, vs []any, _ *env) (any, error) {
			b := vs[0].([]byte)
			if len(b)%4 != 0 {
				return nil, fmt.Errorf("TO_VECTOR32: the length of the bytes, %d, is not a multiple of 4", len(b))
			}
			out := make([]any, 0, len(b)/4)
			for i := 0; i < len(b); i += 4 {
				out = append(out, float64(math.Float32frombits(binary.BigEndian.Uint32(b[i:]))))
			}
			return out, nil
		}},
		"TO_VECTOR64": {sig: fixed(arrayType(float64Type), "BYTES"), run: func(c *call, vs []any, _ *env) (any, error) {
			b := vs[0].([]byte)
			if len(b)%8 != 0 {
				return nil, fmt.Errorf("TO_VECTOR64: the length of the bytes, %d, is not a multiple of 8", len(b))
			}
			out := make([]any, 0, len(b)/8)
			for i := 0; i < len(b); i += 8 {
				out = append(out, math.Float64frombits(binary.BigEndian.Uint64(b[i:])))
			}
			return out, nil
		}},
		"COSINE_DISTANCE":    {sig: vectorSig, run: distance(true)},
		"EUCLIDEAN_DISTANCE": {sig: vectorSig, run: distance(false)},
		// dates
		"CURRENT_DATE": {sig: ranged(dateType, 0, "STRING"), run: func(c *call, vs []any, e *env) (any, error) {
			loc, err := zone(vs, 0)
			if err != nil {
				return nil, err
			}
			return dateOf(e.now.In(loc)), nil
		}},
		"DATE": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			switch {
			case len(ts) == 3 && argOK(ts[0], "INT64") && argOK(ts[1], "INT64") && argOK(ts[2], "INT64"):
			case len(ts) >= 1 && len(ts) <= 2 && argOK(ts[0], "TIMESTAMP") && (len(ts) == 1 || argOK(ts[1], "STRING")):
			default:
				return nil, sigErr(c, ts)
			}
			return dateType, nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			if len(vs) == 3 {
				y, m, d := vs[0].(int64), vs[1].(int64), vs[2].(int64)
				t := time.Date(int(y), time.Month(m), int(d), 0, 0, 0, 0, time.UTC)
				if y < 1 || y > 9999 || int64(t.Month()) != m || int64(t.Day()) != d {
					return nil, fmt.Errorf("Input calculates to invalid date: %04d-%02d-%02d", y, m, d)
				}
				return t, nil
			}
			loc, err := zone(vs, 1)
			if err != nil {
				return nil, err
			}
			return dateOf(vs[0].(time.Time).In(loc)), nil
		}},
		"UNIX_DATE": {sig: fixed(int64Type, "DATE"), run: func(c *call, vs []any, _ *env) (any, error) {
			return floorDiv(vs[0].(time.Time).Unix(), 86400), nil
		}},
		"DATE_FROM_UNIX_DATE": {sig: fixed(dateType, "INT64"), run: func(c *call, vs []any, _ *env) (any, error) {
			return checkDate(time.Unix(0, 0).UTC().AddDate(0, 0, int(vs[0].(int64))))
		}},
		"DATE_ADD": {sig: dateIntervalSig, run: func(c *call, vs []any, _ *env) (any, error) {
			return addDate(vs[0].(time.Time), vs[1].(intervalValue), 1)
		}},
		"DATE_SUB": {sig: dateIntervalSig, run: func(c *call, vs []any, _ *env) (any, error) {
			return addDate(vs[0].(time.Time), vs[1].(intervalValue), -1)
		}},
		"DATE_DIFF": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) != 3 || !argOK(ts[0], "DATE") || !argOK(ts[1], "DATE") || !isPart(c.args[2]) {
				return nil, sigErr(c, ts)
			}
			return int64Type, nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			return dateDiff(vs[0].(time.Time), vs[1].(time.Time), vs[2].(string))
		}},
		"DATE_TRUNC": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) == 2 && argOK(ts[0], "DATE") && isPart(c.args[1]) {
				return dateType, nil
			}
			if len(ts) >= 2 && len(ts) <= 3 && argOK(ts[0], "TIMESTAMP") && isPart(c.args[1]) && (len(ts) == 2 || argOK(ts[2], "STRING")) {
				return timestampType, nil
			}
			return nil, sigErr(c, ts)
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			t := vs[0].(time.Time)
			if isKind(c.t, "DATE") {
				return truncDate(t, vs[1].(string))
			}
			loc, err := zone(vs, 2)
			if err != nil {
				return nil, err
			}
			return truncTimestamp(t.In(loc), vs[1].(string))
		}},
		"LAST_DAY": {sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) < 1 || len(ts) > 2 || !argOK(ts[0], "DATE") || len(ts) == 2 && !isPart(c.args[1]) {
				return nil, sigErr(c, ts)
			}
			return dateType, nil
		}, run: func(c *call, vs []any, _ *env) (any, error) {
			part := "MONTH"
			if len(vs) == 2 {
				part = vs[1].(string)
			}
			return lastDay(vs[0].(time.Time), part)
		}},
		"FORMAT_DATE": {sig: fixed(stringType, "STRING", "DATE"), run: func(c *call, vs []any, _ *env) (any, error) {
			return formatTimestamp(vs[0].(string), vs[1].(time.Time))
		}},
		"PARSE_DATE": {sig: fixed(dateType, "STRING", "STRING"), run: func(c *call, vs []any, _ *env) (any, error) {
			t, err := parseFormatted(vs[0].(string), vs[1].(string), time.UTC, true)
			if err != nil {
				return nil, err
			}
			return checkDate(t)
		}},
		"PARSE_TIMESTAMP": {sig: ranged(timestampType, 2, "STRING", "STRING", "STRING"), run: func(c *call, vs []any, _ *env) (any, error) {
			loc, err := zone(vs, 2)
			if err != nil {
				return nil, err
			}
			return parseFormatted(vs[0].(string), vs[1].(string), loc, false)
		}},
	}
	for k, v := range add {
		functions[k] = v
	}
}

// --- strings ---

func instr(c *call, vs []any, _ *env) (any, error) {
	hay, needle := units(vs[0]), units(vs[1])
	pos, occ := int64(1), int64(1)
	if len(vs) > 2 {
		pos = vs[2].(int64)
	}
	if len(vs) > 3 {
		occ = vs[3].(int64)
	}
	if pos == 0 {
		return nil, fmt.Errorf("INSTR: the position must not be 0")
	}
	if occ <= 0 {
		return nil, fmt.Errorf("INSTR: the occurrence must be positive")
	}
	n := int64(len(hay))
	match := func(i int64) bool {
		if i < 0 || i+int64(len(needle)) > n {
			return false
		}
		for j, r := range needle {
			if hay[i+int64(j)] != r {
				return false
			}
		}
		return true
	}
	found := int64(0)
	if pos > 0 {
		for i := pos - 1; i < n; i++ {
			if match(i) {
				if found++; found == occ {
					return i + 1, nil
				}
			}
		}
		return int64(0), nil
	}
	for i := n + pos; i >= 0; i-- {
		if match(i) {
			if found++; found == occ {
				return i + 1, nil
			}
		}
	}
	return int64(0), nil
}

// soundex is the American Soundex code: the first Latin letter, then the
// codes of the following letters, a code repeated across letters coded
// the same (or separated only by H or W) written once, vowels separating
// codes, and anything not a Latin letter ignored; padded with zeros to
// four characters, empty when no Latin letter is left (the reference).
func soundex(s string) string {
	code := func(r rune) byte {
		switch r {
		case 'B', 'F', 'P', 'V':
			return '1'
		case 'C', 'G', 'J', 'K', 'Q', 'S', 'X', 'Z':
			return '2'
		case 'D', 'T':
			return '3'
		case 'L':
			return '4'
		case 'M', 'N':
			return '5'
		case 'R':
			return '6'
		case 'H', 'W':
			return 'h'
		}
		return '0' // a vowel, or Y
	}
	var out []byte
	var last byte
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		if r < 'A' || r > 'Z' {
			continue
		}
		cd := code(r)
		if out == nil {
			out = append(out, byte(r))
			last = cd
			continue
		}
		switch cd {
		case 'h':
			continue
		case '0':
			last = '0'
			continue
		}
		if cd != last {
			out = append(out, cd)
		}
		last = cd
		if len(out) == 4 {
			break
		}
	}
	if out == nil {
		return ""
	}
	for len(out) < 4 {
		out = append(out, '0')
	}
	return string(out)
}

// format is FORMAT's specifiers, %[flags][width][.precision]specifier,
// as the reference lists them (string_functions#format).
func format(f string, args []any, types []*btpb.Type) (any, error) {
	var b strings.Builder
	next := 0
	take := func() (any, *btpb.Type, error) {
		if next >= len(args) {
			return nil, nil, fmt.Errorf("Too few arguments to FORMAT for pattern %q", f)
		}
		next++
		return args[next-1], types[next-1], nil
	}
	isNull := false
	for i := 0; i < len(f); i++ {
		if f[i] != '%' {
			b.WriteByte(f[i])
			continue
		}
		i++
		if i < len(f) && f[i] == '%' {
			b.WriteByte('%')
			continue
		}
		flags := ""
		for i < len(f) && strings.IndexByte("-+ #0'", f[i]) >= 0 {
			flags += string(f[i])
			i++
		}
		width, prec := -1, -1
		readNum := func() (int, error) {
			if i < len(f) && f[i] == '*' {
				i++
				v, t, err := take()
				if err != nil {
					return 0, err
				}
				if !isKind(t, "INT64") {
					return 0, fmt.Errorf("FORMAT: * takes an INT64 argument")
				}
				if v == nil {
					isNull = true
					return 0, nil
				}
				return int(v.(int64)), nil
			}
			j := i
			for i < len(f) && f[i] >= '0' && f[i] <= '9' {
				i++
			}
			if j == i {
				return -1, nil
			}
			return strconv.Atoi(f[j:i])
		}
		var err error
		if width, err = readNum(); err != nil {
			return nil, err
		}
		if i < len(f) && f[i] == '.' {
			i++
			if prec, err = readNum(); err != nil {
				return nil, err
			}
			if prec < 0 {
				prec = 0
			}
		}
		if i >= len(f) {
			return nil, fmt.Errorf("Invalid format specifier at the end of %q", f)
		}
		spec := f[i]
		v, t, err := take()
		if err != nil {
			return nil, err
		}
		s, null, err := formatOne(spec, flags, width, prec, v, t)
		if err != nil {
			return nil, err
		}
		isNull = isNull || null
		b.WriteString(s)
	}
	if next != len(args) {
		return nil, fmt.Errorf("Too many arguments to FORMAT for pattern %q; expected %d, got %d", f, next, len(args))
	}
	if isNull {
		return nil, nil
	}
	return b.String(), nil
}

func formatOne(spec byte, flags string, width, prec int, v any, t *btpb.Type) (string, bool, error) {
	pad := func(s string, numeric bool) string {
		if width < 0 || len([]rune(s)) >= width {
			return s
		}
		n := width - len([]rune(s))
		switch {
		case strings.Contains(flags, "-"):
			return s + strings.Repeat(" ", n)
		case numeric && strings.Contains(flags, "0"):
			sign := ""
			if s != "" && strings.ContainsRune("+- ", rune(s[0])) {
				sign, s = s[:1], s[1:]
			}
			if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
				sign, s = sign+s[:2], s[2:]
			}
			return sign + strings.Repeat("0", n) + s
		}
		return strings.Repeat(" ", n) + s
	}
	sign := func(neg bool, s string) string {
		switch {
		case neg:
			return "-" + s
		case strings.Contains(flags, "+"):
			return "+" + s
		case strings.Contains(flags, " "):
			return " " + s
		}
		return s
	}
	bad := func() (string, bool, error) {
		return "", false, fmt.Errorf("Invalid type for argument to FORMAT specifier %%%c: %s", spec, kind(t))
	}
	switch spec {
	case 'd', 'i', 'o', 'x', 'X':
		if t != nil && !isKind(t, "INT64") {
			return bad()
		}
		if v == nil {
			return "", true, nil
		}
		n := v.(int64)
		if spec != 'd' && spec != 'i' && n < 0 {
			return "", false, fmt.Errorf("Negative value %d is not allowed for FORMAT specifier %%%c", n, spec)
		}
		var digits string
		abs := uint64(n)
		if n < 0 {
			abs = uint64(-n)
		}
		base, group, sep := 10, 3, ","
		switch spec {
		case 'o':
			base, group = 8, 4
		case 'x', 'X':
			base, group, sep = 16, 4, ":"
		}
		digits = strconv.FormatUint(abs, base)
		if spec == 'X' {
			digits = strings.ToUpper(digits)
		}
		if prec >= 0 {
			if prec == 0 && n == 0 {
				digits = ""
			}
			for len(digits) < prec {
				digits = "0" + digits
			}
		}
		if strings.Contains(flags, "'") {
			var parts []string
			for len(digits) > group {
				parts = append([]string{digits[len(digits)-group:]}, parts...)
				digits = digits[:len(digits)-group]
			}
			digits = strings.Join(append([]string{digits}, parts...), sep)
		}
		if strings.Contains(flags, "#") && n != 0 {
			switch spec {
			case 'o':
				digits = "0" + digits
			case 'x':
				digits = "0x" + digits
			case 'X':
				digits = "0X" + digits
			}
		}
		return pad(sign(n < 0, digits), true), false, nil
	case 'f', 'F', 'e', 'E', 'g', 'G':
		if t != nil && !isKind(t, "FLOAT32", "FLOAT64") {
			return bad()
		}
		if v == nil {
			return "", true, nil
		}
		x := toFloat(v)
		if math.IsNaN(x) || math.IsInf(x, 0) {
			s := "nan"
			if math.IsInf(x, 0) {
				s = "inf"
			}
			if spec == 'F' || spec == 'E' || spec == 'G' {
				s = strings.ToUpper(s)
			}
			return pad(sign(math.IsInf(x, -1), s), false), false, nil
		}
		if prec < 0 {
			prec = 6
		}
		if (spec == 'g' || spec == 'G') && prec == 0 {
			prec = 1
		}
		goFlags := ""
		if strings.Contains(flags, "#") {
			goFlags = "#"
		}
		s := fmt.Sprintf("%"+goFlags+"."+strconv.Itoa(prec)+string(spec), math.Abs(x))
		return pad(sign(math.Signbit(x) && x != 0, s), true), false, nil
	case 's':
		if t != nil && !isKind(t, "STRING") {
			return bad()
		}
		if v == nil {
			return "", true, nil
		}
		s := v.(string)
		if prec >= 0 && len([]rune(s)) > prec {
			s = string([]rune(s)[:prec])
		}
		return pad(s, false), false, nil
	case 't', 'T':
		s, err := formatValue(v, t, spec == 'T')
		if err != nil {
			return "", false, err
		}
		if prec >= 0 && len([]rune(s)) > prec {
			s = string([]rune(s)[:prec])
		}
		return pad(s, false), false, nil
	}
	return "", false, fmt.Errorf("Invalid format specifier %%%c", spec)
}

// formatValue is %t (readable) or %T (a literal) of v, of type t.
func formatValue(v any, t *btpb.Type, literal bool) (string, error) {
	if v == nil {
		return "NULL", nil
	}
	switch x := v.(type) {
	case bool:
		if x {
			return "true", nil
		}
		return "false", nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		s, err := castValue(x, t, stringType)
		if err != nil {
			return "", err
		}
		str := s.(string)
		if !strings.ContainsAny(str, ".eEni") {
			str += ".0"
		}
		if literal && (math.IsNaN(x) || math.IsInf(x, 0)) {
			return fmt.Sprintf("CAST(%q AS %s)", str, kind(t)), nil
		}
		return str, nil
	case string:
		if literal {
			return strconv.Quote(x), nil
		}
		return x, nil
	case []byte:
		var b strings.Builder
		for _, c := range x {
			switch {
			case c == '\\':
				b.WriteString(`\\`)
			case c == '"' && literal:
				b.WriteString(`\"`)
			case c >= 0x20 && c < 0x7f:
				b.WriteByte(c)
			default:
				fmt.Fprintf(&b, `\x%02x`, c)
			}
		}
		if literal {
			return `b"` + b.String() + `"`, nil
		}
		return b.String(), nil
	case time.Time:
		var s string
		if isKind(t, "DATE") {
			s = x.Format("2006-01-02")
			if literal {
				return `DATE "` + s + `"`, nil
			}
			return s, nil
		}
		s = timestampString(x.UTC())
		if literal {
			return `TIMESTAMP "` + s + `"`, nil
		}
		return s, nil
	case []any:
		var parts []string
		if isKind(t, "ARRAY<") {
			for _, e := range x {
				p, err := formatValue(e, t.GetArrayType().GetElementType(), literal)
				if err != nil {
					return "", err
				}
				parts = append(parts, p)
			}
			return "[" + strings.Join(parts, ", ") + "]", nil
		}
		fs := t.GetStructType().GetFields()
		for i, e := range x {
			p, err := formatValue(e, fs[i].GetType(), literal)
			if err != nil {
				return "", err
			}
			parts = append(parts, p)
		}
		if literal && len(parts) <= 1 {
			return "STRUCT(" + strings.Join(parts, "") + ")", nil
		}
		return "(" + strings.Join(parts, ", ") + ")", nil
	}
	return "", unsupportedErr{"FORMAT of a " + kind(t)}
}

// --- vectors ---

// vectorSig types COSINE_DISTANCE and EUCLIDEAN_DISTANCE: two ARRAY<T>,
// T FLOAT32 or FLOAT64 and the same for both, or two sparse vectors,
// ARRAY<STRUCT<dimension INT64|STRING, magnitude FLOAT64>>.
func vectorSig(c *call, ts []*btpb.Type) (*btpb.Type, error) {
	if len(ts) != 2 {
		return nil, sigErr(c, ts)
	}
	var elems []*btpb.Type
	for _, t := range ts {
		if t == nil {
			elems = append(elems, nil)
			continue
		}
		if !isKind(t, "ARRAY<") {
			return nil, sigErr(c, ts)
		}
		e := t.GetArrayType().GetElementType()
		if fs := e.GetStructType().GetFields(); len(fs) == 2 {
			if !isKind(fs[0].GetType(), "INT64", "STRING") || !isKind(fs[1].GetType(), "FLOAT64") {
				return nil, sigErr(c, ts)
			}
		} else if !isKind(e, "FLOAT32", "FLOAT64") {
			return nil, sigErr(c, ts)
		}
		elems = append(elems, e)
	}
	if elems[0] != nil && elems[1] != nil && !sameType(elems[0], elems[1]) {
		return nil, sigErr(c, ts)
	}
	return float64Type, nil
}

func distance(cosine bool) func(*call, []any, *env) (any, error) {
	return func(c *call, vs []any, _ *env) (any, error) {
		a, b := vs[0].([]any), vs[1].([]any)
		sparse := false
		for _, v := range append(append([]any{}, a...), b...) {
			if _, ok := v.([]any); ok {
				sparse = true
			}
		}
		var x, y []float64
		if !sparse {
			if len(a) != len(b) {
				return nil, fmt.Errorf("Array arguments to %s must have equal length. The given arrays have lengths of %d and %d", c.name, len(a), len(b))
			}
			for i := range a {
				if a[i] == nil || b[i] == nil {
					return nil, fmt.Errorf("Cannot compute %s with a NULL element, since it is unclear if NULLs should be ignored, counted as a zero value, or another value", c.name)
				}
				x, y = append(x, toFloat(a[i])), append(y, toFloat(b[i]))
			}
		} else {
			dims := map[string]int{}
			read := func(v []any, into int) error {
				for _, e := range v {
					p, ok := e.([]any)
					if !ok || len(p) != 2 || p[0] == nil || p[1] == nil {
						return fmt.Errorf("Cannot compute %s with a NULL dimension or magnitude", c.name)
					}
					k := groupKey(p[0])
					i, seen := dims[k]
					if !seen {
						i = len(x)
						dims[k] = i
						x, y = append(x, 0), append(y, 0)
					}
					if into == 0 {
						x[i] = toFloat(p[1])
					} else {
						y[i] = toFloat(p[1])
					}
				}
				return nil
			}
			if err := read(a, 0); err != nil {
				return nil, err
			}
			if err := read(b, 1); err != nil {
				return nil, err
			}
		}
		var dot, nx, ny, sq float64
		for i := range x {
			dot += x[i] * y[i]
			nx += x[i] * x[i]
			ny += y[i] * y[i]
			sq += (x[i] - y[i]) * (x[i] - y[i])
		}
		if !cosine {
			return math.Sqrt(sq), nil
		}
		if nx == 0 || ny == 0 {
			return nil, fmt.Errorf("Cannot compute cosine distance against zero vector")
		}
		return 1 - dot/(math.Sqrt(nx)*math.Sqrt(ny)), nil
	}
}

// --- dates ---

func dateOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func checkDate(t time.Time) (any, error) {
	if t.Year() < 1 || t.Year() > 9999 {
		return nil, fmt.Errorf("DATE value out of range: %s", t.Format("2006-01-02"))
	}
	return dateOf(t), nil
}

func dateIntervalSig(c *call, ts []*btpb.Type) (*btpb.Type, error) {
	if len(ts) != 2 || !argOK(ts[0], "DATE") {
		return nil, sigErr(c, ts)
	}
	if _, ok := c.args[1].(*interval); !ok {
		return nil, fmt.Errorf("%s takes an INTERVAL as its second argument", c.name)
	}
	return dateType, nil
}

// addMonths adds n months, the day clamped to the resulting month's last
// day, as DATE_ADD's reference says.
func addMonths(t time.Time, n int) time.Time {
	y, m, d := t.Date()
	first := time.Date(y, m+time.Month(n), 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	if d > last {
		d = last
	}
	return time.Date(first.Year(), first.Month(), d, 0, 0, 0, 0, time.UTC)
}

func addDate(t time.Time, iv intervalValue, sign int64) (any, error) {
	n := int(sign * iv.n)
	switch iv.unit {
	case "DAY":
		return checkDate(t.AddDate(0, 0, n))
	case "WEEK":
		return checkDate(t.AddDate(0, 0, 7*n))
	case "MONTH":
		return checkDate(addMonths(t, n))
	case "QUARTER":
		return checkDate(addMonths(t, 3*n))
	case "YEAR":
		return checkDate(addMonths(t, 12*n))
	}
	return nil, fmt.Errorf("Unsupported date part %s for a DATE", iv.unit)
}

var weekdays = map[string]time.Weekday{"SUNDAY": time.Sunday, "MONDAY": time.Monday, "TUESDAY": time.Tuesday,
	"WEDNESDAY": time.Wednesday, "THURSDAY": time.Thursday, "FRIDAY": time.Friday, "SATURDAY": time.Saturday}

// weekStart is the weekday a WEEK, WEEK(<WEEKDAY>) or ISOWEEK part's
// weeks begin on.
func weekStart(part string) (time.Weekday, bool) {
	switch {
	case part == "WEEK":
		return time.Sunday, true
	case part == "ISOWEEK":
		return time.Monday, true
	case strings.HasPrefix(part, "WEEK(") && strings.HasSuffix(part, ")"):
		d, ok := weekdays[part[5:len(part)-1]]
		return d, ok
	}
	return 0, false
}

// isoYearStart is the Monday the ISO year y begins on.
func isoYearStart(y int) time.Time {
	jan4 := time.Date(y, 1, 4, 0, 0, 0, 0, time.UTC)
	return jan4.AddDate(0, 0, -((int(jan4.Weekday()) + 6) % 7))
}

func truncDate(t time.Time, part string) (any, error) {
	t = dateOf(t)
	if wd, ok := weekStart(part); ok {
		return checkDate(t.AddDate(0, 0, -((int(t.Weekday()) - int(wd) + 7) % 7)))
	}
	switch part {
	case "DAY":
		return t, nil
	case "MONTH":
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC), nil
	case "QUARTER":
		return time.Date(t.Year(), t.Month()-(t.Month()-1)%3, 1, 0, 0, 0, 0, time.UTC), nil
	case "YEAR":
		return time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.UTC), nil
	case "ISOYEAR":
		y, _ := t.ISOWeek()
		return checkDate(isoYearStart(y))
	}
	return nil, fmt.Errorf("Unsupported date part %s for a DATE", part)
}

func dateDiff(end, start time.Time, part string) (any, error) {
	end, start = dateOf(end), dateOf(start)
	days := func(a, b time.Time) int64 { return int64(a.Sub(b).Hours()) / 24 }
	if _, ok := weekStart(part); ok {
		e, _ := truncDate(end, part)
		s, _ := truncDate(start, part)
		return days(e.(time.Time), s.(time.Time)) / 7, nil
	}
	months := func(t time.Time) int64 { return int64(t.Year())*12 + int64(t.Month()) - 1 }
	switch part {
	case "DAY":
		return days(end, start), nil
	case "MONTH":
		return months(end) - months(start), nil
	case "QUARTER":
		return months(end)/3 - months(start)/3, nil
	case "YEAR":
		return int64(end.Year() - start.Year()), nil
	case "ISOYEAR":
		ey, _ := end.ISOWeek()
		sy, _ := start.ISOWeek()
		return int64(ey - sy), nil
	}
	return nil, fmt.Errorf("Unsupported date part %s for DATE_DIFF", part)
}

func lastDay(t time.Time, part string) (any, error) {
	t = dateOf(t)
	if _, ok := weekStart(part); ok {
		s, _ := truncDate(t, part)
		return checkDate(s.(time.Time).AddDate(0, 0, 6))
	}
	switch part {
	case "MONTH":
		return checkDate(time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, time.UTC))
	case "QUARTER":
		q := t.Month() - (t.Month()-1)%3
		return checkDate(time.Date(t.Year(), q+3, 0, 0, 0, 0, 0, time.UTC))
	case "YEAR":
		return checkDate(time.Date(t.Year(), 12, 31, 0, 0, 0, 0, time.UTC))
	case "ISOYEAR":
		y, _ := t.ISOWeek()
		return checkDate(isoYearStart(y+1).AddDate(0, 0, -1))
	}
	return nil, fmt.Errorf("Unsupported date part %s for LAST_DAY", part)
}

var monthNames = []string{"JANUARY", "FEBRUARY", "MARCH", "APRIL", "MAY", "JUNE", "JULY", "AUGUST", "SEPTEMBER", "OCTOBER", "NOVEMBER", "DECEMBER"}

// parseFormatted is PARSE_DATE's and PARSE_TIMESTAMP's format elements
// (the common ones; any other is refused UNIMPLEMENTED by name).
// Unspecified fields come from 1970-01-01 00:00:00; a run of white space
// in the format matches any white space, and the string's leading and
// trailing white space is allowed (the reference's PARSE_DATE).
func parseFormatted(f, s string, loc *time.Location, dateOnly bool) (time.Time, error) {
	year, month, day, hour, min, sec, nanos := 1970, 1, 1, 0, 0, 0, 0
	pm, has12 := false, false
	var zoneOff *int
	var epoch *int64
	s = strings.TrimSpace(s)
	pos := 0
	fail := func() (time.Time, error) {
		return time.Time{}, fmt.Errorf("Failed to parse input string %q", s)
	}
	num := func(max int) (int, bool) {
		j := pos
		if j < len(s) && (s[j] == '-' || s[j] == '+') && max > 4 {
			j++
		}
		for j < len(s) && j-pos < max && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		n, err := strconv.Atoi(s[pos:j])
		if err != nil {
			return 0, false
		}
		pos = j
		return n, true
	}
	name := func(names []string) (int, bool) {
		rest := strings.ToUpper(s[pos:])
		for i, n := range names {
			if strings.HasPrefix(rest, n) {
				pos += len(n)
				return i, true
			}
		}
		for i, n := range names {
			if strings.HasPrefix(rest, n[:3]) {
				pos += 3
				return i, true
			}
		}
		return 0, false
	}
	var elem func(c byte) bool
	elem = func(c byte) bool {
		var ok bool
		switch c {
		case 'Y':
			year, ok = num(5)
		case 'y':
			var y int
			if y, ok = num(2); ok {
				year = 1900 + y
				if y < 69 {
					year = 2000 + y
				}
			}
		case 'm':
			month, ok = num(2)
		case 'd', 'e':
			for pos < len(s) && s[pos] == ' ' {
				pos++
			}
			day, ok = num(2)
		case 'b', 'h', 'B':
			var m int
			if m, ok = name(monthNames); ok {
				month = m + 1
			}
		case 'a', 'A':
			_, ok = name([]string{"SUNDAY", "MONDAY", "TUESDAY", "WEDNESDAY", "THURSDAY", "FRIDAY", "SATURDAY"})
		case 'F':
			ok = elem('Y') && pos < len(s) && s[pos] == '-' && func() bool { pos++; return elem('m') }() &&
				pos < len(s) && s[pos] == '-' && func() bool { pos++; return elem('d') }()
		case 'D':
			ok = elem('m') && pos < len(s) && s[pos] == '/' && func() bool { pos++; return elem('d') }() &&
				pos < len(s) && s[pos] == '/' && func() bool { pos++; return elem('y') }()
		case 'H':
			hour, ok = num(2)
		case 'I':
			hour, ok = num(2)
			has12 = true
		case 'M':
			min, ok = num(2)
		case 'S':
			sec, ok = num(2)
		case 'T':
			ok = elem('H') && pos < len(s) && s[pos] == ':' && func() bool { pos++; return elem('M') }() &&
				pos < len(s) && s[pos] == ':' && func() bool { pos++; return elem('S') }()
		case 'p':
			rest := strings.ToUpper(s[pos:])
			if ok = strings.HasPrefix(rest, "AM") || strings.HasPrefix(rest, "PM"); ok {
				pm = rest[0] == 'P'
				pos += 2
			}
		case 's':
			var n int
			if n, ok = num(20); ok {
				e := int64(n)
				epoch = &e
			}
		case 'z':
			if pos < len(s) && s[pos] == 'Z' {
				pos++
				z := 0
				zoneOff, ok = &z, true
				break
			}
			if pos < len(s) && (s[pos] == '+' || s[pos] == '-') {
				neg := s[pos] == '-'
				pos++
				var h, m int
				if h, ok = num(2); ok {
					if pos < len(s) && s[pos] == ':' {
						pos++
					}
					m, _ = num(2)
					z := h*3600 + m*60
					if neg {
						z = -z
					}
					zoneOff = &z
				}
			}
		case 'n', 't':
			for pos < len(s) && unicode.IsSpace(rune(s[pos])) {
				pos++
			}
			ok = true
		case '%':
			if ok = pos < len(s) && s[pos] == '%'; ok {
				pos++
			}
		}
		return ok
	}
	for i := 0; i < len(f); i++ {
		c := f[i]
		switch {
		case unicode.IsSpace(rune(c)):
			for pos < len(s) && unicode.IsSpace(rune(s[pos])) {
				pos++
			}
		case c == '%' && i+1 < len(f):
			i++
			if !strings.ContainsRune("YymdebhBaAFDHIMSTpszn%t", rune(f[i])) {
				return time.Time{}, unsupportedErr{"the format element %" + string(f[i])}
			}
			if dateOnly && strings.ContainsRune("HIMSTpsz", rune(f[i])) {
				return time.Time{}, fmt.Errorf("PARSE_DATE does not take the format element %%%c", f[i])
			}
			if !elem(f[i]) {
				return fail()
			}
		default:
			if pos >= len(s) || s[pos] != c {
				return fail()
			}
			pos++
		}
	}
	if pos != len(s) {
		return fail()
	}
	if epoch != nil {
		return time.Unix(*epoch, 0).UTC(), nil
	}
	if has12 {
		if hour < 1 || hour > 12 {
			return fail()
		}
		hour %= 12
		if pm {
			hour += 12
		}
	}
	if month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || min > 59 || sec > 60 {
		return fail()
	}
	l := loc
	if zoneOff != nil {
		l = time.FixedZone("", *zoneOff)
	}
	t := time.Date(year, time.Month(month), day, hour, min, sec, nanos, l)
	if t.Day() != day {
		return fail()
	}
	return t.UTC(), nil
}
