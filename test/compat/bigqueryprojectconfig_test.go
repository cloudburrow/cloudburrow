//go:build compat

package compat

import (
	"errors"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
)

// TestBigQueryProjectConfigurations (#1417): the configurations of
// https://cloud.google.com/bigquery/docs/default-configuration beyond
// default_location, set with ALTER PROJECT and ALTER ORGANIZATION SET
// OPTIONS, alone or at the top of a script, and read back from
// `region-REGION`.INFORMATION_SCHEMA.PROJECT_OPTIONS, ORGANIZATION_OPTIONS
// and EFFECTIVE_PROJECT_OPTIONS, the project's setting winning over the
// organization's. Each takes effect where BigQuery documents it:
// default_time_zone in the time functions a query calls without a zone;
// default_kms_key_name as a new table's encryptionConfiguration;
// default_max_time_travel_hours and default_storage_billing_model on a new
// dataset; default_sql_dialect_option only_google_sql refusing legacy SQL.
// A value of the wrong type or out of range, an unknown option, a global
// option with a region and a regional one without, and two regions in one
// statement fail 400. Every setting is in asia-northeast1, which no other
// test uses, so the tests that share the project are unaffected. Before,
// all but default_location, ALTER ORGANIZATION and ALTER PROJECT in a
// script were 501.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.datasets.insert, bigquery.datasets.get, bigquery.tables.insert, bigquery.tables.get
func TestBigQueryProjectConfigurations(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	const region = "asia-northeast1"
	const r = "`region-" + region + "."
	query := func(sql string, legacy bool) ([][]bigquery.Value, error) {
		q := c.Query(sql)
		q.Location, q.UseLegacySQL = region, legacy
		it, err := q.Read(ctx)
		if err != nil {
			return nil, err
		}
		var rows [][]bigquery.Value
		for {
			var row []bigquery.Value
			if err := it.Next(&row); err != nil {
				if err.Error() == "no more items in iterator" {
					return rows, nil
				}
				return rows, err
			}
			rows = append(rows, row)
		}
	}
	run := func(sql string) error {
		q := c.Query(sql)
		q.Location = region
		job, err := q.Run(ctx)
		if err != nil {
			return err
		}
		st, err := job.Wait(ctx)
		if err != nil {
			return err
		}
		return st.Err()
	}
	clear := []string{"default_time_zone", "default_kms_key_name", "default_query_job_timeout_ms",
		"default_interactive_query_queue_timeout_ms", "default_batch_query_queue_timeout_ms",
		"default_max_time_travel_hours", "default_storage_billing_model", "default_sql_dialect_option"}
	t.Cleanup(func() {
		for _, o := range clear {
			_ = run("ALTER PROJECT SET OPTIONS (" + r + o + "` = NULL)")
			_ = run("ALTER ORGANIZATION SET OPTIONS (" + r + o + "` = NULL)")
		}
	})

	key := "projects/" + project + "/locations/" + region + "/keyRings/r/cryptoKeys/k"
	for _, sql := range []string{
		"ALTER ORGANIZATION SET OPTIONS (" + r + "default_time_zone` = 'America/Chicago', " +
			r + "default_query_job_timeout_ms` = 1800000)",
		"ALTER PROJECT `" + project + "` SET OPTIONS (" + r + "default_time_zone` = 'Asia/Tokyo', " +
			r + "default_interactive_query_queue_timeout_ms` = 600000, " + r + "default_batch_query_queue_timeout_ms` = -1)",
	} {
		if err := run(sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// In a script: carried out at its top level.
	script := "DECLARE n INT64 DEFAULT 1;\n" +
		"ALTER PROJECT SET OPTIONS (" + r + "default_kms_key_name` = '" + key + "', " +
		r + "default_max_time_travel_hours` = 72, " + r + "default_storage_billing_model` = 'PHYSICAL');\n" +
		"SELECT n"
	if err := run(script); err != nil {
		t.Fatalf("script: %v", err)
	}

	// default_time_zone: the project's, Asia/Tokyo (UTC+9).
	rows, err := query("SELECT FORMAT_TIMESTAMP('%H:%M', TIMESTAMP_SECONDS(0)), "+
		"DATE(TIMESTAMP '2020-01-01 20:00:00+00'), EXTRACT(HOUR FROM TIMESTAMP_SECONDS(0)), "+
		"TIMESTAMP('2020-01-01 09:00:00') = TIMESTAMP '2020-01-01 00:00:00+00', "+
		"FORMAT_TIMESTAMP('%H:%M', TIMESTAMP_SECONDS(0), 'UTC'), CURRENT_DATE()", false)
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	if err != nil || len(rows) != 1 {
		t.Fatalf("time functions: %v (%v)", rows, err)
	}
	got := rows[0]
	if got[0] != "09:00" || got[1].(interface{ String() string }).String() != "2020-01-02" || got[2] != int64(9) ||
		got[3] != true || got[4] != "00:00" {
		t.Errorf("time functions in Asia/Tokyo: %v", got)
	}
	now := advanceBigQueryClock(t, h, 0) // the instance's clock, #1487
	if d := got[5].(interface{ String() string }).String(); d != now.In(tokyo).Format("2006-01-02") &&
		d != now.Add(-time.Minute).In(tokyo).Format("2006-01-02") {
		t.Errorf("CURRENT_DATE() = %s, want Tokyo's date", d)
	}
	// Another region's jobs keep UTC.
	q := c.Query("SELECT FORMAT_TIMESTAMP('%H:%M', TIMESTAMP_SECONDS(0))")
	q.Location = "US"
	it, err := q.Read(ctx)
	var us []bigquery.Value
	if err == nil {
		err = it.Next(&us)
	}
	if err != nil || len(us) != 1 || us[0] != "00:00" {
		t.Errorf("in US: %v (%v), want 00:00", us, err)
	}

	sel := func(view string) [][]bigquery.Value {
		t.Helper()
		cols := "option_name, option_type, option_value"
		if view == "EFFECTIVE_PROJECT_OPTIONS" {
			cols = "option_name, option_set_level, option_set_on_id, option_value"
		}
		rows, err := query("SELECT "+cols+" FROM `region-"+region+"`.INFORMATION_SCHEMA."+view+
			" WHERE option_name != 'default_location' ORDER BY option_name", false)
		if err != nil {
			t.Fatalf("%s: %v", view, err)
		}
		return rows
	}
	want := func(view string, rows [][]bigquery.Value, w ...[]bigquery.Value) {
		t.Helper()
		if len(rows) != len(w) {
			t.Errorf("%s: %v, want %v", view, rows, w)
			return
		}
		for i := range w {
			for j := range w[i] {
				if rows[i][j] != w[i][j] {
					t.Errorf("%s row %d: %v, want %v", view, i, rows[i], w[i])
					break
				}
			}
		}
	}
	want("ORGANIZATION_OPTIONS", sel("ORGANIZATION_OPTIONS"),
		[]bigquery.Value{"default_query_job_timeout_ms", "INT64", "1800000"},
		[]bigquery.Value{"default_time_zone", "STRING", "America/Chicago"})
	want("PROJECT_OPTIONS", sel("PROJECT_OPTIONS"),
		[]bigquery.Value{"default_batch_query_queue_timeout_ms", "INT64", "-1"},
		[]bigquery.Value{"default_interactive_query_queue_timeout_ms", "INT64", "600000"},
		[]bigquery.Value{"default_kms_key_name", "STRING", key},
		[]bigquery.Value{"default_max_time_travel_hours", "INT64", "72"},
		[]bigquery.Value{"default_storage_billing_model", "STRING", "PHYSICAL"},
		[]bigquery.Value{"default_time_zone", "STRING", "Asia/Tokyo"})
	want("EFFECTIVE_PROJECT_OPTIONS", sel("EFFECTIVE_PROJECT_OPTIONS"),
		[]bigquery.Value{"default_batch_query_queue_timeout_ms", "PROJECTS", project, "-1"},
		[]bigquery.Value{"default_interactive_query_queue_timeout_ms", "PROJECTS", project, "600000"},
		[]bigquery.Value{"default_kms_key_name", "PROJECTS", project, key},
		[]bigquery.Value{"default_max_time_travel_hours", "PROJECTS", project, "72"},
		[]bigquery.Value{"default_query_job_timeout_ms", "ORGANIZATIONS", "100000000001", "1800000"},
		[]bigquery.Value{"default_storage_billing_model", "PROJECTS", project, "PHYSICAL"},
		[]bigquery.Value{"default_time_zone", "PROJECTS", project, "Asia/Tokyo"})

	// A new dataset and table in the region take the defaults.
	ds := c.Dataset(strings.ReplaceAll(project, "-", "_") + "_projconfig")
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{Location: region}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(ctx) })
	if md, err := ds.Metadata(ctx); err != nil || md.MaxTimeTravel != 72*time.Hour || md.StorageBillingModel != "PHYSICAL" {
		t.Errorf("new dataset: %+v (%v), want 72h and PHYSICAL", md, err)
	}
	tbl := ds.Table("t")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "a", Type: bigquery.IntegerFieldType}}}); err != nil {
		t.Fatal(err)
	}
	if md, err := tbl.Metadata(ctx); err != nil || md.EncryptionConfig == nil || md.EncryptionConfig.KMSKeyName != key {
		t.Errorf("new table's encryption: %+v (%v), want %s", md, err, key)
	}

	// The project's zone cleared: the organization's, America/Chicago (UTC-6 on 1 January).
	if err := run("ALTER PROJECT SET OPTIONS (" + r + "default_time_zone` = NULL)"); err != nil {
		t.Fatal(err)
	}
	if rows, err := query("SELECT FORMAT_TIMESTAMP('%H:%M', TIMESTAMP '2020-01-01 00:00:00+00')", false); err != nil ||
		len(rows) != 1 || rows[0][0] != "18:00" {
		t.Errorf("organization's zone: %v (%v), want 18:00", rows, err)
	}

	// only_google_sql refuses legacy SQL.
	if err := run("ALTER PROJECT SET OPTIONS (" + r + "default_sql_dialect_option` = 'only_google_sql')"); err != nil {
		t.Fatal(err)
	}
	if _, err := query("SELECT 1", true); !is400(err) {
		t.Errorf("legacy SQL under only_google_sql: %v, want 400", err)
	}

	for _, sql := range []string{
		"ALTER PROJECT SET OPTIONS (" + r + "default_time_zone` = 'Nowhere/Never')",
		"ALTER PROJECT SET OPTIONS (" + r + "default_query_job_timeout_ms` = 1000)",
		"ALTER PROJECT SET OPTIONS (" + r + "default_query_job_timeout_ms` = 'x')",
		"ALTER PROJECT SET OPTIONS (" + r + "no_such_option` = 'x')",
		"ALTER PROJECT SET OPTIONS (`default_time_zone` = 'UTC')",
		"ALTER PROJECT SET OPTIONS (" + r + "default_location` = 'EU')",
		"ALTER PROJECT SET OPTIONS (" + r + "default_time_zone` = 'UTC', `region-eu.default_time_zone` = 'UTC')",
		"ALTER ORGANIZATION SET OPTIONS (" + r + "enable_reservation_based_fairness` = TRUE)",
	} {
		if err := run(sql); !is400(err) {
			t.Errorf("%s: %v, want 400", sql, err)
		}
	}
	if rows := sel("PROJECT_OPTIONS"); len(rows) != 6 || rows[4][0] != "default_sql_dialect_option" || rows[4][2] != "only_google_sql" {
		t.Errorf("after the refusals: %v", rows)
	}
}

// is400 reports whether err is a 400, or a job that failed invalidQuery.
func is400(err error) bool {
	var e *googleapi.Error
	if errors.As(err, &e) {
		return e.Code == 400
	}
	return err != nil && strings.Contains(err.Error(), "invalidQuery")
}
