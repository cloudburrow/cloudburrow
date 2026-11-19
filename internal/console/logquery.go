package console

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The Logs Explorer's query editor (#1863), in the Logging query language
// Google's Logs Explorer takes
// (https://docs.cloud.google.com/logging/docs/view/logging-query-language),
// over the fields the console's entries carry, each named as the LogEntry
// field it stands for (LogEntryFields):
//
//	query      = group { [ "AND" ] group }
//	group      = term { "OR" term }          (OR binds tighter than AND, as in the language)
//	term       = [ "NOT" | "-" ] ( field op value | value )
//	op         = "=" | "!=" | ":" | ">" | ">=" | "<" | "<="
//	value      = a bare word or a double-quoted string
//
// A value on its own searches every field, as the language's global
// restriction does. ":" is the has operator: the field contains the value,
// ignoring letter case. "=" and "!=" compare whole values; severity compares
// by level (DEFAULT < DEBUG < INFO < NOTICE < WARNING < ERROR < CRITICAL <
// ALERT < EMERGENCY) or number (severity>=400), and timestamp as RFC 3339.
// A comparison on a field the entry does not have is false. Lines starting
// with "--" are comments. Parentheses, regular expressions (=~) and
// functions are refused, naming the term: a query that ignored part of
// itself would show entries the reader excluded.

// LogEntryFields are an entry's fields as Cloud Logging's LogEntry names
// them, flattened to their paths: what the query editor matches, the Fields
// pane counts and an expanded entry shows.
func LogEntryFields(e Entry) map[string]string {
	f := map[string]string{
		"insertId":      strconv.FormatUint(e.ID, 10),
		"timestamp":     e.Timestamp.UTC().Format(time.RFC3339Nano),
		"severity":      string(e.Severity),
		"resource.type": productOf(e.Source),
		"textPayload":   e.Message,
	}
	if e.Source != "" {
		f["resource.labels.source"] = e.Source
	}
	if strings.Contains(e.Resource, "/logs/") {
		f["logName"] = e.Resource
	} else if e.Resource != "" {
		f["resource.labels.name"] = e.Resource
	}
	if e.Project != "" {
		f["resource.labels.project_id"] = e.Project
	}
	if e.OperationID != "" {
		f["operation.id"] = e.OperationID
	}
	return f
}

// logEntryJSON nests LogEntryFields back into the LogEntry shape, for the
// expanded entry.
func logEntryJSON(e Entry) map[string]any {
	out := map[string]any{}
	for path, v := range LogEntryFields(e) {
		parts := strings.Split(path, ".")
		m := out
		for _, p := range parts[:len(parts)-1] {
			next, ok := m[p].(map[string]any)
			if !ok {
				next = map[string]any{}
				m[p] = next
			}
			m = next
		}
		m[parts[len(parts)-1]] = v
	}
	return out
}

// MarshalJSON adds the entry's LogEntry form, so the page shows and copies
// what its query matches.
func (e Entry) MarshalJSON() ([]byte, error) {
	type plain Entry
	return json.Marshal(struct {
		plain
		LogEntry map[string]any `json:"logEntry"`
	}{plain(e), logEntryJSON(e)})
}

// severityLevels are the language's levels and their numbers.
var severityLevels = map[string]int{
	"DEFAULT": 0, "DEBUG": 100, "INFO": 200, "NOTICE": 300, "WARNING": 400,
	"ERROR": 500, "CRITICAL": 600, "ALERT": 700, "EMERGENCY": 800,
}

type entryPred func(map[string]string) bool

func queryTokens(q string) ([]string, error) {
	var lines []string
	for _, line := range strings.Split(q, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		lines = append(lines, line)
	}
	s := strings.Join(lines, "\n")
	var out []string
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("the query has an unterminated string")
			}
			out = append(out, s[i:j+1])
			i = j + 1
		case strings.ContainsRune("<>=!:()~", rune(c)):
			j := i + 1
			for j < len(s) && strings.ContainsRune("=~", rune(s[j])) {
				j++
			}
			out = append(out, s[i:j])
			i = j
		default:
			j := i
			for j < len(s) && !strings.ContainsRune(" \t\n\r\"<>=!:()~", rune(s[j])) {
				j++
			}
			out = append(out, s[i:j])
			i = j
		}
	}
	return out, nil
}

func unquoteValue(v string) string {
	if len(v) >= 2 && v[0] == '"' {
		if u, err := strconv.Unquote(v); err == nil {
			return u
		}
		return v[1 : len(v)-1]
	}
	return v
}

func isOp(t string) bool {
	switch t {
	case "=", "!=", ":", ">", ">=", "<", "<=":
		return true
	}
	return false
}

