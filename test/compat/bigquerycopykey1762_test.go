//go:build compat

package compat

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryCopyKey1762: a copy job's destinationEncryptionConfiguration,
// as customer-managed-encryption documents it (read 2026-11-18):
//   - a copy into a new table gives it the key ("bq cp
//     --destination_kms_key"; the Go sample's DestinationEncryptionConfig);
//   - WRITE_TRUNCATE gives a table that exists the key given, another one
//     included, or, with none, default encryption ("change a table from
//     CMEK-protection to default encryption");
//   - a key given with WRITE_APPEND into a table encrypted otherwise fails
//     the job, "Existing table encryption settings don't match encryption
//     settings specified in the request", and leaves the table; with no key
//     given it appends;
//   - a key of another region than the destination's dataset fails the
//     job, "This region is not supported"; a global key is accepted;
//   - a key with /cryptoKeyVersions/ is refused ("You cannot specify").
//
// Measured first: every copy with a key was 501 (#1762).
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.tables.get
func TestBigQueryCopyKey1762(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	p := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	exec := func(sql string) {
		t.Helper()
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	rows := func(table string) []string {
		t.Helper()
		got := queryRows(t, ctx, c, "", "", "SELECT CAST(n AS STRING) FROM "+p(table))
		sort.Strings(got)
		return got
	}
	keyOf := func(table string) string {
		t.Helper()
		md, err := ds.Table(table).Metadata(ctx)
		if err != nil {
			t.Fatalf("tables.get %s: %v", table, err)
		}
		if md.EncryptionConfig == nil {
			return ""
		}
		return md.EncryptionConfig.KMSKeyName
	}
	copyJob := func(dst, key string, write bigquery.TableWriteDisposition) error {
		t.Helper()
		cp := ds.Table(dst).CopierFrom(ds.Table("src"))
		cp.WriteDisposition = write
		if key != "" {
			cp.DestinationEncryptionConfig = &bigquery.EncryptionConfig{KMSKeyName: key}
		}
		j, err := cp.Run(ctx)
		return waitJob(ctx, j, err)
	}
	key := "projects/" + h.Project() + "/locations/us/keyRings/ring/cryptoKeys/one"
	other := "projects/" + h.Project() + "/locations/us/keyRings/ring/cryptoKeys/two"

	exec("CREATE TABLE " + p("src") + " (n INT64)")
	exec("INSERT INTO " + p("src") + " (n) VALUES (1), (2)")

	if err := copyJob("keyed", key, ""); err != nil {
		t.Fatalf("copy into a new table with a key: %v", err)
	}
	if got := keyOf("keyed"); got != key {
		t.Errorf("keyed's key %q, want %q", got, key)
	}
	if got := rows("keyed"); !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Errorf("keyed rows %v, want 1 2", got)
	}

	err := copyJob("keyed", other, bigquery.WriteAppend)
	var bqErr *bigquery.Error
	if err == nil || !strings.Contains(err.Error(), "Existing table encryption settings don't match encryption settings specified in the request") ||
		!errors.As(err, &bqErr) || bqErr.Reason != "invalid" {
		t.Errorf("WRITE_APPEND with another key: %v, want the job to fail", err)
	}
	exec("CREATE TABLE " + p("plain") + " (n INT64)")
	if err := copyJob("plain", key, bigquery.WriteAppend); err == nil || !strings.Contains(err.Error(), "Existing table encryption settings") {
		t.Errorf("WRITE_APPEND with a key into default encryption: %v, want the job to fail", err)
	}
	if got := rows("keyed"); !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Errorf("keyed rows after the failed append %v, want 1 2", got)
	}
	if err := copyJob("keyed", key, bigquery.WriteAppend); err != nil {
		t.Fatalf("WRITE_APPEND with the same key: %v", err)
	}
	if err := copyJob("keyed", "", bigquery.WriteAppend); err != nil {
		t.Fatalf("WRITE_APPEND with no key: %v", err)
	}
	if got, n := keyOf("keyed"), len(rows("keyed")); got != key || n != 6 {
		t.Errorf("keyed after the appends: key %q, %d rows, want %q, 6", got, n, key)
	}

	if err := copyJob("keyed", other, bigquery.WriteTruncate); err != nil {
		t.Fatalf("WRITE_TRUNCATE with another key: %v", err)
	}
	if got := keyOf("keyed"); got != other {
		t.Errorf("keyed's key after WRITE_TRUNCATE %q, want %q", got, other)
	}
	if err := copyJob("keyed", "", bigquery.WriteTruncate); err != nil {
		t.Fatalf("WRITE_TRUNCATE with no key: %v", err)
	}
	if got := keyOf("keyed"); got != "" {
		t.Errorf("keyed's key after WRITE_TRUNCATE with none %q, want default encryption", got)
	}
	if err := copyJob("plain", key, bigquery.WriteTruncate); err != nil {
		t.Fatalf("WRITE_TRUNCATE with a key into default encryption: %v", err)
	}
	if got := keyOf("plain"); got != key {
		t.Errorf("plain's key after WRITE_TRUNCATE %q, want %q", got, key)
	}

	far := "projects/" + h.Project() + "/locations/europe-west1/keyRings/ring/cryptoKeys/one"
	if err := copyJob("far", far, ""); err == nil || !strings.Contains(err.Error(), "This region is not supported") {
		t.Errorf("copy with a key of another region: %v, want the job to fail", err)
	}
	if _, err := ds.Table("far").Metadata(ctx); err == nil {
		t.Errorf("the failed copy made far")
	}
	global := "projects/" + h.Project() + "/locations/global/keyRings/ring/cryptoKeys/one"
	if err := copyJob("global", global, ""); err != nil {
		t.Fatalf("copy with a global key: %v", err)
	}
	if got := keyOf("global"); got != global {
		t.Errorf("global's key %q, want %q", got, global)
	}
	if err := copyJob("version", key+"/cryptoKeyVersions/1", ""); err == nil || !strings.Contains(err.Error(), "cryptoKeyVersions") {
		t.Errorf("copy with a key version: %v, want it refused", err)
	}
}
