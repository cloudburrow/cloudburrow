package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The position of a script's error in the script (#1585).
//
// The front runs a script a statement at a time (scriptflow.go), each as a
// query of its own, so the position the engine gives a statement's error
// ("Syntax error: SELECT list must not be empty at [1:8]") counts from the
// start of that statement. BigQuery's counts from the start of the
// script: SELECT 1;\nSELECT FROM fails at [2:8], and DECLARE x INT64;
// SELECT nope FROM ds.t at [1:25] (measured through BigQuery, #1585's
// report). inScript and errorsInScript move the position by where the
// statement is in the script.
//
// The text the front runs is not always the statement as written: a
// variable's name is replaced by its value, and a condition is run as
// SELECT (condition). The position is moved only when the text up to it is
// the statement's as written, and is left as the engine gave it otherwise.

// positionAtEnd is the position an error ends with, BigQuery's " at
// [line:column]" or the engine's " [at line:column]".
var positionAtEnd = regexp.MustCompile(` (?:at \[|\[at )([0-9]+):([0-9]+)\]$`)

// inScript moves the positions of flow's error, of the statement at
// flow.pos in the script, to the script's.
func (s *scriptRun) inScript(flow *scriptFlow) {
	if s.depth != 0 || flow.text == "" || flow.pos < 0 || flow.pos > len(s.sql) {
		return
	}
	flow.msg = positionInScript(flow.msg, s.sql, flow.pos, flow.text)
	if len(flow.body) > 0 {
		flow.body = errorsInScript(flow.body, s.sql, flow.pos, flow.text)
	}
}

// errorsInScript moves the positions of the messages of b, an error
// answer or a failed job's, a statement at pos in script, to the script's.
func errorsInScript(b []byte, script string, pos int, text string) []byte {
	if pos < 0 || pos > len(script) {
		return b
	}
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if dec.Decode(&body) != nil {
		return b
	}
	// An error answer, or a job (a statement run as a query job) failed
	// with an errorResult.
	e, _ := body["error"].(map[string]any)
	if e == nil {
		st, _ := body["status"].(map[string]any)
		er, _ := st["errorResult"].(map[string]any)
		if er == nil {
			return b
		}
		e = map[string]any{"errors": append([]any{er}, anySlice(st["errors"])...)}
	}
	changed := false
	move := func(m map[string]any) {
		if msg, ok := m["message"].(string); ok {
			if moved := positionInScript(msg, script, pos, text); moved != msg {
				m["message"], changed = moved, true
			}
		}
	}
	move(e)
	errs, _ := e["errors"].([]any)
	for _, v := range errs {
		if m, ok := v.(map[string]any); ok {
			move(m)
		}
	}
	if !changed {
		return b
	}
	out, err := json.Marshal(body)
	if err != nil {
		return b
	}
	return out
}

func anySlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// positionInScript is msg, an error of text, the statement at pos in
// script, with the position it ends with in the script.
func positionInScript(msg, script string, pos int, text string) string {
	m := positionAtEnd.FindStringSubmatchIndex(msg)
	if m == nil {
		return msg
	}
	line, err1 := strconv.Atoi(msg[m[2]:m[3]])
	col, err2 := strconv.Atoi(msg[m[4]:m[5]])
	if err1 != nil || err2 != nil {
		return msg
	}
	off, ok := offsetOf(text, line, col)
	same := commonPrefix(script[pos:], text)
	if !ok || off > same || off == same && same < len(text) {
		return msg
	}
	l, c := lineColumn(script, pos+off)
	return msg[:m[2]] + strconv.Itoa(l) + ":" + strconv.Itoa(c) + msg[m[5]:]
}

// offsetOf is the byte offset in text of the 1-based line and column, in
// characters (lineColumn's inverse).
func offsetOf(text string, line, col int) (int, bool) {
	if line < 1 || col < 1 {
		return 0, false
	}
	off := 0
	for l := 1; l < line; l++ {
		i := strings.IndexByte(text[off:], '\n')
		if i < 0 {
			return 0, false
		}
		off += i + 1
	}
	for c := 1; c < col; c++ {
		if off >= len(text) || text[off] == '\n' {
			return 0, false
		}
		_, size := utf8.DecodeRuneInString(text[off:])
		off += size
	}
	return off, true
}

// commonPrefix is the length of the longest prefix a and b share.
func commonPrefix(a, b string) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
