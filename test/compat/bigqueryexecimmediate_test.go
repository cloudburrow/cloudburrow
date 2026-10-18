//go:build compat

package compat

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// TestBigQueryExecuteImmediateForms (#1037): every form of EXECUTE
// IMMEDIATE sql_expression [INTO variable[, ...]] [USING identifier[, ...]]
// that BigQuery documents
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/procedural-language#execute_immediate),
// through jobs.query and as a query job: SQL given by a literal, a ||
// concatenation, FORMAT() and a variable; INTO one and several variables
// of the one row, NULL with no row; USING positional (?) and named
// (@name) values, with AS names, and the same variable in INTO and USING;
// DDL and DML inside one; and @@row_count after DML, run directly or by
// EXECUTE IMMEDIATE, read by a query, a SET and an IF. Measured first
// (#1011): the emulator ran nothing for any EXECUTE IMMEDIATE.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryExecuteImmediateForms(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	d := ds.DatasetID
	for _, insert := range []bool{false, true} {
		suffix := "q"
		if insert {
			suffix = "j"
		}
		read := func(sql string) []string {
			t.Helper()
			q := c.Query(sql)
			var it *bigquery.RowIterator
			var err error
			if insert {
				var job *bigquery.Job
				if job, err = q.Run(ctx); err == nil {
					it, err = job.Read(ctx)
				}
			} else {
				it, err = q.Read(ctx)
			}
			if err != nil {
				t.Fatalf("%s (query job %v): %v", sql, insert, err)
			}
			return readRows(t, it, sql)
		}
		tbl := "`" + d + ".forms_" + suffix + "`"
		for _, tc := range []struct {
			sql  string
			want []string
		}{
			// The SQL expression.
			{"EXECUTE IMMEDIATE 'SELECT 1 AS a'", []string{"1"}},
			{"DECLARE w STRING DEFAULT 'cat'; EXECUTE IMMEDIATE 'SELECT \"' || w || '\" AS v'", []string{"cat"}},
			{"DECLARE n INT64 DEFAULT 6; EXECUTE IMMEDIATE FORMAT('SELECT %d * 7 AS v', n)", []string{"42"}},
			{"DECLARE s STRING; SET s = 'SELECT 8 AS v'; EXECUTE IMMEDIATE s", []string{"8"}},
			// INTO.
			{"DECLARE x INT64; EXECUTE IMMEDIATE 'SELECT 3' INTO x; SELECT x", []string{"3"}},
			{"DECLARE a INT64; DECLARE b STRING; EXECUTE IMMEDIATE 'SELECT 4, \"four\"' INTO a, b; SELECT b, a", []string{"four|4"}},
			{"DECLARE a INT64 DEFAULT 1; EXECUTE IMMEDIATE 'SELECT n FROM UNNEST([7]) n WHERE FALSE' INTO a; SELECT a IS NULL", []string{"true"}},
			// USING: positional, named, AS names, the same variable INTO.
			{"EXECUTE IMMEDIATE 'SELECT ? + ?, ?' USING 1, 2, 'x'", []string{"3|x"}},
			{"DECLARE y INT64 DEFAULT 5; EXECUTE IMMEDIATE 'SELECT @y * @k' USING y AS y, 10 AS k", []string{"50"}},
			{"DECLARE y INT64 DEFAULT 5; EXECUTE IMMEDIATE 'SELECT @v + 1' INTO y USING y AS v; SELECT y", []string{"6"}},
			// DDL and DML inside, and @@row_count after DML.
			{"EXECUTE IMMEDIATE 'CREATE TABLE " + tbl + " (a INT64)'; " +
				"EXECUTE IMMEDIATE 'INSERT INTO " + tbl + " (a) VALUES (?), (?), (?)' USING 1, 2, 3; SELECT @@row_count", []string{"3"}},
			{"EXECUTE IMMEDIATE 'UPDATE " + tbl + " SET a = a + 10 WHERE a > @m' USING 1 AS m; SELECT @@row_count", []string{"2"}},
			{"INSERT INTO " + tbl + " (a) VALUES (99); SELECT @@row_count", []string{"1"}},
			{"DECLARE c INT64; DELETE FROM " + tbl + " WHERE a > 10 AND a < 50; SET c = @@row_count; SELECT c", []string{"2"}},
			{"DELETE FROM " + tbl + " WHERE a = 99; IF @@row_count = 1 THEN SELECT 'one'; ELSE SELECT 'other'; END IF", []string{"one"}},
			{"SELECT @@row_count IS NULL", []string{"true"}},
			{"SELECT a FROM " + tbl, []string{"1"}},
		} {
			if got := read(tc.sql); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s (query job %v): %q, want %q", tc.sql, insert, got, tc.want)
			}
		}
		// #1459: beside a TEMP table too, the script run as its
		// session's text (tempsession.go), a statement at a time.
		if got := read("CREATE TEMP TABLE rc (a INT64); INSERT rc VALUES (1); SELECT @@row_count"); !reflect.DeepEqual(got, []string{"1"}) {
			t.Errorf("@@row_count beside a TEMP table (query job %v): %q, want [1]", insert, got)
		}
	}
}

