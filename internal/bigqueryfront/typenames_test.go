package bigqueryfront

import (
	"strings"
	"testing"
)

// TestGoogleSQLTypeNamesAreSentAsLegacyNames (#1034): a schema's GoogleSQL
// type names, at any depth, are sent and read back by the legacy names
// BigQuery reports, the only ones the Go client reads; the rest of the
// body is sent as it came.
func TestGoogleSQLTypeNamesAreSentAsLegacyNames(t *testing.T) {
	const aliases = `{"fields":[{"name":"n","type":"INT64","mode":"REQUIRED"},{"name":"b","type":"bool"},` +
		`{"name":"d","type":"DECIMAL"},{"name":"e","type":"BIGDECIMAL"},{"name":"s","type":"STRING"},` +
		`{"name":"r","type":"STRUCT","fields":[{"name":"x","type":"BOOL"},{"name":"y","type":"INT64"}]}]}`
	const legacy = `{"fields":[{"name":"n","type":"INTEGER","mode":"REQUIRED"},{"name":"b","type":"BOOLEAN"},` +
		`{"name":"d","type":"NUMERIC"},{"name":"e","type":"BIGNUMERIC"},{"name":"s","type":"STRING"},` +
		`{"name":"r","type":"RECORD","fields":[{"name":"x","type":"BOOLEAN"},{"name":"y","type":"INTEGER"}]}]}`

	e := newStateEmulator()
	e.datasets["ds"] = true
	code, got := do(t, Wrap(e), "POST", base+"/datasets/ds/tables",
		`{"tableReference":{"projectId":"p","datasetId":"ds","tableId":"t"},"description":"d","schema":`+aliases+`}`)
	if code != 200 {
		t.Fatalf("tables.insert: %d %v", code, got)
	}
	if !sameJSON(e.tables["ds.t"], `{"type":"TABLE","schema":`+legacy+`}`) {
		t.Errorf("the table reads %s, want the legacy names", e.tables["ds.t"])
	}
	if len(e.log) != 1 || !e.sent(`^POST \S+/datasets/ds/tables .*"description":"d"`) || e.sent(`INT64|BOOL"|STRUCT|DECIMAL`) {
		t.Errorf("tables.insert sent %v", e.log)
	}

	// A FLOAT64 among them is FLOAT's alias: FLOAT64 in the engine,
	// FLOAT read back (#1000).
	e = newStateEmulator()
	e.datasets["ds"] = true
	if code, got := do(t, Wrap(e), "POST", base+"/datasets/ds/tables",
		`{"tableReference":{"tableId":"u"},"schema":{"fields":[{"name":"n","type":"INT64"},{"name":"f","type":"FLOAT64"}]}}`); code != 200 {
		t.Fatalf("tables.insert with FLOAT64: %d %v", code, got)
	}
	if !sameJSON(e.tables["ds.u"], `{"type":"TABLE","schema":{"fields":[{"name":"n","type":"INTEGER"},{"name":"f","type":"FLOAT"}]}}`) ||
		!e.sent(`^POST \S+/tables .*"name":"f","type":"FLOAT64"`) || !e.sent(`^POST \S+/tables .*"name":"n","type":"INTEGER"`) {
		t.Errorf("with FLOAT64: sent %v, the table reads %s", e.log, e.tables["ds.u"])
	}

	// Legacy names only: sent as they came.
	e = newStateEmulator()
	e.datasets["ds"] = true
	body := `{"tableReference":{"tableId":"v"},"schema":` + legacy + `}`
	if code, _ := do(t, Wrap(e), "POST", base+"/datasets/ds/tables", body); code != 200 || len(e.log) != 1 ||
		!strings.HasSuffix(e.log[0], body) {
		t.Errorf("legacy names: %d, sent %v", code, e.log)
	}

	// A load's schema.
	e = newStateEmulator()
	e.datasets["ds"] = true
	code, got = do(t, Wrap(e), "POST", base+"/jobs", `{"jobReference":{"projectId":"p","jobId":"l1"},"configuration":{"load":{`+
		`"sourceFormat":"NEWLINE_DELIMITED_JSON","destinationTable":{"projectId":"p","datasetId":"ds","tableId":"ld"},`+
		`"schema":`+aliases+`}}}`)
	if reason, done := jobState(got); code != 200 || !done || reason != "" {
		t.Fatalf("a load: %d %v", code, got)
	}
	if !e.sent(`^POST \S+/jobs .*"name":"n","type":"INTEGER"`) || e.sent(`^POST \S+/jobs .*(INT64|STRUCT)`) {
		t.Errorf("the load was sent %v", e.log)
	}
	if !sameJSON(e.tables["ds.ld"], `{"type":"TABLE","schema":`+legacy+`}`) {
		t.Errorf("the loaded table reads %s", e.tables["ds.ld"])
	}

	// An unknown type is still refused, by its own name.
	e = newStateEmulator()
	e.datasets["ds"] = true
	code, got = do(t, Wrap(e), "POST", base+"/datasets/ds/tables", `{"tableReference":{"tableId":"w"},"schema":{"fields":[{"name":"n","type":"INT65"}]}}`)
	if code != 400 || !strings.Contains(errMsg(got), `"INT65"`) || len(e.log) != 0 {
		t.Errorf("an unknown type: %d %v, sent %v", code, got, e.log)
	}
}

