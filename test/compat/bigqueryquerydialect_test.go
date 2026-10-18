//go:build compat

package compat

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	bq "google.golang.org/api/bigquery/v2"
)

// TestBigQueryJobsQueryDialect (#1595): region INFORMATION_SCHEMA.JOBS'
// query_dialect is GOOGLE_SQL or LEGACY_SQL for a job that asked one, and,
// for one that asked none, DEFAULT_LEGACY_SQL, or DEFAULT_GOOGLE_SQL when
// the region's default_sql_dialect_option made GoogleSQL the default when
// the job was made (https://docs.cloud.google.com/bigquery/docs/information-schema-jobs,
// read 2026-10-17). The jobs go through the generated REST client, since
// the Go client always sends useLegacySql. Before, a job that asked none
// was always DEFAULT_LEGACY_SQL.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.jobs.query
func TestBigQueryJobsQueryDialect(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	svc := generatedBigQuery(t, h)
	ctx := h.Context()
	stamp := time.Now().UnixNano()
	run := func(name string, legacy *bool) string {
		t.Helper()
		id := fmt.Sprintf("dialect_%s_%d", name, stamp)
		q := &bq.JobConfigurationQuery{Query: "SELECT 1", UseLegacySql: legacy}
		if legacy != nil && !*legacy {
			q.ForceSendFields = []string{"UseLegacySql"}
		}
		job := &bq.Job{JobReference: &bq.JobReference{ProjectId: project, JobId: id, Location: "US"},
			Configuration: &bq.JobConfiguration{Query: q}}
		if _, err := svc.Jobs.Insert(project, job).Context(ctx).Do(); err != nil {
			t.Fatalf("%s: jobs.insert: %v", name, err)
		}
		for deadline := time.Now().Add(time.Minute); ; {
			got, err := svc.Jobs.Get(project, id).Location("US").Context(ctx).Do()
			if err != nil {
				t.Fatalf("%s: jobs.get: %v", name, err)
			}
			if got.Status != nil && got.Status.State == "DONE" {
				if got.Status.ErrorResult != nil {
					t.Fatalf("%s: %+v", name, got.Status.ErrorResult)
				}
				return id
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: not done", name)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	alter := func(value string) {
		t.Helper()
		err := bqRun(ctx, c, "ALTER PROJECT SET OPTIONS (`region-us.default_sql_dialect_option` = "+value+")", true)
		if err != nil {
			t.Fatalf("ALTER PROJECT default_sql_dialect_option = %s: %v", value, err)
		}
	}
	f, tr := false, true
	want := map[string]string{}
	want[run("google", &f)] = "GOOGLE_SQL"
	want[run("legacy", &tr)] = "LEGACY_SQL"
	want[run("none", nil)] = "DEFAULT_LEGACY_SQL"
	alter("'default_google_sql'")
	t.Cleanup(func() { alter("NULL") })
	want[run("nonegoogle", nil)] = "DEFAULT_GOOGLE_SQL"

	var ids, wantRows []string
	for id, d := range want {
		ids = append(ids, "'"+id+"'")
		wantRows = append(wantRows, id+"|"+d)
	}
	sort.Strings(wantRows)
	got := queryRows(t, ctx, c, project, "", "SELECT job_id, query_dialect FROM `region-us`.INFORMATION_SCHEMA.JOBS WHERE job_id IN ("+
		strings.Join(ids, ", ")+")")
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(wantRows, "\n") {
		t.Errorf("JOBS query_dialect:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantRows, "\n"))
	}
}

// TestBigQueryParametersModeOfAProcedure (#1595): PARAMETERS'
// parameter_mode is IN, OUT or INOUT for a procedure's argument, and NULL
// for a function's, as the view's page shows a function argument's
// (https://docs.cloud.google.com/bigquery/docs/information-schema-parameters,
// read 2026-10-17). Before, a function's was IN.
//
// covers: bigquery.jobs.query, bigquery.routines.insert
func TestBigQueryParametersModeOfAProcedure(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	d := ds.DatasetID
	for _, sql := range []string{
		"CREATE PROCEDURE " + d + ".pm(IN a INT64, OUT b INT64, INOUT c INT64) BEGIN SET b = a; SET c = c + a; END",
		"CREATE FUNCTION " + d + ".fm(x INT64) AS (x + 1)",
	} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	got := queryRows(t, ctx, c, project, "", "SELECT specific_name, ordinal_position, IFNULL(parameter_mode, 'NULL') FROM "+
		d+".INFORMATION_SCHEMA.PARAMETERS ORDER BY specific_name, ordinal_position")
	want := []string{"fm|1|NULL", "pm|1|IN", "pm|2|OUT", "pm|3|INOUT"}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("PARAMETERS: %v, want %v", got, want)
	}
}
