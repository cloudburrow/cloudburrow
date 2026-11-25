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
	"cloud.google.com/go/storage"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"google.golang.org/api/iterator"
)

// covers: google.dataflow.v1beta3.FlexTemplatesService/LaunchFlexTemplate
//
// TestDataflowProvidedBigQueryToParquet (#2215): Google's BigQuery export
// to Parquet Flex Template (BigQuery_to_Parquet,
// templates/provided/bigquery-to-parquet, v2 module
// googlecloud-to-googlecloud), launched with `gcloud dataflow
// flex-template run`: its launcher reads the table's schema through the
// BigQuery Storage API's v1beta1 client (BigQueryStorageClient) with its
// application default credentials, both the instance's (its BigQuery
// Storage API and metadata server), and its job reads the table's rows
// with the Storage Read API (DIRECT_READ) into Parquet files of the
// selected fields under the output folder.
func TestDataflowProvidedBigQueryToParquet(t *testing.T) {
	h, _, c := dataflowEnv(t)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvBigQuery)
	bq, project := bigqueryClient(t, h)
	sc := storageClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	bucket := fmt.Sprintf("%s-dataflow-parquet-%s", h.Project(), suffix)
	dataflowBucket(t, ctx, sc, h.Project(), bucket)
	ds := "provided_parquet_" + suffix
	if err := bq.Dataset(ds).Create(ctx, nil); err != nil {
		t.Fatal(err)
	}
	schema := bigquery.Schema{{Name: "name", Type: bigquery.StringFieldType}, {Name: "age", Type: bigquery.IntegerFieldType},
		{Name: "note", Type: bigquery.StringFieldType}}
	if err := bq.Dataset(ds).Table("people").Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatal(err)
	}
	q := bq.Query("INSERT INTO `" + project + "." + ds + ".people` (name, age, note) VALUES ('Ada', 36, 'a'), ('Grace', 45, 'g'), ('Linus', 20, 'l')")
	if j, err := q.Run(ctx); err != nil {
		t.Fatal(err)
	} else if st, err := j.Wait(ctx); err != nil || st.Err() != nil {
		t.Fatalf("insert: %v %v", err, st.Err())
	}
	gc := gcloudDataflow(t, h, project)
	name := "provided-parquet-" + suffix
	out, err := gc("dataflow", "flex-template", "run", name, "--region=us-central1",
		"--template-file-gcs-location=gs://dataflow-templates-us-central1/latest/flex/BigQuery_to_Parquet",
		"--temp-location=gs://"+bucket+"/tmp",
		// gcloud's alternate delimiter (gcloud topic escaping): fields
		// holds a comma.
		"--parameters=^;^tableRef="+project+":"+ds+".people;bucket=gs://"+bucket+"/parquet/;numShards=1;fields=name,age"+
			";rowRestriction=age > 30",
		"--format=value(job.id)")
	id := strings.TrimSpace(out)
	if err != nil || id == "" || strings.ContainsAny(id, " \n") {
		t.Fatalf("flex-template run: %v\n%s", err, out)
	}
	j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_FAILED)
	if j.CurrentState != dataflowpb.JobState_JOB_STATE_DONE {
		t.Fatalf("BigQuery_to_Parquet's job is %s:\n%s", j.CurrentState, jobMessages(t, ctx, c, project, id))
	}
	var got []string
	var columns string
	it := sc.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: "parquet/"})
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(a.Name, ".parquet") {
			t.Errorf("the output has %s; want .parquet files", a.Name)
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
		pf, err := file.NewParquetReader(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("%s: %v", a.Name, err)
		}
		fr, err := pqarrow.NewFileReader(pf, pqarrow.ArrowReadProperties{}, memory.DefaultAllocator)
		if err != nil {
			t.Fatal(err)
		}
		tbl, err := fr.ReadTable(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, f := range tbl.Schema().Fields() {
			names = append(names, f.Name)
		}
		columns = strings.Join(names, ",")
		tr := array.NewTableReader(tbl, 0)
		for tr.Next() {
			rec := tr.Record()
			for i := 0; i < int(rec.NumRows()); i++ {
				var vals []string
				for c := 0; c < int(rec.NumCols()); c++ {
					vals = append(vals, rec.Column(c).ValueStr(i))
				}
				got = append(got, strings.Join(vals, "="))
			}
		}
		tr.Release()
		tbl.Release()
	}
	sort.Strings(got)
	if columns != "name,age" {
		t.Errorf("the Parquet files' columns are %s; want the selected name,age", columns)
	}
	if want := "Ada=36,Grace=45"; strings.Join(got, ",") != want {
		t.Errorf("the Parquet files hold %v; want %s (the rows of age > 30)", got, want)
	}
}
