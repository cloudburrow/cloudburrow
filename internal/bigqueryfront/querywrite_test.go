package bigqueryfront

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// writeEmulator holds tables in dataset ds, each with a schema and rows (a
// list of labels), and answers what queryWrite sends: tables.get,
// tables.delete, tables.insert and tables.patch; a query job, whose result
// (result's columns, one row "result") it writes to its destination table,
// which it makes when it does not exist, and appends to otherwise; and
// jobs.query: the query run alone (LIMIT 0), DELETE, INSERT ... SELECT, and
// a script of both, which fails with failScript and then changes nothing.
type writeEmulator struct {
	mu         sync.Mutex
	tables     map[string]*writeTable
	result     string // the query's columns, a TableSchema's fields
	failScript bool
	failQuery  bool
	jobs       []map[string]any
	queries    []string
}

type writeTable struct {
	schema      json.RawMessage
	rows        []string
	description string
}

func (e *writeEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, base)
	b, _ := io.ReadAll(r.Body)
	notFound := func() { http.Error(w, `{"error":{"code":404,"message":"not found"}}`, http.StatusNotFound) }
	if name, ok := strings.CutPrefix(p, "/datasets/ds/tables/"); ok {
		t := e.tables[name]
		switch r.Method {
		case http.MethodGet:
			if t == nil {
				notFound()
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "TABLE", "schema": t.schema, "numRows": strconv.Itoa(len(t.rows)),
				"description": t.description, "tableReference": map[string]any{"projectId": "p", "datasetId": "ds", "tableId": name}})
		case http.MethodDelete:
			if t == nil {
				notFound()
				return
			}
			delete(e.tables, name)
			w.WriteHeader(http.StatusNoContent)
		default: // tables.patch
			var patch struct {
				Schema json.RawMessage `json:"schema"`
			}
			_ = json.Unmarshal(b, &patch)
			if t == nil {
				notFound()
				return
			}
			t.schema = patch.Schema
			_, _ = w.Write([]byte(`{}`))
		}
		return
	}
	switch p {
	case "/jobs/j1":
		if len(e.jobs) == 0 {
			notFound()
			return
		}
		job := e.jobs[len(e.jobs)-1]
		_ = json.NewEncoder(w).Encode(map[string]any{"jobReference": job["jobReference"], "configuration": job["configuration"],
			"status": map[string]any{"state": "DONE"}})
	case "/datasets/ds/tables":
		var tb struct {
			TableReference tableRef        `json:"tableReference"`
			Schema         json.RawMessage `json:"schema"`
			Description    string          `json:"description"`
		}
		_ = json.Unmarshal(b, &tb)
		e.tables[tb.TableReference.TableID] = &writeTable{schema: tb.Schema, description: tb.Description}
		_, _ = w.Write(b)
	case "/jobs":
		var job map[string]any
		_ = json.Unmarshal(b, &job)
		e.jobs = append(e.jobs, job)
		ans := map[string]any{"jobReference": job["jobReference"], "configuration": job["configuration"],
			"status": map[string]any{"state": "DONE"}}
		q := job["configuration"].(map[string]any)["query"].(map[string]any)
		if e.failQuery {
			ans["status"] = map[string]any{"state": "DONE", "errorResult": map[string]any{"reason": "invalidQuery", "message": "boom"}}
		} else if d, ok := q["destinationTable"].(map[string]any); ok {
			name := d["tableId"].(string)
			if e.tables[name] == nil {
				e.tables[name] = &writeTable{schema: json.RawMessage(`{"fields":` + e.result + `}`)}
			}
			e.tables[name].rows = append(e.tables[name].rows, "result")
		}
		_ = json.NewEncoder(w).Encode(ans)
	case "/queries":
		var q queryOptions
		_ = json.Unmarshal(b, &q)
		e.queries = append(e.queries, q.Query)
		if strings.HasSuffix(q.Query, "LIMIT 0") {
			_, _ = w.Write([]byte(`{"jobComplete":true,"schema":{"fields":` + e.result + `}}`))
			return
		}
		stmts := strings.Split(q.Query, ";\n")
		if len(stmts) > 1 && e.failScript {
			http.Error(w, `{"error":{"code":400,"message":"script failed"}}`, http.StatusBadRequest)
			return
		}
		for _, s := range stmts {
			switch {
			case strings.HasPrefix(s, "DELETE FROM `ds.t`"):
				e.tables["t"].rows = nil
			case strings.HasPrefix(s, "INSERT INTO `ds.t`"):
				from := s[strings.LastIndex(s, "`ds.")+4 : len(s)-1]
				e.tables["t"].rows = append(e.tables["t"].rows, e.tables[from].rows...)
			}
		}
		_, _ = w.Write([]byte(`{"jobComplete":true}`))
	default:
		notFound()
	}
}

