//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigquery/connection/apiv1/connectionpb"
	"cloud.google.com/go/iam"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/logging/logadmin"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// EnvSpark is set when the instance under test may run stored procedures
// for Apache Spark (#2104): its CLI pulls the pinned apache/spark image
// (1.8 GB) and fetches Google's BigQuery connector for Spark on the first
// CALL. Without it the test skips.
const EnvSpark = "CLOUDBURROW_TEST_SPARK"

// sparkEnv returns the clients the Spark procedure tests use, or skips.
func sparkEnv(t *testing.T) (*Harness, *bigquery.Client, string) {
	t.Helper()
	h := New(t)
	h.Endpoint(EnvBigQuery)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvLogging)
	if os.Getenv(EnvSpark) == "" {
		t.Skipf("%s is not set: stored procedures for Apache Spark pull the apache/spark image on the first CALL", EnvSpark)
	}
	c, project := bigqueryClient(t, h)
	return h, c, project
}

// sparkConnection makes a Spark connection in us and grants the caller
// roles/bigquery.connectionAdmin on it (bigquery.connections.delegate and
// .use), and its service account roles/storage.objectViewer on bucket.
func sparkConnection(t *testing.T, h *Harness, project, id string, bh interface {
	IAM() *iam.Handle
}) (*connectionpb.Connection, string) {
	t.Helper()
	cc, _ := remoteConnectionClient(t, h)
	ctx := h.Context()
	cn, err := cc.CreateConnection(ctx, &connectionpb.CreateConnectionRequest{Parent: "projects/" + project + "/locations/us", ConnectionId: id,
		Connection: &connectionpb.Connection{Properties: &connectionpb.Connection_Spark{Spark: &connectionpb.SparkProperties{}}}})
	if err != nil {
		t.Fatalf("CreateConnection (spark): %v", err)
	}
	t.Cleanup(func() { _ = cc.DeleteConnection(h.Context(), &connectionpb.DeleteConnectionRequest{Name: cn.Name}) })
	if _, err := cc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: cn.Name, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/bigquery.connectionAdmin", Members: []string{callerMember(project)}}}}}); err != nil {
		t.Fatalf("SetIamPolicy on the connection: %v", err)
	}
	if bh != nil {
		pol, err := bh.IAM().Policy(ctx)
		if err != nil {
			t.Fatalf("bucket IAM Policy: %v", err)
		}
		pol.Add("serviceAccount:"+cn.GetSpark().GetServiceAccountId(), iam.RoleName("roles/storage.objectViewer"))
		if err := bh.IAM().SetPolicy(ctx, pol); err != nil {
			t.Fatalf("bucket IAM SetPolicy: %v", err)
		}
	}
	return cn, project + ".us." + id
}

// sparkWordCount is the page's word count, over the dataset's words table,
// written with the direct write method; it reads its argument through
// SparkProcParamContext and the BIGQUERY_PROC_PARAM environment variable,
// sets its OUT argument, and records whether a socket to Google's
// BigQuery API could be opened (it must not: the container has no route
// out).
const sparkWordCount = `
import json, os, socket
from pyspark.sql import SparkSession
from bigquery.spark.procedure import SparkProcParamContext

spark = SparkSession.builder.appName("spark-bigquery-demo").getOrCreate()
ctx = SparkProcParamContext.getOrCreate(spark)
words = spark.read.format("bigquery").load("%[1]s.words")
counts = words.groupBy("word").sum("n").withColumnRenamed("sum(n)", "total")
counts.write.format("bigquery").option("writeMethod", "direct").mode("overwrite").save("%[1]s.counts")
print("counted for", ctx.prefix, json.loads(os.environ["BIGQUERY_PROC_PARAM.prefix"]))
ctx.rows = counts.filter(counts.word.startswith(ctx.prefix)).count()
try:
    socket.create_connection(("bigquery.googleapis.com", 443), timeout=5).close()
    ctx.escaped = True
except Exception:
    ctx.escaped = False
`

