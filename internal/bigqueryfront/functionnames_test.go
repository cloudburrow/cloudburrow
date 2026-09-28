package bigqueryfront

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// fnEmulator answers every query as done, and SELECT SAFE.name() as the
// engine does (measured, #1033): a built-in's name "No matching
// signature for function NAME", a function CREATE FUNCTION made "No
// matching signature for function :P_DS_NAME", and any other name
// "Function not found". It records the queries it is sent.
type fnEmulator struct {
	mu      sync.Mutex
	queries []string
}

func (e *fnEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var q queryOptions
	_ = json.Unmarshal(b, &q)
	e.mu.Lock()
	e.queries = append(e.queries, q.Query)
	e.mu.Unlock()
	if !strings.HasSuffix(r.URL.Path, "/queries") {
		_, _ = w.Write([]byte(`{"jobs":[]}`))
		return
	}
	if name, ok := strings.CutPrefix(q.Query, "SELECT SAFE."); ok {
		name = strings.ToUpper(strings.Trim(strings.TrimSuffix(name, "()"), "`"))
		msg := "Function not found: " + name
		switch name {
		case "UPPER":
			msg = "No matching signature for function UPPER with no arguments"
		case "FN":
			msg = "No matching signature for function :P_DS_FN with no arguments"
		}
		writeError(w, http.StatusBadRequest, "invalidQuery", msg)
		return
	}
	if strings.Contains(q.Query, "()") && !strings.Contains(q.Query, "SAFE.") {
		// functionKind's probe of a function before CREATE FUNCTION.
		writeError(w, http.StatusBadRequest, "invalidQuery", "Function not found: x")
		return
	}
	_, _ = w.Write([]byte(`{"jobComplete":true,"jobReference":{"projectId":"p","jobId":"q"}}`))
}

// TestFunctionNamesInTheDefaultDataset (#1033): a call of a function the
// default dataset has, given without a dataset, is sent as dataset.name;
// a built-in's name, a TEMP function's, one in a CREATE FUNCTION's body, one after a dot, and a function the dataset does
// not have are sent as they are, and so is every name with no default
// dataset.
func TestFunctionNamesInTheDefaultDataset(t *testing.T) {
	emu := &fnEmulator{}
	h := Wrap(emu)
	query := func(sql, dataset string) string {
		t.Helper()
		body := map[string]any{"query": sql, "useLegacySql": false}
		if dataset != "" {
			body["defaultDataset"] = map[string]string{"projectId": "p", "datasetId": dataset}
		}
		b, _ := json.Marshal(body)
		emu.mu.Lock()
		emu.queries = nil
		emu.mu.Unlock()
		if code, got := do(t, h, "POST", base+"/queries", string(b)); code != 200 {
			t.Fatalf("%s: %d %v", sql, code, got)
		}
		emu.mu.Lock()
		defer emu.mu.Unlock()
		return emu.queries[len(emu.queries)-1]
	}
	query("CREATE FUNCTION ds.fn(x INT64) AS (x + 1)", "")
	query("CREATE FUNCTION ds.upper(x STRING) AS (x)", "")
	for _, c := range []struct{ sql, dataset, want string }{
		{"SELECT fn(1), FN(2), `fn`(3)", "ds", "SELECT `ds`.`fn`(1), `ds`.`FN`(2), `ds`.`fn`(3)"},
		{"SELECT fn(1)", "", "SELECT fn(1)"},
		{"SELECT fn(1)", "other", "SELECT fn(1)"},
		{"SELECT upper('a'), ds.fn(1), a.fn(1), @fn, 'fn(1)' -- fn(1)\n", "ds", "SELECT upper('a'), ds.fn(1), a.fn(1), @fn, 'fn(1)' -- fn(1)\n"},
		{"CREATE TEMP FUNCTION fn(x INT64) AS (x); SELECT fn(1)", "ds", "CREATE TEMP FUNCTION fn(x INT64) AS (x); SELECT fn(1)"},
		{"CREATE FUNCTION ds.g(x INT64) AS (fn(x)); SELECT g(1), fn(2)", "ds",
			"CREATE FUNCTION ds.g(x INT64) AS (fn(x)); SELECT `ds`.`g`(1), `ds`.`fn`(2)"},
		{"SELECT nosuch(1)", "ds", "SELECT nosuch(1)"},
	} {
		if got := query(c.sql, c.dataset); got != c.want {
			t.Errorf("%q with %q: sent %q, want %q", c.sql, c.dataset, got, c.want)
		}
	}
}
