//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryStatementsInTempScripts (#1459): statements the emulator
// cannot run inside a multi-statement script with a TEMP table, through
// jobs.query and jobs.insert. BigQuery keeps a TEMP table for the script
// (https://cloud.google.com/bigquery/docs/multi-statement-queries#temporary_tables)
// and runs each statement as a child job. Read back: an ALTER TABLE beside
// a TEMP table adds its column; a CREATE OR REPLACE TEMP TABLE and a DROP
// TABLE of the script's TEMP table run; a TEMP table named inside an
// EXECUTE IMMEDIATE string and in an IF is the script's; each statement
// is a child job (but the ALTER TABLE, which the front carries out
// itself); and the TEMP table is gone once the script ends. A
// runtime error in a query before the last statement fails the script,
// keeping the row an INSERT before it wrote, and an ASSERT FALSE fails it
// with its description; @@row_count is the INSERT's count. Measured first: each such script was 501
// notImplemented, nothing run.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.list, bigquery.tables.get
func TestBigQueryStatementsInTempScripts(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	path := func(name string) string { return "`" + ds.DatasetID + "." + name + "`" }
	for _, insert := range []bool{false, true} {
		base := fmt.Sprintf("talter%v", insert)
		script := "CREATE TEMP TABLE staged AS SELECT 1 AS v UNION ALL SELECT 2;\n" +
			"CREATE TABLE " + path(base) + " AS SELECT v FROM staged;\n" +
			"ALTER TABLE " + path(base) + " ADD COLUMN note STRING;\n" +
			"CREATE OR REPLACE TEMP TABLE staged AS SELECT 10 AS v;\n" +
			"INSERT INTO " + path(base) + " (v, note) SELECT v, 'replaced' FROM staged;\n" +
			"IF (SELECT COUNT(*) FROM staged) = 1 THEN\n" +
			"  EXECUTE IMMEDIATE 'INSERT INTO staged (v) VALUES (20)';\n" +
			"END IF;\n" +
			"SELECT (SELECT SUM(v) FROM staged) AS s, (SELECT COUNT(*) FROM " + path(base) + " WHERE note = 'replaced') AS n"
		job, it := runScriptJob(t, c, script, insert)
		var out []bigquery.Value
		if err := it.Next(&out); err != nil || fmt.Sprint(out) != "[30 1]" {
			t.Errorf("insert %v: the script's rows %v (%v), want [30 1]", insert, out, err)
		}
		md, err := ds.Table(base).Metadata(ctx)
		if err != nil || len(md.Schema) != 2 || md.Schema[1].Name != "note" {
			t.Errorf("insert %v: the ALTER TABLE's table: %v %+v", insert, err, md)
		}
		types, texts := childTypes(t, job)
		if !types["INSERT"] || !strings.Contains(texts, "INSERT INTO staged (v) VALUES (20)") ||
			!strings.Contains(texts, "CREATE OR REPLACE TEMP TABLE staged AS SELECT 10 AS v") || strings.Contains(texts, "_cloudburrow") {
			t.Errorf("insert %v: the child jobs: %v\n%s", insert, types, texts)
		}
		if err := bqRun(ctx, c, "SELECT * FROM staged", insert); err == nil {
			t.Errorf("insert %v: the TEMP table outlived its script", insert)
		}

		// DROP TABLE of the TEMP table, and a new one of its name.
		_, it = runScriptJob(t, c, "CREATE TEMP TABLE x AS SELECT 1 AS a;\nDROP TABLE x;\n"+
			"CREATE TEMP TABLE x AS SELECT 3 AS a;\nSELECT a FROM x", insert)
		if err := it.Next(&out); err != nil || fmt.Sprint(out) != "[3]" {
			t.Errorf("insert %v: after DROP TABLE of the TEMP table: %v (%v), want [3]", insert, out, err)
		}

		// @@row_count beside a TEMP table.
		_, it = runScriptJob(t, c, "CREATE TEMP TABLE r (a INT64);\nINSERT INTO r VALUES (1), (2), (3);\nSELECT @@row_count AS n", insert)
		if err := it.Next(&out); err != nil || fmt.Sprint(out) != "[3]" {
			t.Errorf("insert %v: @@row_count in a TEMP script: %v (%v), want [3]", insert, out, err)
		}

		// A runtime error before the last statement, and an ASSERT.
		kept := fmt.Sprintf("tkept%v", insert)
		if err := bqRun(ctx, c, "CREATE TABLE "+path(kept)+" (v INT64)", false); err != nil {
			t.Fatal(err)
		}
		err = bqRun(ctx, c, "CREATE TEMP TABLE e AS SELECT 0 AS z;\nINSERT INTO "+path(kept)+" VALUES (5);\n"+
			"SELECT 1/z FROM e;\nINSERT INTO "+path(kept)+" VALUES (6)", insert)
		wantHTTPStatus(t, "a runtime error before the last statement", err, http.StatusBadRequest)
		err = bqRun(ctx, c, "CREATE TEMP TABLE e AS SELECT 0 AS z;\nINSERT INTO "+path(kept)+" VALUES (7);\n"+
			"ASSERT (SELECT COUNT(*) FROM e) = 2 AS 'two rows expected';\nINSERT INTO "+path(kept)+" VALUES (8)", insert)
		if err == nil || !strings.Contains(err.Error(), "two rows expected") {
			t.Errorf("insert %v: ASSERT FALSE in a TEMP script: %v", insert, err)
		}
		it, err = c.Query("SELECT ARRAY_AGG(v ORDER BY v) FROM " + path(kept)).Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := it.Next(&out); err != nil || fmt.Sprint(out) != "[[5 7]]" {
			t.Errorf("insert %v: the rows kept: %v (%v), want [[5 7]]", insert, out, err)
		}
	}
}

