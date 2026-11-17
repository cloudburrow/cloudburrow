//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/googleapi"
)

// TestBigQueryLoadHiveCSVAndManifests (#1547): with the official Go
// client, a CSV load from Cloud Storage with hivePartitioningOptions loads
// each file's keys as columns (a key the schema lacks after its columns,
// one it names where it names it, typed by it), the job answering with its
// hivePartitioningOptions; a Parquet and an Avro hive-partitioned load take
// a schema that is the file's, with or without the keys. A load with
// fileSetSpecType FILE_SET_SPEC_TYPE_NEW_LINE_DELIMITED_MANIFEST loads the
// files its manifest lists (the generated client: the official one has no
// fileSetSpecType on a load); a wildcard source URI or listed URI and
// another fileSetSpecType are 400, a listed object that does not exist
// 404.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.tables.get, bigquery.jobs.query
func TestBigQueryLoadHiveCSVAndManifests(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	bucket := loadBucket(t, h, "bq-hive-csv", map[string]string{
		"t/dt=2024-01-02/n=7/a.csv": "v,w\nx,1\ny,2\n",
		"t/dt=2024-01-03/n=8/b.csv": "v,w\nz,3\n",
		"m/a.csv":                   "a,1\n",
		"m/b.csv":                   "b,2\nc,3\n",
	})
	gs := "gs://" + bucket.BucketName() + "/"
	put := func(name, data string) {
		w := bucket.Object(name).NewWriter(ctx)
		if _, err := w.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = bucket.Object(name).Delete(context.Background()) })
	}
	put("manifest.txt", gs+"m/a.csv\n"+gs+"m/b.csv\n")
	put("wild.txt", gs+"m/*.csv\n")
	put("gone.txt", gs+"m/none.csv\n")
	put("pq/dt=2024-01-02/a.parquet", arrowParquet(t, arrow.NewSchema([]arrow.Field{{Name: "v", Type: arrow.PrimitiveTypes.Int64, Nullable: true}}, nil),
		func(b *array.RecordBuilder) { b.Field(0).(*array.Int64Builder).Append(5) }))
	put("av/k=1/a.avro", avroFile(t, `{"type":"record","name":"R","fields":[{"name":"v","type":"long"}]}`, "", map[string]any{"v": int64(6)}))
	q := func(sql string) string {
		return fmt.Sprint(rows(t, ctx, c, strings.ReplaceAll(sql, "DS.", ds.DatasetID+".")))
	}
	columns := func(table string) string {
		t.Helper()
		md, err := ds.Table(table).Metadata(ctx)
		if err != nil {
			t.Fatalf("tables.get %s: %v", table, err)
		}
		var out []string
		for _, f := range md.Schema {
			out = append(out, f.Name+":"+string(f.Type))
		}
		return strings.Join(out, ",")
	}
	want := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s: got %s want %s", what, got, want)
		}
	}
	load := func(table, format string, uri string, schema bigquery.Schema, mode bigquery.HivePartitioningMode, prefix string) (*bigquery.Job, error) {
		g := bigquery.NewGCSReference(gs + uri)
		g.SourceFormat = bigquery.DataFormat(format)
		g.Schema = schema
		if format == "CSV" {
			g.SkipLeadingRows = 1
		}
		l := ds.Table(table).LoaderFrom(g)
		l.HivePartitioningOptions = &bigquery.HivePartitioningOptions{Mode: mode, SourceURIPrefix: gs + prefix}
		lctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		job, err := l.Run(lctx)
		if err != nil {
			return nil, err
		}
		st, err := job.Wait(lctx)
		if err != nil {
			return nil, err
		}
		return job, st.Err()
	}

	// CSV, AUTO, a schema without the keys: they follow its columns.
	job, err := load("csvauto", "CSV", "t/*", bigquery.Schema{{Name: "v", Type: bigquery.StringFieldType},
		{Name: "w", Type: bigquery.IntegerFieldType}}, bigquery.AutoHivePartitioningMode, "t")
	if err != nil {
		t.Fatalf("CSV AUTO: %v", err)
	}
	want("CSV AUTO's columns", columns("csvauto"), "v:STRING,w:INTEGER,dt:DATE,n:INTEGER")
	want("CSV AUTO's rows", q("SELECT v, w, CAST(dt AS STRING), n FROM DS.csvauto ORDER BY v"),
		"[[x 1 2024-01-02 7] [y 2 2024-01-02 7] [z 3 2024-01-03 8]]")
	if lc, ok := job.LastStatus().Statistics.Details.(*bigquery.LoadStatistics); !ok || lc.OutputRows != 3 || lc.InputFiles != 2 {
		t.Errorf("CSV AUTO's statistics: %+v", job.LastStatus().Statistics.Details)
	}
	if cfg, err := job.Config(); err != nil {
		t.Errorf("CSV AUTO's configuration: %v", err)
	} else if lc, ok := cfg.(*bigquery.LoadConfig); !ok || lc.HivePartitioningOptions == nil ||
		lc.HivePartitioningOptions.Mode != bigquery.AutoHivePartitioningMode {
		t.Errorf("CSV AUTO's job answers without its hivePartitioningOptions: %+v", cfg)
	}

	// CSV, a schema naming a key between the file's columns, typing it.
	if _, err := load("csvnamed", "CSV", "t/*", bigquery.Schema{{Name: "v", Type: bigquery.StringFieldType},
		{Name: "n", Type: bigquery.StringFieldType}, {Name: "w", Type: bigquery.IntegerFieldType}},
		bigquery.AutoHivePartitioningMode, "t/"); err != nil {
		t.Fatalf("CSV with a key in its schema: %v", err)
	}
	want("CSV named key's columns", columns("csvnamed"), "v:STRING,n:STRING,w:INTEGER,dt:DATE")
	want("CSV named key's rows", q("SELECT v, n, w FROM DS.csvnamed WHERE v = 'z'"), "[[z 8 3]]")

	// Parquet and Avro with a schema: the file's, with or without the keys.
	if _, err := load("pqschema", "PARQUET", "pq/*", bigquery.Schema{{Name: "v", Type: bigquery.IntegerFieldType}},
		bigquery.AutoHivePartitioningMode, "pq"); err != nil {
		t.Fatalf("Parquet with a schema: %v", err)
	}
	want("Parquet with a schema", q("SELECT v, CAST(dt AS STRING) FROM DS.pqschema"), "[[5 2024-01-02]]")
	if _, err := load("avschema", "AVRO", "av/*", bigquery.Schema{{Name: "v", Type: bigquery.IntegerFieldType},
		{Name: "k", Type: bigquery.StringFieldType}}, bigquery.StringHivePartitioningMode, "av"); err != nil {
		t.Fatalf("Avro with a schema with its key: %v", err)
	}
	want("Avro with a schema's columns", columns("avschema"), "v:INTEGER,k:STRING")
	want("Avro with a schema", q("SELECT v, k FROM DS.avschema"), "[[6 1]]")

	// Manifests, by the generated client.
	svc := generatedBigQuery(t, h)
	manifest := func(table, spec string, uris ...string) (*bq.Job, error) {
		l := &bq.JobConfigurationLoad{SourceFormat: "CSV", SourceUris: uris, FileSetSpecType: spec,
			DestinationTable: &bq.TableReference{ProjectId: project, DatasetId: ds.DatasetID, TableId: table},
			Schema:           &bq.TableSchema{Fields: []*bq.TableFieldSchema{{Name: "s", Type: "STRING"}, {Name: "i", Type: "INTEGER"}}}}
		got, err := svc.Jobs.Insert(project, &bq.Job{Configuration: &bq.JobConfiguration{Load: l}}).Context(ctx).Do()
		if err != nil {
			return nil, err
		}
		waitGeneratedJob(t, h, svc, project, got.JobReference.JobId)
		return svc.Jobs.Get(project, got.JobReference.JobId).Context(ctx).Do()
	}
	const spec = "FILE_SET_SPEC_TYPE_NEW_LINE_DELIMITED_MANIFEST"
	j, err := manifest("manifest", spec, gs+"manifest.txt")
	if err != nil {
		t.Fatalf("a manifest load: %v", err)
	}
	if j.Status.ErrorResult != nil {
		t.Fatalf("a manifest load failed: %+v", j.Status.ErrorResult)
	}
	want("a manifest's rows", q("SELECT s, i FROM DS.manifest ORDER BY s"), "[[a 1] [b 2] [c 3]]")
	if l := j.Configuration.Load; l.FileSetSpecType != spec || fmt.Sprint(l.SourceUris) != "["+gs+"manifest.txt]" {
		t.Errorf("the manifest job's load: fileSetSpecType %q, sourceUris %v", l.FileSetSpecType, l.SourceUris)
	}
	if j.Statistics == nil || j.Statistics.Load == nil || j.Statistics.Load.InputFiles != 2 || j.Statistics.Load.OutputRows != 3 {
		t.Errorf("the manifest job's statistics: %+v", j.Statistics)
	}
	if _, err := manifest("manifestfs", "FILE_SET_SPEC_TYPE_FILE_SYSTEM_MATCH", gs+"m/*"); err != nil {
		t.Errorf("FILE_SET_SPEC_TYPE_FILE_SYSTEM_MATCH: %v", err)
	}
	for _, tc := range []struct {
		what string
		code int
		spec string
		uri  string
	}{
		{"a wildcard manifest URI", http.StatusBadRequest, spec, gs + "*.txt"},
		{"a wildcard in the manifest", http.StatusBadRequest, spec, gs + "wild.txt"},
		{"a listed object that does not exist", http.StatusNotFound, spec, gs + "gone.txt"},
		{"another fileSetSpecType", http.StatusBadRequest, "FILE_SET_SPEC_TYPE_NONE", gs + "manifest.txt"},
	} {
		_, err := manifest("refused", tc.spec, tc.uri)
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != tc.code {
			t.Errorf("%s: got %v, want %d", tc.what, err, tc.code)
		}
	}
}

