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

// TestBigQueryCopyKeyPartition1822: a copy job's
// destinationEncryptionConfiguration with a $ partition decorator, as
// managing-partitioned-tables documents a partition's copy (read
// 2026-11-18: "--destination_kms_key is the customer-managed Cloud KMS key
// used to encrypt the destination table or partition"):
//   - a copy of a partition into a new table gives the table the key; into
//     a table encrypted otherwise, WRITE_APPEND fails the job ("Existing
//     table encryption settings don't match") and WRITE_TRUNCATE gives the
//     table the key, as a copy of a table (#1762);
//   - a copy into a partition with the table's own key writes the
//     partition; with another key, or a key into a table of default
//     encryption, it fails the job under every writeDisposition, the
//     partition unchanged, as a partition's WRITE_TRUNCATE does not replace
//     the table; with no key given, a WRITE_TRUNCATE of the partition keeps
//     the table's key;
//   - a key of another region fails the job.
//
// Before #1822, every copy with a key and a decorator was 501.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.tables.get
func TestBigQueryCopyKeyPartition1822(t *testing.T) {
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
	rows := func(table, where string) []string {
		t.Helper()
		got := queryRows(t, ctx, c, "", "", "SELECT CAST(n AS STRING) FROM "+p(table)+" WHERE "+where)
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
	copyJob := func(src, dst, key string, write bigquery.TableWriteDisposition) error {
		t.Helper()
		cp := ds.Table(dst).CopierFrom(ds.Table(src))
		cp.WriteDisposition = write
		if key != "" {
			cp.DestinationEncryptionConfig = &bigquery.EncryptionConfig{KMSKeyName: key}
		}
		j, err := cp.Run(ctx)
		return waitJob(ctx, j, err)
	}
	mismatch := func(what string, err error) {
		t.Helper()
		var bqErr *bigquery.Error
		if err == nil || !strings.Contains(err.Error(), "Existing table encryption settings don't match encryption settings specified in the request") ||
			!errors.As(err, &bqErr) || bqErr.Reason != "invalid" {
			t.Errorf("%s: %v, want the job to fail", what, err)
		}
	}
	key := "projects/" + h.Project() + "/locations/us/keyRings/ring/cryptoKeys/one"
	other := "projects/" + h.Project() + "/locations/us/keyRings/ring/cryptoKeys/two"
	far := "projects/" + h.Project() + "/locations/europe-west1/keyRings/ring/cryptoKeys/one"
	day1 := "d = '2018-01-30'"
	day2 := "d = '2018-01-31'"

	exec("CREATE TABLE " + p("src") + " (n INT64, d DATE) PARTITION BY d")
	exec("INSERT INTO " + p("src") + " (n, d) VALUES (1, '2018-01-30'), (2, '2018-01-30'), (3, '2018-01-31')")

	// A partition's copy into a table.
	if err := copyJob("src$20180130", "fromday", key, ""); err != nil {
		t.Fatalf("copy of a partition into a new table with a key: %v", err)
	}
	if got := keyOf("fromday"); got != key {
		t.Errorf("fromday's key %q, want %q", got, key)
	}
	if got := rows("fromday", "TRUE"); !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Errorf("fromday rows %v, want 1 2", got)
	}
	mismatch("copy of a partition, WRITE_APPEND with another key", copyJob("src$20180131", "fromday", other, bigquery.WriteAppend))
	if err := copyJob("src$20180131", "fromday", key, bigquery.WriteAppend); err != nil {
		t.Fatalf("copy of a partition, WRITE_APPEND with the same key: %v", err)
	}
	if got := rows("fromday", "TRUE"); !reflect.DeepEqual(got, []string{"1", "2", "3"}) {
		t.Errorf("fromday rows after the append %v, want 1 2 3", got)
	}
	if err := copyJob("src$20180131", "fromday", other, bigquery.WriteTruncate); err != nil {
		t.Fatalf("copy of a partition, WRITE_TRUNCATE with another key: %v", err)
	}
	if got, n := keyOf("fromday"), rows("fromday", "TRUE"); got != other || !reflect.DeepEqual(n, []string{"3"}) {
		t.Errorf("fromday after WRITE_TRUNCATE: key %q, rows %v, want %q, 3", got, n, other)
	}

	// A copy into a partition of a table with a key.
	exec("CREATE TABLE " + p("dest") + " (n INT64, d DATE) PARTITION BY d OPTIONS (kms_key_name = '" + key + "')")
	if got := keyOf("dest"); got != key {
		t.Fatalf("dest's key %q, want %q", got, key)
	}
	if err := copyJob("src$20180130", "dest$20180130", key, ""); err != nil {
		t.Fatalf("copy into a partition with the table's key: %v", err)
	}
	if got := rows("dest", day1); !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Errorf("dest$20180130 rows %v, want 1 2", got)
	}
	for _, w := range []bigquery.TableWriteDisposition{bigquery.WriteAppend, bigquery.WriteTruncate} {
		mismatch("copy into a partition, "+string(w)+" with another key", copyJob("src$20180130", "dest$20180130", other, w))
	}
	mismatch("copy into an empty partition, WRITE_EMPTY with another key", copyJob("src$20180131", "dest$20180131", other, ""))
	if got, n := keyOf("dest"), rows("dest", "TRUE"); got != key || !reflect.DeepEqual(n, []string{"1", "2"}) {
		t.Errorf("dest after the failed copies: key %q, rows %v, want %q, 1 2", got, n, key)
	}
	if err := copyJob("src$20180131", "dest$20180131", key, bigquery.WriteAppend); err != nil {
		t.Fatalf("copy into a partition, WRITE_APPEND with the table's key: %v", err)
	}
	if err := copyJob("src$20180131", "dest$20180131", "", bigquery.WriteTruncate); err != nil {
		t.Fatalf("copy into a partition, WRITE_TRUNCATE with no key: %v", err)
	}
	if got, n := keyOf("dest"), rows("dest", day2); got != key || !reflect.DeepEqual(n, []string{"3"}) {
		t.Errorf("dest after WRITE_TRUNCATE of a partition with no key: key %q, rows %v, want %q, 3", got, n, key)
	}
	if err := copyJob("src$20180130", "dest$20180130", key, bigquery.WriteTruncate); err != nil {
		t.Fatalf("copy into a partition, WRITE_TRUNCATE with the table's key: %v", err)
	}
	if got := rows("dest", day1); !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Errorf("dest$20180130 rows after WRITE_TRUNCATE %v, want 1 2", got)
	}

	// Into a partition of a table of default encryption.
	exec("CREATE TABLE " + p("plain") + " (n INT64, d DATE) PARTITION BY d")
	mismatch("copy into a partition of a table of default encryption with a key", copyJob("src$20180130", "plain$20180130", key, bigquery.WriteTruncate))
	if got, n := keyOf("plain"), rows("plain", "TRUE"); got != "" || len(n) != 0 {
		t.Errorf("plain after the failed copy: key %q, rows %v, want default encryption, none", got, n)
	}

	// A key of another region.
	if err := copyJob("src$20180130", "dest$20180130", far, bigquery.WriteAppend); err == nil || !strings.Contains(err.Error(), "This region is not supported") {
		t.Errorf("copy into a partition with a key of another region: %v, want the job to fail", err)
	}
	if err := copyJob("src$20180130", "farday", far, ""); err == nil || !strings.Contains(err.Error(), "This region is not supported") {
		t.Errorf("copy of a partition with a key of another region: %v, want the job to fail", err)
	}
	if _, err := ds.Table("farday").Metadata(ctx); err == nil {
		t.Errorf("the failed copy made farday")
	}
}
