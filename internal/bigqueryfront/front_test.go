package bigqueryfront

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeEmulator answers the reads the front makes and records every write it
// is sent.
type fakeEmulator struct {
	datasets map[string]bool
	schema   string
	writes   []string
	answer   string
}

func (e *fakeEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		switch {
		case strings.Contains(r.URL.Path, "/tables/"):
			if e.schema == "" {
				http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, `{"schema":`+e.schema+`}`)
		default:
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			if !e.datasets[name] {
				http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, `{}`)
		}
		return
	}
	b, _ := io.ReadAll(r.Body)
	e.writes = append(e.writes, string(b))
	if e.answer != "" {
		_, _ = io.WriteString(w, e.answer)
		return
	}
	_, _ = io.WriteString(w, `{}`)
}

func do(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	return w.Code, got
}

const base = "/bigquery/v2/projects/p"

func TestDatasetIDs(t *testing.T) {
	emu := &fakeEmulator{datasets: map[string]bool{"taken": true}}
	h := Wrap(emu)
	for _, c := range []struct {
		id   string
		want int
	}{
		{"ok_1", 200}, {"d-1", 400}, {"a b", 400}, {"", 400}, {strings.Repeat("a", 1024), 200},
		{strings.Repeat("a", 1025), 400}, {"taken", 409},
	} {
		code, got := do(t, h, "POST", base+"/datasets", `{"datasetReference":{"datasetId":"`+c.id+`"}}`)
		if code != c.want {
			t.Errorf("dataset %.20q: %d %v, want %d", c.id, code, got, c.want)
		}
	}
	if len(emu.writes) != 2 {
		t.Errorf("emulator was sent %d creates, want the 2 valid ones", len(emu.writes))
	}
}

func TestTableIDsAndSchemas(t *testing.T) {
	emu := &fakeEmulator{}
	h := Wrap(emu)
	for _, c := range []struct {
		name, body string
		want       int
	}{
		{"unicode table", `{"tableReference":{"tableId":"étudiant-01 ग्राहक"}}`, 200},
		{"bang in table", `{"tableReference":{"tableId":"t!"}}`, 400},
		{"long table", `{"tableReference":{"tableId":"` + strings.Repeat("é", 513) + `"}}`, 400},
		{"space in column", `{"tableReference":{"tableId":"t"},"schema":{"fields":[{"name":"first name","type":"STRING"}]}}`, 200},
		{"bang in column", `{"tableReference":{"tableId":"t"},"schema":{"fields":[{"name":"a!","type":"STRING"}]}}`, 400},
		{"dot in column", `{"tableReference":{"tableId":"t"},"schema":{"fields":[{"name":"a.b","type":"STRING"}]}}`, 400},
		{"reserved prefix", `{"tableReference":{"tableId":"t"},"schema":{"fields":[{"name":"_partitiontime","type":"STRING"}]}}`, 400},
		{"301 chars", `{"tableReference":{"tableId":"t"},"schema":{"fields":[{"name":"` + strings.Repeat("a", 301) + `","type":"STRING"}]}}`, 400},
		{"duplicate column", `{"tableReference":{"tableId":"t"},"schema":{"fields":[{"name":"a","type":"STRING"},{"name":"A","type":"INTEGER"}]}}`, 400},
		{"nested duplicate", `{"tableReference":{"tableId":"t"},"schema":{"fields":[{"name":"r","type":"RECORD","fields":[{"name":"x","type":"STRING"},{"name":"X","type":"STRING"}]}]}}`, 400},
		{"bad type", `{"tableReference":{"tableId":"t"},"schema":{"fields":[{"name":"a","type":"WORD"}]}}`, 400},
		{"bad mode", `{"tableReference":{"tableId":"t"},"schema":{"fields":[{"name":"a","type":"STRING","mode":"SOMETIMES"}]}}`, 400},
		{"empty record", `{"tableReference":{"tableId":"t"},"schema":{"fields":[{"name":"a","type":"RECORD"}]}}`, 400},
		// What the emulator can and cannot read back (#874, measured).
		{"record in record in record", `{"tableReference":{"tableId":"t"},"schema":{"fields":[{"name":"a","type":"RECORD","fields":[` +
			`{"name":"b","type":"RECORD","fields":[{"name":"c","type":"RECORD","fields":[{"name":"d","type":"STRING"}]}]}]}]}}`, 200},
		{"repeated record of scalars", `{"tableReference":{"tableId":"t"},"schema":{"fields":[{"name":"a","type":"RECORD","mode":"REPEATED","fields":[` +
			`{"name":"s","type":"STRING"},{"name":"tags","type":"STRING","mode":"REPEATED"}]}]}}`, 200},
		{"record in repeated record", `{"tableReference":{"tableId":"t"},"schema":{"fields":[{"name":"a","type":"RECORD","mode":"REPEATED","fields":[` +
			`{"name":"b","type":"RECORD","fields":[{"name":"c","type":"STRING"}]}]}]}}`, 501},
		{"repeated record in record", `{"tableReference":{"tableId":"t"},"schema":{"fields":[{"name":"a","type":"STRUCT","fields":[` +
			`{"name":"b","type":"RECORD","mode":"REPEATED","fields":[{"name":"c","type":"STRING"}]}]}]}}`, 501},
		{"deep under repeated", `{"tableReference":{"tableId":"t"},"schema":{"fields":[{"name":"a","type":"RECORD","mode":"REPEATED","fields":[` +
			`{"name":"s","type":"STRING"},{"name":"b","type":"RECORD","fields":[{"name":"c","type":"RECORD","fields":[{"name":"d","type":"STRING"}]}]}]}]}}`, 501},
	} {
		code, got := do(t, h, "POST", base+"/datasets/d/tables", c.body)
		if code != c.want {
			t.Errorf("%s: %d %v, want %d", c.name, code, got, c.want)
		}
	}
	code, _ := do(t, h, "PATCH", base+"/datasets/d/tables/t", `{"schema":{"fields":[{"name":"a","type":"STRING"},{"name":"a","type":"STRING"}]}}`)
	if code != 400 {
		t.Errorf("patch with a duplicate column: %d, want 400", code)
	}
	// The 501 is UNIMPLEMENTED and names the field, and nothing reaches the
	// emulator; a PATCH that adds such a field is refused the same way.
	before := len(emu.writes)
	code, got := do(t, h, "PATCH", base+"/datasets/d/tables/t", `{"schema":{"fields":[{"name":"a","type":"RECORD","mode":"REPEATED",`+
		`"fields":[{"name":"b","type":"RECORD","fields":[{"name":"c","type":"STRING"}]}]}]}}`)
	e, _ := got["error"].(map[string]any)
	if msg, _ := e["message"].(string); code != 501 || e["status"] != "UNIMPLEMENTED" || !strings.Contains(msg, "field a.b is a RECORD") {
		t.Errorf("patch adding a RECORD in a REPEATED RECORD: %d %v", code, got)
	}
	if len(emu.writes) != before {
		t.Errorf("the refused patch reached the emulator: %v", emu.writes[before:])
	}
}

