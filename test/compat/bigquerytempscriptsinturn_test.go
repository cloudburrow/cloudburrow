//go:build compat

package compat

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// TestBigQueryTempScriptsInTurn (#1459): a multi-statement script with a
// TEMP table or function and nothing else the front carries out in its
// turn, which the emulator ran whole, is carried out a statement at a
// time, as BigQuery runs every script: the TEMP table and function last
// for the script
// (https://cloud.google.com/bigquery/docs/multi-statement-queries#temporary_tables),
// and "each statement in the script creates a child job". Read back,
// through jobs.query and jobs.insert: a child job per statement, the last
// INSERT's too, each with its own statementType (JobStatistics2), and a
// SELECT child's destination table; a script that fails after an INSERT
// keeps its row, through jobs.query too; CREATE TEMP TABLE ... AS SELECT
// after an INSERT reads the inserted row; and a dry run of a TEMP script
// succeeds, statementType SCRIPT, and one naming a column the TEMP table
// lacks fails 400; CREATE SCHEMA of a dataset that exists and CREATE
// FUNCTION of a function that exists, later in a TEMP script, fail it
// "Already Exists", the statements before kept; DROP FUNCTION then CREATE
// FUNCTION of its name replaces it; DROP SCHEMA ... CASCADE of a dataset
// the script made a table in drops it. Measured first: the parent had no
// child jobs, the child jobs' statementType was SELECT for each DDL
// statement, the dry run answered statementType SELECT and an unknown
// column as a job done with an errorResult, and the failed script through
// jobs.query, the CREATE SCHEMA, CREATE FUNCTION, DROP FUNCTION and DROP
// SCHEMA were 501.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.list, bigquery.jobs.get
func TestBigQueryTempScriptsInTurn(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	path := func(name string) string { return "`" + ds.DatasetID + "." + name + "`" }
	for _, insert := range []bool{false, true} {
		dst := fmt.Sprintf("ttemp%v", insert)
		if err := bqRun(ctx, c, "CREATE TABLE "+path(dst)+" (v INT64)", false); err != nil {
			t.Fatal(err)
		}
		// Children, each with its statementType; the last DML its own.
		script := "CREATE TEMP FUNCTION twice(x INT64) AS (x * 2);\n" +
			"CREATE TEMP TABLE staged (v INT64);\n" +
			"INSERT INTO staged VALUES (1), (2);\n" +
			"SELECT twice(v) AS w FROM staged;\n" +
			"INSERT INTO " + path(dst) + " SELECT twice(v) FROM staged"
		job, _ := runScriptJob(t, c, script, insert)
		kids := job.Children(ctx)
		var types []string
		selectDst := false
		lastInsert := false
		for {
			k, err := kids.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			qs, _ := k.LastStatus().Statistics.Details.(*bigquery.QueryStatistics)
			if qs == nil {
				continue
			}
			types = append(types, qs.StatementType)
			cfg, err := k.Config()
			if err != nil {
				t.Fatal(err)
			}
			qc, _ := cfg.(*bigquery.QueryConfig)
			if qc == nil {
				continue
			}
			if strings.Contains(qc.Q, "_cloudburrow") {
				t.Errorf("insert %v: a child's text names the hidden table: %s", insert, qc.Q)
			}
			if qs.StatementType == "SELECT" && qc.Dst != nil && qc.Dst.TableID != "" {
				selectDst = true
			}
			if qs.StatementType == "INSERT" && strings.Contains(qc.Q, path(dst)) {
				lastInsert = true
			}
		}
		got := strings.Join(types, ",")
		for _, want := range []string{"CREATE_FUNCTION", "CREATE_TABLE", "INSERT", "SELECT"} {
			if !strings.Contains(","+got+",", ","+want+",") {
				t.Errorf("insert %v: the child jobs' statementTypes %s, want %s among them", insert, got, want)
			}
		}
		if len(types) != 5 || !lastInsert {
			t.Errorf("insert %v: %d child jobs (%s), last INSERT its own: %v; want 5", insert, len(types), got, lastInsert)
		}
		if !selectDst {
			t.Errorf("insert %v: the SELECT child names no destination table", insert)
		}
		it, err := c.Query("SELECT ARRAY_AGG(v ORDER BY v) FROM " + path(dst)).Read(ctx)
		var out []bigquery.Value
		if err == nil {
			err = it.Next(&out)
		}
		if err != nil || fmt.Sprint(out) != "[[2 4]]" {
			t.Errorf("insert %v: the rows the last INSERT wrote: %v (%v), want [[2 4]]", insert, out, err)
		}
		if err := bqRun(ctx, c, "SELECT * FROM staged", insert); err == nil {
			t.Errorf("insert %v: the TEMP table outlived its script", insert)
		}

		// A failure after an INSERT keeps the row.
		kept := fmt.Sprintf("tkeept%v", insert)
		if err := bqRun(ctx, c, "CREATE TABLE "+path(kept)+" (v INT64)", false); err != nil {
			t.Fatal(err)
		}
		err = bqRun(ctx, c, "CREATE TEMP TABLE s AS SELECT 5 AS v;\nINSERT INTO "+path(kept)+" SELECT v FROM s;\n"+
			"INSERT INTO "+path("nope")+" VALUES (1);\nINSERT INTO "+path(kept)+" VALUES (6)", insert)
		if err == nil || !strings.Contains(err.Error(), "nope") {
			t.Errorf("insert %v: a TEMP script failing at its third statement: %v", insert, err)
		}
		it, err = c.Query("SELECT ARRAY_AGG(v ORDER BY v) FROM " + path(kept)).Read(ctx)
		if err == nil {
			err = it.Next(&out)
		}
		if err != nil || fmt.Sprint(out) != "[[5]]" {
			t.Errorf("insert %v: the rows kept: %v (%v), want [[5]]", insert, out, err)
		}

		// Later in a TEMP script: CREATE SCHEMA of a dataset that exists,
		// CREATE FUNCTION of one that exists, DROP FUNCTION, DROP SCHEMA.
		made := fmt.Sprintf("tmadet%v", insert)
		err = bqRun(ctx, c, "CREATE TEMP FUNCTION tf() AS (1);\nCREATE TABLE "+path(made)+" (a INT64);\nCREATE SCHEMA `"+ds.DatasetID+"`", insert)
		if err == nil || !strings.Contains(err.Error(), "Already Exists") {
			t.Errorf("insert %v: CREATE SCHEMA of an existing dataset in a TEMP script: %v, want Already Exists", insert, err)
		}
		if _, err := ds.Table(made).Metadata(ctx); err != nil {
			t.Errorf("insert %v: the table made before the failing CREATE SCHEMA: %v", insert, err)
		}
		fn := path(fmt.Sprintf("fnt%v", insert))
		if err := bqRun(ctx, c, "CREATE FUNCTION "+fn+"(x INT64) AS (x + 1)", false); err != nil {
			t.Fatal(err)
		}
		err = bqRun(ctx, c, "CREATE TEMP FUNCTION tf() AS (1);\nCREATE FUNCTION "+fn+"(x INT64) AS (x + tf())", insert)
		if err == nil || !strings.Contains(err.Error(), "Already Exists") {
			t.Errorf("insert %v: CREATE FUNCTION of an existing function in a TEMP script: %v, want Already Exists", insert, err)
		}
		if err := bqRun(ctx, c, "CREATE TEMP FUNCTION tf() AS (1);\nDROP FUNCTION "+fn+";\nCREATE FUNCTION "+fn+"(x INT64) AS (x + 2)", insert); err != nil {
			t.Errorf("insert %v: DROP FUNCTION and CREATE FUNCTION in a TEMP script: %v", insert, err)
		}
		it, err = c.Query("SELECT " + fn + "(1)").Read(ctx)
		if err == nil {
			err = it.Next(&out)
		}
		if err != nil || fmt.Sprint(out) != "[3]" {
			t.Errorf("insert %v: the function after the TEMP script: %v (%v), want [3]", insert, out, err)
		}
		other := fmt.Sprintf("%s_t%v", ds.DatasetID, insert)
		if err := bqRun(ctx, c, "CREATE TEMP TABLE z AS SELECT 1 AS a;\nCREATE SCHEMA `"+other+"`;\n"+
			"CREATE TABLE `"+other+".t` AS SELECT a FROM z;\nDROP SCHEMA `"+other+"` CASCADE", insert); err != nil {
			t.Errorf("insert %v: a TEMP script that drops the dataset it made: %v", insert, err)
		}
		if _, err := c.Dataset(other).Metadata(ctx); err == nil {
			t.Errorf("insert %v: dataset %s outlived its DROP SCHEMA", insert, other)
			_ = c.Dataset(other).DeleteWithContents(ctx)
		}

		// CREATE TEMP TABLE ... AS SELECT after a statement that changes data.
		_, it = runScriptJob(t, c, "INSERT INTO "+path(kept)+" VALUES (7);\n"+
			"CREATE TEMP TABLE seen AS SELECT v FROM "+path(kept)+" WHERE v > 5;\nSELECT ARRAY_AGG(v) AS a FROM seen", insert)
		if err := it.Next(&out); err != nil || fmt.Sprint(out) != "[[7]]" {
			t.Errorf("insert %v: CREATE TEMP TABLE AS SELECT after an INSERT: %v (%v), want [[7]]", insert, out, err)
		}
	}

	// A dry run of a TEMP script.
	q := c.Query("CREATE TEMP TABLE d AS SELECT 1 AS a;\nSELECT a FROM d")
	q.DryRun = true
	job, err := q.Run(ctx)
	if err != nil {
		t.Fatalf("a dry run of a TEMP script: %v", err)
	}
	st := job.LastStatus()
	if st == nil || st.Err() != nil || st.Statistics == nil {
		t.Fatalf("a dry run of a TEMP script: %+v", st)
	}
	if qs, ok := st.Statistics.Details.(*bigquery.QueryStatistics); !ok || qs.StatementType != "SCRIPT" {
		t.Errorf("a dry run of a TEMP script: statistics %+v, want statementType SCRIPT", st.Statistics.Details)
	}
	q = c.Query("CREATE TEMP TABLE d AS SELECT 1 AS a;\nSELECT nope FROM d")
	q.DryRun = true
	if _, err := q.Run(ctx); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("a dry run of a TEMP script naming a column it lacks: %v, want an error naming it", err)
	}
}
