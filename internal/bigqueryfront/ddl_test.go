package bigqueryfront

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDDLInBlocksAndAlterTable (#901): ALTER TABLE's new names and the
// statements inside a script's blocks are held to the rules, 400; an ALTER
// TABLE or a control-flow block that passes is 501, as the emulator would
// report it done and do nothing (measured); a BEGIN ... END block is not.
func TestDDLInBlocksAndAlterTable(t *testing.T) {
	for _, c := range []struct {
		sql  string
		code int
		want string
	}{
		{"ALTER TABLE ds.t ADD COLUMN `b!` STRING", 400, `Invalid field name "b!"`},
		{"ALTER TABLE IF EXISTS ds.t ADD COLUMN IF NOT EXISTS ok STRING, ADD COLUMN s STRUCT<`q?` INT64>", 400, `Invalid field name "s.q?"`},
		{"ALTER TABLE ds.t RENAME TO `t!`", 400, `Invalid table ID "t!"`},
		{"ALTER TABLE ds.t RENAME COLUMN IF EXISTS a TO `c!`", 400, `Invalid field name "c!"`},
		{"ALTER TABLE ds.t ADD COLUMN `first name` STRING", 501, "ALTER TABLE"},
		{"alter table `p.ds.t` rename to `t 2`", 501, "ALTER TABLE"},
		{"ALTER TABLE ds.t SET OPTIONS (description = 'ADD COLUMN `x!`')", 501, "ALTER TABLE"},
		{"IF TRUE THEN CREATE TABLE ds.`t2!` (a INT64); END IF", 400, `Invalid table ID "t2!"`},
		{"IF FALSE THEN SELECT 1; ELSEIF TRUE THEN SELECT 2; ELSE CREATE SCHEMA `x-y`; END IF", 400, `Invalid dataset ID "x-y"`},
		{"IF (SELECT CASE WHEN a THEN 1 END FROM ds.t) = 1 THEN CREATE TABLE ds.`q!` (a INT64); END IF", 400, `"q!"`},
		{"LOOP CREATE TABLE ds.`t5!` (a INT64); LEAVE; END LOOP", 400, `"t5!"`},
		{"lbl: WHILE TRUE DO ALTER TABLE ds.t ADD COLUMN `w!` INT64; END WHILE lbl", 400, `"w!"`},
		{"FOR x IN (SELECT 1 AS v) DO CREATE TABLE ds.`f!` (a INT64); END FOR", 400, `"f!"`},
		{"REPEAT CREATE TABLE ds.`r!` (a INT64); UNTIL TRUE END REPEAT", 400, `"r!"`},
		{"CASE WHEN TRUE THEN CREATE TABLE ds.ok (a INT64); WHEN FALSE THEN CREATE TABLE ds.`c!` (a INT64); END CASE", 400, `"c!"`},
		{"BEGIN SELECT 1; EXCEPTION WHEN ERROR THEN CREATE TABLE ds.`e!` (a INT64); END", 400, `"e!"`},
		{"IF TRUE THEN INSERT INTO ds.t (a) VALUES (1); END IF", 501, "control-flow block (IF)"},
		{"DECLARE i INT64; LOOP SET i = 1; LEAVE; END LOOP", 501, "control-flow block (LOOP)"},
		{"FOR x IN (SELECT 1 AS v) DO SELECT x.v; END FOR", 501, "control-flow block (FOR)"},
		{"BEGIN CREATE TABLE ds.t7 (a INT64); END", 0, ""},
		{"BEGIN TRANSACTION; INSERT INTO ds.t (a) VALUES (1); COMMIT TRANSACTION", 0, ""},
		{"SELECT IF(a > 1, 'x', 'y'), CASE WHEN a THEN 1 END FROM ds.t", 0, ""},
		{"SELECT 'IF TRUE THEN SELECT 1; END IF'", 0, ""},
	} {
		got := checkDDL(c.sql)
		if got.code != c.code || !strings.Contains(got.msg, c.want) {
			t.Errorf("%q: %d %q, want %d %q", c.sql, got.code, got.msg, c.code, c.want)
		}
	}
}

// A CREATE TABLE ... AS with no column list gives its query to be run
// alone; with a column list, the names are known.
func TestDDLFindsTheQueryOfCreateTableAsSelect(t *testing.T) {
	for sql, want := range map[string][]string{
		"CREATE TABLE ds.c AS SELECT 1 AS `x!`": {"SELECT 1 AS `x!`"},
		"create or replace table ds.c partition by d cluster by a options(description='AS') as (select 1 a)": {"(select 1 a)"},
		"SELECT 1; CREATE TEMP TABLE tt AS WITH w AS (SELECT 2 AS b) SELECT * FROM w; SELECT 3":              {"WITH w AS (SELECT 2 AS b) SELECT * FROM w"},
		"CREATE TABLE ds.c (a INT64) AS SELECT 1":                                                            nil,
	} {
		got := checkDDL(sql)
		var queries []string
		for _, c := range got.selects() {
			queries = append(queries, c.query)
		}
		if got.code != 0 || strings.Join(queries, "|") != strings.Join(want, "|") {
			t.Errorf("%q: %+v, want the queries %q", sql, got, want)
		}
	}
}

// ctasEmulator answers jobs.query with the schema it is given for the
// query the front runs alone, and records every query.
type ctasEmulator struct {
	schema  string
	status  int
	queries []string
}

func (e *ctasEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
		return
	}
	var body struct {
		Query          string          `json:"query"`
		DefaultDataset json.RawMessage `json:"defaultDataset"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &body)
	e.queries = append(e.queries, body.Query+" @"+string(body.DefaultDataset))
	if strings.HasPrefix(body.Query, "SELECT * FROM (") && e.status != 0 {
		http.Error(w, `{"error":{"code":400}}`, e.status)
		return
	}
	if strings.HasPrefix(body.Query, "SELECT * FROM (") {
		_, _ = io.WriteString(w, `{"schema":`+e.schema+`,"jobComplete":true}`)
		return
	}
	_, _ = io.WriteString(w, `{"jobComplete":true}`)
}

// TestCreateTableAsSelectColumnsAreChecked (#901): the query of a CREATE
// TABLE ... AS SELECT is run alone with its default dataset, and a result
// column BigQuery would refuse, nested ones included, refuses the
// statement with 400 before it is sent. The emulator's name for a column
// with no alias is let through, and so is a statement whose query cannot
// run alone.
func TestCreateTableAsSelectColumnsAreChecked(t *testing.T) {
	for _, c := range []struct {
		name, schema string
		status, want int
	}{
		{"valid", `{"fields":[{"name":"first name","type":"INTEGER"},{"name":"$col2","type":"INTEGER"}]}`, 0, 200},
		{"top level", `{"fields":[{"name":"x!","type":"INTEGER"}]}`, 0, 400},
		{"nested", `{"fields":[{"name":"s","type":"RECORD","fields":[{"name":"y?","type":"INTEGER"}]}]}`, 0, 400},
		{"cannot run alone", ``, 400, 200},
	} {
		for _, path := range []string{"/queries", "/jobs"} {
			emu := &ctasEmulator{schema: c.schema, status: c.status}
			body := `{"query":"CREATE TABLE ds.c AS SELECT q FROM t","defaultDataset":{"datasetId":"ds"}}`
			if path == "/jobs" {
				body = `{"jobReference":{"jobId":"j"},"configuration":{"query":{"query":"CREATE TABLE ds.c AS SELECT q FROM t","defaultDataset":{"datasetId":"ds"}}}}`
			}
			code, got := do(t, Wrap(emu), "POST", base+path, body)
			if code != c.want {
				t.Errorf("%s %s: %d %v, want %d", c.name, path, code, got, c.want)
			}
			if len(emu.queries) == 0 || emu.queries[0] != "SELECT * FROM (\nSELECT q FROM `ds.t`\n) LIMIT 0 @{\"datasetId\":\"ds\"}" {
				t.Errorf("%s %s: the query run alone was %q", c.name, path, emu.queries)
			}
			if sent := len(emu.queries) == 2; sent != (c.want == 200) {
				t.Errorf("%s %s: the statement reached the emulator: %v", c.name, path, sent)
			}
		}
	}
	// Legacy SQL is not read.
	emu := &ctasEmulator{schema: `{"fields":[{"name":"x!","type":"INTEGER"}]}`}
	if code, _ := do(t, Wrap(emu), "POST", base+"/queries", `{"query":"CREATE TABLE ds.c AS SELECT 1","useLegacySql":true}`); code != 200 || len(emu.queries) != 1 {
		t.Errorf("legacy SQL: %d, queries %q", code, emu.queries)
	}
}

// TestRefusalsReachTheClientAsBigQueryAnswers checks the status and reason
// the verdicts are written with.
func TestRefusalsReachTheClientAsBigQueryAnswers(t *testing.T) {
	for sql, want := range map[string][2]any{
		"ALTER TABLE ds.t ADD COLUMN ok STRING":   {501, "notImplemented"},
		"ALTER TABLE ds.t ADD COLUMN `a!` STRING": {400, "invalidQuery"},
		"IF TRUE THEN SELECT 1; END IF":           {501, "notImplemented"},
	} {
		emu := &fakeEmulator{}
		b, _ := json.Marshal(map[string]string{"query": sql})
		r := httptest.NewRequest("POST", base+"/queries", strings.NewReader(string(b)))
		w := httptest.NewRecorder()
		Wrap(emu).ServeHTTP(w, r)
		var got struct {
			Error struct {
				Errors []struct{ Reason string } `json:"errors"`
			} `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code != want[0] || len(got.Error.Errors) != 1 || got.Error.Errors[0].Reason != want[1] || len(emu.writes) != 0 {
			t.Errorf("%q: %d %s, %d writes; want %v", sql, w.Code, w.Body, len(emu.writes), want)
		}
	}
}
