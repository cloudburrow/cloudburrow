package bigqueryfront

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestParseDML(t *testing.T) {
	for _, c := range []struct {
		sql, kind, target string
		ok                bool
	}{
		{"INSERT INTO ds.t (a) VALUES (1)", "INSERT", "ds.t", true},
		{"INSERT t SELECT 1", "INSERT", "t", true},
		{"DELETE FROM `p.ds.t` WHERE a = 1;", "DELETE", "`p.ds.t`", true},
		{"DELETE t WHERE true", "DELETE", "t", true},
		{"TRUNCATE TABLE ds.t", "TRUNCATE_TABLE", "ds.t", true},
		{"UPDATE ds.t AS x SET a = (SELECT MAX(b) FROM u WHERE u.c = x.c) WHERE x.a > 0", "UPDATE", "ds.t AS x", true},
		{"UPDATE ds.t x SET a = 1 FROM u WHERE x.a = u.a", "", "", false},
		{"UPDATE ds.t SET a = 1", "", "", false},
		{"MERGE ds.t o USING ds.s ON o.id = s.id WHEN MATCHED THEN DELETE", "MERGE", "ds.t o", true},
		{"SELECT 1", "", "", false},
		{"INSERT INTO t VALUES (1); INSERT INTO t VALUES (2)", "", "", false},
		{"TRUNCATE t", "", "", false},
	} {
		d, ok := parseDML(c.sql)
		if ok != c.ok || ok && (d.kind != c.kind || (d.kind == "UPDATE" || d.kind == "MERGE") && d.targetText != c.target ||
			d.kind != "UPDATE" && d.kind != "MERGE" && d.pathText != c.target) {
			t.Errorf("parseDML(%q) = %+v, %v; want %s %s %v", c.sql, d, ok, c.kind, c.target, c.ok)
		}
	}
	d, ok := parseDML("UPDATE ds.t AS x SET a = (SELECT MAX(b) FROM u WHERE u.c = x.c) WHERE x.a > 0")
	if !ok || d.where != "x.a > 0" {
		t.Errorf("UPDATE's WHERE: %q", d.where)
	}
}

func TestParseMergeClauses(t *testing.T) {
	sql := "MERGE INTO ds.t AS o USING (SELECT 1 AS id, CASE WHEN x THEN 1 END AS y FROM (SELECT TRUE AS x)) AS s " +
		"ON o.id = s.id " +
		"WHEN MATCHED AND CASE WHEN s.y = 1 THEN TRUE END THEN DELETE " +
		"WHEN MATCHED THEN UPDATE SET y = CASE WHEN s.y > 0 THEN 1 ELSE 2 END " +
		"WHEN NOT MATCHED BY TARGET THEN INSERT ROW " +
		"WHEN NOT MATCHED BY SOURCE AND o.id < 0 THEN UPDATE SET y = 0"
	d, ok := parseDML(sql)
	if !ok {
		t.Fatal("not read")
	}
	if d.targetText != "ds.t AS o" || d.aliasText != "AS s" || d.on != "o.id = s.id" ||
		d.subquery != "SELECT 1 AS id, CASE WHEN x THEN 1 END AS y FROM (SELECT TRUE AS x)" ||
		sql[d.subPos:d.subEnd] != "("+d.subquery+")" {
		t.Errorf("read %+v", d)
	}
	want := []mergeClause{
		{matchedRows, "CASE WHEN s.y = 1 THEN TRUE END", "DELETE"},
		{matchedRows, "", "UPDATE"},
		{notMatchedByTarget, "", "INSERT"},
		{notMatchedBySource, "o.id < 0", "UPDATE"},
	}
	if len(d.clauses) != len(want) {
		t.Fatalf("clauses %+v", d.clauses)
	}
	for i := range want {
		if d.clauses[i] != want[i] {
			t.Errorf("clause %d: %+v, want %+v", i, d.clauses[i], want[i])
		}
	}
	got := mergeCounts(d, "`ds.scratch` AS s")
	for _, part := range []string{
		"SELECT c0, c1, c2, c3 FROM ",
		"COUNTIF(COALESCE((CASE WHEN s.y = 1 THEN TRUE END), FALSE)) AS c0",
		"COUNTIF(NOT COALESCE((CASE WHEN s.y = 1 THEN TRUE END), FALSE) AND TRUE) AS c1",
		"FROM ds.t AS o INNER JOIN `ds.scratch` AS s ON o.id = s.id",
		"(SELECT COUNTIF(TRUE) AS c2 FROM `ds.scratch` AS s WHERE NOT EXISTS (SELECT 1 FROM ds.t AS o WHERE o.id = s.id))",
		"(SELECT COUNTIF(COALESCE((o.id < 0), FALSE)) AS c3 FROM ds.t AS o WHERE NOT EXISTS (SELECT 1 FROM `ds.scratch` AS s WHERE o.id = s.id))",
	} {
		if !strings.Contains(got, part) {
			t.Errorf("the count query %q lacks %q", got, part)
		}
	}
	for _, bad := range []string{
		"MERGE t USING s ON a = b",
		"MERGE t USING s ON a = b WHEN MATCHED THEN INSERT ROW",
		"MERGE t USING s ON a = b WHEN NOT MATCHED THEN DELETE",
	} {
		if _, ok := parseDML(bad); ok {
			t.Errorf("parseDML(%q) read it", bad)
		}
	}
	if msg := mergeFromSubquery("SELECT 1; MERGE t USING (SELECT 1 AS a) s ON t.a = s.a WHEN MATCHED THEN DELETE"); msg == "" {
		t.Error("a script's MERGE from a subquery was not refused")
	}
	if msg := mergeFromSubquery("SELECT 1; MERGE t USING s ON t.a = s.a WHEN MATCHED THEN DELETE"); msg != "" {
		t.Errorf("a script's MERGE from a table was refused: %s", msg)
	}
}