const schema = `{"fields":[{"name":"n","type":"INTEGER","mode":"REQUIRED"},{"name":"r","type":"INTEGER","mode":"REPEATED"},
	{"name":"x","type":"NUMERIC"},{"name":"b","type":"BOOLEAN"},{"name":"ts","type":"TIMESTAMP"},{"name":"d","type":"DATE"},
	{"name":"rec","type":"RECORD","fields":[{"name":"s","type":"STRING"}]},{"name":"j","type":"JSON"},{"name":"by","type":"BYTES"}]}`

func errorsOf(t *testing.T, got map[string]any) map[int]string {
	t.Helper()
	out := map[int]string{}
	list, _ := got["insertErrors"].([]any)
	for _, e := range list {
		m := e.(map[string]any)
		errs := m["errors"].([]any)
		out[int(m["index"].(float64))] = errs[0].(map[string]any)["reason"].(string)
	}
	return out
}

func TestInsertAllChecksEachValue(t *testing.T) {
	for _, c := range []struct {
		name, row string
		ok        bool
	}{
		{"valid", `{"n":1,"r":[1,"2"],"x":"1.5","b":true,"ts":"2026-09-28 10:00:00.123456+00:00","d":"2026-02-28","rec":{"s":"a"}}`, true},
		{"string integer", `{"n":"42"}`, true},
		{"epoch timestamp", `{"n":1,"ts":1790000000.5}`, true},
		{"zone name", `{"n":1,"ts":"2026-09-28T10:00:00 America/Los_Angeles"}`, true},
		{"missing required", `{"r":[1]}`, false},
		{"null required", `{"n":null}`, false},
		{"unknown field", `{"n":1,"zz":1}`, false},
		{"bad integer", `{"n":"abc"}`, false},
		{"fractional integer", `{"n":1.5}`, false},
		{"bad repeated element", `{"n":1,"r":["q"]}`, false},
		{"scalar for repeated", `{"n":1,"r":1}`, false},
		{"null in array", `{"n":1,"r":[null]}`, false},
		{"bad numeric", `{"n":1,"x":"x"}`, false},
		{"numeric out of range", `{"n":1,"x":"1e30"}`, false},
		{"bad boolean", `{"n":1,"b":"yes"}`, false},
		{"bad date", `{"n":1,"d":"2026-02-30"}`, false},
		{"bad timestamp", `{"n":1,"ts":"yesterday"}`, false},
		{"scalar for record", `{"n":1,"rec":"a"}`, false},
		{"unknown nested", `{"n":1,"rec":{"q":1}}`, false},
		{"json text", `{"n":1,"j":"{\"a\":[1,2]}"}`, true},
		{"bad json text", `{"n":1,"j":"{a"}`, false},
		{"base64", `{"n":1,"by":"aGk="}`, true},
		{"bad base64", `{"n":1,"by":"!!"}`, false},
	} {
		emu := &fakeEmulator{schema: schema}
		code, got := do(t, Wrap(emu), "POST", base+"/datasets/d/tables/t/insertAll", `{"rows":[{"json":`+c.row+`}]}`)
		if code != 200 {
			t.Errorf("%s: status %d, want 200", c.name, code)
			continue
		}
		errs := errorsOf(t, got)
		if c.ok && (len(errs) > 0 || len(emu.writes) != 1) {
			t.Errorf("%s: refused %v", c.name, got)
		}
		if !c.ok && (errs[0] != "invalid" || len(emu.writes) != 0) {
			t.Errorf("%s: accepted (%v), emulator sent %d writes", c.name, got, len(emu.writes))
		}
	}
}

