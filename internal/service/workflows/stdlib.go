package workflows

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"hash"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// fn is a standard library function (docs/reference/stdlib/overview):
// its parameters, by name for a call step's args and by position in an
// expression, and how many are required.
type fn struct {
	params   []string
	required int
	defaults map[string]any
	call     func(f *frame, a []any) (any, error)
}

// blocking are the functions Google allows only in call steps
// (syntax/expressions: "Blocking calls").
var blocking = map[string]bool{
	"events.await_callback": true, "sys.log": true, "sys.sleep": true, "sys.sleep_until": true,
	"http.get": true, "http.post": true, "http.put": true, "http.patch": true, "http.delete": true, "http.request": true,
	"experimental.executions.map": true, "experimental.executions.run": true,
}

// callStep are the functions the engine runs itself in a call step.
var callStep = map[string]bool{
	"events.await_callback": true, "events.create_callback_endpoint": true,
	"sys.log": true, "sys.sleep": true, "sys.sleep_until": true,
	"http.get": true, "http.post": true, "http.put": true, "http.patch": true, "http.delete": true, "http.request": true,
}

var stdlib map[string]*fn

// defaultBackoff is retry.default_backoff.
func defaultBackoff() map[string]any {
	return map[string]any{"initial_delay": int64(1), "max_delay": int64(60), "multiplier": 1.25}
}

// constant is a module's object: http.default_retry and its
// non-idempotent form, and retry.default_backoff.
func constant(name string) (any, bool) {
	switch name {
	case "http.default_retry":
		return map[string]any{"predicate": funcRef{name: "http.default_retry_predicate"}, "max_retries": int64(5), "backoff": defaultBackoff()}, true
	case "http.default_retry_non_idempotent":
		return map[string]any{"predicate": funcRef{name: "http.default_retry_predicate_non_idempotent"}, "max_retries": int64(5), "backoff": defaultBackoff()}, true
	case "retry.default_backoff":
		return defaultBackoff(), true
	}
	return nil, false
}

func str(v any, what string) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", raiseErr("TypeError", "%s must be a string, not %s", what, typeName(v))
	}
	return s, nil
}

func bytesOf(v any, what string) ([]byte, error) {
	switch b := v.(type) {
	case []byte:
		return b, nil
	case string:
		return []byte(b), nil
	}
	return nil, raiseErr("TypeError", "%s must be bytes, not %s", what, typeName(v))
}

func intArg(v any, what string) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case float64:
		if n == math.Trunc(n) {
			return int64(n), nil
		}
	}
	return 0, raiseErr("TypeError", "%s must be an integer, not %s", what, typeName(v))
}

func strResult(s string) (any, error) {
	if len(s) > MaxStringBytes {
		return nil, raiseErr("ResourceLimitError", "string length limit exceeded: %d bytes", MaxStringBytes)
	}
	return s, nil
}

