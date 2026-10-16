package bigqueryfront

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// linkedEmulator answers datasets.get of "logs" as a linked dataset and of
// "mine" as a plain one, and records every other request.
type linkedEmulator struct {
	sent []string
	made bool // "logs" was made
}

func (e *linkedEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && !strings.Contains(r.URL.Path, "/tables") {
		switch r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:] {
		case "logs":
			if !e.made {
				http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, `{"labels":{"`+linkedLabel+`":"true"}}`)
		case "mine":
			_, _ = io.WriteString(w, `{}`)
		default:
			http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
		}
		return
	}
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tables") {
		_, _ = io.WriteString(w, `{"totalItems":2,"tables":[{"tableReference":{"tableId":"_AllLogs"},"type":"VIEW"},`+
			`{"tableReference":{"tableId":"`+linkedEntriesTable+`"},"type":"VIEW"}]}`)
		return
	}
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/datasets") {
		e.made = true
	}
	e.sent = append(e.sent, r.Method+" "+r.URL.Path)
	_, _ = io.WriteString(w, `{}`)
}

// TestLinkedDatasetIsReadOnly (#1423): every write into a Log Analytics
// linked dataset is refused 403 before the emulator sees it, unless it is
// the Logging server's (linkedWriterHeader); a plain dataset is written as
// before; tables.list leaves out the entries table.
func TestLinkedDatasetIsReadOnly(t *testing.T) {
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/datasets/logs/tables", `{"tableReference":{"tableId":"x"}}`},
		{"DELETE", "/datasets/logs/tables/_AllLogs", ""},
		{"PATCH", "/datasets/logs/tables/_AllLogs", `{}`},
		{"PATCH", "/datasets/logs", `{"description":"d"}`},
		{"DELETE", "/datasets/logs", ""},
		{"POST", "/datasets/logs/tables/" + linkedEntriesTable + "/insertAll", `{"rows":[{"json":{"a":1}}]}`},
		{"POST", "/queries", `{"query":"DELETE FROM logs._cloudburrow_log_entries WHERE true","useLegacySql":false}`},
		{"POST", "/queries", `{"query":"CREATE TABLE ` + "`p.logs.t`" + ` (a STRING)","useLegacySql":false}`},
		{"POST", "/queries", `{"query":"DROP VIEW logs._AllLogs","useLegacySql":false}`},
	} {
		emu := &linkedEmulator{}
		h := linkedFront(t, emu)
		code, got := do(t, h, c.method, base+c.path, c.body)
		if code != http.StatusForbidden || !strings.Contains(strings.ToLower(jsonString(got)), "read-only") || len(emu.sent) != 0 {
			t.Errorf("%s %s %s = %d %v, sent %v; want 403 read-only, nothing sent", c.method, c.path, c.body, code, got, emu.sent)
		}
	}
	// The Logging server's own write is let through.
	emu := &linkedEmulator{}
	h := linkedFront(t, emu)
	r := httptest.NewRequest("POST", base+"/datasets/logs/tables/"+linkedEntriesTable+"/insertAll", strings.NewReader(`{"rows":[]}`))
	r.Header.Set(linkedWriterHeader, "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("the Logging server's insertAll = %d %s; want it answered", w.Code, w.Body)
	}
	// A plain dataset is written.
	emu = &linkedEmulator{}
	if code, got := do(t, linkedFront(t, emu), "DELETE", base+"/datasets/mine/tables/t", ""); code == http.StatusForbidden || len(emu.sent) != 1 {
		t.Errorf("tables.delete in a plain dataset = %d %v, sent %v", code, got, emu.sent)
	}
	// tables.list shows _AllLogs alone.
	code, got := do(t, linkedFront(t, &linkedEmulator{}), "GET", base+"/datasets/logs/tables", "")
	if tables, _ := got["tables"].([]any); code != 200 || len(tables) != 1 || got["totalItems"] != float64(1) {
		t.Errorf("tables.list = %d %v; want _AllLogs alone", code, got)
	}
	// Once the Logging server deletes it, the dataset is no longer linked.
	emu = &linkedEmulator{}
	h = linkedFront(t, emu)
	r = httptest.NewRequest("DELETE", base+"/datasets/logs", nil)
	r.Header.Set(linkedWriterHeader, "1")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if code, _ := do(t, h, "DELETE", base+"/datasets/logs/tables/t", ""); code == http.StatusForbidden {
		t.Error("a write after the Logging server deleted the dataset was refused")
	}
}

// linkedFront is the front over emu after the Logging server made the
// linked dataset "logs" through it.
func linkedFront(t *testing.T, emu *linkedEmulator) http.Handler {
	t.Helper()
	h := Wrap(emu)
	r := httptest.NewRequest("POST", base+"/datasets", strings.NewReader(`{"datasetReference":{"datasetId":"logs"},"labels":{"`+linkedLabel+`":"true"}}`))
	r.Header.Set(linkedWriterHeader, "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("the Logging server's datasets.insert = %d %s", w.Code, w.Body)
	}
	emu.sent = nil
	return h
}

func jsonString(m map[string]any) string {
	var b strings.Builder
	for k, v := range m {
		b.WriteString(k)
		if s, ok := v.(map[string]any); ok {
			b.WriteString(jsonString(s))
		} else if s, ok := v.(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}