// TestQueryJobWriteDispositions (#1080): a query job into a table that
// exists is carried out as BigQuery does: WRITE_TRUNCATE_DATA and
// WRITE_TRUNCATE with the table's columns replace its rows in one script,
// keeping the table; WRITE_TRUNCATE with other columns makes the table
// again with the result's, keeping its description; the query's result
// goes first to a scratch table, which is deleted. The job reads back with
// the client's destination and writeDisposition. WRITE_TRUNCATE_DATA with
// other columns, or into a table with a REQUIRED column, is 501 and runs
// nothing; a failed script fails the job and leaves the rows; a failed
// query leaves the table.
func TestQueryJobWriteDispositions(t *testing.T) {
	const same = `[{"name":"a","type":"INTEGER"},{"name":"b","type":"STRING"}]`
	const other = `[{"name":"z","type":"FLOAT"}]`
	for _, c := range []struct {
		name, write, table, result string
		failScript, failQuery      bool
		code                       int
		rows                       string // the table's rows after, "" when unchanged
		schema                     string // the table's columns after
		failed                     string // the job's errorResult reason
	}{
		{"WRITE_TRUNCATE_DATA", "WRITE_TRUNCATE_DATA", same, same, false, false, 200, "result", same, ""},
		{"WRITE_TRUNCATE, same columns", "WRITE_TRUNCATE", same, same, false, false, 200, "result", same, ""},
		{"WRITE_TRUNCATE, other columns", "WRITE_TRUNCATE", same, other, false, false, 200, "result", other, ""},
		{"WRITE_TRUNCATE_DATA, other columns", "WRITE_TRUNCATE_DATA", same, other, false, false, 501, "", same, ""},
		{"WRITE_TRUNCATE_DATA, REQUIRED", "WRITE_TRUNCATE_DATA", `[{"name":"a","type":"INTEGER","mode":"REQUIRED"}]`,
			`[{"name":"a","type":"INTEGER"}]`, false, false, 501, "", `[{"name":"a","type":"INTEGER","mode":"REQUIRED"}]`, ""},
		{"a failed script", "WRITE_TRUNCATE_DATA", same, same, true, false, 200, "", same, "backendError"},
		{"a failed query", "WRITE_TRUNCATE", same, same, false, true, 200, "", same, "invalidQuery"},
	} {
		emu := &writeEmulator{result: c.result, failScript: c.failScript, failQuery: c.failQuery,
			tables: map[string]*writeTable{"t": {schema: json.RawMessage(`{"fields":` + c.table + `}`), rows: []string{"old"},
				description: "kept"}}}
		h := Wrap(emu)
		job := `{"jobReference":{"projectId":"p","jobId":"j1"},"configuration":{"query":{"query":"SELECT 1 AS a",` +
			`"useLegacySql":false,"destinationTable":{"projectId":"p","datasetId":"ds","tableId":"t"},"writeDisposition":"` +
			c.write + `"}}}`
		code, got := do(t, h, "POST", base+"/jobs", job)
		if code != c.code {
			t.Errorf("%s: %d %v, want %d", c.name, code, got, c.code)
			continue
		}
		tbl := emu.tables["t"]
		rows := "old"
		if c.rows != "" {
			rows = c.rows
		}
		if tbl == nil || strings.Join(tbl.rows, ",") != rows || tbl.description != "kept" {
			t.Errorf("%s: the table after: %+v, want rows %s", c.name, tbl, rows)
		} else {
			var s tableSchema
			var want []field
			_ = json.Unmarshal(tbl.schema, &s)
			_ = json.Unmarshal([]byte(c.schema), &want)
			if !sameSchema(s.Fields, want) {
				t.Errorf("%s: the table's schema after: %s, want %s", c.name, tbl.schema, c.schema)
			}
		}
		if c.code == 501 {
			if len(emu.jobs) != 0 {
				t.Errorf("%s: sent %v", c.name, emu.jobs)
			}
			continue
		}
		if len(emu.tables) != 1 && c.failed != "backendError" {
			t.Errorf("%s: tables left: %v", c.name, emu.tables)
		}
		status, _ := got["status"].(map[string]any)
		res, _ := status["errorResult"].(map[string]any)
		if reason, _ := res["reason"].(string); reason != c.failed {
			t.Errorf("%s: errorResult %v, want %q", c.name, res, c.failed)
		}
		for what, j := range map[string]map[string]any{"jobs.insert": got, "jobs.get": jobGot(t, h)} {
			q, _ := j["configuration"].(map[string]any)["query"].(map[string]any)
			d, _ := q["destinationTable"].(map[string]any)
			if d["tableId"] != "t" || q["writeDisposition"] != c.write {
				t.Errorf("%s: %s shows %v", c.name, what, q)
			}
		}
		if sent := emu.jobs[0]["configuration"].(map[string]any)["query"].(map[string]any); strings.Contains(
			sent["destinationTable"].(map[string]any)["tableId"].(string), "_cloudburrow_") != true ||
			sent["writeDisposition"] != "WRITE_EMPTY" {
			t.Errorf("%s: sent %v, want a scratch table with WRITE_EMPTY", c.name, sent)
		}
	}
}

func jobGot(t *testing.T, h http.Handler) map[string]any {
	t.Helper()
	_, got := do(t, h, "GET", base+"/jobs/j1", "")
	return got
}