func TestInsertAllWithoutSkipInsertsNothing(t *testing.T) {
	emu := &fakeEmulator{schema: schema}
	_, got := do(t, Wrap(emu), "POST", base+"/datasets/d/tables/t/insertAll",
		`{"rows":[{"json":{"n":1}},{"json":{"n":"bad"}},{"json":{"n":3}}]}`)
	want := map[int]string{0: "stopped", 1: "invalid", 2: "stopped"}
	if e := errorsOf(t, got); len(e) != 3 || e[0] != want[0] || e[1] != want[1] || e[2] != want[2] {
		t.Errorf("insertErrors %v, want %v", e, want)
	}
	if len(emu.writes) != 0 {
		t.Errorf("emulator was sent %v", emu.writes)
	}
}

func TestInsertAllSkipInvalidRowsKeepsIndexes(t *testing.T) {
	// The emulator reports its own error for the second row it was sent,
	// which is the request's third.
	emu := &fakeEmulator{schema: schema, answer: `{"insertErrors":[{"index":1,"errors":[{"reason":"invalid","message":"theirs"}]}]}`}
	_, got := do(t, Wrap(emu), "POST", base+"/datasets/d/tables/t/insertAll",
		`{"skipInvalidRows":true,"rows":[{"insertId":"a","json":{"n":1}},{"json":{"n":"bad"}},{"insertId":"c","json":{"n":3}}]}`)
	if e := errorsOf(t, got); len(e) != 2 || e[1] != "invalid" || e[2] != "invalid" {
		t.Errorf("insertErrors %v, want rows 1 and 2 invalid", e)
	}
	if len(emu.writes) != 1 || strings.Contains(emu.writes[0], "bad") || !strings.Contains(emu.writes[0], `"insertId":"c"`) {
		t.Errorf("emulator was sent %v, want rows 0 and 2 with their insert IDs", emu.writes)
	}
}

func TestInsertAllIgnoreUnknownValuesDropsFields(t *testing.T) {
	emu := &fakeEmulator{schema: schema}
	_, got := do(t, Wrap(emu), "POST", base+"/datasets/d/tables/t/insertAll",
		`{"ignoreUnknownValues":true,"rows":[{"json":{"n":1,"zz":2,"rec":{"s":"a","q":1}}}]}`)
	if len(errorsOf(t, got)) != 0 {
		t.Errorf("refused: %v", got)
	}
	if len(emu.writes) != 1 || strings.Contains(emu.writes[0], "zz") || strings.Contains(emu.writes[0], `"q"`) {
		t.Errorf("emulator was sent %v, want the unknown fields dropped", emu.writes)
	}
}

func TestOtherRequestsPassThrough(t *testing.T) {
	emu := &fakeEmulator{}
	h := Wrap(emu)
	for _, c := range []struct{ method, path, body string }{
		{"POST", base + "/queries", `{"query":"SELECT 1"}`},
		{"POST", base + "/jobs", `{}`},
		{"POST", base + "/datasets", `not json`},
		{"DELETE", base + "/datasets/d", ``},
	} {
		emu.writes = nil
		do(t, h, c.method, c.path, c.body)
		if len(emu.writes) != 1 || emu.writes[0] != c.body {
			t.Errorf("%s %s: emulator was sent %q", c.method, c.path, emu.writes)
		}
	}
}
