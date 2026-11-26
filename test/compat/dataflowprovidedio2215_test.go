//go:build compat

package compat

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"cloud.google.com/go/spanner"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/cloudburrow/cloudburrow/internal/localclient"
)

// covers: google.dataflow.v1beta3.TemplatesService/LaunchTemplate
//
// TestDataflowProvidedSpanner (#2215): Google's Spanner import and export
// templates, from Google's paths and the instance's build of Google's
// DataflowTemplates sources, against the instance's Spanner:
//   - Cloud_Spanner_to_GCS_Avro (templates/provided/cloud-spanner-to-avro)
//     exports a database as Avro files with a spanner-export.json manifest;
//   - GCS_Avro_to_Cloud_Spanner (templates/provided/avro-to-cloud-spanner)
//     imports that export into a new, empty database, its tables made from
//     the export's DDL;
//   - GCS_Text_to_Cloud_Spanner (templates/provided/cloud-storage-to-cloud-spanner)
//     imports CSV files, named by an import manifest, into an existing table.
func TestDataflowProvidedSpanner(t *testing.T) {
	h, _, c := dataflowEnv(t)
	h.Endpoint(EnvStorage)
	spannerAddr := h.Endpoint(EnvSpanner)
	project := h.Project()
	sc := storageClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	bucket := fmt.Sprintf("%s-dataflow-provsp-%s", project, suffix)
	dataflowBucket(t, ctx, sc, project, bucket)
	launch := providedLauncher(t, ctx, c, project, bucket, suffix)
	put := bucketWriter(t, ctx, sc, bucket)

	// The Go Spanner client exports built-in metrics to Google unless told
	// it talks to an emulator: localclient turns them off, and so does
	// SPANNER_EMULATOR_HOST.
	t.Setenv("SPANNER_EMULATOR_HOST", spannerAddr)
	t.Setenv("SPANNER_DISABLE_BUILTIN_METRICS", "true")
	spInstance := "provsp-" + suffix
	src := spannerDatabase(t, ctx, spannerAddr, project, spInstance, "orchard",
		"CREATE TABLE Fruit (Name STRING(64) NOT NULL, N INT64, Ripe BOOL) PRIMARY KEY (Name)")
	client := func(db string) *spanner.Client {
		t.Helper()
		sp, err := localclient.Spanner(ctx, db, spannerOpts(spannerAddr)...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(sp.Close)
		return sp
	}
	if _, err := client(src).Apply(ctx, []*spanner.Mutation{
		spanner.Insert("Fruit", []string{"Name", "N", "Ripe"}, []any{"apple", 3, true}),
		spanner.Insert("Fruit", []string{"Name", "N", "Ripe"}, []any{"kiwi", 2, false})}); err != nil {
		t.Fatal(err)
	}
	rows := func(t *testing.T, db string) string {
		t.Helper()
		it := client(db).Single().Query(ctx, spanner.Statement{SQL: "SELECT Name, N, Ripe FROM Fruit ORDER BY Name"})
		defer it.Stop()
		var got []string
		for {
			r, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				t.Fatalf("query %s: %v", db, err)
			}
			var name string
			var n spanner.NullInt64
			var ripe spanner.NullBool
			if err := r.Columns(&name, &n, &ripe); err != nil {
				t.Fatal(err)
			}
			got = append(got, fmt.Sprintf("%s=%v/%v", name, n, ripe))
		}
		return strings.Join(got, ",")
	}

	t.Run("Avro_export_and_import", func(t *testing.T) {
		done(t, ctx, c, project, "Cloud_Spanner_to_GCS_Avro", launch(t, "Cloud_Spanner_to_GCS_Avro", "provided-spanner-avro", map[string]string{
			"instanceId": spInstance, "databaseId": "orchard", "outputDir": "gs://" + bucket + "/avro"}))
		// The export is a folder named INSTANCE-DATABASE-JOB, holding the
		// spanner-export.json manifest, each table's manifest and its Avro
		// files.
		var export string
		var objects []string
		it := sc.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: "avro/"})
		for {
			a, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			objects = append(objects, a.Name)
			if strings.HasSuffix(a.Name, "/spanner-export.json") {
				export = strings.TrimSuffix(a.Name, "spanner-export.json")
			}
		}
		if export == "" || !strings.HasPrefix(export, "avro/"+spInstance+"-orchard-") {
			t.Fatalf("the export wrote %v; want INSTANCE-DATABASE-JOB/spanner-export.json", objects)
		}
		var avro bool
		for _, o := range objects {
			if strings.HasPrefix(o, export+"Fruit.avro-") {
				avro = true
			}
		}
		if !avro {
			t.Fatalf("the export wrote %v; want Fruit.avro-* files", objects)
		}
		dst := spannerEmptyDatabase(t, ctx, spannerAddr, project, spInstance, "restored")
		done(t, ctx, c, project, "GCS_Avro_to_Cloud_Spanner", launch(t, "GCS_Avro_to_Cloud_Spanner", "provided-avro-spanner", map[string]string{
			"instanceId": spInstance, "databaseId": "restored", "inputDir": "gs://" + bucket + "/" + strings.TrimSuffix(export, "/")}))
		if got, want := rows(t, dst), "apple=3/true,kiwi=2/false"; got != want {
			t.Errorf("the imported database has %s; want %s", got, want)
		}
	})

	t.Run("GCS_Text_to_Cloud_Spanner", func(t *testing.T) {
		dst := spannerDatabase(t, ctx, spannerAddr, project, spInstance, "fromtext",
			"CREATE TABLE Fruit (Name STRING(64) NOT NULL, N INT64, Ripe BOOL) PRIMARY KEY (Name)")
		put("text/fruit-1.csv", "pear,7,true\n")
		put("text/fruit-2.csv", "fig,1,false\n")
		put("text/manifest.json", `{"tables": [{"table_name": "Fruit", "file_patterns": ["gs://`+bucket+`/text/fruit-*.csv"]}]}`)
		done(t, ctx, c, project, "GCS_Text_to_Cloud_Spanner", launch(t, "GCS_Text_to_Cloud_Spanner", "provided-text-spanner", map[string]string{
			"instanceId": spInstance, "databaseId": "fromtext", "importManifest": "gs://" + bucket + "/text/manifest.json"}))
		if got, want := rows(t, dst), "fig=1/false,pear=7/true"; got != want {
			t.Errorf("the table has %s; want %s", got, want)
		}
	})
}