// TestConsoleBigQueryLoadHiveCSVAndManifest (#1547): Load from Cloud
// Storage in the console loads hive-partitioned CSV with a schema (AUTO,
// its prefix), and the files a manifest lists ("Source URIs are"
// FILE_SET_SPEC_TYPE_NEW_LINE_DELIMITED_MANIFEST), read back with the
// official Go client.
//
// covers: bigquery.jobs.insert, bigquery.tables.get
func TestConsoleBigQueryLoadHiveCSVAndManifest(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	bucket := loadBucket(t, h, "bq-console-manifest", map[string]string{
		"t/dt=2024-01-02/a.csv": "x,1\n",
		"m/a.csv":               "y,2\n",
	})
	gs := "gs://" + bucket.BucketName() + "/"
	w := bucket.Object("manifest.txt").NewWriter(ctx)
	if _, err := w.Write([]byte(gs + "m/a.csv\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bucket.Object("manifest.txt").Delete(context.Background()) })
	schema := `[{"name":"v","type":"STRING","mode":"NULLABLE"},{"name":"n","type":"INTEGER","mode":"NULLABLE"}]`
	for _, tc := range []struct {
		table  string
		values map[string]string
		sql    string
		want   string
	}{
		{"hivecsv", map[string]string{"uris": gs + "t/*", "hiveMode": "AUTO", "hiveSourceUriPrefix": gs + "t"},
			"SELECT v, n, CAST(dt AS STRING) FROM DS.hivecsv", "[[x 1 2024-01-02]]"},
		{"manifest", map[string]string{"uris": gs + "manifest.txt", "fileSetSpecType": "FILE_SET_SPEC_TYPE_NEW_LINE_DELIMITED_MANIFEST"},
			"SELECT v, n FROM DS.manifest", "[[y 2]]"},
	} {
		tc.values["tableId"], tc.values["format"], tc.values["schema"] = tc.table, "CSV", schema
		tc.values["writeDisposition"] = "WRITE_APPEND"
		_, fields, _ := consoleJobResult(t, addr, project, []string{ds.DatasetID}, "load", tc.values)
		if fields["State"] != "Succeeded" || fields["Output rows"] != "1" {
			t.Errorf("%s: the console's load answered %v", tc.table, fields)
		}
		if got := fmt.Sprint(rows(t, ctx, c, strings.ReplaceAll(tc.sql, "DS.", ds.DatasetID+"."))); got != tc.want {
			t.Errorf("%s: got %s want %s", tc.table, got, tc.want)
		}
	}
}
