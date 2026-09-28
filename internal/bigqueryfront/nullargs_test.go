package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGuardNullArguments (#1109): a call of a function the engine fails
// on a NULL argument is sent as IF(each argument IS NULL, NULL, the call),
// with SAFE. and NET., nested, in any case; text in a string or a comment,
// a function of a dataset, a field and a call with a positional parameter
// are left as they are. A parameter alone is not tested for NULL: its
// value is known.
func TestGuardNullArguments(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"SELECT ARRAY_TO_STRING(a, ',')", "SELECT IF((a) IS NULL OR (',') IS NULL, NULL, ARRAY_TO_STRING(a, ','))"},
		{"  SELECT safe.array_to_string ( x , 'd', 'n' ) -- c\n", "  SELECT IF((x) IS NULL OR ('d') IS NULL OR ('n') IS NULL, NULL, " +
			"safe.array_to_string (x, 'd', 'n')) -- c\n"},
		{"SELECT ARRAY_TO_STRING(ARRAY_REVERSE(f(a, b)), @e)", "SELECT IF((IF((f(a, b)) IS NULL, NULL, ARRAY_REVERSE(f(a, b)))) IS NULL OR " +
			"(@e) IS NULL, NULL, ARRAY_TO_STRING(IF((f(a, b)) IS NULL, NULL, ARRAY_REVERSE(f(a, b))), @e))"},
		{"SELECT ARRAY_CONCAT([1, 2], b, c)", "SELECT IF(([1, 2]) IS NULL OR (b) IS NULL OR (c) IS NULL, NULL, ARRAY_CONCAT([1, 2], b, c))"},
		{"SELECT NET.IPV4_TO_INT64(b), SAFE.NET.IP_TO_STRING(b)", "SELECT IF((b) IS NULL, NULL, NET.IPV4_TO_INT64(b)), " +
			"IF((b) IS NULL, NULL, SAFE.NET.IP_TO_STRING(b))"},
		{"SELECT 'ARRAY_TO_STRING(a, b)' /* ARRAY_REVERSE(a) */", ""},
		{"SELECT ds.ARRAY_REVERSE(a), t.net.ip_to_string(b), x.SAFE.ARRAY_REVERSE(a)", ""},
		{"SELECT ARRAY_TO_STRING(?, ',')", ""},
		{"SELECT ARRAY_TO_STRING(a, ',', )", ""},
		{"SELECT REVERSE(a), ARRAY_LENGTH(a)", ""},
		// A parameter alone: its value is known.
		{"SELECT ARRAY_TO_STRING(@a, @D, @n)", "SELECT IF(TRUE, NULL, ARRAY_TO_STRING(@a, @D, @n))"},
		{"SELECT ARRAY_TO_STRING(@a, @x)", "SELECT IF(FALSE, NULL, ARRAY_TO_STRING(@a, @x))"},
		{"SELECT ARRAY_TO_STRING(@a, @other)", "SELECT IF((@other) IS NULL, NULL, ARRAY_TO_STRING(@a, @other))"},
	} {
		params := `[{"name":"a","parameterType":{"type":"ARRAY","arrayType":{"type":"STRING"}},"parameterValue":{"arrayValues":[{"value":"x"}]}},` +
			`{"name":"d","parameterType":{"type":"STRING"},"parameterValue":{"value":null}},` +
			`{"name":"n","parameterType":{"type":"STRING"},"parameterValue":{}},` +
			`{"name":"x","parameterType":{"type":"STRING"},"parameterValue":{"value":""}}]`
		got, changed := guardNullArguments(c.in, json.RawMessage(params))
		want := c.want
		if want == "" {
			want = c.in
		}
		if got != want || changed != (c.want != "") {
			t.Errorf("%q:\n got %q %v\nwant %q", c.in, got, changed, want)
		}
	}
}

// TestNoRetry (#1109): a 500 the emulator gave a query is answered 400
// invalidQuery with its text; any other answer, and a 500 to anything
// else, is passed on.
func TestNoRetry(t *testing.T) {
	rec := newRecorder()
	writeError(rec, http.StatusInternalServerError, "internalError", "runtime error: invalid memory address")
	w := httptest.NewRecorder()
	noRetry(w, rec, &queryFailures{failed: true})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"invalidQuery"`) ||
		!strings.Contains(w.Body.String(), "runtime error: invalid memory address") {
		t.Errorf("a 500: %d %s", w.Code, w.Body)
	}
	rec = newRecorder()
	writeError(rec, http.StatusBadRequest, "jobInternalError", "no such table")
	w = httptest.NewRecorder()
	noRetry(w, rec, &queryFailures{failed: true})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "jobInternalError") {
		t.Errorf("a 400: %d %s", w.Code, w.Body)
	}
	rec = newRecorder()
	writeError(rec, http.StatusInternalServerError, "internalError", "no dataset")
	w = httptest.NewRecorder()
	noRetry(w, rec, &queryFailures{})
	if w.Code != http.StatusInternalServerError {
		t.Errorf("a 500 to something else: %d %s", w.Code, w.Body)
	}

	// Through the front: the emulator's 500 to jobs.query and to a query
	// job's jobs.insert.
	emu := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusInternalServerError, "internalError", "runtime error: invalid memory address or nil pointer dereference")
	})
	for _, c := range []struct{ path, body string }{
		{base + "/queries", `{"query":"SELECT JSON_OBJECT(CAST(NULL AS STRING), 1)","useLegacySql":false}`},
		{base + "/jobs", `{"jobReference":{"projectId":"p","jobId":"j"},"configuration":{"query":{"query":"SELECT 1","useLegacySql":false}}}`},
	} {
		if code, body := do(t, Wrap(emu), "POST", c.path, c.body); code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(body), "invalidQuery") {
			t.Errorf("%s: %d %s", c.path, code, body)
		}
	}
}
