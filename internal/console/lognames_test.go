package console

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeLogNames map[string][]string

func (f fakeLogNames) ListLogs(_ context.Context, parent string) ([]string, error) {
	names, ok := f[parent]
	if !ok {
		return nil, errors.New("unknown parent " + parent)
	}
	return names, nil
}

// TestLogNamesRoute (#1554): /api/logs/names, behind the Logs Explorer's Log
// name picker, answers the lister's names for the parent asked for, an empty
// list rather than null, 400 for no parent or the lister's refusal, and 501
// on an instance with no Cloud Logging.
func TestLogNamesRoute(t *testing.T) {
	t.Parallel()
	s := New("127.0.0.1:0", nil)
	s.SetLogNameLister(fakeLogNames{
		"projects/p": {"projects/p/logs/app", "projects/p/logs/audit"},
		"folders/7":  nil,
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	code, body := get(t, srv, "/api/logs/names?parent=projects/p/", nil)
	if code != http.StatusOK || !strings.Contains(body, `"projects/p/logs/app"`) || !strings.Contains(body, `"parent":"projects/p"`) {
		t.Errorf("the project's log names = %d: %s", code, body)
	}
	if code, body := get(t, srv, "/api/logs/names?parent=folders/7", nil); code != http.StatusOK || !strings.Contains(body, `"logNames":[]`) {
		t.Errorf("a parent with no logs = %d: %s", code, body)
	}
	if code, body := get(t, srv, "/api/logs/names?parent=nonsense", nil); code != http.StatusBadRequest || !strings.Contains(body, "nonsense") {
		t.Errorf("a refused parent = %d: %s", code, body)
	}
	if code, _ := get(t, srv, "/api/logs/names", nil); code != http.StatusBadRequest {
		t.Errorf("no parent = %d, want 400", code)
	}

	bare := httptest.NewServer(New("127.0.0.1:0", nil).Handler())
	defer bare.Close()
	if code, _ := get(t, bare, "/api/logs/names?parent=projects/p", nil); code != http.StatusNotImplemented {
		t.Errorf("no Cloud Logging = %d, want 501", code)
	}
}