// covers: google.dataflow.v1beta3.TemplatesService/LaunchTemplate
//
// TestDataflowProvidedBigQueryToTFRecord (#2215): Google's BigQuery to
// TensorFlow Records template (Cloud_BigQuery_to_GCS_TensorFlow_Records,
// templates/provided/bigquery-to-tfrecords) runs a query on the instance's
// BigQuery, reads its result with the BigQuery Storage Read API and writes
// each row as a tf.Example in TFRecord files under OUTPUT/train/.
func TestDataflowProvidedBigQueryToTFRecord(t *testing.T) {
	h, _, c := dataflowEnv(t)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvBigQuery)
	bq, project := bigqueryClient(t, h)
	sc := storageClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	bucket := fmt.Sprintf("%s-dataflow-provtf-%s", h.Project(), suffix)
	dataflowBucket(t, ctx, sc, h.Project(), bucket)
	launch := providedLauncher(t, ctx, c, project, bucket, suffix)

	ds := "provided_tf_" + suffix
	if err := bq.Dataset(ds).Create(ctx, nil); err != nil {
		t.Fatal(err)
	}
	schema := bigquery.Schema{{Name: "name", Type: bigquery.StringFieldType}, {Name: "age", Type: bigquery.IntegerFieldType}}
	tbl := bq.Dataset(ds).Table("people")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatal(err)
	}
	q := bq.Query("INSERT INTO `" + project + "." + ds + ".people` (name, age) VALUES ('Ada', 36), ('Grace', 45)")
	if j, err := q.Run(ctx); err != nil {
		t.Fatal(err)
	} else if st, err := j.Wait(ctx); err != nil || st.Err() != nil {
		t.Fatalf("insert: %v %v", err, st.Err())
	}
	done(t, ctx, c, project, "Cloud_BigQuery_to_GCS_TensorFlow_Records", launch(t, "Cloud_BigQuery_to_GCS_TensorFlow_Records", "provided-bq-tf", map[string]string{
		"readQuery": "SELECT name, age FROM `" + project + "." + ds + ".people`", "outputDirectory": "gs://" + bucket + "/tf/"}))

	var got []string
	it := sc.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: "tf/train/"})
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(a.Name, ".tfrecord") {
			t.Errorf("the output has %s; want .tfrecord files", a.Name)
		}
		r, err := sc.Bucket(bucket).Object(a.Name).NewReader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range tfRecords(t, b) {
			got = append(got, tfExample(t, rec))
		}
	}
	sort.Strings(got)
	if want := "age=36 name=Ada,age=45 name=Grace"; strings.Join(got, ",") != want {
		t.Errorf("the TFRecords hold %v; want %s", got, want)
	}
}

