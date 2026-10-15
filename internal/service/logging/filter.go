package logging

import (
	"strconv"
	"strings"
	"time"

	ltype "google.golang.org/genproto/googleapis/logging/type"

	"cloud.google.com/go/logging/apiv2/loggingpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// The filter grammar ListLogEntries accepts (#304), a documented subset of
// the Logging query language:
//
//	filter     = term { [ "AND" ] term }
//	term       = field op value
//	field      = "logName" | "severity" | "resource.type" | "timestamp"
//	           | "resource.labels." key | "labels." key
//	key        = a bare word, or a double-quoted string (labels."a/b")
//	op         = "=" | "!=" | ">" | ">=" | "<" | "<="    (logName and resource.type: "=" and "!=" only)
//	value      = a bare word, or a double-quoted string
//
// Terms are ANDed; an explicit AND is optional, as in the real language.
// severity compares by level (DEFAULT < DEBUG < INFO < NOTICE < WARNING <
// ERROR < CRITICAL < ALERT < EMERGENCY), or by its number (severity >= 400).
// timestamp values are RFC 3339.
//
// A label term compares the monitored resource's label (resource.labels.*)
// or the entry's own label (labels.*) as the query language documents for
// strings: keys are case-sensitive, comparisons ignore letter case and are
// lexicographic, and every comparison on a missing label is false, != included
// (docs.cloud.google.com/logging/docs/view/logging-query-language).
//
// Anything else — OR, NOT, parentheses, a field outside these, the ":"
// has operator, a function — is INVALID_ARGUMENT naming the term. A filter
// that ignored a term it did not understand would return entries the caller
// excluded, which is worse than refusing.

type pred func(*loggingpb.LogEntry) bool

// tokens splits a filter into words, keeping double-quoted strings whole and
// splitting operators from their operands.
func tokens(f string) ([]string, error) {
	var out []string
	i := 0
	for i < len(f) {
		c := f[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			i++
		case c == '"':
			j := i + 1
			for j < len(f) && f[j] != '"' {
				if f[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(f) {
				return nil, apierror.InvalidArgument("filter has an unterminated string")
			}
			out = append(out, f[i:j+1])
			i = j + 1
		case strings.ContainsRune("<>=!:()", rune(c)):
			j := i + 1
			if j < len(f) && f[j] == '=' {
				j++
			}
			out = append(out, f[i:j])
			i = j
		default:
			j := i
			for j < len(f) && !strings.ContainsRune(" \t\n\"<>=!:()", rune(f[j])) {
				j++
			}
			out = append(out, f[i:j])
			i = j
		}
	}
	return out, nil
}

func unquote(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return strings.ReplaceAll(v[1:len(v)-1], `\"`, `"`)
	}
	return v
}

// compile turns a filter into a predicate.
func compile(filter string) (pred, error) {
	toks, err := tokens(filter)
	if err != nil {
		return nil, err
	}
	var preds []pred
	for i := 0; i < len(toks); {
		if toks[i] == "AND" {
			i++
			continue
		}
		if i+2 >= len(toks) {
			return nil, apierror.InvalidArgument("filter term %q is incomplete; the supported form is field op value",
				strings.Join(toks[i:], " "))
		}
		// A map key with special characters is double-quoted after its dot,
		// labels."compute.googleapis.com/resource_id": one field.
		if i+1 < len(toks) && strings.HasSuffix(toks[i], ".") && strings.HasPrefix(toks[i+1], `"`) {
			joined := toks[i] + toks[i+1]
			toks = append(append(toks[:i:i], joined), toks[i+2:]...)
		}
		if i+2 >= len(toks) {
			return nil, apierror.InvalidArgument("filter term %q is incomplete; the supported form is field op value",
				strings.Join(toks[i:], " "))
		}
		field, op, value := toks[i], toks[i+1], unquote(toks[i+2])
		term := toks[i] + " " + toks[i+1] + " " + toks[i+2]
		p, err := termPred(field, op, value, term)
		if err != nil {
			return nil, err
		}
		preds = append(preds, p)
		i += 3
	}
	return func(e *loggingpb.LogEntry) bool {
		for _, p := range preds {
			if !p(e) {
				return false
			}
		}
		return true
	}, nil
}

func termPred(field, op, value, term string) (pred, error) {
	if field == "OR" || field == "NOT" || field == "(" || field == ")" || strings.HasPrefix(field, "-") {
		return nil, apierror.InvalidArgument("filter term %q is not supported: only ANDed field op value terms are", field)
	}
	switch op {
	case "=", "!=", ">", ">=", "<", "<=":
	default:
		return nil, apierror.InvalidArgument("filter term %q is not supported: operator %q is not one of = != > >= < <=", term, op)
	}
	eq := func(got string) bool {
		if op == "=" {
			return got == value
		}
		return got != value
	}
	switch field {
	case "logName":
		if op != "=" && op != "!=" {
			return nil, apierror.InvalidArgument("filter term %q is not supported: logName takes = or !=", term)
		}
		return func(e *loggingpb.LogEntry) bool { return eq(e.GetLogName()) }, nil
	case "resource.type":
		if op != "=" && op != "!=" {
			return nil, apierror.InvalidArgument("filter term %q is not supported: resource.type takes = or !=", term)
		}
		return func(e *loggingpb.LogEntry) bool { return eq(e.GetResource().GetType()) }, nil
	case "severity":
		want, ok := ltype.LogSeverity_value[strings.ToUpper(value)]
		if n, err := strconv.ParseInt(value, 10, 32); err == nil {
			want, ok = int32(n), true
		}
		if !ok {
			return nil, apierror.InvalidArgument("filter term %q is not supported: %q is not a severity", term, value)
		}
		return func(e *loggingpb.LogEntry) bool { return compare(int64(e.GetSeverity()), int64(want), op) }, nil
	case "timestamp":
		t, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return nil, apierror.InvalidArgument("filter term %q is not supported: timestamps are RFC 3339", term)
		}
		return func(e *loggingpb.LogEntry) bool {
			return compare(e.GetTimestamp().AsTime().UnixNano(), t.UnixNano(), op)
		}, nil
	}
	if key, ok := labelKey(field, "resource.labels."); ok {
		return labelPred(op, value, func(e *loggingpb.LogEntry) map[string]string { return e.GetResource().GetLabels() }, key), nil
	}
	if key, ok := labelKey(field, "labels."); ok {
		return labelPred(op, value, func(e *loggingpb.LogEntry) map[string]string { return e.GetLabels() }, key), nil
	}
	return nil, apierror.InvalidArgument(
		"filter term %q is not supported: the fields supported are logName, severity, resource.type, timestamp, resource.labels.* and labels.*", term)
}

// labelKey returns the map key a label field names: the rest of the path
// after prefix, unquoted when it was double-quoted. A key holding a further
// unquoted dot is a nested path, which a string map does not have.
func labelKey(field, prefix string) (string, bool) {
	if !strings.HasPrefix(field, prefix) || len(field) == len(prefix) {
		return "", false
	}
	key := field[len(prefix):]
	if strings.HasPrefix(key, `"`) {
		return unquote(key), true
	}
	if strings.Contains(key, ".") {
		return "", false
	}
	return key, true
}

// labelPred compares a string label as the query language documents:
// ignoring letter case, lexicographically, and false whenever the label is
// missing, for != as for every other operator.
func labelPred(op, value string, labels func(*loggingpb.LogEntry) map[string]string, key string) pred {
	want := strings.ToLower(value)
	return func(e *loggingpb.LogEntry) bool {
		got, ok := labels(e)[key]
		if !ok {
			return false
		}
		return compare(int64(strings.Compare(strings.ToLower(got), want)), 0, op)
	}
}

func compare(got, want int64, op string) bool {
	switch op {
	case "=":
		return got == want
	case "!=":
		return got != want
	case ">":
		return got > want
	case ">=":
		return got >= want
	case "<":
		return got < want
	default:
		return got <= want
	}
}
