//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/googleapi"
)

// TestBigQueryHivePartitionedAndGeoJSONLoads (#1547): a
// NEWLINE_DELIMITED_JSON load from Cloud Storage with
// hivePartitioningOptions loads each file's keys as columns (AUTO typing
// them INTEGER, DATE, TIMESTAMP or STRING, STRINGS as STRING, CUSTOM as
// the prefix encodes them, into a given schema too); a key layout that is
// not key=value, keys in another order, a value that is not CUSTOM's
// type, a file column that is a key, requirePartitionFilter and a missing
// prefix are 400; CSV with hive partitioning is 501. A Parquet load adds
// the keys after the file's columns. jsonExtension
// GEOJSON loads Features, by autodetect and with a schema, from Cloud
// Storage and as an upload; a Feature that is not one is 400, as is
// GEOJSON with CSV. The generated client: the official one has no
// jsonExtension on a load, nor hivePartitioningOptions.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.tables.get, bigquery.jobs.query
func TestBigQueryHivePartitionedAndGeoJSONLoads(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	bucket := loadBucket(t, h, "bq-hive", map[string]string{
		"t/dt=2024-01-02/n=7/a.json":            `{"v":"x"}` + "\n" + `{"v":"y"}` + "\n",
		"t/dt=2024-01-03/n=8/b.json":            `{"v":"z"}` + "\n",
		"ts/hour=2018-10-18 16:00:00+00/a.json": `{"v":"t"}` + "\n",
		"flat/2024-01-02/a.json":                `{"v":"x"}` + "\n",
		"swap/n=7/dt=2024-01-02/a.json":         `{"v":"x"}` + "\n",
		"swap/dt=2024-01-02/n=7/a.json":         `{"v":"x"}` + "\n",
		"dup/n=7/a.json":                        `{"n":1}` + "\n",
		"bad/n=x/a.json":                        `{"v":"x"}` + "\n",
		"geo/f.json": `{"type":"Feature","id":1,"geometry":{"type":"Point","coordinates":[1,2]},"properties":{"name":"a","pop":3}}` + "\n" +
			`{"type":"Feature","id":2,"geometry":null,"properties":{"name":"b","pop":4}}` + "\n",
		"geo/bad.json": `{"type":"FeatureCollection","features":[]}` + "\n",
	})
	gs := "gs://" + bucket.BucketName() + "/"
	pq := func(v int64) string {
		return arrowParquet(t, arrow.NewSchema([]arrow.Field{{Name: "v", Type: arrow.PrimitiveTypes.Int64, Nullable: true}}, nil),
			func(b *array.RecordBuilder) { b.Field(0).(*array.Int64Builder).Append(v) })
	}
	for name, data := range map[string]string{"pq/dt=2024-01-02/n=7/a.parquet": pq(1), "pq/dt=2024-01-03/n=8/b.parquet": pq(2)} {
		w := bucket.Object(name).NewWriter(ctx)
		if _, err := w.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = bucket.Object(name).Delete(context.Background()) })
	}
	svc := generatedBigQuery(t, h)
	q := func(sql string) string {
		return fmt.Sprint(rows(t, ctx, c, strings.ReplaceAll(sql, "DS.", ds.DatasetID+".")))
	}
	run := func(table string, l *bq.JobConfigurationLoad, media string) error {
		t.Helper()
		l.DestinationTable = &bq.TableReference{ProjectId: project, DatasetId: ds.DatasetID, TableId: table}
		call := svc.Jobs.Insert(project, &bq.Job{Configuration: &bq.JobConfiguration{Load: l}})
		if media != "" {
			call = call.Media(strings.NewReader(media))
		}
		got, err := call.Context(ctx).Do()
		if err != nil {
			return err
		}
		waitGeneratedJob(t, h, svc, project, got.JobReference.JobId)
		j, err := svc.Jobs.Get(project, got.JobReference.JobId).Context(ctx).Do()
		if err != nil {
			t.Fatalf("jobs.get: %v", err)
		}
		if l.HivePartitioningOptions != nil && (j.Configuration.Load.HivePartitioningOptions == nil ||
			j.Configuration.Load.HivePartitioningOptions.Mode != l.HivePartitioningOptions.Mode) {
			t.Errorf("%s: the job's hivePartitioningOptions: %+v", table, j.Configuration.Load.HivePartitioningOptions)
		}
		if l.JsonExtension != "" && j.Configuration.Load.JsonExtension != l.JsonExtension {
			t.Errorf("%s: the job's jsonExtension: %q", table, j.Configuration.Load.JsonExtension)
		}
		return nil
	}
	types := func(table string) string {
		t.Helper()
		tb, err := svc.Tables.Get(project, ds.DatasetID, table).Context(ctx).Do()
		if err != nil {
			t.Fatalf("tables.get %s: %v", table, err)
		}
		var out []string
		for _, f := range tb.Schema.Fields {
			out = append(out, f.Name+":"+f.Type)
		}
		return strings.Join(out, ",")
	}
	hive := func(mode, prefix string) *bq.HivePartitioningOptions {
		return &bq.HivePartitioningOptions{Mode: mode, SourceUriPrefix: gs + prefix}
	}
	want := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s: got %s want %s", what, got, want)
		}
	}

	// AUTO, by autodetect.
	if err := run("auto", &bq.JobConfigurationLoad{SourceFormat: "NEWLINE_DELIMITED_JSON", Autodetect: true,
		SourceUris: []string{gs + "t/*"}, HivePartitioningOptions: hive("AUTO", "t")}, ""); err != nil {
		t.Fatalf("AUTO: %v", err)
	}
	want("AUTO's columns", types("auto"), "v:STRING,dt:DATE,n:INTEGER")
	want("AUTO's rows", q("SELECT v, CAST(dt AS STRING), n FROM DS.auto ORDER BY v"),
		"[[x 2024-01-02 7] [y 2024-01-02 7] [z 2024-01-03 8]]")
	if err := run("autots", &bq.JobConfigurationLoad{SourceFormat: "NEWLINE_DELIMITED_JSON", Autodetect: true,
		SourceUris: []string{gs + "ts/*"}, HivePartitioningOptions: hive("AUTO", "ts/")}, ""); err != nil {
		t.Fatalf("AUTO TIMESTAMP: %v", err)
	}
	want("a TIMESTAMP key", types("autots"), "v:STRING,hour:TIMESTAMP")
	want("a TIMESTAMP key's value", q("SELECT UNIX_SECONDS(hour) FROM DS.autots"), "[[1539878400]]")

	// STRINGS, with a schema that has no key columns.
	if err := run("strings", &bq.JobConfigurationLoad{SourceFormat: "NEWLINE_DELIMITED_JSON",
		Schema:     &bq.TableSchema{Fields: []*bq.TableFieldSchema{{Name: "v", Type: "STRING"}}},
		SourceUris: []string{gs + "t/*"}, HivePartitioningOptions: hive("STRINGS", "t")}, ""); err != nil {
		t.Fatalf("STRINGS: %v", err)
	}
	want("STRINGS's columns", types("strings"), "v:STRING,dt:STRING,n:STRING")
	want("STRINGS's rows", q("SELECT dt, n FROM DS.strings WHERE v = 'z'"), "[[2024-01-03 8]]")

	// CUSTOM.
	if err := run("custom", &bq.JobConfigurationLoad{SourceFormat: "NEWLINE_DELIMITED_JSON", Autodetect: true,
		SourceUris: []string{gs + "t/*"}, HivePartitioningOptions: hive("CUSTOM", "t/{dt:STRING}/{n:INTEGER}")}, ""); err != nil {
		t.Fatalf("CUSTOM: %v", err)
	}
	want("CUSTOM's columns", types("custom"), "v:STRING,dt:STRING,n:INTEGER")

	// Parquet, AUTO: the keys after the file's columns.
	if err := run("pqhive", &bq.JobConfigurationLoad{SourceFormat: "PARQUET",
		SourceUris: []string{gs + "pq/*"}, HivePartitioningOptions: hive("AUTO", "pq")}, ""); err != nil {
		t.Fatalf("Parquet AUTO: %v", err)
	}
	want("Parquet's columns", types("pqhive"), "v:INTEGER,dt:DATE,n:INTEGER")
	want("Parquet's rows", q("SELECT v, CAST(dt AS STRING), n FROM DS.pqhive ORDER BY v"),
		"[[1 2024-01-02 7] [2 2024-01-03 8]]")

	// 400 and 501.
	for _, tc := range []struct {
		what string
		code int
		load *bq.JobConfigurationLoad
	}{
		{"no key names", 400, &bq.JobConfigurationLoad{SourceUris: []string{gs + "flat/*"}, HivePartitioningOptions: hive("AUTO", "flat")}},
		{"keys out of order", 400, &bq.JobConfigurationLoad{SourceUris: []string{gs + "swap/*"}, HivePartitioningOptions: hive("AUTO", "swap")}},
		{"a file column that is a key", 400, &bq.JobConfigurationLoad{SourceUris: []string{gs + "dup/*"}, HivePartitioningOptions: hive("AUTO", "dup")}},
		{"a value not of CUSTOM's type", 400, &bq.JobConfigurationLoad{SourceUris: []string{gs + "bad/*"}, HivePartitioningOptions: hive("CUSTOM", "bad/{n:INTEGER}")}},
		{"requirePartitionFilter", 400, &bq.JobConfigurationLoad{SourceUris: []string{gs + "t/*"},
			HivePartitioningOptions: &bq.HivePartitioningOptions{Mode: "AUTO", SourceUriPrefix: gs + "t", RequirePartitionFilter: true}}},
		{"no prefix", 400, &bq.JobConfigurationLoad{SourceUris: []string{gs + "t/*"}, HivePartitioningOptions: &bq.HivePartitioningOptions{Mode: "AUTO"}}},
		{"hive CSV", 501, &bq.JobConfigurationLoad{SourceFormat: "CSV", SourceUris: []string{gs + "t/*"}, HivePartitioningOptions: hive("AUTO", "t")}},
		{"GEOJSON with CSV", 400, &bq.JobConfigurationLoad{SourceFormat: "CSV", JsonExtension: "GEOJSON", SourceUris: []string{gs + "geo/f.json"}}},
		{"a FeatureCollection", 400, &bq.JobConfigurationLoad{JsonExtension: "GEOJSON", Autodetect: true, SourceUris: []string{gs + "geo/bad.json"}}},
	} {
		if tc.load.SourceFormat == "" {
			tc.load.SourceFormat = "NEWLINE_DELIMITED_JSON"
		}
		tc.load.Autodetect = true
		err := run("refused", tc.load, "")
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != tc.code {
			t.Errorf("%s: got %v, want %d", tc.what, err, tc.code)
		}
	}

	// GEOJSON, by autodetect from Cloud Storage, and with a schema as an
	// upload.
	if err := run("geo", &bq.JobConfigurationLoad{SourceFormat: "NEWLINE_DELIMITED_JSON", JsonExtension: "GEOJSON",
		Autodetect: true, SourceUris: []string{gs + "geo/f.json"}}, ""); err != nil {
		t.Fatalf("GEOJSON: %v", err)
	}
	want("GEOJSON's columns", types("geo"), "id:INTEGER,geometry:GEOGRAPHY,name:STRING,pop:INTEGER")
	want("GEOJSON's rows", q("SELECT id, ST_ASTEXT(geometry), name, pop FROM DS.geo ORDER BY id"),
		"[[1 POINT(1 2) a 3] [2 <nil> b 4]]")
	if err := run("geoup", &bq.JobConfigurationLoad{SourceFormat: "NEWLINE_DELIMITED_JSON", JsonExtension: "GEOJSON",
		Schema: &bq.TableSchema{Fields: []*bq.TableFieldSchema{{Name: "geometry", Type: "GEOGRAPHY"}, {Name: "name", Type: "STRING"}}}},
		`{"type":"Feature","geometry":{"type":"LineString","coordinates":[[0,0],[1,1]]},"properties":{"name":"l"}}`+"\n"); err != nil {
		t.Fatalf("GEOJSON upload: %v", err)
	}
	want("the GEOJSON upload's rows", q("SELECT ST_ASTEXT(geometry), name FROM DS.geoup"), "[[LINESTRING(0 0, 1 1) l]]")
}