// TestViewsNameTheirTablesWithADataset (#1035): a view's query that names
// a table without a dataset is refused 400 through tables.insert,
// tables.update and tables.patch, and nothing is sent; a qualified one,
// and a legacy SQL view, are sent.
func TestViewsNameTheirTablesWithADataset(t *testing.T) {
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/datasets/ds/tables", `{"tableReference":{"tableId":"v"},"view":{"query":"SELECT * FROM t","useLegacySql":false}}`, 400},
		{"POST", "/datasets/ds/tables", `{"tableReference":{"tableId":"v"},"view":{"query":"SELECT a FROM ds.t JOIN u USING (a)"}}`, 400},
		{"POST", "/datasets/ds/tables", `{"tableReference":{"tableId":"v"},"materializedView":{"query":"SELECT a FROM t"}}`, 400},
		{"PATCH", "/datasets/ds/tables/v", `{"view":{"query":"SELECT * FROM t","useLegacySql":false}}`, 400},
		{"PUT", "/datasets/ds/tables/v", `{"view":{"query":"SELECT * FROM t","useLegacySql":false}}`, 400},
		{"POST", "/datasets/ds/tables", `{"tableReference":{"tableId":"v"},"view":{"query":"SELECT * FROM ds.t","useLegacySql":false}}`, 200},
		{"POST", "/datasets/ds/tables", `{"tableReference":{"tableId":"v"},"view":{"query":"WITH c AS (SELECT 1 AS a) SELECT a FROM c"}}`, 200},
		{"POST", "/datasets/ds/tables", `{"tableReference":{"tableId":"v"},"view":{"query":"SELECT a FROM [ds.t]","useLegacySql":true}}`, 200},
	} {
		e := newStateEmulator()
		e.datasets["ds"] = true
		e.tables["ds.t"] = `{"type":"TABLE","schema":{"fields":[{"name":"a","type":"INTEGER"}]}}`
		e.tables["ds.v"] = `{"type":"VIEW","view":{"query":"SELECT * FROM ds.t"}}`
		code, got := do(t, Wrap(e), c.method, base+c.path, c.body)
		if code != c.want {
			t.Errorf("%s %s: %d %v, want %d", c.method, c.body, code, got, c.want)
			continue
		}
		if c.want == 400 {
			if !strings.Contains(errMsg(got), "must be qualified with a dataset") || e.sent(`^(POST|PATCH|PUT) \S+/tables`) {
				t.Errorf("%s %s: %v, sent %v", c.method, c.body, got, e.log)
			}
		}
	}
}
