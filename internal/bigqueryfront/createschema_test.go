package bigqueryfront

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// The tests in this file are #951.

// datasetEmulator keeps datasets as datasets.insert and datasets.delete
// make and remove them, answers every query with answer (status code), and
// records each request.
type datasetEmulator struct {
	mu       sync.Mutex
	datasets map[string]string // id -> datasets.insert body
	code     int
	answer   string
	requests []string // "METHOD path body"
}

func (e *datasetEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var b []byte
	if r.Body != nil {
		b, _ = io.ReadAll(r.Body)
	}
	e.requests = append(e.requests, r.Method+" "+r.URL.Path+" "+string(b))
	name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	switch {
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/datasets/"):
		if _, ok := e.datasets[name]; !ok {
			http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, e.datasets[name])
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/datasets"):
		var d newDataset
		_ = json.Unmarshal(b, &d)
		e.datasets[d.DatasetReference.DatasetID] = string(b)
		_, _ = w.Write(b)
	case r.Method == http.MethodDelete:
		delete(e.datasets, name)
		w.WriteHeader(http.StatusNoContent)
	default:
		if e.code != 0 {
			w.WriteHeader(e.code)
		}
		answer := e.answer
		if answer == "" {
			answer = `{"jobComplete":true}`
		}
		_, _ = io.WriteString(w, answer)
	}
}

func (e *datasetEmulator) writes() []string {
	var out []string
	for _, r := range e.requests {
		if !strings.HasPrefix(r, "GET ") {
			out = append(out, r)
		}
	}
	return out
}

func schemaQuery(sql string, extra ...string) string {
	b, _ := json.Marshal(map[string]any{"query": sql, "useLegacySql": false})
	s := string(b)
	for _, x := range extra {
		s = s[:len(s)-1] + "," + x + "}"
	}
	return s
}

// TestCreateSchemaMakesTheDataset (#951): CREATE SCHEMA of a dataset that
// does not exist makes it through datasets.insert, with its description,
// friendly_name, labels and location, before the query is sent on
// unchanged; in a script too.
func TestCreateSchemaMakesTheDataset(t *testing.T) {
	emu := &datasetEmulator{datasets: map[string]string{}}
	sql := "CREATE SCHEMA IF NOT EXISTS p.fresh OPTIONS(description='it\\'s \\\"d\\\"', friendly_name=\"F\", " +
		"labels=[('k', 'v'), (\"k2\", r'v\\2')], location='EU')"
	code, got := do(t, Wrap(emu), "POST", base+"/queries", schemaQuery(sql))
	if code != 200 {
		t.Fatalf("%d %v", code, got)
	}
	w := emu.writes()
	if len(w) != 2 || !strings.HasPrefix(w[0], "POST "+base+"/datasets ") || !strings.HasPrefix(w[1], "POST "+base+"/queries ") {
		t.Fatalf("sent %q, want datasets.insert then the query", w)
	}
	var d map[string]any
	_ = json.Unmarshal([]byte(emu.datasets["fresh"]), &d)
	want := `{"datasetReference":{"datasetId":"fresh","projectId":"p"},"description":"it's \"d\"","friendlyName":"F",` +
		`"labels":{"k":"v","k2":"v\\2"},"location":"EU"}`
	if b, _ := json.Marshal(d); string(b) != want {
		t.Errorf("datasets.insert %s, want %s", b, want)
	}
	if !strings.Contains(w[1], "CREATE SCHEMA IF NOT EXISTS p.fresh") {
		t.Errorf("the query was sent as %s", w[1])
	}

	// The query's location, when the statement gives none.
	for _, c := range []struct{ path, body string }{
		{base + "/queries", schemaQuery("CREATE SCHEMA q1", `"location":"asia-northeast1"`)},
		{base + "/jobs", `{"jobReference":{"projectId":"p","jobId":"j","location":"asia-northeast1"},"configuration":{"query":{"query":"CREATE SCHEMA q2","useLegacySql":false}}}`},
	} {
		emu := &datasetEmulator{datasets: map[string]string{}}
		if code, got := do(t, Wrap(emu), "POST", c.path, c.body); code != 200 {
			t.Fatalf("%s: %d %v", c.path, code, got)
		}
		for id, body := range emu.datasets {
			if !strings.Contains(body, `"location":"asia-northeast1"`) {
				t.Errorf("%s: dataset %s made as %s, want the query's location", c.path, id, body)
			}
		}
		if len(emu.datasets) != 1 {
			t.Errorf("%s: made %v", c.path, emu.datasets)
		}
	}

	// A script: the dataset is made before it is sent, and a DROP SCHEMA
	// IF EXISTS of it first, which does nothing, is sent as a statement
	// that does nothing.
	for _, sql := range []string{
		"CREATE SCHEMA fresh; CREATE TABLE fresh.t (a INT64); SELECT 1",
		"DECLARE x INT64 DEFAULT 1; BEGIN CREATE SCHEMA fresh; END; CREATE TABLE fresh.t (a INT64)",
		"DROP SCHEMA IF EXISTS fresh CASCADE; CREATE SCHEMA fresh; CREATE TABLE fresh.t (a INT64)",
	} {
		emu := &datasetEmulator{datasets: map[string]string{}}
		if code, got := do(t, Wrap(emu), "POST", base+"/queries", schemaQuery(sql)); code != 200 {
			t.Errorf("%s: %d %v", sql, code, got)
			continue
		}
		w := emu.writes()
		if _, ok := emu.datasets["fresh"]; !ok || len(w) != 2 || !strings.Contains(w[1], "CREATE TABLE fresh.t") {
			t.Errorf("%s: sent %q", sql, w)
		}
		if strings.Contains(w[len(w)-1], "DROP SCHEMA") || strings.HasPrefix(sql, "DROP") && !strings.Contains(w[1], "DROP TABLE IF EXISTS `fresh._cloudburrow_") {
			t.Errorf("%s: sent %q, want no DROP SCHEMA", sql, w)
		}
	}
}