func TestDMLCountsPatch(t *testing.T) {
	d := dmlCounts{statementType: "MERGE", inserted: 1, updated: 2}
	job := map[string]any{"configuration": map[string]any{}, "statistics": map[string]any{"query": map[string]any{"statementType": "SELECT"}}}
	d.patch(job)
	b, _ := json.Marshal(job["statistics"])
	if string(b) != `{"query":{"dmlStats":{"insertedRowCount":"1","updatedRowCount":"2"},"numDmlAffectedRows":"3","statementType":"MERGE"}}` {
		t.Errorf("a job's statistics: %s", b)
	}
	resp := map[string]any{"jobComplete": true}
	d.patch(resp)
	if resp["numDmlAffectedRows"] != "3" || resp["dmlStats"] == nil {
		t.Errorf("a QueryResponse: %v", resp)
	}
}

// dmlEmulator runs a table of n rows: a COUNT(*) query answers n, and an
// INSERT adds two rows.
type dmlEmulator struct {
	n    int
	sent []string
}

func (e *dmlEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/jobs/") {
		_, _ = io.WriteString(w, `{"jobReference":{"projectId":"p","jobId":"j1"},"statistics":{"query":{"statementType":"SELECT"}},"status":{"state":"DONE"}}`)
		return
	}
	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, `{"type":"TABLE"}`)
		return
	}
	var body struct {
		Query         string `json:"query"`
		Configuration struct {
			Query struct {
				Query string `json:"query"`
			} `json:"query"`
		} `json:"configuration"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	q := body.Query + body.Configuration.Query.Query
	e.sent = append(e.sent, q)
	switch {
	case strings.HasPrefix(q, "SELECT COUNT(*)"):
		_, _ = io.WriteString(w, `{"jobReference":{"jobId":"c"},"rows":[{"f":[{"v":"`+strings.Repeat("", 0)+itoa(e.n)+`"}]}]}`)
	case strings.HasPrefix(q, "INSERT"):
		e.n += 2
		_, _ = io.WriteString(w, `{"configuration":{"query":{"query":"`+q+`"}},"jobReference":{"projectId":"p","jobId":"j1"},`+
			`"statistics":{"query":{"statementType":"SELECT"}},"status":{"state":"DONE"}}`)
	default:
		_, _ = io.WriteString(w, `{}`)
	}
}

func TestInsertJobReportsTheRowsItAdded(t *testing.T) {
	emu := &dmlEmulator{n: 3}
	h := Wrap(emu)
	code, got := do(t, h, "POST", base+"/jobs",
		`{"jobReference":{"projectId":"p","jobId":"j1"},"configuration":{"query":{"query":"INSERT INTO ds.t (a) VALUES (1), (2)","useLegacySql":false}}}`)
	stats, _ := got["statistics"].(map[string]any)
	q, _ := stats["query"].(map[string]any)
	if code != 200 || q["statementType"] != "INSERT" || q["numDmlAffectedRows"] != "2" {
		t.Errorf("jobs.insert: %d %v", code, got)
	}
	// jobs.get reports it too.
	emu.sent = nil
	code, got = do(t, h, "GET", base+"/jobs/j1", "")
	stats, _ = got["statistics"].(map[string]any)
	q, _ = stats["query"].(map[string]any)
	if code != 200 || q["numDmlAffectedRows"] != "2" {
		t.Errorf("jobs.get: %d %v", code, got)
	}
}

// TestDMLIsQualifiedFirst (#1008 with #1015): a lone DML statement that
// names its table without a dataset is sent, and counted, in the default
// dataset, and the job shows the client's text.
func TestDMLIsQualifiedFirst(t *testing.T) {
	emu := &dmlEmulator{n: 3}
	code, got := do(t, Wrap(emu), "POST", base+"/jobs",
		`{"jobReference":{"projectId":"p","jobId":"j1"},"configuration":{"query":{"query":"INSERT INTO t (a) VALUES (1), (2)",`+
			`"defaultDataset":{"projectId":"p","datasetId":"ds"},"useLegacySql":false}}}`)
	stats, _ := got["statistics"].(map[string]any)
	q, _ := stats["query"].(map[string]any)
	conf, _ := got["configuration"].(map[string]any)["query"].(map[string]any)
	if code != 200 || q["numDmlAffectedRows"] != "2" || conf["query"] != "INSERT INTO t (a) VALUES (1), (2)" {
		t.Errorf("jobs.insert: %d %v", code, got)
	}
	for _, sent := range emu.sent {
		if !strings.Contains(sent, "`ds.t`") {
			t.Errorf("sent %q, want ds.t", sent)
		}
	}
	if len(emu.sent) != 3 {
		t.Errorf("sent %q, want two counts and the statement", emu.sent)
	}
}

func TestPatchTableMergesLabels(t *testing.T) {
	current := map[string]json.RawMessage{
		"description": json.RawMessage(`"old"`), "labels": json.RawMessage(`{"a":"1","b":"2","keep":"k"}`),
		"schema": json.RawMessage(`{"fields":[{"name":"id","type":"INTEGER"}]}`),
	}
	b, ok := patchTable(current, map[string]json.RawMessage{
		"labels": json.RawMessage(`{"a":"9","b":null,"c":"3"}`), "description": json.RawMessage(`""`),
	})
	if !ok {
		t.Fatal("not patched")
	}
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	labels, _ := json.Marshal(out["labels"])
	if string(labels) != `{"a":"9","c":"3","keep":"k"}` {
		t.Errorf("labels %s", labels)
	}
	if _, ok := out["description"]; ok {
		t.Errorf("the description was kept: %v", out["description"])
	}
	if out["schema"] == nil {
		t.Error("the schema was dropped")
	}
	b, _ = patchTable(current, map[string]json.RawMessage{"labels": json.RawMessage(`{"a":null,"b":null,"keep":null}`)})
	out = nil
	_ = json.Unmarshal(b, &out)
	if _, ok := out["labels"]; ok || out["description"] != "old" {
		t.Errorf("every label removed: %v", out)
	}
}

func TestSchemaChange(t *testing.T) {
	have := []field{{Name: "id", Type: "INTEGER", Mode: "REQUIRED"}, {Name: "r", Type: "RECORD", Fields: []field{{Name: "x", Type: "STRING"}}},
		{Name: "tags", Type: "STRING", Mode: "REPEATED"}}
	const refused = "Provided Schema does not match Table p:ds.t. "
	for _, c := range []struct {
		name  string
		want  []field
		code  int
		msg   string
		added int
	}{
		{"same, by type aliases", []field{{Name: "id", Type: "INT64", Mode: "REQUIRED"}, {Name: "r", Type: "STRUCT", Fields: []field{{Name: "x", Type: "STRING"}}},
			{Name: "tags", Type: "STRING", Mode: "REPEATED"}}, 0, "", 0},
		{"relaxed and a NULLABLE added", []field{{Name: "id", Type: "INTEGER"}, have[1], have[2], {Name: "n", Type: "STRING"}}, 0, "", 1},
		{"dropped", []field{have[0], have[1]}, 400, refused + "Field tags is missing in new schema", 0},
		{"retyped", []field{{Name: "id", Type: "STRING", Mode: "REQUIRED"}, have[1], have[2]}, 400, refused + "Field id has changed type from INTEGER to STRING", 0},
		{"moded", []field{have[0], have[1], {Name: "tags", Type: "STRING"}}, 400, refused + "Field tags has changed mode from REPEATED to NULLABLE", 0},
		{"required added", []field{have[0], have[1], have[2], {Name: "m", Type: "STRING", Mode: "REQUIRED"}}, 400,
			refused + "Cannot add required fields to an existing schema. (field: m)", 0},
		// Refused before the 501 for a new column before the table's own.
		{"required added first", []field{{Name: "m", Type: "STRING", Mode: "REQUIRED"}, have[0], have[1], have[2]}, 400,
			refused + "Cannot add required fields to an existing schema. (field: m)", 0},
		// Refused before the 501 for a field added to a RECORD.
		{"required added to a RECORD", []field{have[0], {Name: "r", Type: "RECORD", Fields: []field{{Name: "x", Type: "STRING"}, {Name: "y", Type: "STRING", Mode: "REQUIRED"}}}, have[2]},
			400, refused + "Cannot add required fields to an existing schema. (field: r.y)", 0},
		{"nested dropped", []field{have[0], {Name: "r", Type: "RECORD"}, have[2]}, 400, refused + "Field r.x is missing in new schema", 0},
		{"renamed in another case", []field{{Name: "ID", Type: "INTEGER", Mode: "REQUIRED"}, have[1], have[2]}, 501, "renames the column id to ID", 0},
		{"a NULLABLE added to a RECORD", []field{have[0], {Name: "r", Type: "RECORD", Fields: []field{{Name: "x", Type: "STRING"}, {Name: "y", Type: "STRING"}}}, have[2]},
			501, "adds a field to the RECORD r", 0},
	} {
		added, code, msg := schemaChange("p:ds.t", have, c.want)
		if code != c.code || len(added) != c.added || (c.code == 400 && msg != c.msg) || !strings.Contains(msg, c.msg) {
			t.Errorf("%s: %d %q, %d added; want %d %q, %d added", c.name, code, msg, len(added), c.code, c.msg, c.added)
		}
	}
}

// viewEmulator keeps one view, whose query it rewrites as the emulator
// does.
type viewEmulator struct{ query string }

func (e *viewEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if e.query == "" {
			http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
			return
		}
		b, _ := json.Marshal(map[string]any{"type": "VIEW", "view": map[string]any{"query": e.query}})
		_, _ = w.Write(b)
		return
	}
	var body struct {
		Query string `json:"query"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if strings.HasPrefix(body.Query, "CREATE VIEW") {
		e.query = "SELECT `a#1` AS `a` FROM (rewritten)"
	}
	_, _ = io.WriteString(w, `{"jobReference":{"jobId":"q"},"schema":{"fields":[{"name":"a","type":"INTEGER"}]}}`)
}

func TestCreateViewReadsBackAsWritten(t *testing.T) {
	emu := &viewEmulator{}
	h := Wrap(emu)
	if code, got := do(t, h, "POST", base+"/queries", `{"query":"CREATE VIEW ds.v AS  SELECT 1 AS a ","useLegacySql":false}`); code != 200 {
		t.Fatalf("CREATE VIEW: %d %v", code, got)
	}
	_, got := do(t, h, "GET", base+"/datasets/ds/tables/v", "")
	if q := got["view"].(map[string]any)["query"]; q != "SELECT 1 AS a" {
		t.Errorf("tables.get gives %q", q)
	}
	// Changed since, by other means: the emulator's text stands.
	emu.query = "SELECT 2 AS a"
	_, got = do(t, h, "GET", base+"/datasets/ds/tables/v", "")
	if q := got["view"].(map[string]any)["query"]; q != "SELECT 2 AS a" {
		t.Errorf("tables.get of a view changed since gives %q", q)
	}
}
