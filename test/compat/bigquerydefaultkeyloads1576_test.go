//go:build compat

package compat

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
)

// TestBigQueryDefaultKeyOfResumableAndCloudStorageLoads (#1576): the two
// load paths TestBigQueryDefaultKeyOfScriptAndUploadTables leaves out. With
// the region's default_kms_key_name set, the new destination table of a
// load sent through a resumable upload (the official client, chunked at
// 256 KiB, given a file of three chunks) and of a load from Cloud Storage
// (jobs.insert as JSON) takes the key, as a multipart upload's does.
// covers: bigquery.jobs.insert
func TestBigQueryDefaultKeyOfResumableAndCloudStorageLoads(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	var csv strings.Builder
	for n := 1; csv.Len() < 3*googleapi.MinUploadChunkSize; n++ {
		fmt.Fprintf(&csv, "%d\n", n)
	}
	bucket := loadBucket(t, h, "bq-key", map[string]string{"rows.csv": "1\n2\n3\n"})
	ctx := h.Context()
	const region = "US"
	const r = "`region-us."
	exec := func(sql string) {
		t.Helper()
		q := c.Query(sql)
		q.Location = region
		if _, err := q.Read(ctx); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		q := c.Query("ALTER PROJECT SET OPTIONS (" + r + "default_kms_key_name` = NULL)")
		q.Location = region
		_, _ = q.Read(ctx)
	})
	ds := c.Dataset(strings.ReplaceAll(project, "-", "_") + "_loadkeys")
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{Location: region}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(ctx) })
	key := "projects/" + project + "/locations/us/keyRings/r/cryptoKeys/k"
	exec("ALTER PROJECT SET OPTIONS (" + r + "default_kms_key_name` = '" + key + "')")
	schema := bigquery.Schema{{Name: "a", Type: bigquery.IntegerFieldType}}
	keyOf := func(table string) string {
		t.Helper()
		md, err := ds.Table(table).Metadata(ctx)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if md.EncryptionConfig == nil {
			return ""
		}
		return md.EncryptionConfig.KMSKeyName
	}

	src := bigquery.NewReaderSource(strings.NewReader(csv.String()))
	src.Schema = schema
	ld := ds.Table("resumable").LoaderFrom(src)
	ld.Location = region
	ld.MediaOptions = []googleapi.MediaOption{googleapi.ChunkSize(googleapi.MinUploadChunkSize)}
	if err := runLoad(ctx, ld); err != nil {
		t.Fatalf("a resumable upload load: %v", err)
	}
	if got := keyOf("resumable"); got != key {
		t.Errorf("a resumable upload load's new destination table: key %q, want %q", got, key)
	}
	if got := rows(t, ctx, c, "SELECT COUNT(*) FROM `"+ds.DatasetID+".resumable`"); len(got) != 1 || got[0][0].(int64) != int64(strings.Count(csv.String(), "\n")) {
		t.Errorf("the resumable load holds %v rows, want %d", got, strings.Count(csv.String(), "\n"))
	}

	gcs := bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/rows.csv")
	gcs.Schema = schema
	ld = ds.Table("fromgcs").LoaderFrom(gcs)
	ld.Location = region
	if err := runLoad(ctx, ld); err != nil {
		t.Fatalf("a load from Cloud Storage: %v", err)
	}
	if got := keyOf("fromgcs"); got != key {
		t.Errorf("a Cloud Storage load's new destination table: key %q, want %q", got, key)
	}
}

// TestBigQueryAlterSchemaSetsEveryOption (#1576): a lone ALTER SCHEMA …
// SET OPTIONS with the options other than max_time_travel_hours
// (TestBigQueryAlterSchemaTimeTravelLetsGoFirst's), through jobs.query and
// jobs.insert, is read back by datasets.get as the Dataset fields they
// set; an option set to NULL is cleared and labels replace the dataset's;
// IF EXISTS of a dataset that does not exist does nothing, and without it
// the query fails notFound (404 through jobs.query).
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.datasets.get
func TestBigQueryAlterSchemaSetsEveryOption(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	for _, insert := range []bool{false, true} {
		t.Run(fmt.Sprint("insert=", insert), func(t *testing.T) {
			run := func(sql string) error {
				t.Helper()
				q := c.Query(sql)
				if !insert {
					_, err := q.Read(ctx)
					return err
				}
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
			id := fmt.Sprintf("%s_alter_%v", strings.ReplaceAll(project, "-", "_"), insert)
			ds := c.Dataset(id)
			if err := ds.Create(ctx, &bigquery.DatasetMetadata{Labels: map[string]string{"old": "1"}}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ds.DeleteWithContents(ctx) })
			key := "projects/" + project + "/locations/us/keyRings/r/cryptoKeys/k"
			if err := run("ALTER SCHEMA " + id + " SET OPTIONS (description = 'altered', friendly_name = 'Altered', " +
				"labels = [('team', 'a'), ('tier', 'b')], default_table_expiration_days = 2, " +
				"default_partition_expiration_days = 1.5, default_kms_key_name = '" + key + "', storage_billing_model = 'PHYSICAL')"); err != nil {
				t.Fatalf("ALTER SCHEMA: %v", err)
			}
			md, err := ds.Metadata(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if md.Description != "altered" || md.Name != "Altered" || len(md.Labels) != 2 || md.Labels["team"] != "a" || md.Labels["tier"] != "b" ||
				md.DefaultTableExpiration != 48*time.Hour || md.DefaultPartitionExpiration != 36*time.Hour ||
				md.DefaultEncryptionConfig == nil || md.DefaultEncryptionConfig.KMSKeyName != key || md.StorageBillingModel != "PHYSICAL" {
				t.Errorf("datasets.get after ALTER SCHEMA = description %q, name %q, labels %v, table exp %v, partition exp %v, key %+v, billing %q",
					md.Description, md.Name, md.Labels, md.DefaultTableExpiration, md.DefaultPartitionExpiration, md.DefaultEncryptionConfig, md.StorageBillingModel)
			}
			if err := run("ALTER SCHEMA " + id + " SET OPTIONS (description = NULL, default_table_expiration_days = NULL)"); err != nil {
				t.Fatalf("ALTER SCHEMA to NULL: %v", err)
			}
			if md, err = ds.Metadata(ctx); err != nil || md.Description != "" || md.DefaultTableExpiration != 0 || md.Name != "Altered" {
				t.Errorf("after NULL: description %q, table exp %v, name %q (%v); want the two cleared and the name kept", md.Description, md.DefaultTableExpiration, md.Name, err)
			}
			if err := run("ALTER SCHEMA IF EXISTS " + id + "_none SET OPTIONS (description = 'x')"); err != nil {
				t.Errorf("ALTER SCHEMA IF EXISTS of a missing dataset = %v, want nothing done", err)
			}
			// jobs.query answers 404; a query job fails with the reason
			// notFound (the HTTP code the client's wait then reads is the
			// front's for every job that fails before it runs).
			var gerr *googleapi.Error
			err = run("ALTER SCHEMA " + id + "_none SET OPTIONS (description = 'x')")
			if !errors.As(err, &gerr) || (!insert && gerr.Code != 404) || !strings.Contains(err.Error(), "notFound") ||
				!strings.Contains(err.Error(), "Not found: Dataset") {
				t.Errorf("ALTER SCHEMA of a missing dataset = %v, want notFound (404 through jobs.query)", err)
			}
		})
	}
}
