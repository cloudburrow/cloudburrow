//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/googleapi"
)

// TestBigQueryLoadMakesItsTableWithItsProperties (#1457): a load that
// makes its table makes it with the job's timePartitioning,
// rangePartitioning, clustering and destinationTableProperties, which the
// pinned emulator dropped (measured: a JSON load with each read back with
// none): a JSON upload with a schema, a CSV with autodetect, a Parquet
// load (ingestion-time partitioned) and an Avro one (from Cloud Storage),
// WRITE_TRUNCATE into a new table too; a load that fails leaves no table;
// a table that exists keeps its own properties. ORC, DATASTORE_BACKUP,
// hivePartitioningOptions and jsonExtension GEOJSON are 501.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.tables.get, bigquery.jobs.query
func TestBigQueryLoadMakesItsTableWithItsProperties(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	q := func(sql string) string {
		return fmt.Sprint(rows(t, ctx, c, strings.ReplaceAll(sql, "DS.", ds.DatasetID+".")))
	}
	meta := func(name string) *bigquery.TableMetadata {
		t.Helper()
		md, err := ds.Table(name).Metadata(ctx)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return md
	}

	// JSON with a schema, a column-partitioned and clustered table, with a
	// description, friendly name and labels (the generated client: the
	// official one has no destinationTableProperties), by WRITE_TRUNCATE.
	svc := generatedBigQuery(t, h)
	job := &bq.Job{Configuration: &bq.JobConfiguration{Load: &bq.JobConfigurationLoad{
		SourceFormat:     "NEWLINE_DELIMITED_JSON",
		DestinationTable: &bq.TableReference{ProjectId: project, DatasetId: ds.DatasetID, TableId: "byday"},
		Schema: &bq.TableSchema{Fields: []*bq.TableFieldSchema{{Name: "a", Type: "INTEGER"}, {Name: "d", Type: "DATE"},
			{Name: "s", Type: "STRING"}}},
		TimePartitioning:           &bq.TimePartitioning{Type: "DAY", Field: "d"},
		Clustering:                 &bq.Clustering{Fields: []string{"s"}},
		WriteDisposition:           "WRITE_TRUNCATE",
		DestinationTableProperties: &bq.DestinationTableProperties{Description: "loaded", FriendlyName: "By day", Labels: map[string]string{"k": "v"}},
	}}}
	got, err := svc.Jobs.Insert(project, job).Media(strings.NewReader(`{"a":1,"d":"2024-01-01","s":"x"}`+"\n"+
		`{"a":2,"d":"2024-01-02","s":"y"}`+"\n")).Context(ctx).Do()
	if err != nil {
		t.Fatalf("JSON load: %v", err)
	}
	waitGeneratedJob(t, h, svc, project, got.JobReference.JobId)
	md := meta("byday")
	if md.TimePartitioning == nil || md.TimePartitioning.Field != "d" || md.TimePartitioning.Type != bigquery.DayPartitioningType ||
		md.Clustering == nil || fmt.Sprint(md.Clustering.Fields) != "[s]" || md.Description != "loaded" || md.Name != "By day" ||
		md.Labels["k"] != "v" {
		t.Errorf("the JSON load's table: %+v %+v %q %q %v", md.TimePartitioning, md.Clustering,
			md.Description, md.Name, md.Labels)
	}
	if got, want := q("SELECT a FROM DS.`byday$20240102`"), "[[2]]"; got != want {
		t.Errorf("a partition of the loaded table: got %s want %s", got, want)
	}

	// CSV with autodetect into an integer-range partitioned table.
	csv := bigquery.NewReaderSource(strings.NewReader("n,s\n1,a\n15,b\n"))
	csv.AutoDetect, csv.SkipLeadingRows = true, 1
	l := ds.Table("byrange").LoaderFrom(csv)
	l.RangePartitioning = &bigquery.RangePartitioning{Field: "n", Range: &bigquery.RangePartitioningRange{Start: 0, End: 100, Interval: 10}}
	if err := runLoad(ctx, l); err != nil {
		t.Fatalf("CSV load: %v", err)
	}
	if md := meta("byrange"); md.RangePartitioning == nil || md.RangePartitioning.Field != "n" || md.RangePartitioning.Range.Interval != 10 {
		t.Errorf("the CSV load's table: %+v", md.RangePartitioning)
	}
	if got, want := q("SELECT s FROM DS.`byrange$10`"), "[[b]]"; got != want {
		t.Errorf("a range partition: got %s want %s", got, want)
	}

	// Parquet into an ingestion-time partitioned table.
	l = ds.Table("pq").LoaderFrom(parquetUpload(parquetFixture(t, "ab.parquet"), nil))
	l.TimePartitioning = &bigquery.TimePartitioning{Type: bigquery.DayPartitioningType}
	if err := runLoad(ctx, l); err != nil {
		t.Fatalf("Parquet load: %v", err)
	}
	if md := meta("pq"); md.TimePartitioning == nil || md.TimePartitioning.Field != "" {
		t.Errorf("the Parquet load's table: %+v", md.TimePartitioning)
	}
	if got := q("SELECT COUNT(*) FROM DS.pq WHERE _PARTITIONTIME IS NOT NULL"); got == "[[0]]" {
		t.Errorf("the Parquet rows have no _PARTITIONTIME: %s", got)
	}

	// Avro from Cloud Storage, clustered.
	bucket := loadBucket(t, h, "bq-loadtable", map[string]string{
		"a.avro": avroFile(t, `{"type":"record","name":"R","fields":[{"name":"k","type":"string"},{"name":"v","type":"long"}]}`,
			"", map[string]any{"k": "a", "v": int64(1)}),
	})
	g := bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/a.avro")
	g.SourceFormat = bigquery.Avro
	l = ds.Table("av").LoaderFrom(g)
	l.Clustering = &bigquery.Clustering{Fields: []string{"k"}}
	if err := runLoad(ctx, l); err != nil {
		t.Fatalf("Avro load: %v", err)
	}
	if md := meta("av"); md.Clustering == nil || fmt.Sprint(md.Clustering.Fields) != "[k]" {
		t.Errorf("the Avro load's table: %+v", md.Clustering)
	}

	// A load that fails leaves no table.
	bad := bigquery.NewReaderSource(strings.NewReader(`{"a":"not a number"}` + "\n"))
	bad.SourceFormat = bigquery.JSON
	bad.Schema = bigquery.Schema{{Name: "a", Type: bigquery.IntegerFieldType}}
	l = ds.Table("failed").LoaderFrom(bad)
	l.Clustering = &bigquery.Clustering{Fields: []string{"a"}}
	if err := runLoad(ctx, l); err == nil {
		t.Error("a load of a bad value succeeded")
	}
	if _, err := ds.Table("failed").Metadata(ctx); err == nil {
		t.Error("a load that failed left its table")
	}

	// A table that exists keeps its own properties.
	keep := ds.Table("keep")
	if err := keep.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "a", Type: bigquery.IntegerFieldType}},
		Description: "mine"}); err != nil {
		t.Fatal(err)
	}
	src := bigquery.NewReaderSource(strings.NewReader(`{"a":5}` + "\n"))
	src.SourceFormat = bigquery.JSON
	src.Schema = bigquery.Schema{{Name: "a", Type: bigquery.IntegerFieldType}}
	l = keep.LoaderFrom(src)
	l.Clustering = &bigquery.Clustering{Fields: []string{"a"}}
	if err := runLoad(ctx, l); err != nil {
		t.Fatalf("load into an existing table: %v", err)
	}
	if md := meta("keep"); md.Clustering != nil || md.Description != "mine" {
		t.Errorf("the existing table: %+v %q", md.Clustering, md.Description)
	}

	// 501: ORC, DATASTORE_BACKUP, hivePartitioningOptions, GEOJSON.
	for _, tc := range []struct {
		what string
		load *bq.JobConfigurationLoad
	}{
		{"ORC", &bq.JobConfigurationLoad{SourceFormat: "ORC"}},
		{"DATASTORE_BACKUP", &bq.JobConfigurationLoad{SourceFormat: "DATASTORE_BACKUP"}},
		{"hive", &bq.JobConfigurationLoad{SourceFormat: "NEWLINE_DELIMITED_JSON",
			HivePartitioningOptions: &bq.HivePartitioningOptions{Mode: "AUTO", SourceUriPrefix: "gs://" + bucket.BucketName() + "/"}}},
		{"GEOJSON", &bq.JobConfigurationLoad{SourceFormat: "NEWLINE_DELIMITED_JSON", JsonExtension: "GEOJSON"}},
	} {
		tc.load.DestinationTable = &bq.TableReference{ProjectId: project, DatasetId: ds.DatasetID, TableId: "refused"}
		tc.load.SourceUris = []string{"gs://" + bucket.BucketName() + "/a.avro"}
		_, err := svc.Jobs.Insert(project, &bq.Job{Configuration: &bq.JobConfiguration{Load: tc.load}}).Context(ctx).Do()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != http.StatusNotImplemented {
			t.Errorf("%s: got %v, want 501", tc.what, err)
		}
	}
}

// waitGeneratedJob waits for a job run through the generated client to be
// done, and fails the test if it failed.
func waitGeneratedJob(t *testing.T, h *Harness, svc *bq.Service, project, id string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		j, err := svc.Jobs.Get(project, id).Context(h.Context()).Do()
		if err != nil {
			t.Fatalf("jobs.get %s: %v", id, err)
		}
		if j.Status != nil && j.Status.State == "DONE" {
			if j.Status.ErrorResult != nil {
				t.Fatalf("job %s: %+v", id, j.Status.ErrorResult)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s not done", id)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