func init() {
	retryPredicate := func(codes []int64, tags []string) func(*frame, []any) (any, error) {
		return func(_ *frame, a []any) (any, error) {
			m, ok := a[0].(map[string]any)
			if !ok {
				return false, nil
			}
			e := &wfError{val: m}
			for _, t := range tags {
				if e.hasTag(t) {
					return true, nil
				}
			}
			if e.hasTag("HttpError") {
				code, _ := m["code"].(int64)
				for _, c := range codes {
					if code == c {
						return true, nil
					}
				}
			}
			return false, nil
		}
	}
	checksum := func(alg string) (func() hash.Hash, error) {
		switch strings.ToUpper(alg) {
		case "MD5":
			return md5.New, nil
		case "SHA1":
			return sha1.New, nil
		case "SHA256":
			return sha256.New, nil
		case "SHA384":
			return sha512.New384, nil
		case "SHA512":
			return sha512.New, nil
		}
		return nil, raiseErr("ValueError", "unsupported algorithm %q: use MD5, SHA1, SHA256, SHA384 or SHA512", alg)
	}
	stdlib = map[string]*fn{
		// Expression helpers (stdlib/expression-helpers).
		"default": {params: []string{"val", "default_val"}, required: 2, call: func(_ *frame, a []any) (any, error) {
			if a[0] == nil {
				return a[1], nil
			}
			return a[0], nil
		}},
		"if": {params: []string{"condition", "if_true", "if_false"}, required: 3, call: func(_ *frame, a []any) (any, error) {
			if a[0] == nil {
				return a[2], nil
			}
			b, ok := a[0].(bool)
			if !ok {
				return nil, raiseErr("TypeError", "if() needs a boolean condition, not %s", typeName(a[0]))
			}
			if b {
				return a[1], nil
			}
			return a[2], nil
		}},
		"keys": {params: []string{"map"}, required: 1, call: func(_ *frame, a []any) (any, error) {
			m, ok := a[0].(map[string]any)
			if !ok {
				return nil, raiseErr("TypeError", "keys() needs a map, not %s", typeName(a[0]))
			}
			out := []any{}
			for _, k := range sortedKeys(m) {
				out = append(out, k)
			}
			return out, nil
		}},
		"len": {params: []string{"value"}, required: 1, call: func(_ *frame, a []any) (any, error) {
			switch x := a[0].(type) {
			case string:
				return int64(utf8.RuneCountInString(x)), nil
			case []any:
				return int64(len(x)), nil
			case map[string]any:
				return int64(len(x)), nil
			case []byte:
				return int64(len(x)), nil
			}
			return nil, raiseErr("TypeError", "len() needs a list, a map or a string, not %s", typeName(a[0]))
		}},
		"get_type": {params: []string{"value"}, required: 1, call: func(_ *frame, a []any) (any, error) {
			if _, ok := a[0].(funcRef); ok {
				return nil, raiseErr("TypeError", "get_type() cannot take a function or a subworkflow")
			}
			return typeName(a[0]), nil
		}},
		"int": {params: []string{"value"}, required: 1, call: func(_ *frame, a []any) (any, error) {
			switch x := a[0].(type) {
			case int64:
				return x, nil
			case float64:
				if math.IsNaN(x) || math.IsInf(x, 0) || x > math.MaxInt64 || x < math.MinInt64 {
					return nil, raiseErr("ValueError", "%v cannot be converted to an integer", x)
				}
				return int64(x), nil
			case string:
				n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
				if err != nil {
					f, ferr := strconv.ParseFloat(strings.TrimSpace(x), 64)
					if ferr != nil {
						return nil, raiseErr("ValueError", "%q cannot be converted to an integer", x)
					}
					return int64(f), nil
				}
				return n, nil
			case bool:
				return nil, raiseErr("TypeError", "int() takes a string or a double, not boolean")
			}
			return nil, raiseErr("TypeError", "int() takes a string or a double, not %s", typeName(a[0]))
		}},
		"double": {params: []string{"value"}, required: 1, call: func(_ *frame, a []any) (any, error) {
			switch x := a[0].(type) {
			case int64:
				return float64(x), nil
			case float64:
				return x, nil
			case string:
				f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
				if err != nil {
					return nil, raiseErr("ValueError", "%q cannot be converted to a double", x)
				}
				return f, nil
			}
			return nil, raiseErr("TypeError", "double() takes a string or an integer, not %s", typeName(a[0]))
		}},
		"string": {params: []string{"value"}, required: 1, call: func(_ *frame, a []any) (any, error) {
			s, ok := scalarString(a[0])
			if !ok {
				return nil, raiseErr("TypeError", "string() takes an integer, a double or a boolean, not %s", typeName(a[0]))
			}
			return s, nil
		}},
		// base64
		"base64.encode": {params: []string{"data", "padding"}, required: 1, defaults: map[string]any{"padding": true}, call: func(_ *frame, a []any) (any, error) {
			b, err := bytesOf(a[0], "data")
			if err != nil {
				return nil, err
			}
			if pad, _ := a[1].(bool); !pad {
				return base64.RawStdEncoding.EncodeToString(b), nil
			}
			return base64.StdEncoding.EncodeToString(b), nil
		}},
		"base64.decode": {params: []string{"data", "padding"}, required: 1, defaults: map[string]any{"padding": true}, call: func(_ *frame, a []any) (any, error) {
			s, err := str(a[0], "data")
			if err != nil {
				return nil, err
			}
			enc := base64.StdEncoding
			if pad, _ := a[1].(bool); !pad {
				enc = base64.RawStdEncoding
			}
			b, err := enc.DecodeString(s)
			if err != nil {
				return nil, raiseErr("ValueError", "invalid base64: %v", err)
			}
			return b, nil
		}},
		// hash
		"hash.compute_checksum": {params: []string{"data", "algorithm"}, required: 2, call: func(_ *frame, a []any) (any, error) {
			b, err := bytesOf(a[0], "data")
			if err != nil {
				return nil, err
			}
			alg, err := str(a[1], "algorithm")
			if err != nil {
				return nil, err
			}
			h, err := checksum(alg)
			if err != nil {
				return nil, err
			}
			x := h()
			x.Write(b)
			return x.Sum(nil), nil
		}},
		"hash.compute_hmac": {params: []string{"key", "data", "algorithm"}, required: 3, call: func(_ *frame, a []any) (any, error) {
			k, err := bytesOf(a[0], "key")
			if err != nil {
				return nil, err
			}
			b, err := bytesOf(a[1], "data")
			if err != nil {
				return nil, err
			}
			alg, err := str(a[2], "algorithm")
			if err != nil {
				return nil, err
			}
			h, err := checksum(alg)
			if err != nil {
				return nil, err
			}
			m := hmac.New(h, k)
			m.Write(b)
			return m.Sum(nil), nil
		}},
		// http's predicates (stdlib/http/default_retry).
		"http.default_retry_predicate":                {params: []string{"e"}, required: 1, call: retryPredicate([]int64{429, 502, 503, 504}, []string{"ConnectionError", "ConnectionFailedError", "TimeoutError"})},
		"http.default_retry_predicate_non_idempotent": {params: []string{"e"}, required: 1, call: retryPredicate([]int64{429, 503}, []string{"ConnectionFailedError"})},
		"retry.always": {params: []string{"e"}, required: 1, call: func(*frame, []any) (any, error) { return true, nil }},
		"retry.never":  {params: []string{"e"}, required: 1, call: func(*frame, []any) (any, error) { return false, nil }},
		// json
		"json.encode": {params: []string{"data", "indent"}, required: 1, call: func(_ *frame, a []any) (any, error) {
			ind, _ := a[1].(bool)
			if _, ok := a[1].(map[string]any); ok {
				ind = true
			}
			return encodeJSON(a[0], ind)
		}},
		"json.encode_to_string": {params: []string{"data", "indent"}, required: 1, call: func(_ *frame, a []any) (any, error) {
			ind, _ := a[1].(bool)
			if _, ok := a[1].(map[string]any); ok {
				ind = true
			}
			b, err := encodeJSON(a[0], ind)
			if err != nil {
				return nil, err
			}
			return strResult(string(b))
		}},
		"json.decode": {params: []string{"data"}, required: 1, call: func(_ *frame, a []any) (any, error) {
			b, err := bytesOf(a[0], "data")
			if err != nil {
				return nil, err
			}
			v, err := decodeJSON(b)
			if err != nil {
				return nil, raiseErr("ValueError", "invalid JSON: %v", err)
			}
			return v, nil
		}},
		// list
		"list.concat": {params: []string{"objs", "val"}, required: 2, call: func(_ *frame, a []any) (any, error) {
			l, ok := a[0].([]any)
			if !ok {
				return nil, raiseErr("TypeError", "list.concat needs a list, not %s", typeName(a[0]))
			}
			out := append(append([]any{}, deepCopy(l).([]any)...), deepCopy(a[1]))
			return out, nil
		}},
		"list.prepend": {params: []string{"objs", "val"}, required: 2, call: func(_ *frame, a []any) (any, error) {
			l, ok := a[0].([]any)
			if !ok {
				return nil, raiseErr("TypeError", "list.prepend needs a list, not %s", typeName(a[0]))
			}
			return append([]any{deepCopy(a[1])}, deepCopy(l).([]any)...), nil
		}},
		// map
		"map.get": {params: []string{"map", "keys"}, required: 2, call: func(_ *frame, a []any) (any, error) {
			m, ok := a[0].(map[string]any)
			if !ok {
				if a[0] == nil {
					return nil, nil
				}
				return nil, raiseErr("TypeError", "map.get needs a map, not %s", typeName(a[0]))
			}
			var path []any
			switch k := a[1].(type) {
			case string:
				path = []any{k}
			case []any:
				path = k
			default:
				return nil, raiseErr("TypeError", "map.get keys must be a string or a list of strings")
			}
			var cur any = m
			for _, k := range path {
				ks, ok := k.(string)
				if !ok {
					return nil, raiseErr("TypeError", "map.get keys must be strings")
				}
				cm, ok := cur.(map[string]any)
				if !ok {
					return nil, nil
				}
				if cur, ok = cm[ks]; !ok {
					return nil, nil
				}
			}
			return cur, nil
		}},
		"map.delete": {params: []string{"map", "key"}, required: 2, call: func(_ *frame, a []any) (any, error) {
			m, ok := a[0].(map[string]any)
			if !ok {
				return nil, raiseErr("TypeError", "map.delete needs a map, not %s", typeName(a[0]))
			}
			k, err := str(a[1], "key")
			if err != nil {
				return nil, err
			}
			out := deepCopy(m).(map[string]any)
			delete(out, k)
			return out, nil
		}},
		"map.merge": {params: []string{"first", "second"}, required: 2, call: func(_ *frame, a []any) (any, error) {
			m1, ok1 := a[0].(map[string]any)
			m2, ok2 := a[1].(map[string]any)
			if !ok1 || !ok2 {
				return nil, raiseErr("TypeError", "map.merge needs two maps")
			}
			out := deepCopy(m1).(map[string]any)
			for k, v := range m2 {
				out[k] = deepCopy(v)
			}
			return out, nil
		}},
		"map.merge_nested": {params: []string{"first", "second"}, required: 2, call: func(_ *frame, a []any) (any, error) {
			m1, ok1 := a[0].(map[string]any)
			m2, ok2 := a[1].(map[string]any)
			if !ok1 || !ok2 {
				return nil, raiseErr("TypeError", "map.merge_nested needs two maps")
			}
			return mergeNested(deepCopy(m1).(map[string]any), m2), nil
		}},
		// math
		"math.abs": {params: []string{"x"}, required: 1, call: func(_ *frame, a []any) (any, error) {
			switch x := a[0].(type) {
			case int64:
				if x == math.MinInt64 {
					return nil, raiseErr("ValueError", "integer overflow")
				}
				if x < 0 {
					return -x, nil
				}
				return x, nil
			case float64:
				return math.Abs(x), nil
			}
			return nil, raiseErr("TypeError", "math.abs needs a number, not %s", typeName(a[0]))
		}},
		"math.floor": {params: []string{"x"}, required: 1, call: func(_ *frame, a []any) (any, error) {
			switch x := a[0].(type) {
			case int64:
				return x, nil
			case float64:
				return int64(math.Floor(x)), nil
			}
			return nil, raiseErr("TypeError", "math.floor needs a number, not %s", typeName(a[0]))
		}},
		"math.max": {params: []string{"x", "y"}, required: 2, call: func(_ *frame, a []any) (any, error) {
			gt, err := binary(">", a[0], a[1])
			if err != nil {
				return nil, err
			}
			if gt.(bool) {
				return a[0], nil
			}
			return a[1], nil
		}},
		"math.min": {params: []string{"x", "y"}, required: 2, call: func(_ *frame, a []any) (any, error) {
			lt, err := binary("<", a[0], a[1])
			if err != nil {
				return nil, err
			}
			if lt.(bool) {
				return a[0], nil
			}
			return a[1], nil
		}},
		// sys's non-blocking functions.
		"sys.get_env": {params: []string{"name", "default"}, required: 1, call: func(f *frame, a []any) (any, error) {
			n, err := str(a[0], "name")
			if err != nil {
				return nil, err
			}
			if v, ok := f.x.env[n]; ok {
				return v, nil
			}
			return a[1], nil
		}},
		"sys.now": {params: nil, call: func(f *frame, _ []any) (any, error) {
			return float64(f.x.now().UnixNano()) / 1e9, nil
		}},
		// text
		"text.decode": {params: []string{"data", "charset"}, required: 1, call: func(_ *frame, a []any) (any, error) {
			b, err := bytesOf(a[0], "data")
			if err != nil {
				return nil, err
			}
			if err := checkCharset(a[1]); err != nil {
				return nil, err
			}
			if !utf8.Valid(b) {
				return nil, raiseErr("ValueError", "the data is not valid UTF-8")
			}
			return strResult(string(b))
		}},
		"text.encode": {params: []string{"text", "charset"}, required: 1, call: func(_ *frame, a []any) (any, error) {
			s, err := str(a[0], "text")
			if err != nil {
				return nil, err
			}
			if err := checkCharset(a[1]); err != nil {
				return nil, err
			}
			return []byte(s), nil
		}},
		"text.find_all": {params: []string{"source", "substr"}, required: 2, call: func(_ *frame, a []any) (any, error) {
			s, err := str(a[0], "source")
			if err != nil {
				return nil, err
			}
			sub, err := str(a[1], "substr")
			if err != nil {
				return nil, err
			}
			out := []any{}
			if sub == "" {
				return out, nil
			}
			for i := 0; ; {
				j := strings.Index(s[i:], sub)
				if j < 0 {
					break
				}
				out = append(out, map[string]any{"index": int64(utf8.RuneCountInString(s[:i+j])), "match": sub})
				i += j + len(sub)
			}
			return out, nil
		}},
		"text.find_all_regex": {params: []string{"source", "regexp"}, required: 2, call: func(_ *frame, a []any) (any, error) {
			s, re, err := regexArgs(a)
			if err != nil {
				return nil, err
			}
			out := []any{}
			for _, loc := range re.FindAllStringIndex(s, -1) {
				out = append(out, map[string]any{"index": int64(utf8.RuneCountInString(s[:loc[0]])), "match": s[loc[0]:loc[1]]})
			}
			return out, nil
		}},
		"text.match_regex": {params: []string{"source", "regexp"}, required: 2, call: func(_ *frame, a []any) (any, error) {
			s, re, err := regexArgs(a)
			if err != nil {
				return nil, err
			}
			return re.MatchString(s), nil
		}},
		"text.replace_all": {params: []string{"source", "substr", "repl"}, required: 3, call: func(_ *frame, a []any) (any, error) {
			s, err := str(a[0], "source")
			if err != nil {
				return nil, err
			}
			sub, err := str(a[1], "substr")
			if err != nil {
				return nil, err
			}
			repl, err := str(a[2], "repl")
			if err != nil {
				return nil, err
			}
			return strResult(strings.ReplaceAll(s, sub, repl))
		}},
		"text.replace_all_regex": {params: []string{"source", "regexp", "repl"}, required: 3, call: func(_ *frame, a []any) (any, error) {
			s, re, err := regexArgs(a)
			if err != nil {
				return nil, err
			}
			repl, err := str(a[2], "repl")
			if err != nil {
				return nil, err
			}
			// Google's replacement names groups \1 .. \9, and \0 the match.
			repl = regexp.MustCompile(`\\(\d)`).ReplaceAllString(strings.ReplaceAll(repl, "$", "$$"), "$${$1}")
			return strResult(re.ReplaceAllString(s, repl))
		}},
		"text.split": {params: []string{"source", "separator"}, required: 2, call: func(_ *frame, a []any) (any, error) {
			s, err := str(a[0], "source")
			if err != nil {
				return nil, err
			}
			sep, err := str(a[1], "separator")
			if err != nil {
				return nil, err
			}
			out := []any{}
			for _, p := range strings.Split(s, sep) {
				out = append(out, p)
			}
			return out, nil
		}},
		"text.substring": {params: []string{"source", "start", "end"}, required: 3, call: func(_ *frame, a []any) (any, error) {
			s, err := str(a[0], "source")
			if err != nil {
				return nil, err
			}
			st, err := intArg(a[1], "start")
			if err != nil {
				return nil, err
			}
			en, err := intArg(a[2], "end")
			if err != nil {
				return nil, err
			}
			r := []rune(s)
			if st < 0 {
				st = 0
			}
			if en > int64(len(r)) {
				en = int64(len(r))
			}
			if st >= en {
				return "", nil
			}
			return string(r[st:en]), nil
		}},
		"text.to_lower": {params: []string{"source"}, required: 1, call: func(_ *frame, a []any) (any, error) {
			s, err := str(a[0], "source")
			return strings.ToLower(s), err
		}},
		"text.to_upper": {params: []string{"source"}, required: 1, call: func(_ *frame, a []any) (any, error) {
			s, err := str(a[0], "source")
			return strings.ToUpper(s), err
		}},
		"text.url_encode": {params: []string{"source"}, required: 1, call: func(_ *frame, a []any) (any, error) {
			s, err := str(a[0], "source")
			if err != nil {
				return nil, err
			}
			return strResult(strings.ReplaceAll(url.QueryEscape(s), "+", "%20"))
		}},
		"text.url_encode_plus": {params: []string{"source"}, required: 1, call: func(_ *frame, a []any) (any, error) {
			s, err := str(a[0], "source")
			if err != nil {
				return nil, err
			}
			return strResult(url.QueryEscape(s))
		}},
		"text.url_decode": {params: []string{"source"}, required: 1, call: func(_ *frame, a []any) (any, error) {
			s, err := str(a[0], "source")
			if err != nil {
				return nil, err
			}
			out, err := url.QueryUnescape(s)
			if err != nil {
				return nil, raiseErr("ValueError", "invalid URL encoding: %v", err)
			}
			return out, nil
		}},
		// time
		"time.format": {params: []string{"seconds", "timezone"}, required: 1, call: func(_ *frame, a []any) (any, error) {
			sec, ok := toFloat(a[0])
			if !ok {
				return nil, raiseErr("TypeError", "time.format needs seconds as a number, not %s", typeName(a[0]))
			}
			loc := time.UTC
			if a[1] != nil {
				tz, err := str(a[1], "timezone")
				if err != nil {
					return nil, err
				}
				if loc, err = time.LoadLocation(tz); err != nil {
					return nil, raiseErr("ValueError", "unknown time zone %q", tz)
				}
			}
			whole, frac := math.Modf(sec)
			t := time.Unix(int64(whole), int64(frac*1e9)).In(loc)
			return t.Format("2006-01-02T15:04:05.000000Z07:00"), nil
		}},
		"time.parse": {params: []string{"value"}, required: 1, call: func(_ *frame, a []any) (any, error) {
			s, err := str(a[0], "value")
			if err != nil {
				return nil, err
			}
			t, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return nil, raiseErr("ValueError", "%q is not an RFC 3339 timestamp", s)
			}
			return float64(t.UnixNano()) / 1e9, nil
		}},
		// uuid
		"uuid.generate": {params: nil, call: func(*frame, []any) (any, error) {
			var b [16]byte
			_, _ = rand.Read(b[:])
			b[6] = b[6]&0x0f | 0x40
			b[8] = b[8]&0x3f | 0x80
			return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
		}},
		"events.create_callback_endpoint": {params: []string{"http_callback_method"}, defaults: map[string]any{"http_callback_method": "POST"},
			call: func(f *frame, a []any) (any, error) { return f.x.createCallback(a[0]) }},
	}
}

