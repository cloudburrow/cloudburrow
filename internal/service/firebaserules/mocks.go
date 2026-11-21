package firebaserules

// Function mocks in projects.test (#2050).
//
// Google's TestCase.functionMocks mock "service-defined functions": "If not
// set, any service-defined Rules function is expected to return an error".
// A FunctionMock names the function, matches its arguments in order (each
// an exactValue or anyValue) and answers a Result (a value, or undefined:
// "the result could not be computed"); "more than one FunctionMock may be
// provided for a given function name so long as the Arg matchers are
// distinct", and at most one overload may match all anyValue.
// (firebase.google.com/docs/reference/rules/rest/v1/projects/test)
//
// Cloud Firestore's service functions are get, exists, getAfter and
// existsAfter. The emulator evaluates the case, so the source the case
// runs is rewritten: each call of a service function becomes a call of a
// function defined in the source, which answers the mocks' results for
// matching arguments and an error otherwise (an unmocked call, an
// undefined result, arguments no mock matches).

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	rules "google.golang.org/api/firebaserules/v1"
)

// serviceFunctions are cloud.firestore's service-defined functions.
var serviceFunctions = map[string]bool{"get": true, "exists": true, "getAfter": true, "existsAfter": true}

// mockPrefix prefixes the functions a rewritten source defines.
const mockPrefix = "cloudburrowMock_"

// mockError is an expression that compiles and fails when evaluated: a
// map's missing key.
const mockError = "{'cloudburrow': 0}['no-mock']"

// checkMocks validates case i's mocks as Google documents them.
func checkMocks(i int, mocks []*rules.FunctionMock) error {
	seen := map[string]bool{}
	for j, m := range mocks {
		at := fmt.Sprintf("testSuite.testCases[%d].functionMocks[%d]", i, j)
		if !serviceFunctions[m.Function] {
			return invalid("%s.function %q is not a function cloud.firestore declares: get, exists, getAfter or existsAfter.", at, m.Function)
		}
		if len(m.Args) != 1 {
			return invalid("%s.args must hold one Arg: %s takes one path.", at, m.Function)
		}
		var key []string
		for k, a := range m.Args {
			switch {
			case a == nil:
				return invalid("%s.args[%d] must set exactValue or anyValue.", at, k)
			case a.AnyValue != nil && a.ExactValue != nil:
				return invalid("%s.args[%d] sets both exactValue and anyValue.", at, k)
			case a.AnyValue != nil:
				key = append(key, "*")
			case a.ExactValue != nil:
				b, _ := json.Marshal(a.ExactValue)
				key = append(key, string(b))
			default:
				return invalid("%s.args[%d] must set exactValue or anyValue.", at, k)
			}
		}
		if m.Result == nil || (m.Result.Value == nil) == (m.Result.Undefined == nil) {
			return invalid("%s.result must set exactly one of value or undefined.", at)
		}
		k := m.Function + "\x00" + strings.Join(key, "\x00")
		if seen[k] {
			return invalid("%s repeats the Arg matchers of another mock of %s.", at, m.Function)
		}
		seen[k] = true
	}
	return nil
}

// mockedSource is src with each call of a service function answered by
// mocks, or src itself when it calls none.
func mockedSource(src string, mocks []*rules.FunctionMock) (string, error) {
	out, called := renameCalls(src)
	if len(called) == 0 {
		return src, nil
	}
	byFn := map[string][]*rules.FunctionMock{}
	for _, m := range mocks {
		byFn[m.Function] = append(byFn[m.Function], m)
	}
	var fns []string
	for fn := range called {
		fns = append(fns, fn)
	}
	sort.Strings(fns)
	var defs strings.Builder
	for _, fn := range fns {
		expr := mockError
		ms := byFn[fn]
		// Exact matchers before anyValue ones: the any overload answers
		// what no exact one matched.
		sort.SliceStable(ms, func(i, j int) bool { return ms[i].Args[0].AnyValue == nil && ms[j].Args[0].AnyValue != nil })
		for k := len(ms) - 1; k >= 0; k-- {
			m := ms[k]
			res := mockError
			if m.Result.Value != nil {
				v, err := literal(m.Result.Value)
				if err != nil {
					return "", invalid("the result of a mock of %s: %v", fn, err)
				}
				res = v
			}
			if m.Args[0].AnyValue != nil {
				expr = res
				continue
			}
			cond, err := argMatch("p", m.Args[0].ExactValue)
			if err != nil {
				return "", invalid("an argument of a mock of %s: %v", fn, err)
			}
			expr = "(" + cond + ") ? (" + res + ") : (" + expr + ")"
		}
		fmt.Fprintf(&defs, "\n    function %s%s(p) {\n      return %s;\n    }\n", mockPrefix, fn, expr)
	}
	i := insertAt(out)
	if i < 0 {
		return "", invalid("the source has no service cloud.firestore block for the function mocks.")
	}
	return out[:i] + defs.String() + out[i:], nil
}

