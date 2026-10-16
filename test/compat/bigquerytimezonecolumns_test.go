//go:build compat

package compat

import (
	"fmt"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryDefaultTimeZoneOfColumns (#1456): a project's
// default_time_zone reaches every time function and cast BigQuery
// documents as using it, where a TIMESTAMP comes from a column the query
// text does not show the type of: DATE, DATETIME, TIME, STRING and
// EXTRACT of it, TIMESTAMP_TRUNC, DATE_TRUNC and DATETIME_TRUNC of it,
// FORMAT_TIMESTAMP, CAST between TIMESTAMP and STRING, DATE or DATETIME,
// TIMESTAMP and PARSE_TIMESTAMP of a column, and a TIMESTAMP literal with
// no zone. A DATETIME column and an explicit zone are not moved, and
// jobs.get shows the query as the client wrote it. Before, each of these
// stayed in UTC: the front could not tell a TIMESTAMP column from a
// DATETIME one, and the engine knew only UTC. The zone is set in
// southamerica-west1, which no other test uses.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.get, bigquery.datasets.insert, bigquery.tables.insert, bigquery.tabledata.insertAll
func TestBigQueryDefaultTimeZoneOfColumns(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	const region = "southamerica-west1"
	const r = "`region-" + region + "."
	run := func(sql string) (*bigquery.Job, error) {
		q := c.Query(sql)
		q.Location = region
		job, err := q.Run(ctx)
		if err != nil {
			return nil, err
		}
		st, err := job.Wait(ctx)
		if err != nil {
			return job, err
		}
		return job, st.Err()
	}
	ds := fmt.Sprintf("tzcols_%d", time.Now().UnixNano())
	if err := c.Dataset(ds).Create(ctx, &bigquery.DatasetMetadata{Location: region}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Dataset(ds).DeleteWithContents(ctx) })
	if _, err := run("ALTER PROJECT SET OPTIONS (" + r + "default_time_zone` = 'America/New_York')"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = run("ALTER PROJECT SET OPTIONS (" + r + "default_time_zone` = NULL)") })
	tbl := c.Dataset(ds).Table("t")
	schema := bigquery.Schema{
		{Name: "ts", Type: bigquery.TimestampFieldType},
		{Name: "dt", Type: bigquery.DateTimeFieldType},
		{Name: "d", Type: bigquery.DateFieldType},
		{Name: "s", Type: bigquery.StringFieldType},
	}
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatal(err)
	}
	if _, err := run("INSERT " + ds + ".t (ts, dt, d, s) VALUES (TIMESTAMP '2020-01-01 03:00:00+00', " +
		"DATETIME '2020-01-01 03:00:00', DATE '2020-01-01', '2020-01-01 00:00:00')"); err != nil {
		t.Fatal(err)
	}

	// 2020-01-01 03:00 UTC is 2019-12-31 22:00 in New York (-05);
	// 2020-01-01 00:00 in New York is 1577854800.
	sql := "SELECT STRING(ts), CAST(DATE(ts) AS STRING), CAST(DATETIME(ts) AS STRING), CAST(TIME(ts) AS STRING), " +
		"EXTRACT(HOUR FROM ts), CAST(EXTRACT(DATE FROM ts) AS STRING), UNIX_SECONDS(TIMESTAMP_TRUNC(ts, DAY)), " +
		"UNIX_SECONDS(DATE_TRUNC(ts, MONTH)), FORMAT_TIMESTAMP('%F %H', ts), CAST(ts AS STRING), " +
		"CAST(CAST(ts AS DATE) AS STRING), CAST(CAST(ts AS DATETIME) AS STRING), UNIX_SECONDS(CAST(d AS TIMESTAMP)), " +
		"UNIX_SECONDS(CAST(s AS TIMESTAMP)), UNIX_SECONDS(TIMESTAMP(dt)), UNIX_SECONDS(PARSE_TIMESTAMP('%F %T', s)), " +
		"UNIX_SECONDS(TIMESTAMP '2020-01-01 00:00:00'), " +
		// Not moved: an explicit zone, a DATETIME.
		"STRING(ts, 'UTC'), EXTRACT(HOUR FROM ts AT TIME ZONE 'UTC'), EXTRACT(HOUR FROM dt), CAST(DATE(dt) AS STRING) " +
		"FROM " + ds + ".t"
	want := []bigquery.Value{"2019-12-31 22:00:00-05", "2019-12-31", "2019-12-31 22:00:00", "22:00:00",
		int64(22), "2019-12-31", int64(1577768400),
		int64(1575176400), "2019-12-31 22", "2019-12-31 22:00:00-05",
		"2019-12-31", "2019-12-31 22:00:00", int64(1577854800),
		int64(1577854800), int64(1577865600), int64(1577854800),
		int64(1577854800),
		"2020-01-01 03:00:00+00", int64(3), int64(3), "2020-01-01"}
	check := func(how string, rows []bigquery.Value) {
		t.Helper()
		if len(rows) != len(want) {
			t.Fatalf("%s: %v, want %v", how, rows, want)
		}
		for i := range want {
			if rows[i] != want[i] {
				t.Errorf("%s: column %d = %v (%T), want %v", how, i, rows[i], rows[i], want[i])
			}
		}
	}
	// jobs.query.
	q := c.Query(sql)
	q.Location = region
	it, err := q.Read(ctx)
	var row []bigquery.Value
	if err == nil {
		err = it.Next(&row)
	}
	if err != nil {
		t.Fatalf("jobs.query: %v", err)
	}
	check("jobs.query", row)
	// jobs.insert, and the job keeps the client's text.
	job, err := run(sql)
	if err != nil {
		t.Fatalf("jobs.insert: %v", err)
	}
	it, err = job.Read(ctx)
	row = nil
	if err == nil {
		err = it.Next(&row)
	}
	if err != nil {
		t.Fatalf("jobs.insert read: %v", err)
	}
	check("jobs.insert", row)
	got, err := c.JobFromIDLocation(ctx, job.ID(), region)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := got.Config()
	if err != nil {
		t.Fatal(err)
	}
	if qc, ok := cfg.(*bigquery.QueryConfig); !ok || qc.Q != sql {
		t.Errorf("jobs.get query = %v, want the client's text", cfg)
	}

	// Another region keeps UTC.
	q = c.Query("SELECT STRING(TIMESTAMP '2020-01-01 03:00:00')")
	q.Location = "US"
	it, err = q.Read(ctx)
	row = nil
	if err == nil {
		err = it.Next(&row)
	}
	if err != nil || len(row) != 1 || row[0] != "2020-01-01 03:00:00+00" {
		t.Errorf("in US: %v (%v), want UTC", row, err)
	}
}
