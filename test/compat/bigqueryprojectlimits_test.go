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

// TestBigQueryProjectConfigurationEffects (#1456): what #1417 left of the
// configurations of https://cloud.google.com/bigquery/docs/default-configuration.
// A query job's jobTimeoutMs, and its region's default_query_job_timeout_ms
// when it gives none, stop the job: it fails with reason timeout, and a
// DML statement so stopped writes nothing. default_time_zone reads a
// TIMESTAMP literal with no zone in it. default_kms_key_name is the key of
// a table CREATE TABLE makes naming none. Every setting is in
// australia-southeast2, which no other test uses. Before, the timeouts
// were stored only, the literal read in UTC, and the key given to
// tables.insert's tables alone.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.get, bigquery.tables.get
func TestBigQueryProjectConfigurationEffects(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	const region = "australia-southeast2"
	const r = "`region-" + region + "."
	run := func(sql string, timeout time.Duration) (*bigquery.Job, error) {
		q := c.Query(sql)
		q.Location, q.JobTimeout = region, timeout
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
	one := func(sql string) ([]bigquery.Value, error) {
		q := c.Query(sql)
		q.Location = region
		it, err := q.Read(ctx)
		if err != nil {
			return nil, err
		}
		var row []bigquery.Value
		if err := it.Next(&row); err != nil {
			return nil, err
		}
		return row, nil
	}
	t.Cleanup(func() {
		for _, o := range []string{"default_time_zone", "default_kms_key_name", "default_query_job_timeout_ms"} {
			_, _ = run("ALTER PROJECT SET OPTIONS ("+r+o+"` = NULL)", 0)
		}
	})
	timedOut := func(err error) bool {
		var ge *googleapi.Error
		if errors.As(err, &ge) {
			for _, e := range ge.Errors {
				if e.Reason == "timeout" {
					return true
				}
			}
		}
		var be *bigquery.Error
		return errors.As(err, &be) && be.Reason == "timeout"
	}
	// About 25 million rows: many seconds in the emulator's engine.
	const slow = "SELECT COUNT(*) FROM UNNEST(GENERATE_ARRAY(1, 5000)) a, UNNEST(GENERATE_ARRAY(1, 5000)) b WHERE MOD(a * b, 7) = 3"

	// jobTimeoutMs on a query job (jobs.insert) and on jobs.query.
	start := time.Now()
	job, err := run(slow, time.Second)
	if !timedOut(err) {
		t.Fatalf("query job with a 1s timeout: %v, want reason timeout", err)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("the timed-out job took %v", d)
	}
	if st, err := job.Status(ctx); err != nil || st.State != bigquery.Done || !timedOut(st.Err()) {
		t.Errorf("jobs.get of the timed-out job: %+v (%v)", st, err)
	}
	q := c.Query(slow)
	q.Location, q.JobTimeout = region, time.Second
	if _, err := q.Read(ctx); !timedOut(err) {
		t.Errorf("jobs.query with a 1s timeout: %v, want reason timeout", err)
	}
	// A quick job under its timeout runs.
	if _, err := run("SELECT 1", time.Minute); err != nil {
		t.Errorf("quick job: %v", err)
	}

	// A DML statement stopped writes nothing.
	ds := c.Dataset(strings.ReplaceAll(project, "-", "_") + "_projlimits")
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{Location: region}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(ctx) })
	if _, err := run("CREATE TABLE "+ds.DatasetID+".n (x INT64)", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := run("INSERT INTO "+ds.DatasetID+".n (x) "+slow, time.Second); !timedOut(err) {
		t.Errorf("INSERT with a 1s timeout: %v, want reason timeout", err)
	}
	if row, err := one("SELECT COUNT(*) FROM " + ds.DatasetID + ".n"); err != nil || row[0] != int64(0) {
		t.Errorf("rows the stopped INSERT left: %v (%v), want 0", row, err)
	}

	// default_query_job_timeout_ms is at least five minutes, which no
	// test waits for: set, a quick job given none still runs.
	if _, err := run("ALTER PROJECT SET OPTIONS ("+r+"default_query_job_timeout_ms` = 300000)", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := run("SELECT 1", 0); err != nil {
		t.Errorf("quick job under the default timeout: %v", err)
	}

	// default_time_zone: a TIMESTAMP literal with no zone is read in it.
	if _, err := run("ALTER PROJECT SET OPTIONS ("+r+"default_time_zone` = 'Asia/Tokyo')", 0); err != nil {
		t.Fatal(err)
	}
	row, err := one("SELECT TIMESTAMP '2020-01-01 09:00:00' = TIMESTAMP '2020-01-01 00:00:00+00', " +
		"TIMESTAMP '2020-01-02' = TIMESTAMP '2020-01-01 15:00:00 UTC', TIMESTAMP '2020-01-01 09:00:00Z' = TIMESTAMP '2020-01-01 09:00:00+00'")
	if err != nil || row[0] != true || row[1] != true || row[2] != true {
		t.Errorf("TIMESTAMP literals in Asia/Tokyo: %v (%v), want all true", row, err)
	}

	// default_kms_key_name: CREATE TABLE naming no key takes it; one
	// naming a key keeps its own.
	key := "projects/" + project + "/locations/" + region + "/keyRings/r/cryptoKeys/k"
	own := "projects/" + project + "/locations/" + region + "/keyRings/r/cryptoKeys/own"
	if _, err := run("ALTER PROJECT SET OPTIONS ("+r+"default_kms_key_name` = '"+key+"')", 0); err != nil {
		t.Fatal(err)
	}
	for table, sql := range map[string]string{
		"k1": "CREATE TABLE " + ds.DatasetID + ".k1 (a INT64)",
		"k2": "CREATE OR REPLACE TABLE " + ds.DatasetID + ".k2 AS SELECT 1 AS a",
		"k3": "CREATE TABLE " + ds.DatasetID + ".k3 (a INT64) OPTIONS (description = 'd')",
		"k4": "CREATE TABLE " + ds.DatasetID + ".k4 (a INT64) OPTIONS (kms_key_name = '" + own + "')",
	} {
		if _, err := run(sql, 0); err != nil {
			t.Errorf("%s: %v", sql, err)
			continue
		}
		want := key
		if table == "k4" {
			want = own
		}
		if md, err := ds.Table(table).Metadata(ctx); err != nil || md.EncryptionConfig == nil || md.EncryptionConfig.KMSKeyName != want {
			t.Errorf("%s: encryption %+v (%v), want %s", sql, md, err, want)
		}
	}
	// The key's existing table, under IF NOT EXISTS, is left as it is.
	if _, err := run("ALTER PROJECT SET OPTIONS ("+r+"default_kms_key_name` = NULL)", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := run("CREATE TABLE "+ds.DatasetID+".k5 (a INT64)", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := run("ALTER PROJECT SET OPTIONS ("+r+"default_kms_key_name` = '"+key+"')", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := run("CREATE TABLE IF NOT EXISTS "+ds.DatasetID+".k5 (a INT64)", 0); err != nil {
		t.Fatal(err)
	}
	if md, err := ds.Table("k5").Metadata(ctx); err != nil || md.EncryptionConfig != nil {
		t.Errorf("k5 after CREATE TABLE IF NOT EXISTS: %+v (%v), want no key", md, err)
	}
}
