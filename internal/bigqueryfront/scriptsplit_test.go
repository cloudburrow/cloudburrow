package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// exprQuery is the query the front's run of a script evaluates a DECLARE's
// DEFAULT or a SET's literal value with (scriptRun.evaluate).
var exprQuery = regexp.MustCompile(`^SELECT CAST\(\(('?[^()']*'?)\) AS (\w+)\)$`)

// withExprs answers each such query with its literal, as the emulator
// would, and sends every other request to the fake it wraps: since #1459
// a script of several statements is carried out a statement at a time,
// its expressions too, and the unit tests' fakes answer statements only.
type withExprs struct{ http.Handler }

func (e withExprs) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/queries") {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		var q struct {
			Query string `json:"query"`
		}
		if json.Unmarshal(b, &q) == nil {
			if m := exprQuery.FindStringSubmatch(q.Query); m != nil {
				typ := map[string]string{"INT64": "INTEGER", "FLOAT64": "FLOAT", "BOOL": "BOOLEAN"}[m[2]]
				if typ == "" {
					typ = m[2]
				}
				writeJSON(w, http.StatusOK, map[string]any{"jobComplete": true, "totalRows": "1",
					"schema": map[string]any{"fields": []any{map[string]any{"name": "f0_", "type": typ}}},
					"rows":   []any{map[string]any{"f": []any{map[string]any{"v": strings.Trim(m[1], "'")}}}}})
				return
			}
		}
	}
	e.Handler.ServeHTTP(w, r)
}

// TestScriptsCarriedOutInTurn (#1459): a script of several statements with
// no TEMP table or function is carried out a statement at a time, as
// BigQuery runs a script: each statement sent alone, each a child job of a
// parent of statementType SCRIPT; a failure keeps what the statements
// before it did, through jobs.query too. One with a TEMP function is sent
// whole.
func TestScriptsCarriedOutInTurn(t *testing.T) {
	sql := "CREATE SCHEMA fresh; CREATE TABLE fresh.t (a INT64); SELECT 1"
	emu := &datasetEmulator{datasets: map[string]string{}}
	if code, got := do(t, Wrap(emu), "POST", base+"/queries", schemaQuery(sql)); code != 200 {
		t.Fatalf("%s: %d %v", sql, code, got)
	}
	var sent []string
	for _, w := range emu.writes() {
		if i := strings.Index(w, `"query":"`); i >= 0 {
			sent = append(sent, w[i+len(`"query":"`):][:strings.Index(w[i+len(`"query":"`):], `"`)])
		}
	}
	if len(sent) < 3 || sent[0] != "CREATE SCHEMA fresh" || sent[1] != "CREATE TABLE fresh.t (a INT64)" || sent[2] != "SELECT 1" {
		t.Errorf("%s: sent %q, want each statement alone", sql, sent)
	}
	if _, ok := emu.datasets["fresh"]; !ok {
		t.Errorf("%s: no dataset fresh", sql)
	}

	// A query job: its children, and statementType SCRIPT.
	jobs := &jobsEmulator{}
	h := Wrap(jobs)
	code, got := do(t, h, "POST", base+"/jobs", queryBody("/jobs", "INSERT INTO ds.t VALUES (1); INSERT INTO ds.t VALUES (2); SELECT 3"))
	if code != 200 {
		t.Fatalf("jobs.insert: %d %v", code, got)
	}
	stats, _ := got["statistics"].(map[string]any)
	qs, _ := stats["query"].(map[string]any)
	if stats["numChildJobs"] != "3" || qs["statementType"] != "SCRIPT" {
		t.Errorf("the script's job: %v, want 3 children and statementType SCRIPT", stats)
	}

	// A failure after a write keeps the write (no 501), through jobs.query.
	fails := &jobsEmulator{run: func(q string) (int, string) {
		if strings.Contains(q, "nope") {
			return 400, `{"error":{"code":400,"message":"Table not found: nope.nope","errors":[{"reason":"invalidQuery"}]}}`
		}
		return 200, `{"jobComplete":true}`
	}}
	code, got = do(t, Wrap(fails), "POST", base+"/queries", queryBody("/queries", "INSERT INTO ds.t VALUES (1); SELECT * FROM nope.nope; INSERT INTO ds.t VALUES (2)"))
	if code != 400 || errorReason(got) != "invalidQuery" {
		t.Errorf("a failed script: %d %v, want 400", code, got)
	}
	inserts := 0
	for _, l := range fails.log {
		if strings.Contains(l, "INSERT INTO ds.t VALUES (2)") {
			t.Errorf("the statement after the failing one ran: %v", fails.log)
		}
		if strings.Contains(l, "INSERT INTO ds.t VALUES (1)") {
			inserts++
		}
	}
	if inserts == 0 {
		t.Errorf("the INSERT before the failing statement was not sent alone: %v", fails.log)
	}

	// With a TEMP function the script is sent whole.
	whole := &datasetEmulator{datasets: map[string]string{}}
	sql = "CREATE TEMP FUNCTION tf() AS (1); CREATE TABLE ds.u (a INT64); SELECT tf()"
	if code, got := do(t, Wrap(whole), "POST", base+"/queries", schemaQuery(sql)); code != 200 {
		t.Fatalf("%s: %d %v", sql, code, got)
	}
	if w := whole.writes(); len(w) != 1 || !strings.Contains(w[0], "CREATE TEMP FUNCTION") || !strings.Contains(w[0], "SELECT tf()") {
		t.Errorf("%s: sent %q, want it whole", sql, w)
	}
}
