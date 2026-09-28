package bigqueryfront

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
