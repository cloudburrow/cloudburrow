//go:build compat

package compat

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
)

// TestBigQueryAlterTableRefusedLeavesTheTable (#1612): an ALTER TABLE of
// several actions whose last is refused changes nothing. BigQuery fails
// such a statement as a whole ("If any of the columns named A, C, or D
// already exist, the statement fails", the DDL reference, docs re-read
// 2026-10-17:
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language).
// The front carries the actions out a run at a time, and a refusal the
// tables.patch path made when the last run was sent (a data governance
// tag key not "parent-id/key", checked when the patch is sent) left a
// DROP COLUMN or a RENAME TO before it done. Each statement through jobs.query and jobs.insert, 400, the table
// read back with its columns, its row, its name and its description.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get, bigquery.tabledata.list
func TestBigQueryAlterTableRefusedLeavesTheTable(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	for _, insert := range []bool{false, true} {
		name := "atomic_q"
		if insert {
			name = "atomic_j"
		}
		tbl := ds.Table(name)
		ref := "`" + ds.DatasetID + "." + name + "`"
		if err := bqRun(ctx, c, "CREATE TABLE "+ref+" (a INT64, b STRING, c INT64) OPTIONS (description = 'before')", insert); err != nil {
			t.Fatal(err)
		}
		if err := bqRun(ctx, c, "INSERT INTO "+ref+" (a, b, c) VALUES (1, 'x', 3)", insert); err != nil {
			t.Fatal(err)
		}
		bad := "ALTER COLUMN a SET OPTIONS (data_governance_tags = [('nokey', 'v')])"
		for _, sql := range []string{
			"ALTER TABLE " + ref + " DROP COLUMN c, " + bad,
			"ALTER TABLE " + ref + " RENAME COLUMN b TO bb, DROP COLUMN c, " + bad,
			"ALTER TABLE " + ref + " RENAME TO " + name + "_moved, " + bad,
			"ALTER TABLE " + ref + " SET OPTIONS (description = 'after'), ADD COLUMN d STRING, " + bad,
		} {
			wantHTTPStatus(t, sql, bqRun(ctx, c, sql, insert), http.StatusBadRequest)
			md, err := tbl.Metadata(ctx)
			if err != nil {
				t.Fatalf("insert %v: after %s: %v", insert, sql, err)
			}
			var cols []string
			for _, f := range md.Schema {
				cols = append(cols, f.Name)
			}
			if got := strings.Join(cols, ","); got != "a,b,c" || md.Description != "before" {
				t.Errorf("insert %v: after %s: columns %s, description %q; want a,b,c and \"before\"", insert, sql, got, md.Description)
			}
			if _, err := ds.Table(name + "_moved").Metadata(ctx); err == nil {
				t.Errorf("insert %v: after %s: the table was renamed", insert, sql)
			}
			q := c.Query("SELECT a, b, c FROM " + ref)
			it, err := q.Read(ctx)
			if err != nil {
				t.Fatalf("insert %v: after %s: %v", insert, sql, err)
			}
			var row []bigquery.Value
			if err := it.Next(&row); err != nil || len(row) != 3 || row[2] != int64(3) {
				t.Errorf("insert %v: after %s: row %v (%v), want [1 x 3]", insert, sql, row, err)
			}
		}
		// The statement without its refused action is carried out whole.
		if err := bqRun(ctx, c, "ALTER TABLE "+ref+" DROP COLUMN c, SET OPTIONS (description = 'after')", insert); err != nil {
			t.Fatal(err)
		}
		if md, err := tbl.Metadata(ctx); err != nil || len(md.Schema) != 2 || md.Description != "after" {
			t.Errorf("insert %v: the valid statement: %+v (%v)", insert, md, err)
		}
	}
}

