package bigqueryfront

import "testing"

// TestPositionInScript (#1585): a statement's error position counts from
// the start of the script, where the text up to it is the statement's.
func TestPositionInScript(t *testing.T) {
	for _, tc := range []struct {
		script string
		pos    int
		text   string
		msg    string
		want   string
	}{
		{"SELECT 1;\nSELECT FROM", 10, "SELECT FROM", "Syntax error: SELECT list must not be empty at [1:8]",
			"Syntax error: SELECT list must not be empty at [2:8]"},
		{"DECLARE x INT64; SELECT nope FROM ds.t", 17, "SELECT nope FROM ds.t", "Unrecognized name: nope at [1:8]",
			"Unrecognized name: nope at [1:25]"},
		{"DECLARE x INT64; SELECT nope FROM ds.t", 17, "SELECT nope FROM ds.t", "failed to analyze: Unrecognized name: nope [at 1:8]",
			"failed to analyze: Unrecognized name: nope [at 1:25]"},
		{"SELECT 1;\n  SELECT\n nope", 12, "SELECT\n nope", "Unrecognized name: nope at [2:2]",
			"Unrecognized name: nope at [3:2]"},
		{"SELECT 'é';SELECT nope", 12, "SELECT nope", "Unrecognized name: nope at [1:8]",
			"Unrecognized name: nope at [1:19]"},
		// The text run is not the statement's from the position on (a
		// variable's value in place of its name): left as it is.
		{"DECLARE x INT64; SELECT x, nope FROM ds.t", 17, "SELECT NULL, nope FROM ds.t", "Unrecognized name: nope at [1:14]",
			"Unrecognized name: nope at [1:14]"},
		// Before the change, it is moved.
		{"DECLARE x INT64; SELECT nope, x FROM ds.t", 17, "SELECT nope, NULL FROM ds.t", "Unrecognized name: nope at [1:8]",
			"Unrecognized name: nope at [1:25]"},
		{"SELECT 1; SELECT", 10, "SELECT", "Syntax error: Unexpected end of statement at [1:7]",
			"Syntax error: Unexpected end of statement at [1:17]"},
		{"SELECT 1; SELECT 1/0", 10, "SELECT 1/0", "division by zero: 1 / 0", "division by zero: 1 / 0"},
	} {
		if got := positionInScript(tc.msg, tc.script, tc.pos, tc.text); got != tc.want {
			t.Errorf("positionInScript(%q, %q) = %q, want %q", tc.msg, tc.script, got, tc.want)
		}
	}
	b := errorsInScript([]byte(`{"error":{"code":400,"message":"Unrecognized name: nope at [1:8]","errors":[{"reason":"invalidQuery","message":"Unrecognized name: nope at [1:8]"}]}}`),
		"SELECT 1; SELECT nope", 10, "SELECT nope")
	want := `{"error":{"code":400,"errors":[{"message":"Unrecognized name: nope at [1:18]","reason":"invalidQuery"}],"message":"Unrecognized name: nope at [1:18]"}}`
	if string(b) != want {
		t.Errorf("errorsInScript = %s, want %s", b, want)
	}
}

// TestErrorsInScriptJob (#1585): a statement run as a query job fails with
// a job whose errorResult and errors are moved.
func TestErrorsInScriptJob(t *testing.T) {
	b := errorsInScript([]byte(`{"jobReference":{"jobId":"j"},"status":{"state":"DONE","errorResult":{"reason":"invalidQuery","message":"Unrecognized name: nope at [1:8]"},"errors":[{"reason":"invalidQuery","message":"Unrecognized name: nope at [1:8]"}]}}`),
		"SELECT 1; SELECT nope", 10, "SELECT nope")
	want := `{"jobReference":{"jobId":"j"},"status":{"errorResult":{"message":"Unrecognized name: nope at [1:18]","reason":"invalidQuery"},"errors":[{"message":"Unrecognized name: nope at [1:18]","reason":"invalidQuery"}],"state":"DONE"}}`
	if string(b) != want {
		t.Errorf("errorsInScript = %s, want %s", b, want)
	}
}