// TestBigQueryExecuteImmediateErrors (#1037): what BigQuery documents as
// an error fails the query, 400 invalidQuery through jobs.query and a
// failed job as a query job, and the statements after it do not run:
// INTO of a query of more than one row ("an error is thrown if you
// attempt to return more than one row"), INTO a number of variables other
// than the query's columns (they are "assigned by position"), and an
// EXECUTE IMMEDIATE, IF, LOOP or CALL inside one ("restricted from being
// executed dynamically as a nested element"). BigQuery's wording of each
// error is UNVERIFIED.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/procedural-language
func TestBigQueryExecuteImmediateErrors(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	d := ds.DatasetID
	for _, insert := range []bool{false, true} {
		for i, c2 := range []struct{ sql, word string }{
			{"DECLARE x INT64; EXECUTE IMMEDIATE 'SELECT n FROM UNNEST([1, 2]) n' INTO x", "more than one row"},
			{"DECLARE a, b INT64; EXECUTE IMMEDIATE 'SELECT 1' INTO a, b", "2 variables"},
			{"DECLARE a INT64; EXECUTE IMMEDIATE 'SELECT 1, 2' INTO a", "1 variables"},
			{"EXECUTE IMMEDIATE \"EXECUTE IMMEDIATE 'SELECT 1'\"", "EXECUTE IMMEDIATE is restricted"},
			{"EXECUTE IMMEDIATE 'IF TRUE THEN SELECT 1; END IF'", "IF is restricted"},
			{"EXECUTE IMMEDIATE 'LOOP LEAVE; END LOOP'", "LOOP is restricted"},
			{"EXECUTE IMMEDIATE 'CALL " + d + ".p()'", "CALL is restricted"},
		} {
			after := "after_" + string(rune('a'+i))
			if insert {
				after += "_j"
			}
			sql := c2.sql + "; CREATE TABLE " + d + "." + after + " (x INT64)"
			err := runIn(ctx, c, project, "", sql, insert)
			if !insert {
				if code, _ := apiError(err); code != http.StatusBadRequest || !strings.Contains(err.Error(), c2.word) {
					t.Errorf("%s: %v, want 400 with %q", sql, err, c2.word)
				}
			} else if errReason(err) != "invalidQuery" || !strings.Contains(err.Error(), c2.word) {
				t.Errorf("%s (query job): %v, want invalidQuery with %q", sql, err, c2.word)
			}
			if _, err := ds.Table(after).Metadata(ctx); err == nil {
				t.Errorf("%s (query job %v): the statement after the error ran", sql, insert)
			}
		}
	}
}

