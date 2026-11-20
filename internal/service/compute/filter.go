package compute

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// A list's filter, as Compute's list methods document it: comparisons of a
// field with = or != (a literal, * as a wildcard), or the legacy eq and ne
// (a regular expression the whole value must match), joined by AND or by
// juxtaposition, each optionally in parentheses. OR and nested fields are
// not served (501).

type term struct {
	field string
	op    string
	value string
	re    *regexp.Regexp
}

type filter []term

var termRE = regexp.MustCompile(`^\s*(?:AND\s+)?\(?\s*([a-zA-Z][a-zA-Z0-9_]*)\s*(!=|=|\beq\b|\bne\b)\s*("(?:[^"\\]|\\.)*"|'[^']*'|[^\s()]+)\s*\)?`)

func parseFilter(f string) (filter, error) {
	var out filter
	rest := strings.TrimSpace(f)
	for rest != "" {
		m := termRE.FindStringSubmatchIndex(rest)
		if m == nil {
			return nil, unimplemented("a list filter other than field comparisons joined by AND (%q)", f)
		}
		t := term{field: rest[m[2]:m[3]], op: rest[m[4]:m[5]], value: rest[m[6]:m[7]]}
		if len(t.value) >= 2 && (t.value[0] == '"' || t.value[0] == '\'') {
			if t.value[0] == '"' {
				if u, err := strconv.Unquote(t.value); err == nil {
					t.value = u
				} else {
					t.value = t.value[1 : len(t.value)-1]
				}
			} else {
				t.value = t.value[1 : len(t.value)-1]
			}
		}
		switch t.op {
		case "eq", "ne":
			re, err := regexp.Compile("^(?:" + t.value + ")$")
			if err != nil {
				return nil, invalid("Invalid value for field 'filter': '%s'. Invalid regular expression.", f)
			}
			t.re = re
		default:
			if strings.Contains(t.value, "*") {
				t.re = regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(t.value), `\*`, ".*") + "$")
			}
		}
		out = append(out, t)
		rest = strings.TrimSpace(rest[m[1]:])
	}
	return out, nil
}

func (f filter) match(o obj) bool {
	for _, t := range f {
		v := ""
		switch x := o[t.field].(type) {
		case string:
			v = x
			// A link matches by its name or its relative or full name.
			if linkKeys[t.field] && t.re == nil && (Rel(t.value, "") == x || strings.HasSuffix(x, "/"+t.value)) {
				v = t.value
			}
		case bool:
			v = strconv.FormatBool(x)
		case float64:
			v = strconv.FormatFloat(x, 'f', -1, 64)
		case nil:
		default:
			v = fmt.Sprint(x)
		}
		ok := v == t.value
		if t.re != nil {
			ok = t.re.MatchString(v)
		}
		if t.op == "!=" || t.op == "ne" {
			ok = !ok
		}
		if !ok {
			return false
		}
	}
	return true
}

func strconvU(v uint64) string { return strconv.FormatUint(v, 10) }
