//go:build compat

package compat

import (
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryAlterProjectStatementType (#1595): a lone ALTER PROJECT or
// ALTER ORGANIZATION SET OPTIONS job reports the statementType the REST
// reference lists for it, ALTER_PROJECT or ALTER_ORGANIZATION
// (https://docs.cloud.google.com/bigquery/docs/reference/rest/v2/Job, read
// 2026-10-17), and no DDL target table: the statement names none. Before,
// it reported the statement the front ran in its place, DROP_TABLE.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get
func TestBigQueryAlterProjectStatementType(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	for _, tc := range []struct{ sql, want string }{
		{"ALTER PROJECT `" + project + "` SET OPTIONS (`region-us.default_time_zone` = NULL)", "ALTER_PROJECT"},
		{"ALTER PROJECT SET OPTIONS (`region-us.default_time_zone` = NULL)", "ALTER_PROJECT"},
		{"ALTER ORGANIZATION SET OPTIONS (`region-us.default_time_zone` = NULL)", "ALTER_ORGANIZATION"},
	} {
		job, err := c.Query(tc.sql).Run(ctx)
		if err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		st, err := job.Wait(ctx)
		if err != nil || st.Err() != nil {
			t.Fatalf("%s: %v %v", tc.sql, err, st.Err())
		}
		again, err := c.JobFromID(ctx, job.ID())
		if err != nil {
			t.Fatal(err)
		}
		st, err = again.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		qs, ok := st.Statistics.Details.(*bigquery.QueryStatistics)
		if !ok {
			t.Fatalf("%s: statistics %T, want query statistics", tc.sql, st.Statistics.Details)
		}
		if qs.StatementType != tc.want {
			t.Errorf("%s: statementType %q, want %q", tc.sql, qs.StatementType, tc.want)
		}
		if qs.DDLTargetTable != nil {
			t.Errorf("%s: ddlTargetTable %v, want none", tc.sql, qs.DDLTargetTable)
		}
		if qs.DDLOperationPerformed != "" {
			t.Errorf("%s: ddlOperationPerformed %q, want none", tc.sql, qs.DDLOperationPerformed)
		}
	}
}