// CompileQuery compiles a Logs Explorer query; empty matches everything.
func CompileQuery(q string) (func(Entry) bool, error) {
	toks, err := queryTokens(q)
	if err != nil {
		return nil, err
	}
	var groups []entryPred
	for i := 0; i < len(toks); {
		if toks[i] == "AND" {
			i++
			continue
		}
		var alts []entryPred
		for {
			p, n, err := queryTerm(toks, i)
			if err != nil {
				return nil, err
			}
			alts = append(alts, p)
			i = n
			if i < len(toks) && toks[i] == "OR" {
				i++
				if i >= len(toks) {
					return nil, fmt.Errorf("the query ends with OR")
				}
				continue
			}
			break
		}
		if len(alts) == 1 {
			groups = append(groups, alts[0])
			continue
		}
		groups = append(groups, func(f map[string]string) bool {
			for _, a := range alts {
				if a(f) {
					return true
				}
			}
			return false
		})
	}
	if len(groups) == 0 {
		return nil, nil
	}
	return func(e Entry) bool {
		f := LogEntryFields(e)
		for _, g := range groups {
			if !g(f) {
				return false
			}
		}
		return true
	}, nil
}

func queryTerm(toks []string, i int) (entryPred, int, error) {
	negate := false
	if toks[i] == "NOT" {
		negate = true
		i++
	} else if strings.HasPrefix(toks[i], "-") && len(toks[i]) > 1 {
		negate = true
		toks = append(append(append([]string{}, toks[:i]...), toks[i][1:]), toks[i+1:]...)
	}
	if i >= len(toks) {
		return nil, 0, fmt.Errorf("the query ends with NOT")
	}
	t := toks[i]
	if t == "(" || t == ")" {
		return nil, 0, fmt.Errorf("parentheses are not supported in this console's queries; write the terms without them")
	}
	if strings.Contains(t, "~") {
		return nil, 0, fmt.Errorf("regular expressions (%s) are not supported in this console's queries", t)
	}
	if isOp(t) {
		return nil, 0, fmt.Errorf("the term %q has no field before it", t)
	}
	var p entryPred
	next := i + 1
	if next < len(toks) && strings.Contains(toks[next], "~") {
		return nil, 0, fmt.Errorf("regular expressions (%s %s) are not supported in this console's queries", t, toks[next])
	}
	if next < len(toks) && isOp(toks[next]) {
		if next+1 >= len(toks) || isOp(toks[next+1]) || toks[next+1] == "(" || toks[next+1] == ")" {
			return nil, 0, fmt.Errorf("the term %s%s has no value", t, toks[next])
		}
		var err error
		p, err = comparison(t, toks[next], unquoteValue(toks[next+1]))
		if err != nil {
			return nil, 0, err
		}
		next += 2
	} else {
		want := strings.ToLower(unquoteValue(t))
		p = func(f map[string]string) bool {
			for _, v := range f {
				if strings.Contains(strings.ToLower(v), want) {
					return true
				}
			}
			return false
		}
	}
	if negate {
		inner := p
		p = func(f map[string]string) bool { return !inner(f) }
	}
	return p, next, nil
}

var queryFields = []string{"insertId", "timestamp", "severity", "logName", "resource.type",
	"resource.labels.source", "resource.labels.name", "resource.labels.project_id", "textPayload", "operation.id"}

func comparison(field, op, value string) (entryPred, error) {
	known := false
	for _, k := range queryFields {
		if k == field {
			known = true
		}
	}
	if !known {
		return nil, fmt.Errorf("the field %s is not one this console's entries carry; use one of %s", field, strings.Join(queryFields, ", "))
	}
	switch field {
	case "severity":
		want, ok := severityLevels[strings.ToUpper(value)]
		if !ok {
			n, err := strconv.Atoi(value)
			if err != nil {
				return nil, fmt.Errorf("severity%s%s: %q is not a severity", op, value, value)
			}
			want = n
		}
		if op == ":" {
			op = "="
		}
		return func(f map[string]string) bool {
			got, ok := severityLevels[f["severity"]]
			return ok && compareInts(got, want, op)
		}, nil
	case "timestamp":
		if op == ":" {
			break
		}
		want, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return nil, fmt.Errorf("timestamp%s%s: the time is not RFC 3339", op, value)
		}
		return func(f map[string]string) bool {
			got, err := time.Parse(time.RFC3339Nano, f["timestamp"])
			if err != nil {
				return false
			}
			c := 0
			if got.Before(want) {
				c = -1
			} else if got.After(want) {
				c = 1
			}
			return compareInts(c, 0, op)
		}, nil
	}
	return func(f map[string]string) bool {
		got, ok := f[field]
		if !ok {
			return false
		}
		if op == ":" {
			return strings.Contains(strings.ToLower(got), strings.ToLower(value))
		}
		return compareInts(strings.Compare(got, value), 0, op)
	}, nil
}

func compareInts(got, want int, op string) bool {
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
	case "<=":
		return got <= want
	}
	return false
}