// providedLauncher launches a Google-provided template by name, with its
// temporary files in bucket, answering the job's ID.
func providedLauncher(t *testing.T, ctx context.Context, c dataflowClients, project, bucket, suffix string) func(*testing.T, string, string, map[string]string) string {
	return func(t *testing.T, name, job string, params map[string]string) string {
		t.Helper()
		resp, err := c.tmpl.LaunchTemplate(ctx, &dataflowpb.LaunchTemplateRequest{ProjectId: project, Location: "us-central1",
			Template: &dataflowpb.LaunchTemplateRequest_GcsPath{GcsPath: "gs://dataflow-templates-us-central1/latest/" + name},
			LaunchParameters: &dataflowpb.LaunchTemplateParameters{JobName: job + "-" + suffix, Parameters: params,
				Environment: &dataflowpb.RuntimeEnvironment{TempLocation: "gs://" + bucket + "/tmp"}}})
		if err != nil {
			t.Fatalf("LaunchTemplate(%s): %v", name, err)
		}
		return resp.GetJob().GetId()
	}
}

// done waits for a batch job to finish, failing the test unless it is
// DONE.
func done(t *testing.T, ctx context.Context, c dataflowClients, project, name, id string) {
	t.Helper()
	if j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED); j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE {
		t.Fatalf("%s's job is %s:\n%s", name, j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
}

// bucketWriter writes a text object into bucket.
func bucketWriter(t *testing.T, ctx context.Context, sc *storage.Client, bucket string) func(string, string) {
	return func(object, text string) {
		t.Helper()
		w := sc.Bucket(bucket).Object(object).NewWriter(ctx)
		if _, err := io.WriteString(w, text); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// spannerEmptyDatabase creates a database with no tables in an existing
// instance.
func spannerEmptyDatabase(t *testing.T, ctx context.Context, addr, project, instanceID, dbID string) string {
	t.Helper()
	admin, err := database.NewDatabaseAdminClient(ctx, spannerOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	op, err := admin.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{Parent: fmt.Sprintf("projects/%s/instances/%s", project, instanceID),
		CreateStatement: "CREATE DATABASE " + dbID})
	if err != nil {
		t.Fatalf("CreateDatabase %s: %v", dbID, err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("waiting for database %s: %v", dbID, err)
	}
	return fmt.Sprintf("projects/%s/instances/%s/databases/%s", project, instanceID, dbID)
}

// tfRecords splits a TFRecord file into its records: each a little-endian
// uint64 length, its masked CRC-32C, the data and the data's masked
// CRC-32C (TensorFlow's record format).
func tfRecords(t *testing.T, b []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for len(b) > 0 {
		if len(b) < 12 {
			t.Fatalf("a TFRecord file ends in a %d-byte header", len(b))
		}
		n := binary.LittleEndian.Uint64(b)
		if uint64(len(b)) < 16+n {
			t.Fatalf("a TFRecord of %d bytes in %d", n, len(b))
		}
		out = append(out, b[12:12+n])
		b = b[16+n:]
	}
	return out
}

// tfExample is a tf.Example's features as sorted name=value pairs: an
// Example's field 1 is its Features, whose field 1 is a map entry per
// feature (key 1, Feature 2); a Feature's bytes_list (1) and int64_list
// (3) each hold their values in field 1.
func tfExample(t *testing.T, b []byte) string {
	t.Helper()
	fields := func(b []byte) map[protowire.Number][][]byte {
		out := map[protowire.Number][][]byte{}
		for len(b) > 0 {
			num, typ, n := protowire.ConsumeTag(b)
			if n < 0 {
				t.Fatalf("a malformed tf.Example: %v", protowire.ParseError(n))
			}
			b = b[n:]
			var v []byte
			switch typ {
			case protowire.BytesType:
				v, n = protowire.ConsumeBytes(b)
			case protowire.VarintType:
				var x uint64
				x, n = protowire.ConsumeVarint(b)
				v = protowire.AppendVarint(nil, x)
			default:
				n = protowire.ConsumeFieldValue(num, typ, b)
			}
			if n < 0 {
				t.Fatalf("a malformed tf.Example: %v", protowire.ParseError(n))
			}
			b = b[n:]
			out[num] = append(out[num], v)
		}
		return out
	}
	var got []string
	for _, features := range fields(b)[1] {
		for _, entry := range fields(features)[1] {
			e := fields(entry)
			if len(e[1]) != 1 || len(e[2]) != 1 {
				t.Fatalf("a malformed feature entry %x", entry)
			}
			name, f := string(e[1][0]), fields(e[2][0])
			switch {
			case len(f[1]) == 1:
				for _, v := range fields(f[1][0])[1] {
					got = append(got, name+"="+string(v))
				}
			case len(f[3]) == 1:
				// Int64List's values are varints, packed or not.
				for _, vs := range fields(f[3][0])[1] {
					for len(vs) > 0 {
						x, n := protowire.ConsumeVarint(vs)
						if n < 0 {
							t.Fatal("a malformed int64_list")
						}
						got = append(got, fmt.Sprintf("%s=%d", name, int64(x)))
						vs = vs[n:]
					}
				}
			default:
				t.Fatalf("feature %s is neither bytes nor int64: %x", name, e[2][0])
			}
		}
	}
	sort.Strings(got)
	return strings.Join(got, " ")
}
