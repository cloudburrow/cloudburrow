package workflows

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The limits Google documents (docs.cloud.google.com/workflows/quotas,
// "Limits"), enforced by the interpreter.
const (
	// MaxSteps is "Steps: 100,000 per execution".
	MaxSteps = 100000
	// MaxAssignments is "Assignments: 50 per assign step".
	MaxAssignments = 50
	// MaxConditions is "Conditions: 50 per switch step".
	MaxConditions = 50
	// MaxCallDepth is "Call stack depth: 20".
	MaxCallDepth = 20
	// MaxBranches is "Branches: 10 per parallel step".
	MaxBranches = 10
	// MaxParallelDepth is "Parallel nesting depth: 2".
	MaxParallelDepth = 2
	// MaxConcurrentBranches is "Concurrent branches and iterations: 20 per
	// execution"; more wait their turn.
	MaxConcurrentBranches = 20
	// MaxUnhandledExceptions is "Unhandled exceptions: 100 per execution"
	// of parallel branches and iterations.
	MaxUnhandledExceptions = 100
	// MaxSourceBytes is "Source code size: 128 KB".
	MaxSourceBytes = 128 * 1024
	// MaxHTTPResponseBytes is "HTTP response size: 2 MB".
	MaxHTTPResponseBytes = 2 * 1024 * 1024
	// MaxMemoryBytes is "Memory: 512 KB" for variables, arguments and
	// events.
	MaxMemoryBytes = 512 * 1024
	// MaxStringBytes is "String length: 256 KB".
	MaxStringBytes = 256 * 1024
	// MaxEnvVars and MaxEnvVarBytes are the user-defined environment
	// variables' limits: 20, of 4 KiB each.
	MaxEnvVars     = 20
	MaxEnvVarBytes = 4 * 1024
	// DefaultHTTPTimeout and MaxHTTPTimeout are an HTTP step's: 300 s by
	// default, 1,800 s at most; connectors default to 1,800 s.
	DefaultHTTPTimeout = 300
	MaxHTTPTimeout     = 1800
)

// wfError is an error a workflow can catch: the raised value, which for
// the platform's errors is a map with message and tags.
type wfError struct {
	val any
	// at is the step and routine it was raised in, for the stack trace.
	step, routine string
	line, col     int
}

func (e *wfError) Error() string {
	if m, ok := e.val.(map[string]any); ok {
		msg, _ := m["message"].(string)
		var tags []string
		if t, ok := m["tags"].([]any); ok {
			for _, x := range t {
				if s, ok := x.(string); ok {
					tags = append(tags, s)
				}
			}
		}
		if len(tags) > 0 {
			return tags[0] + ": " + msg
		}
		if msg != "" {
			return msg
		}
	}
	b, _ := json.Marshal(jsonable(e.val))
	return string(b)
}

// tags are the error's tags.
func (e *wfError) tags() []string {
	m, ok := e.val.(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	if t, ok := m["tags"].([]any); ok {
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func (e *wfError) hasTag(tag string) bool {
	for _, t := range e.tags() {
		if t == tag {
			return true
		}
	}
	return false
}

// raiseErr is one of the platform's errors (syntax/error-types): a map of
// the message and the tag.
func raiseErr(tag, format string, args ...any) *wfError {
	return &wfError{val: map[string]any{"message": fmt.Sprintf(format, args...), "tags": []any{tag}}}
}

// context is Error.context: Google's "RuntimeError" line with where it
// was raised.
func (e *wfError) context() string {
	var b strings.Builder
	b.WriteString("RuntimeError: ")
	b.WriteString(e.Error())
	if e.step != "" {
		fmt.Fprintf(&b, "\nin step %q, routine %q, line: %d", e.step, e.routine, e.line)
	}
	return b.String()
}

// jsonable turns a value into what encoding/json writes as Google does,
// sorting nothing (maps are written in key order by encoding/json).
func jsonable(v any) any {
	switch x := v.(type) {
	case funcRef:
		return x.name
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = jsonable(x[i])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, w := range x {
			out[k] = jsonable(w)
		}
		return out
	}
	return v
}

// encodeJSON is json.encode: bytes cannot be encoded (datatypes).
func encodeJSON(v any, indent bool) ([]byte, error) {
	if err := checkEncodable(v); err != nil {
		return nil, err
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(jsonable(v)); err != nil {
		return nil, raiseErr("TypeError", "the value cannot be JSON-encoded: %v", err)
	}
	return []byte(strings.TrimSuffix(b.String(), "\n")), nil
}

func checkEncodable(v any) error {
	switch x := v.(type) {
	case []byte:
		return raiseErr("TypeError", "bytes cannot be JSON-encoded; base64.encode them first")
	case funcRef:
		return raiseErr("TypeError", "a function cannot be JSON-encoded")
	case []any:
		for _, w := range x {
			if err := checkEncodable(w); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, w := range x {
			if err := checkEncodable(w); err != nil {
				return err
			}
		}
	}
	return nil
}

// fromJSON converts a value decoded with UseNumber into the language's:
// integers where the number has no fraction, doubles otherwise.
func fromJSON(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil && !strings.ContainsAny(x.String(), ".eE") {
			return i
		}
		f, _ := x.Float64()
		return f
	case float64:
		return x
	case int:
		return int64(x)
	case []any:
		for i := range x {
			x[i] = fromJSON(x[i])
		}
		return x
	case map[string]any:
		for k, w := range x {
			x[k] = fromJSON(w)
		}
		return x
	}
	return v
}

func decodeJSON(b []byte) (any, error) {
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if d.More() {
		return nil, fmt.Errorf("trailing data after the JSON value")
	}
	return fromJSON(v), nil
}

// deepCopy copies a value, so an assignment never aliases another
// variable's list or map.
func deepCopy(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = deepCopy(x[i])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, w := range x {
			out[k] = deepCopy(w)
		}
		return out
	case []byte:
		return append([]byte(nil), x...)
	}
	return v
}

// sizeOf estimates a value's memory as its JSON size, bytes as their length.
func sizeOf(v any) int {
	switch x := v.(type) {
	case nil:
		return 4
	case bool:
		return 5
	case int64, float64:
		return 8
	case string:
		return len(x) + 2
	case []byte:
		return len(x)
	case []any:
		n := 2
		for _, w := range x {
			n += sizeOf(w) + 1
		}
		return n
	case map[string]any:
		n := 2
		for k, w := range x {
			n += len(k) + 3 + sizeOf(w) + 1
		}
		return n
	}
	return 8
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