var (
	documentsMatch = regexp.MustCompile(`match\s+/databases/\{[A-Za-z_][A-Za-z0-9_]*\}/documents\s*\{`)
	serviceBlock   = regexp.MustCompile(`service\s+cloud\.firestore\s*\{`)
)

// insertAt is where the mock functions are defined: inside the first
// match /databases/{database}/documents, else the service block.
func insertAt(src string) int {
	for _, re := range []*regexp.Regexp{documentsMatch, serviceBlock} {
		if loc := codeMatch(src, re); loc >= 0 {
			return loc
		}
	}
	return -1
}

// codeMatch is the end of re's first match outside comments and strings.
func codeMatch(src string, re *regexp.Regexp) int {
	mask := codeMask(src)
	for _, loc := range re.FindAllStringIndex(src, -1) {
		if mask[loc[0]] {
			return loc[1]
		}
	}
	return -1
}

// codeMask reports, per byte, whether it is code: not in a comment or a
// string literal.
func codeMask(src string) []bool {
	mask := make([]bool, len(src)+1)
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], "//"):
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				i = len(src)
			} else {
				i += end + 4
			}
		case src[i] == '\'' || src[i] == '"':
			q := src[i]
			i++
			for i < len(src) && src[i] != q && src[i] != '\n' {
				if src[i] == '\\' {
					i++
				}
				i++
			}
			i++
		default:
			mask[i] = true
			i++
		}
	}
	return mask
}

var callName = regexp.MustCompile(`\b(getAfter|existsAfter|get|exists)\s*\(`)

// renameCalls renames each call of a service function in code (not a
// method call, x.get(...), nor a declaration, function get(...)) to its
// mock, and reports which were called.
func renameCalls(src string) (string, map[string]bool) {
	mask := codeMask(src)
	called := map[string]bool{}
	var b strings.Builder
	last := 0
	for _, loc := range callName.FindAllStringSubmatchIndex(src, -1) {
		s, e := loc[2], loc[3]
		if !mask[s] {
			continue
		}
		before := strings.TrimRight(src[:s], " \t\r\n")
		if strings.HasSuffix(before, ".") || strings.HasSuffix(before, "function") {
			continue
		}
		fn := src[s:e]
		called[fn] = true
		b.WriteString(src[last:s])
		b.WriteString(mockPrefix + fn)
		last = e
	}
	b.WriteString(src[last:])
	return b.String(), called
}

// argMatch is the condition that p, a path, equals v. Google's paths are
// strings (request.path), so a string matches the path it spells.
func argMatch(p string, v any) (string, error) {
	if s, ok := v.(string); ok {
		lit := quote(s)
		return fmt.Sprintf("(%[1]s is path && %[1]s == path(%[2]s)) || (%[1]s is string && %[1]s == %[2]s)", p, lit), nil
	}
	lit, err := literal(v)
	if err != nil {
		return "", err
	}
	return p + " == " + lit, nil
}

// literal is a JSON value as a rules expression.
func literal(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "null", nil
	case bool:
		return strconv.FormatBool(x), nil
	case string:
		return quote(x), nil
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return strconv.FormatInt(int64(x), 10), nil
		}
		s := strconv.FormatFloat(x, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		return s, nil
	case json.Number:
		return x.String(), nil
	case []any:
		var parts []string
		for _, e := range x {
			s, err := literal(e)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			s, err := literal(x[k])
			if err != nil {
				return "", err
			}
			parts = append(parts, quote(k)+": "+s)
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	}
	return "", fmt.Errorf("%v (%T) is not a JSON value", v, v)
}

// quote is s as a rules string literal.
func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return "'" + r.Replace(s) + "'"
}