// TestBigQueryExecuteImmediateChildJobs (#1037): each statement EXECUTE
// IMMEDIATE runs is a child job of the script's, as BigQuery runs one:
// a STATEMENT child whose own query is the statement run, with its USING
// values bound, and whose stack frame is the EXECUTE IMMEDIATE's place in
// the script (BigQuery's frame of such a statement UNVERIFIED); the SQL
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/reference/rest/v2/Job
// expression and each USING value are EXPRESSION children; the DML
// child's numDmlAffectedRows is @@row_count, which the script's last
// query reads. A lone EXECUTE IMMEDIATE is a SCRIPT job with one child.
//
// covers: bigquery.jobs.list, bigquery.jobs.get, bigquery.jobs.insert
func TestBigQueryExecuteImmediateChildJobs(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	tbl := "`" + ds.DatasetID + ".kids1037`"
	script := "DECLARE n INT64 DEFAULT 2;\n" +
		"EXECUTE IMMEDIATE FORMAT('CREATE TABLE %s (a INT64)', '" + tbl + "');\n" +
		"EXECUTE IMMEDIATE 'INSERT INTO " + tbl + " (a) VALUES (?), (?)' USING 1, n;\n" +
		"SELECT @@row_count AS c"
	job, err := c.Query(script).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status, err := job.Wait(ctx); err != nil || status.Err() != nil {
		t.Fatal(err, status.Err())
	}
	it, err := job.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := readRows(t, it, script); !reflect.DeepEqual(got, []string{"2"}) {
		t.Errorf("the script's rows: %q, want [2]", got)
	}
	children := func(job *bigquery.Job) []*bigquery.Job {
		var kids []*bigquery.Job
		cit := job.Children(ctx)
		for {
			k, err := cit.Next()
			if errors.Is(err, iterator.Done) {
				return kids
			}
			if err != nil {
				t.Fatal(err)
			}
			kids = append(kids, k)
		}
	}
	kids := children(job)
	parent, err := c.JobFromID(ctx, job.ID())
	if err != nil {
		t.Fatal(err)
	}
	if n := parent.LastStatus().Statistics.NumChildJobs; n != int64(len(kids)) || n == 0 {
		t.Errorf("numChildJobs = %d, children %d", n, len(kids))
	}
	found := map[string]bool{}
	for _, k := range kids {
		st := k.LastStatus().Statistics
		ss := st.ScriptStatistics
		if st.ParentJobID != job.ID() || ss == nil || len(ss.StackFrames) != 1 {
			t.Errorf("child %s: parent %q, script statistics %+v", k.ID(), st.ParentJobID, ss)
			continue
		}
		f := ss.StackFrames[0]
		qc, _ := k.Config()
		q := ""
		if qc, ok := qc.(*bigquery.QueryConfig); ok {
			q = qc.Q
		}
		switch {
		case ss.EvaluationKind == "STATEMENT" && strings.HasPrefix(q, "CREATE TABLE"):
			found["create"] = true
			if f.StartLine != 2 || f.StartColumn != 1 || !strings.HasPrefix(f.Text, "EXECUTE IMMEDIATE FORMAT(") {
				t.Errorf("the CREATE TABLE child's frame: %+v, want at 2:1, the EXECUTE IMMEDIATE", f)
			}
		case ss.EvaluationKind == "STATEMENT" && strings.HasPrefix(q, "INSERT INTO"):
			found["insert"] = true
			if f.StartLine != 3 || f.StartColumn != 1 || !strings.HasPrefix(f.Text, "EXECUTE IMMEDIATE 'INSERT") {
				t.Errorf("the INSERT child's frame: %+v, want at 3:1, the EXECUTE IMMEDIATE", f)
			}
			if strings.Contains(q, "?") {
				t.Errorf("the INSERT child's query %q, want its USING values bound", q)
			}
			if qs, ok := st.Details.(*bigquery.QueryStatistics); !ok || qs.NumDMLAffectedRows != 2 {
				t.Errorf("the INSERT child's statistics: %+v, want 2 rows", st.Details)
			}
		case ss.EvaluationKind == "EXPRESSION" && strings.HasPrefix(q, "SELECT CAST((FORMAT("):
			// Its frame is in the script, as written (#1569).
			found["format"] = true
			if f.StartLine != 2 || f.StartColumn != 1 || !strings.HasPrefix(f.Text, "EXECUTE IMMEDIATE FORMAT(") {
				t.Errorf("the FORMAT expression child's frame: %+v, want at 2:1, the EXECUTE IMMEDIATE", f)
			}
		}
	}
	for _, w := range []string{"create", "insert", "format"} {
		if !found[w] {
			t.Errorf("no %s child among %d: %v", w, len(kids), found)
		}
	}

	// A lone EXECUTE IMMEDIATE: a SCRIPT parent with one child.
	lone, err := c.Query("EXECUTE IMMEDIATE 'SELECT 1 AS a'").Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status, err := lone.Wait(ctx); err != nil || status.Err() != nil {
		t.Fatal(err, status.Err())
	}
	got, err := c.JobFromID(ctx, lone.ID())
	if err != nil {
		t.Fatal(err)
	}
	st := got.LastStatus().Statistics
	if qs, ok := st.Details.(*bigquery.QueryStatistics); !ok || qs.StatementType != "SCRIPT" || st.NumChildJobs != 1 {
		t.Errorf("a lone EXECUTE IMMEDIATE: %+v, %d children; want SCRIPT, 1", st.Details, st.NumChildJobs)
	}
	if kids := children(lone); len(kids) != 1 {
		t.Errorf("a lone EXECUTE IMMEDIATE's children: %d, want 1", len(kids))
	}
}
