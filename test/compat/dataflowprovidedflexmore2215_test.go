//go:build compat

package compat

import (
	"bytes"
	"context"
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
	"cloud.google.com/go/storage"
	"github.com/linkedin/goavro/v2"
	"google.golang.org/api/iterator"

	"github.com/cloudburrow/cloudburrow/internal/localclient"
)

// covers: google.dataflow.v1beta3.FlexTemplatesService/LaunchFlexTemplate
//
// TestDataflowProvidedFlexTemplatesMore (#2215): more of the v2 module
// googlecloud-to-googlecloud's Flex Templates, launched through
// LaunchFlexTemplate:
//   - File_Format_Conversion (templates/provided/file-format-conversion)
//     converts a CSV file, with the Avro schema it is given, to Avro
//     files under the output folder;
//   - Cloud_Spanner_to_BigQuery_Flex (templates/provided/cloud-spanner-
//     to-bigquery) reads a Cloud Spanner query's rows into an existing
//     BigQuery table (its spannerTableId alone fails at launch, on Google
//     too: Beam's SpannerIO.read() of a table requires its columns, which
//     the template does not give).
func TestDataflowProvidedFlexTemplatesMore(t *testing.T) {
	h, _, c := dataflowEnv(t)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvBigQuery)
	bq, project := bigqueryClient(t, h)
	sc := storageClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	bucket := fmt.Sprintf("%s-dataflow-provflex2-%s", h.Project(), suffix)
	dataflowBucket(t, ctx, sc, h.Project(), bucket)
	put := bucketWriter(t, ctx, sc, bucket)
	launch := func(t *testing.T, name, job string, p map[string]string) string {
		t.Helper()
		resp, err := c.flex.LaunchFlexTemplate(ctx, &dataflowpb.LaunchFlexTemplateRequest{ProjectId: project, Location: "us-central1",
			LaunchParameter: &dataflowpb.LaunchFlexTemplateParameter{JobName: job + "-" + suffix,
				Template:    &dataflowpb.LaunchFlexTemplateParameter_ContainerSpecGcsPath{ContainerSpecGcsPath: "gs://dataflow-templates-us-central1/latest/flex/" + name},
				Parameters:  p,
				Environment: &dataflowpb.FlexTemplateRuntimeEnvironment{TempLocation: "gs://" + bucket + "/tmp"}}})
		if err != nil {
			t.Fatalf("LaunchFlexTemplate(%s): %v", name, err)
		}
		return resp.GetJob().GetId()
	}

	t.Run("File_Format_Conversion", func(t *testing.T) {
		put("csv/people.csv", "Ada,36\nGrace,45\n")
		put("csv/people.avsc", `{"type": "record", "name": "Person", "fields": [{"name": "name", "type": "string"}, {"name": "age", "type": "string"}]}`)
		done(t, ctx, c, project, "File_Format_Conversion", launch(t, "File_Format_Conversion", "provided-ffc", map[string]string{
			"inputFileFormat": "csv", "outputFileFormat": "avro", "inputFileSpec": "gs://" + bucket + "/csv/people.csv",
			"outputBucket": "gs://" + bucket + "/avro/", "schema": "gs://" + bucket + "/csv/people.avsc", "numShards": "1",
			"outputFilePrefix": "people"}))
		var got []string
		it := sc.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: "avro/"})
		for {
			a, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(a.Name, "avro/people") {
				t.Errorf("the output has %s; want files prefixed people", a.Name)
				continue
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
			ocf, err := goavro.NewOCFReader(bytes.NewReader(b))
			if err != nil {
				t.Fatalf("%s is not an Avro file: %v", a.Name, err)
			}
			for ocf.Scan() {
				rec, err := ocf.Read()
				if err != nil {
					t.Fatal(err)
				}
				m, _ := rec.(map[string]any)
				got = append(got, fmt.Sprintf("%v=%v", m["name"], m["age"]))
			}
		}
		sort.Strings(got)
		if want := "Ada=36,Grace=45"; strings.Join(got, ",") != want {
			t.Errorf("the Avro files hold %v; want %s", got, want)
		}
	})

	t.Run("Cloud_Spanner_to_BigQuery_Flex", func(t *testing.T) {
		h := h.For(t)
		spannerAddr := h.Endpoint(EnvSpanner)
		t.Setenv("SPANNER_EMULATOR_HOST", spannerAddr)
		t.Setenv("SPANNER_DISABLE_BUILTIN_METRICS", "true")
		inst := "provflsp-" + suffix
		db := spannerDatabase(t, ctx, spannerAddr, project, inst, "orchard",
			"CREATE TABLE Fruit (Name STRING(64) NOT NULL, N INT64) PRIMARY KEY (Name)")
		sp, err := localclient.Spanner(ctx, db, spannerOpts(spannerAddr)...)
		if err != nil {
			t.Fatal(err)
		}
		defer sp.Close()
		if _, err := sp.Apply(ctx, []*spanner.Mutation{
			spanner.Insert("Fruit", []string{"Name", "N"}, []any{"apple", 3}),
			spanner.Insert("Fruit", []string{"Name", "N"}, []any{"kiwi", 2})}); err != nil {
			t.Fatal(err)
		}
		ds := "provided_flex_sp_" + suffix
		if err := bq.Dataset(ds).Create(ctx, nil); err != nil {
			t.Fatal(err)
		}
		schema := bigquery.Schema{{Name: "Name", Type: bigquery.StringFieldType}, {Name: "N", Type: bigquery.IntegerFieldType}}
		if err := bq.Dataset(ds).Table("fruit").Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
			t.Fatal(err)
		}
		done(t, ctx, c, project, "Cloud_Spanner_to_BigQuery_Flex", launch(t, "Cloud_Spanner_to_BigQuery_Flex", "provided-sp-bq", map[string]string{
			"spannerInstanceId": inst, "spannerDatabaseId": "orchard", "sqlQuery": "SELECT Name, N FROM Fruit",
			"outputTableSpec": project + ":" + ds + ".fruit", "createDisposition": "CREATE_NEVER", "writeDisposition": "WRITE_APPEND"}))
		if got := providedRows(t, ctx, bq, "SELECT Name, N FROM `"+project+"."+ds+".fruit` ORDER BY Name"); got != "apple=3,kiwi=2" {
			t.Errorf("the table has %s; want apple=3,kiwi=2", got)
		}
	})
}