// TestBigQueryStatementsLaterInAScript (#1459): statements the emulator
// runs alone but not later in a multi-statement script, carried out by the
// front a statement at a time, each a child job, through jobs.query and
// jobs.insert: CREATE OR REPLACE TABLE of a table that exists, CREATE
// TABLE ... LIKE and COPY, and MERGE from a subquery. Measured first: each
// script was 501 notImplemented, nothing run.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.list, bigquery.tables.get
func TestBigQueryStatementsLaterInAScript(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	path := func(name string) string { return "`" + ds.DatasetID + "." + name + "`" }
	for _, insert := range []bool{false, true} {
		src, like, cp := fmt.Sprintf("tsrc%v", insert), fmt.Sprintf("tlike%v", insert), fmt.Sprintf("tcopy%v", insert)
		script := "CREATE TABLE " + path(src) + " (k INT64, v STRING);\n" +
			"INSERT INTO " + path(src) + " VALUES (1, 'a');\n" +
			"CREATE OR REPLACE TABLE " + path(src) + " AS SELECT 1 AS k, 'b' AS v UNION ALL SELECT 2, 'c';\n" +
			"CREATE TABLE " + path(like) + " LIKE " + path(src) + ";\n" +
			"CREATE TABLE " + path(cp) + " COPY " + path(src) + ";\n" +
			"MERGE " + path(cp) + " T USING (SELECT k, CONCAT(v, '!') AS v FROM " + path(src) + " WHERE k = 2) S ON T.k = S.k\n" +
			"  WHEN MATCHED THEN UPDATE SET v = S.v;\n" +
			"SELECT (SELECT STRING_AGG(v ORDER BY k) FROM " + path(cp) + ") AS c, (SELECT COUNT(*) FROM " + path(like) + ") AS n"
		job, it := runScriptJob(t, c, script, insert)
		var out []bigquery.Value
		if err := it.Next(&out); err != nil || fmt.Sprint(out) != "[b,c! 0]" {
			t.Errorf("insert %v: the script's rows %v (%v), want [b,c! 0]", insert, out, err)
		}
		md, err := ds.Table(like).Metadata(ctx)
		if err != nil || len(md.Schema) != 2 || md.Schema[0].Name != "k" || md.Schema[1].Name != "v" {
			t.Errorf("insert %v: the LIKE table: %v %+v", insert, err, md)
		}
		types, texts := childTypes(t, job)
		if !types["INSERT"] || !strings.Contains(texts, "CREATE OR REPLACE TABLE "+path(src)) || !strings.Contains(texts, "MERGE "+path(cp)) {
			t.Errorf("insert %v: the child jobs: %v\n%s", insert, types, texts)
		}
	}
}