// TestCreateSchemaNotCarriedOut (#951): what the front cannot carry out
// is 501 naming it, and nothing is sent.
func TestCreateSchemaNotCarriedOut(t *testing.T) {
	for _, c := range []struct{ sql, names string }{
		{"CREATE SCHEMA fresh OPTIONS(default_table_expiration_days=1)", "default_table_expiration_days"},
		{"CREATE SCHEMA fresh OPTIONS(is_case_insensitive=true)", "is_case_insensitive"},
		{"CREATE SCHEMA fresh DEFAULT COLLATE 'und:ci'", "DEFAULT COLLATE"},
		{"CREATE SCHEMA fresh OPTIONS(description=CONCAT('a', 'b'))", "description"},
		{"CREATE SCHEMA fresh OPTIONS(labels=[('k', 1)])", "labels"},
		{"CREATE SCHEMA fresh OPTIONS(description=b'x')", "description"},
		{"SELECT * FROM fresh.t; CREATE SCHEMA fresh", "name it"},
		{"SELECT 1; RETURN; CREATE SCHEMA fresh", "RETURN"},
		{"BEGIN SELECT 1; EXCEPTION WHEN ERROR THEN CREATE SCHEMA fresh; END", "EXCEPTION handler"},
		{"DROP SCHEMA taken CASCADE; CREATE SCHEMA taken", "DROP SCHEMA"},
		{"CREATE SCHEMA fresh; DROP SCHEMA fresh; CREATE SCHEMA fresh", "DROP SCHEMA"},
	} {
		emu := &datasetEmulator{datasets: map[string]string{"taken": "{}"}}
		code, got := do(t, Wrap(emu), "POST", base+"/queries", schemaQuery(c.sql))
		msg, _ := got["error"].(map[string]any)["message"].(string)
		if code != 501 || !strings.Contains(msg, c.names) {
			t.Errorf("%s: %d %v, want 501 naming %s", c.sql, code, got, c.names)
		}
		if w := emu.writes(); len(w) != 0 {
			t.Errorf("%s: sent %q", c.sql, w)
		}
	}
}

// TestCreateSchemaUndoneWhenTheQueryFails (#951): when the query the
// front made datasets for fails, they are deleted again.
func TestCreateSchemaUndoneWhenTheQueryFails(t *testing.T) {
	for _, c := range []struct {
		name, path, body, answer string
		code                     int
	}{
		{"jobs.query", base + "/queries", schemaQuery("CREATE SCHEMA fresh; SELECT 1/0"), `{"error":{"code":400,"message":"division by zero"}}`, 400},
		{"jobs.insert", base + "/jobs", `{"jobReference":{"projectId":"p","jobId":"j"},"configuration":{"query":{"query":"CREATE SCHEMA fresh; SELECT 1/0","useLegacySql":false}}}`,
			`{"jobReference":{"projectId":"p","jobId":"j"},"status":{"state":"DONE","errorResult":{"reason":"invalidQuery","message":"division by zero"}}}`, 0},
	} {
		emu := &datasetEmulator{datasets: map[string]string{}, code: c.code, answer: c.answer}
		do(t, Wrap(emu), "POST", c.path, c.body)
		w := emu.writes()
		if _, ok := emu.datasets["fresh"]; ok || len(w) != 3 || !strings.HasPrefix(w[2], "DELETE "+base+"/datasets/fresh") {
			t.Errorf("%s: sent %q; want the dataset made, the query, and the dataset deleted", c.name, w)
		}
	}

	// A datasets.insert that fails fails the query, which is not sent.
	emu := &datasetEmulator{datasets: map[string]string{}}
	failing := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/datasets") {
			http.Error(w, `{"error":{"code":500,"message":"no"}}`, http.StatusInternalServerError)
			return
		}
		emu.ServeHTTP(w, r)
	})
	if code, _ := do(t, Wrap(failing), "POST", base+"/queries", schemaQuery("CREATE SCHEMA fresh")); code != 500 || len(emu.writes()) != 0 {
		t.Errorf("a failed datasets.insert: %d, sent %q", code, emu.writes())
	}
}