// TestBigQueryScriptDryRunStopsAtCallAndExecuteImmediate (#1612): a dry
// run of a script that reaches an ALTER TABLE after a CALL or an EXECUTE
// IMMEDIATE stops there, as BigQuery documents
// (https://docs.cloud.google.com/bigquery/docs/multi-statement-queries,
// "Dry-run a multi-statement query", docs re-read 2026-10-17): "the dry
// run validates that the called procedure exists and has a signature
// matching the arguments provided", and "validates the query expression,
// but does not evaluate the dynamic query itself"; the statements after
// are not read. So a CALL of a procedure that exists, with arguments of
// its types, and an EXECUTE IMMEDIATE of a STRING whose query would fail,
// each before an ALTER TABLE that would fail and a SELECT of a table that
// does not exist, answer statementType SCRIPT and change nothing; a CALL
// of no such procedure is 404 notFound, of the wrong number of arguments,
// a value of another type, an OUT argument that is not a variable, or an
// undeclared variable 400 invalidQuery, and an EXECUTE IMMEDIATE of an
// INT64 or of an undeclared variable 400 invalidQuery. Before, each was
// 501 (checkDDL, as #1612 reports). Through jobs.insert (the Go client) and jobs.query.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryScriptDryRunStopsAtCallAndExecuteImmediate(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	svc := generatedBigQuery(t, h)
	tbl := ds.Table("drycall")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "a", Type: bigquery.IntegerFieldType},
	}}); err != nil {
		t.Fatal(err)
	}
	ref := "`" + ds.DatasetID + ".drycall`"
	proc := "`" + ds.DatasetID + ".drycall_p`"
	if err := bqRun(ctx, c, "CREATE PROCEDURE "+proc+"(x INT64, OUT y STRING) BEGIN SET y = CAST(x AS STRING); END", false); err != nil {
		t.Fatal(err)
	}
	alter := "; ALTER TABLE " + ref + " ADD COLUMN a STRING; SELECT nope FROM nowhere"
	dry := func(sql string) (*bigquery.QueryStatistics, error) {
		q := c.Query(sql)
		q.DryRun = true
		job, err := q.Run(ctx)
		if err != nil {
			return nil, err
		}
		st := job.LastStatus()
		if st == nil || st.Statistics == nil {
			return nil, nil
		}
		qs, _ := st.Statistics.Details.(*bigquery.QueryStatistics)
		return qs, nil
	}
	for _, sql := range []string{
		"DECLARE s STRING; CALL " + proc + "(1, s)" + alter,
		"DECLARE s STRING; SELECT a FROM " + ref + "; CALL " + proc + "(1 + 1, s)" + alter,
		"DECLARE q STRING DEFAULT 'SELECT nope FROM nowhere'; EXECUTE IMMEDIATE q" + alter,
		"EXECUTE IMMEDIATE CONCAT('SELECT ', 'nope') INTO x USING 1 AS y" + alter,
	} {
		qs, err := dry(sql)
		if err != nil || qs == nil || qs.StatementType != "SCRIPT" {
			t.Errorf("%s: %+v (%v)", sql, qs, err)
		}
		gq, err := svc.Jobs.Query(project, &bq.QueryRequest{Query: sql, DryRun: true, UseLegacySql: new(bool)}).Context(ctx).Do()
		if err != nil || len(gq.Rows) != 0 {
			t.Errorf("%s through jobs.query: %+v (%v)", sql, gq, err)
		}
	}
	for sql, want := range map[string]struct {
		code   int
		reason string
	}{
		"CALL `" + ds.DatasetID + ".nosuch`(1)" + alter:                                   {http.StatusNotFound, "notFound"},
		"DECLARE s STRING; CALL " + proc + "(1)" + alter:                                  {http.StatusBadRequest, "invalidQuery"},
		"DECLARE s STRING; CALL " + proc + "('one', s)" + alter:                           {http.StatusBadRequest, "invalidQuery"},
		"CALL " + proc + "(1, 'literal')" + alter:                                         {http.StatusBadRequest, "invalidQuery"},
		"CALL " + proc + "(1, undeclared)" + alter:                                        {http.StatusBadRequest, "invalidQuery"},
		"DECLARE s STRING; SELECT nope FROM " + ref + "; CALL " + proc + "(1, s)" + alter: {http.StatusBadRequest, "invalidQuery"},
		"EXECUTE IMMEDIATE 5" + alter:                                                     {http.StatusBadRequest, "invalidQuery"},
		"EXECUTE IMMEDIATE undeclared" + alter:                                            {http.StatusBadRequest, "invalidQuery"},
	} {
		_, err := dry(sql)
		wantReason(t, sql, err, want.code, want.reason)
		_, err = svc.Jobs.Query(project, &bq.QueryRequest{Query: sql, DryRun: true, UseLegacySql: new(bool)}).Context(ctx).Do()
		wantReason(t, sql+" through jobs.query", err, want.code, want.reason)
	}
	if md, err := tbl.Metadata(ctx); err != nil || len(md.Schema) != 1 {
		t.Errorf("a dry run changed the table: %+v (%v)", md, err)
	}
}

// TestConsoleBigQueryEditorAlterTableRefusedLeavesTheTable (#1612): the
// query editor's Read-write mode reaches the same: an ALTER TABLE whose
// last action is refused is answered in the API's words and leaves the
// table's columns as they were.
func TestConsoleBigQueryEditorAlterTableRefusedLeavesTheTable(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	if err := bqRun(ctx, c, "CREATE TABLE `"+ds.DatasetID+".editor_atomic` (a INT64, c INT64)", false); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"Path": []string{ds.DatasetID}, "Mode": "read-write",
		"Statement": "ALTER TABLE editor_atomic DROP COLUMN c, ALTER COLUMN a SET OPTIONS (data_governance_tags = [('nokey', 'v')])"})
	code, out := consoleDo(t, addr, http.MethodPost, "/api/query/bigquery?project="+url.QueryEscape(project), string(body))
	if msg := consoleError(t, out); code != http.StatusBadRequest || !strings.Contains(msg, "parent-id/tag_key") {
		t.Errorf("the editor answered %d %q, want 400 naming the tag key's format", code, msg)
	}
	if md, err := ds.Table("editor_atomic").Metadata(ctx); err != nil || len(md.Schema) != 2 {
		t.Errorf("the refused statement changed the table: %+v (%v)", md, err)
	}
}