func checkCharset(v any) error {
	if v == nil {
		return nil
	}
	cs, ok := v.(string)
	if !ok || !(strings.EqualFold(cs, "UTF-8") || strings.EqualFold(cs, "UTF8")) {
		return raiseErr("ValueError", "unsupported charset %v: UTF-8 is supported", v)
	}
	return nil
}

func regexArgs(a []any) (string, *regexp.Regexp, error) {
	s, err := str(a[0], "source")
	if err != nil {
		return "", nil, err
	}
	pat, err := str(a[1], "regexp")
	if err != nil {
		return "", nil, err
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return "", nil, raiseErr("ValueError", "invalid regular expression %q: %v", pat, err)
	}
	return s, re, nil
}

func mergeNested(dst, src map[string]any) map[string]any {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				dst[k] = mergeNested(dm, sm)
				continue
			}
		}
		dst[k] = deepCopy(v)
	}
	return dst
}

// bind orders a call's arguments by the function's parameters: positional
// from an expression, named from a call step.
func (d *fn) bind(name string, positional []any, named map[string]any) ([]any, error) {
	out := make([]any, len(d.params))
	if named != nil {
		known := map[string]int{}
		for i, p := range d.params {
			known[p] = i
		}
		for k, v := range named {
			i, ok := known[k]
			if !ok {
				return nil, raiseErr("TypeError", "%s() got an unexpected argument %q", name, k)
			}
			out[i] = v
		}
		for i, p := range d.params {
			if _, ok := named[p]; !ok {
				if i < d.required {
					return nil, raiseErr("TypeError", "%s() is missing the argument %q", name, p)
				}
				out[i] = d.defaults[p]
			}
		}
		return out, nil
	}
	if len(positional) < d.required || len(positional) > len(d.params) {
		return nil, raiseErr("TypeError", "%s() takes %d to %d arguments, not %d", name, d.required, len(d.params), len(positional))
	}
	copy(out, positional)
	for i := len(positional); i < len(d.params); i++ {
		out[i] = d.defaults[d.params[i]]
	}
	return out, nil
}