// TestBigQueryEveryScriptInTurn (#1459): a multi-statement script with no
// TEMP table or function, no control flow and no query before its last
// statement, which the emulator ran whole, is carried out by the front a
// statement at a time, as BigQuery runs a script: "each statement in the
// script creates a child job"
// (https://cloud.google.com/bigquery/docs/multi-statement-queries), and a
// failing statement fails the script, keeping what the statements before it
// did. Read back, through jobs.query and jobs.insert: the parent's
// statementType SCRIPT and its child jobs, one per statement; a script
// failing after an INSERT keeps the row and does not run the INSERT after
// it; CREATE SCHEMA of a dataset that exists later in a script fails it
// "Already Exists", keeping the table made before; CREATE FUNCTION of one
// that exists fails it, and DROP FUNCTION then CREATE FUNCTION of its name
// replaces it; DROP SCHEMA ... CASCADE of a dataset an earlier statement of
// the script made a table in drops it. Measured first: the parent had
// numChildJobs 0; the failed script through jobs.query, DROP FUNCTION in a
// script and the DROP SCHEMA were 501; the CREATE SCHEMA and CREATE
// FUNCTION were 501.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.get, bigquery.jobs.list, bigquery.tables.get, bigquery.datasets.get
func TestBigQueryEveryScriptInTurn(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	path := func(name string) string { return "`" + ds.DatasetID + "." + name + "`" }
	for _, insert := range []bool{false, true} {
		// A plain script: its children.
		plain := fmt.Sprintf("tplain%v", insert)
		job, it := runScriptJob(t, c, "CREATE TABLE "+path(plain)+" (a INT64);\nINSERT INTO "+path(plain)+" VALUES (1), (2);\n"+
			"SELECT COUNT(*) AS n FROM "+path(plain), insert)
		var out []bigquery.Value
		if err := it.Next(&out); err != nil || fmt.Sprint(out) != "[2]" {
			t.Errorf("insert %v: the script's rows %v (%v), want [2]", insert, out, err)
		}
		types, texts := childTypes(t, job)
		if !types["INSERT"] || !strings.Contains(texts, "CREATE TABLE "+path(plain)) || !strings.Contains(texts, "SELECT COUNT(*) AS n FROM") {
			t.Errorf("insert %v: the child jobs: %v\n%s", insert, types, texts)
		}
		if parent, err := c.JobFromID(ctx, job.ID()); err != nil {
			t.Errorf("insert %v: jobs.get of the script: %v", insert, err)
		} else if st := parent.LastStatus(); st == nil || st.Statistics == nil || st.Statistics.NumChildJobs < 3 {
			t.Errorf("insert %v: the script's job has %+v, want 3 children", insert, st)
		} else if qs, ok := st.Statistics.Details.(*bigquery.QueryStatistics); !ok || qs.StatementType != "SCRIPT" {
			t.Errorf("insert %v: the script's statementType %+v, want SCRIPT", insert, st.Statistics.Details)
		}

		// A failure after an INSERT keeps its row.
		kept := fmt.Sprintf("tkeep%v", insert)
		if err := bqRun(ctx, c, "CREATE TABLE "+path(kept)+" (v INT64)", false); err != nil {
			t.Fatal(err)
		}
		err := bqRun(ctx, c, "INSERT INTO "+path(kept)+" VALUES (5);\nINSERT INTO "+path("nope")+" VALUES (1);\n"+
			"INSERT INTO "+path(kept)+" VALUES (6)", insert)
		if err == nil || !strings.Contains(err.Error(), "nope") {
			t.Errorf("insert %v: a script failing at its second statement: %v", insert, err)
		}
		it, err = c.Query("SELECT ARRAY_AGG(v ORDER BY v) FROM " + path(kept)).Read(ctx)
		if err == nil {
			err = it.Next(&out)
		}
		if err != nil || fmt.Sprint(out) != "[[5]]" {
			t.Errorf("insert %v: the rows kept: %v (%v), want [[5]]", insert, out, err)
		}

		// CREATE SCHEMA of a dataset that exists, later in a script.
		made := fmt.Sprintf("tmade%v", insert)
		err = bqRun(ctx, c, "CREATE TABLE "+path(made)+" (a INT64);\nCREATE SCHEMA `"+ds.DatasetID+"`", insert)
		if err == nil || !strings.Contains(err.Error(), "Already Exists") {
			t.Errorf("insert %v: CREATE SCHEMA of an existing dataset in a script: %v, want Already Exists", insert, err)
		}
		if _, err := ds.Table(made).Metadata(ctx); err != nil {
			t.Errorf("insert %v: the table made before the failing CREATE SCHEMA: %v", insert, err)
		}

		// CREATE FUNCTION of one that exists; DROP FUNCTION and a new one.
		fn := path(fmt.Sprintf("fn%v", insert))
		if err := bqRun(ctx, c, "CREATE FUNCTION "+fn+"(x INT64) AS (x + 1)", false); err != nil {
			t.Fatal(err)
		}
		err = bqRun(ctx, c, "CREATE TABLE "+path(made+"2")+" (a INT64);\nCREATE FUNCTION "+fn+"(x INT64) AS (x + 5)", insert)
		if err == nil || !strings.Contains(err.Error(), "Already Exists") {
			t.Errorf("insert %v: CREATE FUNCTION of an existing function in a script: %v, want Already Exists", insert, err)
		}
		if err := bqRun(ctx, c, "DROP FUNCTION "+fn+";\nCREATE FUNCTION "+fn+"(x INT64) AS (x + 2)", insert); err != nil {
			t.Errorf("insert %v: DROP FUNCTION and CREATE FUNCTION in a script: %v", insert, err)
		}
		it, err = c.Query("SELECT " + fn + "(1)").Read(ctx)
		if err == nil {
			err = it.Next(&out)
		}
		if err != nil || fmt.Sprint(out) != "[3]" {
			t.Errorf("insert %v: the function after the script: %v (%v), want [3]", insert, out, err)
		}

		// DROP SCHEMA of a dataset the script made a table in.
		other := fmt.Sprintf("%s_s%v", ds.DatasetID, insert)
		if err := bqRun(ctx, c, "CREATE SCHEMA `"+other+"`;\nCREATE TABLE `"+other+".t` (a INT64);\nDROP SCHEMA `"+other+"` CASCADE", insert); err != nil {
			t.Errorf("insert %v: a script that drops the dataset it made: %v", insert, err)
		}
		if _, err := c.Dataset(other).Metadata(ctx); err == nil {
			t.Errorf("insert %v: dataset %s outlived its DROP SCHEMA", insert, other)
			_ = c.Dataset(other).DeleteWithContents(ctx)
		}
	}
}