// TestConsoleBigQueryHiveAndGeoJSONLoads (#1547): Load from Cloud Storage
// in the console loads hive-partitioned JSON (AUTO, its prefix) and a
// GeoJSON file (JSON extension GEOJSON), read back with the official Go
// client.
//
// covers: bigquery.jobs.insert, bigquery.tables.get
func TestConsoleBigQueryHiveAndGeoJSONLoads(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	bucket := loadBucket(t, h, "bq-console-hive", map[string]string{
		"t/dt=2024-01-02/a.json": `{"v":"x"}` + "\n",
		"g.json":                 `{"type":"Feature","geometry":{"type":"Point","coordinates":[1,2]},"properties":{"name":"a"}}` + "\n",
	})
	gs := "gs://" + bucket.BucketName() + "/"
	for _, tc := range []struct {
		table  string
		values map[string]string
		sql    string
		want   string
	}{
		{"hive", map[string]string{"uris": gs + "t/*", "hiveMode": "AUTO", "hiveSourceUriPrefix": gs + "t"},
			"SELECT v, CAST(dt AS STRING) FROM DS.hive", "[[x 2024-01-02]]"},
		{"geo", map[string]string{"uris": gs + "g.json", "jsonExtension": "GEOJSON"},
			"SELECT ST_ASTEXT(geometry), name FROM DS.geo", "[[POINT(1 2) a]]"},
	} {
		tc.values["tableId"], tc.values["format"], tc.values["autodetect"] = tc.table, "NEWLINE_DELIMITED_JSON", "true"
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
