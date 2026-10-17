package spannerfront

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// The list filters of backups and operations (#1498), as Google documents
// them: comparisons `field op value`, op one of < > <= >= != = and : (has),
// joined by AND or OR, a term in parentheses. A field is a proto field
// name, dotted into messages (metadata.database, metadata.@type). A value
// is compared as a number when both sides are numbers, as a time when both
// are RFC 3339 times, and as a string otherwise.

type filter struct {
	any [][]term // OR of ANDs
}

type term struct{ path, op, value string }

var filterOps = []string{"<=", ">=", "!=", "<", ">", "=", ":"}

func parseFilter(s string) (*filter, error) {
	f := &filter{}
	s = strings.TrimSpace(s)
	if s == "" {
		return f, nil
	}
	// AND binds tighter than OR in Google's examples, which put each
	// comparison in parentheses: split on OR, then on AND.
	for _, or := range splitTop(s, " OR ") {
		var and []term
		for _, t := range splitTop(or, " AND ") {
			t = strings.TrimSpace(t)
			for strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")") {
				t = strings.TrimSpace(t[1 : len(t)-1])
			}
			if strings.Contains(t, " OR ") || strings.Contains(t, " AND ") {
				return nil, status.Errorf(codes.InvalidArgument, "Invalid filter %q: nested expressions are not supported here; put each comparison in its own parentheses.", s)
			}
			parsed, ok := parseTerm(t)
			if !ok {
				return nil, status.Errorf(codes.InvalidArgument, "Invalid filter %q: %q is not a comparison.", s, t)
			}
			and = append(and, parsed)
		}
		f.any = append(f.any, and)
	}
	return f, nil
}

func parseTerm(t string) (term, bool) {
	best, at := "", -1
	for _, op := range filterOps {
		if i := strings.Index(t, op); i > 0 && (at < 0 || i < at || i == at && len(op) > len(best)) {
			best, at = op, i
		}
	}
	if at < 0 {
		return term{}, false
	}
	path := strings.TrimSpace(t[:at])
	v := strings.TrimSpace(t[at+len(best):])
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		v = v[1 : len(v)-1]
	}
	return term{path: path, op: best, value: v}, path != ""
}

// splitTop splits s on sep outside parentheses and quotes.
func splitTop(s, sep string) []string {
	var out []string
	depth, quote, start := 0, byte(0), 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth == 0 && strings.HasPrefix(s[i:], sep):
			out = append(out, s[start:i])
			start = i + len(sep)
			i += len(sep) - 1
		}
	}
	return append(out, s[start:])
}

func (f *filter) match(m proto.Message) bool {
	if len(f.any) == 0 {
		return true
	}
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	if err != nil {
		return false
	}
	var doc map[string]any
	if json.Unmarshal(b, &doc) != nil {
		return false
	}
	for _, and := range f.any {
		ok := true
		for _, t := range and {
			ok = ok && t.match(doc)
		}
		if ok {
			return true
		}
	}
	return false
}

func (t term) match(doc map[string]any) bool {
	var v any = doc
	for _, p := range strings.Split(t.path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			v = nil
			break
		}
		v = m[p]
	}
	got := ""
	switch x := v.(type) {
	case nil:
	case string:
		got = x
	case []any:
		for _, e := range x {
			if t.op == ":" && strings.Contains(fmt.Sprint(e), t.value) {
				return true
			}
		}
		return false
	default:
		got = fmt.Sprint(x)
	}
	if t.op == ":" {
		return strings.Contains(got, t.value)
	}
	c := compare(got, t.value)
	switch t.op {
	case "=":
		return c == 0
	case "!=":
		return c != 0
	case "<":
		return c < 0
	case ">":
		return c > 0
	case "<=":
		return c <= 0
	default:
		return c >= 0
	}
}

func compare(a, b string) int {
	if x, err := strconv.ParseFloat(a, 64); err == nil {
		if y, err := strconv.ParseFloat(b, 64); err == nil {
			switch {
			case x < y:
				return -1
			case x > y:
				return 1
			}
			return 0
		}
	}
	if x, err := time.Parse(time.RFC3339Nano, a); err == nil {
		if y, err := time.Parse(time.RFC3339Nano, b); err == nil {
			return x.Compare(y)
		}
	}
	return strings.Compare(a, b)
}