// TestBigQuerySparkStoredProcedures (#2104): stored procedures for Apache
// Spark, as https://docs.cloud.google.com/bigquery/docs/spark-procedures
// and the DDL reference's CREATE PROCEDURE document them, through the
// official cloud.google.com/go bigquery, bigquery/connection/apiv1,
// storage and logging/logadmin clients and google.golang.org/api's
// bigquery/v2: a PySpark procedure written inline (LANGUAGE PYTHON AS
// r"""..."""), CALLed in a script, reads a table through Google's BigQuery
// connector for Spark (the instance's Storage Read API) and writes one
// (its Storage Write API, writeMethod direct), takes an IN argument from
// SparkProcParamContext and the BIGQUERY_PROC_PARAM.<name> variable, sets
// an OUT one the script reads, and cannot reach Google; one whose code is
// main_file_uri, importing a module of its py_file_uris, both in the
// instance's Cloud Storage, with properties; routines.get shows language
// PYTHON and sparkOptions; the CALL's child job has
// statistics.query.sparkStatistics (sparkJobId, sparkJobLocation,
// loggingInfo) whose filter finds the driver's output in Cloud Logging;
// a failing procedure fails its CALL. The documented refusals: no engine,
// inline code with main_file_uri, a YARN property (the page's own error),
// a connection that is not a Spark one, a runtime version Google does
// not list, a container_image outside Artifact Registry.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.get, bigquery.jobs.list, bigquery.routines.get, google.logging.v2.LoggingServiceV2/ListLogEntries
func TestBigQuerySparkStoredProcedures(t *testing.T) {
	h, c, project := sparkEnv(t)
	sc := storageClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	bh := bucket(t, h, sc)
	_, conn := sparkConnection(t, h, project, strings.ReplaceAll(h.Project(), "-", "_")+"_spark", bh)
	ds := "compat_spark_" + strings.ReplaceAll(h.Project(), "-", "_")
	if err := c.Dataset(ds).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Dataset(ds).DeleteWithContents(h.Context()) })
	full := project + "." + ds
	if err := mlRunIn(ctx, c, fmt.Sprintf("CREATE TABLE `%s.words` AS SELECT * FROM UNNEST([STRUCT('apple' AS word, 2 AS n), ('apricot', 1), ('apple', 3), ('banana', 4)])", ds)); err != nil {
		t.Fatalf("CREATE TABLE words: %v", err)
	}

	create := fmt.Sprintf("CREATE PROCEDURE `%s.wordcount`(prefix STRING, OUT rows INT64, OUT escaped BOOL)\nWITH CONNECTION `%s`\n"+
		"OPTIONS(engine=\"SPARK\", runtime_version=\"2.2\", description=\"word count\")\nLANGUAGE PYTHON AS r\"\"\"%s\"\"\"",
		ds, conn, fmt.Sprintf(sparkWordCount, full))
	if err := mlRunIn(ctx, c, create); err != nil {
		t.Fatalf("CREATE PROCEDURE (Spark, inline): %v", err)
	}
	svc, err := bq.NewService(ctx, option.WithEndpoint(h.Endpoint(EnvBigQuery)), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	r, err := svc.Routines.Get(project, ds, "wordcount").Context(ctx).Do()
	if err != nil {
		t.Fatalf("routines.get: %v", err)
	}
	if r.RoutineType != "PROCEDURE" || r.Language != "PYTHON" || r.SparkOptions == nil || r.SparkOptions.RuntimeVersion != "2.2" ||
		r.SparkOptions.Connection != "projects/"+project+"/locations/us/connections/"+strings.ReplaceAll(h.Project(), "-", "_")+"_spark" ||
		!strings.Contains(r.DefinitionBody, "SparkProcParamContext") || r.Description != "word count" || len(r.Arguments) != 3 || r.Arguments[1].Mode != "OUT" {
		t.Errorf("routine %+v, sparkOptions %+v", r, r.SparkOptions)
	}

	q := c.Query(fmt.Sprintf("DECLARE rows INT64;\nDECLARE escaped BOOL;\nCALL `%s.wordcount`('ap', rows, escaped);\nSELECT rows, escaped", ds))
	job, err := q.Run(ctx)
	if err != nil {
		t.Fatalf("CALL: %v", err)
	}
	it, err := job.Read(ctx)
	if err != nil {
		t.Fatalf("CALL: %v", err)
	}
	var out []bigquery.Value
	if err := it.Next(&out); err != nil {
		t.Fatalf("CALL's SELECT: %v", err)
	}
	if len(out) != 2 || out[0] != int64(2) || out[1] != false {
		t.Errorf("rows, escaped = %v; want 2 (apple, apricot) and false (no route out)", out)
	}
	counts := map[string]int64{}
	rit := c.Dataset(ds).Table("counts").Read(ctx)
	for {
		var row map[string]bigquery.Value
		err := rit.Next(&row)
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("read counts: %v", err)
		}
		w, _ := row["word"].(string)
		n, _ := row["total"].(int64)
		counts[w] = n
	}
	if len(counts) != 3 || counts["apple"] != 5 || counts["apricot"] != 1 || counts["banana"] != 4 {
		t.Errorf("counts written by Spark: %v", counts)
	}

	// The CALL's child job: its sparkStatistics.
	kids, err := svc.Jobs.List(project).ParentJobId(job.ID()).AllUsers(true).Context(ctx).Do()
	if err != nil {
		t.Fatalf("jobs.list parentJobId: %v", err)
	}
	var stats *bq.SparkStatistics
	for _, k := range kids.Jobs {
		full, err := svc.Jobs.Get(project, k.JobReference.JobId).Location(k.JobReference.Location).Context(ctx).Do()
		if err != nil {
			t.Fatalf("jobs.get %s: %v", k.JobReference.JobId, err)
		}
		if full.Statistics != nil && full.Statistics.Query != nil && full.Statistics.Query.SparkStatistics != nil {
			stats = full.Statistics.Query.SparkStatistics
		}
	}
	if stats == nil || stats.SparkJobId == "" || stats.SparkJobLocation == "" || stats.LoggingInfo == nil ||
		stats.LoggingInfo.ResourceType != "bigquery.googleapis.com/SparkJob" || stats.LoggingInfo.ProjectId != project {
		t.Fatalf("the CALL's child job's sparkStatistics: %+v (of %d children)", stats, len(kids.Jobs))
	}

	// The driver's output, by the page's filter built from sparkStatistics.
	ac, err := logadmin.NewClient(ctx, "projects/"+project, option.WithEndpoint(h.Endpoint(EnvLogging)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	filter := fmt.Sprintf(`resource.type="%s" resource.labels.resource_container="%s" resource.labels.spark_job_id="%s" `+
		`resource.labels.location="%s" labels.component="DRIVER"`, stats.LoggingInfo.ResourceType, stats.LoggingInfo.ProjectId,
		stats.SparkJobId, stats.SparkJobLocation)
	found := false
	for deadline := time.Now().Add(30 * time.Second); !found && time.Now().Before(deadline); time.Sleep(time.Second) {
		eit := ac.Entries(ctx, logadmin.Filter(filter))
		for {
			e, err := eit.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("entries.list %s: %v", filter, err)
			}
			if s, _ := e.Payload.(string); strings.Contains(s, "counted for ap ap") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no driver entry with the procedure's output for %s", filter)
	}

	// main_file_uri and py_file_uris, from Cloud Storage, with properties.
	put := func(name, body string) {
		t.Helper()
		w := bh.Object(name).NewWriter(ctx)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	put("spark/helper.py", "def label(x):\n    return 'n=' + str(x)\n")
	put("spark/main.py", fmt.Sprintf(`
from pyspark.sql import SparkSession
from bigquery.spark.procedure import SparkProcParamContext
import helper
spark = SparkSession.builder.getOrCreate()
ctx = SparkProcParamContext.getOrCreate(spark)
n = spark.read.format("bigquery").load("%s.words").count()
ctx.label = helper.label(n) + ":" + spark.conf.get("spark.cb.marker")
`, full))
	b := bh.BucketName()
	if err := mlRunIn(ctx, c, fmt.Sprintf("CREATE PROCEDURE `%s.fromfile`(OUT label STRING) WITH CONNECTION `%s` "+
		"OPTIONS(engine=\"SPARK\", main_file_uri=\"gs://%s/spark/main.py\", py_file_uris=[\"gs://%[3]s/spark/helper.py\"], "+
		"properties=[(\"spark.cb.marker\", \"m1\"), (\"spark.executor.instances\", \"1\")]) LANGUAGE PYTHON", ds, conn, b)); err != nil {
		t.Fatalf("CREATE PROCEDURE (main_file_uri): %v", err)
	}
	r, err = svc.Routines.Get(project, ds, "fromfile").Context(ctx).Do()
	if err != nil {
		t.Fatalf("routines.get: %v", err)
	}
	if o := r.SparkOptions; o == nil || o.MainFileUri != "gs://"+b+"/spark/main.py" || len(o.PyFileUris) != 1 || o.Properties["spark.cb.marker"] != "m1" {
		t.Errorf("sparkOptions %+v", r.SparkOptions)
	}
	it, err = c.Query(fmt.Sprintf("DECLARE label STRING; CALL `%s.fromfile`(label); SELECT label", ds)).Read(ctx)
	if err != nil {
		t.Fatalf("CALL fromfile: %v", err)
	}
	if err := it.Next(&out); err != nil || len(out) != 1 || out[0] != "n=4:m1" {
		t.Errorf("CALL fromfile: %v %v, want n=4:m1", out, err)
	}

	// A failing application fails its CALL.
	if err := mlRunIn(ctx, c, fmt.Sprintf("CREATE PROCEDURE `%s.boom`() WITH CONNECTION `%s` OPTIONS(engine=\"SPARK\") "+
		"LANGUAGE PYTHON AS r\"\"\"raise ValueError('kaboom')\"\"\"", ds, conn)); err != nil {
		t.Fatalf("CREATE PROCEDURE boom: %v", err)
	}
	if err := mlRunIn(ctx, c, fmt.Sprintf("CALL `%s.boom`()", ds)); err == nil || !strings.Contains(err.Error(), "kaboom") {
		t.Errorf("CALL of a failing procedure: %v, want its error", err)
	}

	// The documented refusals.
	for _, tc := range []struct{ sql, want string }{
		{fmt.Sprintf("CREATE PROCEDURE `%s.x1`() WITH CONNECTION `%s` LANGUAGE PYTHON AS r'print(1)'", ds, conn), "engine"},
		{fmt.Sprintf("CREATE PROCEDURE `%s.x2`() WITH CONNECTION `%s` OPTIONS(engine=\"SPARK\", main_file_uri=\"gs://%s/spark/main.py\") LANGUAGE PYTHON AS r'print(1)'", ds, conn, b), "main_file_uri"},
		{fmt.Sprintf("CREATE PROCEDURE `%s.x3`() WITH CONNECTION `%s` OPTIONS(engine=\"SPARK\", main_file_uri=\"gs://%s/spark/main.py\", "+
			"properties=[(\"spark.executor.instances\", \"3\"), (\"spark.yarn.am.memory\", \"3g\")]) LANGUAGE PYTHON", ds, conn, b), "[spark:spark.yarn.am.memory]"},
		{fmt.Sprintf("CREATE PROCEDURE `%s.x4`() WITH CONNECTION `%s` OPTIONS(engine=\"SPARK\", runtime_version=\"5.0\") LANGUAGE PYTHON AS r'print(1)'", ds, conn), "runtime_version"},
		{fmt.Sprintf("CREATE PROCEDURE `%s.x5`() WITH CONNECTION `%s` OPTIONS(engine=\"SPARK\", container_image=\"gcr.io/p/i\") LANGUAGE PYTHON AS r'print(1)'", ds, conn), "Artifact Registry"},
		{fmt.Sprintf("CREATE PROCEDURE `%s.x6`() WITH CONNECTION `%s` OPTIONS(engine=\"SPARK\", main_class=\"com.example.Main\") LANGUAGE PYTHON AS r'print(1)'", ds, conn), "main_class"},
	} {
		if err := mlRunIn(ctx, c, tc.sql); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error naming %q", tc.sql, err, tc.want)
		}
	}
	names := []string{}
	rit2 := c.Dataset(ds).Routines(ctx)
	for {
		rt, err := rit2.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("routines.list: %v", err)
		}
		names = append(names, rt.RoutineID)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "boom,fromfile,wordcount" {
		t.Errorf("routines %v, want the three made (none of the refused)", names)
	}
}
