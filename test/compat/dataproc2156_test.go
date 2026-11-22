//go:build compat

package compat

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	dataproc "cloud.google.com/go/dataproc/v2/apiv1"
	"cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	"cloud.google.com/go/logging/logadmin"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/emptypb"
)

// EnvDataproc is the Dataproc API's serverless surface (#2156), gRPC and
// JSON on one port: Serverless for Apache Spark's batches, sessions and
// session templates.
const EnvDataproc = "CLOUDBURROW_TEST_DATAPROC"

// dataprocEnv returns the official Go clients of the Dataproc API, gRPC
// and REST, after the endpoints the tests use, or skips.
func dataprocEnv(t *testing.T) (*Harness, string, string) {
	t.Helper()
	h := New(t)
	addr := h.Endpoint(EnvDataproc)
	h.Endpoint(EnvBigQuery)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvLogging)
	if os.Getenv(EnvSpark) == "" {
		t.Skipf("%s is not set: Dataproc's batches and sessions run in the pinned apache/spark images", EnvSpark)
	}
	project := strings.TrimSpace(os.Getenv(EnvBigQueryProject))
	if project == "" {
		t.Skipf("%s is not set: the emulator serves one project, and the test must use it", EnvBigQueryProject)
	}
	return h, addr, project
}

func dataprocGRPC() []option.ClientOption {
	return []option.ClientOption{option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

// dataprocWordCount reads the dataset's words through Google's BigQuery
// connector for Spark (the instance's Storage Read API), writes the counts
// back (its Storage Write API), and prints its arguments and total.
const dataprocWordCount = `
import sys
from pyspark.sql import SparkSession

spark = SparkSession.builder.appName("dataproc-wordcount").getOrCreate()
words = spark.read.format("bigquery").load(sys.argv[1] + ".words")
counts = words.groupBy("word").sum("n").withColumnRenamed("sum(n)", "total")
counts.write.format("bigquery").option("writeMethod", "direct").mode("overwrite").save(sys.argv[1] + ".counts")
print("counted", counts.count(), "words for", sys.argv[2])
`

// TestDataprocServerlessBatches (#2156): Serverless for Apache Spark's
// batches, as https://docs.cloud.google.com/dataproc-serverless/docs/quickstarts/spark-batch
// and the v1 reference document them, through the official
// cloud.google.com/go/dataproc/v2 clients over gRPC and REST: a PySpark
// batch whose main file is in Cloud Storage reads a BigQuery table and
// writes one, ends SUCCEEDED with its state history, runtime version,
// usage and output URI, whose driver output is in Cloud Storage, its lines
// in Cloud Logging under cloud_dataproc_batch; batches.list filters by
// state and label; a Spark SQL batch takes query variables; a running
// batch cannot be deleted and is cancelled through its operation; a
// failing batch ends FAILED. And the documented refusals: an ID of
// capitals, a duplicate, an unknown runtime version.
//
// covers: google.cloud.dataproc.v1.BatchController/CreateBatch, google.cloud.dataproc.v1.BatchController/GetBatch, google.cloud.dataproc.v1.BatchController/ListBatches, google.cloud.dataproc.v1.BatchController/DeleteBatch, google.longrunning.Operations/GetOperation, google.longrunning.Operations/CancelOperation, google.logging.v2.LoggingServiceV2/ListLogEntries
func TestDataprocServerlessBatches(t *testing.T) {
	h, addr, project := dataprocEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	g, err := dataproc.NewBatchControllerClient(ctx, append(dataprocGRPC(), option.WithEndpoint(addr))...)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	r, err := dataproc.NewBatchControllerRESTClient(ctx, option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	bq, _ := bigqueryClient(t, h)
	sc := storageClient(t, h)
	bh := bucket(t, h, sc)
	ds := "compat_dataproc_" + strings.ReplaceAll(h.Project(), "-", "_")
	if err := bq.Dataset(ds).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = bq.Dataset(ds).DeleteWithContents(h.Context()) })
	if err := mlRunIn(ctx, bq, fmt.Sprintf("CREATE TABLE `%s.words` AS SELECT * FROM UNNEST([STRUCT('apple' AS word, 2 AS n), ('apricot', 1), ('apple', 3), ('banana', 4)])", ds)); err != nil {
		t.Fatalf("CREATE TABLE words: %v", err)
	}
	put := func(name, body string) string {
		w := bh.Object(name).NewWriter(ctx)
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return "gs://" + bh.BucketName() + "/" + name
	}
	parent := "projects/" + project + "/locations/us-central1"
	id := "wc-" + strings.ToLower(strings.ReplaceAll(h.Project(), "_", "-"))
	if len(id) > 50 {
		id = id[:50]
	}

	// The PySpark batch.
	op, err := g.CreateBatch(ctx, &dataprocpb.CreateBatchRequest{Parent: parent, BatchId: id, Batch: &dataprocpb.Batch{
		BatchConfig: &dataprocpb.Batch_PysparkBatch{PysparkBatch: &dataprocpb.PySparkBatch{
			MainPythonFileUri: put("wordcount.py", dataprocWordCount), Args: []string{project + "." + ds, "compat"}}},
		RuntimeConfig: &dataprocpb.RuntimeConfig{Version: "2.2", Properties: map[string]string{"spark.executor.instances": "2"}},
		Labels:        map[string]string{"suite": "compat"}}})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	md, err := op.Metadata()
	if err != nil || md.GetBatch() != parent+"/batches/"+id || md.GetOperationType() != dataprocpb.BatchOperationMetadata_BATCH {
		t.Errorf("BatchOperationMetadata: %v %v", md, err)
	}
	b, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("the batch's operation: %v", err)
	}
	t.Cleanup(func() { _ = g.DeleteBatch(h.Context(), &dataprocpb.DeleteBatchRequest{Name: b.GetName()}) })
	if b.State != dataprocpb.Batch_SUCCEEDED || b.RuntimeConfig.GetVersion() != "2.2" || len(b.StateHistory) < 2 ||
		b.RuntimeInfo.GetOutputUri() == "" || b.RuntimeInfo.GetApproximateUsage().GetMilliDcuSeconds() <= 0 || b.Creator == "" {
		t.Fatalf("the batch: %v", b)
	}
	got, err := r.GetBatch(ctx, &dataprocpb.GetBatchRequest{Name: b.Name})
	if err != nil || got.Uuid != b.Uuid || got.State != dataprocpb.Batch_SUCCEEDED {
		t.Errorf("GetBatch over REST: %v %v", got, err)
	}
	// The table it wrote.
	it := bq.Dataset(ds).Table("counts").Read(ctx)
	totals := map[string]int64{}
	for {
		var row struct {
			Word  string `bigquery:"word"`
			Total int64  `bigquery:"total"`
		}
		if err := it.Next(&row); err == iterator.Done {
			break
		} else if err != nil {
			t.Fatalf("reading counts: %v", err)
		}
		totals[row.Word] = row.Total
	}
	if totals["apple"] != 5 || totals["banana"] != 4 || len(totals) != 3 {
		t.Errorf("counts: %v", totals)
	}
	// The driver's output in Cloud Storage.
	ob, ok := strings.CutPrefix(b.RuntimeInfo.OutputUri, "gs://")
	bucketName, prefix, _ := strings.Cut(ob, "/")
	if !ok {
		t.Fatalf("outputUri %s", b.RuntimeInfo.OutputUri)
	}
	var output strings.Builder
	oit := sc.Bucket(bucketName).Objects(ctx, nil)
	for {
		a, err := oit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("listing %s: %v", bucketName, err)
		}
		if strings.HasPrefix(a.Name, prefix) {
			rd, err := sc.Bucket(bucketName).Object(a.Name).NewReader(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(&output, rd)
			_ = rd.Close()
		}
	}
	if !strings.Contains(output.String(), "counted 3 words for compat") {
		t.Errorf("driver output at %s: %q", b.RuntimeInfo.OutputUri, output.String())
	}
	// Its lines in Cloud Logging.
	ac, err := logadmin.NewClient(ctx, "projects/"+project, option.WithEndpoint(h.Endpoint(EnvLogging)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	filter := fmt.Sprintf(`resource.type="cloud_dataproc_batch" resource.labels.batch_id="%s" logName="projects/%s/logs/dataproc.googleapis.com%%2Foutput"`, id, project)
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
			if s, _ := e.Payload.(string); strings.Contains(s, "counted 3 words") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no driver output under %s", filter)
	}

	// The list, by state and label.
	for filter, want := range map[string]bool{`state = SUCCEEDED AND labels.suite = compat`: true, `state = FAILED`: false} {
		lit := g.ListBatches(ctx, &dataprocpb.ListBatchesRequest{Parent: parent, Filter: filter, OrderBy: "create_time desc"})
		seen := false
		for {
			x, err := lit.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("ListBatches %q: %v", filter, err)
			}
			seen = seen || x.Name == b.Name
		}
		if seen != want {
			t.Errorf("ListBatches %q lists the batch: %v", filter, seen)
		}
	}

	// The refusals.
	if _, err := g.CreateBatch(ctx, &dataprocpb.CreateBatchRequest{Parent: parent, BatchId: id, Batch: &dataprocpb.Batch{
		BatchConfig: b.BatchConfig}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a duplicate batch ID: %v", err)
	}
	if _, err := g.CreateBatch(ctx, &dataprocpb.CreateBatchRequest{Parent: parent, BatchId: "Capitals", Batch: &dataprocpb.Batch{
		BatchConfig: b.BatchConfig}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a batch ID of capitals: %v", err)
	}
	if _, err := g.CreateBatch(ctx, &dataprocpb.CreateBatchRequest{Parent: parent, Batch: &dataprocpb.Batch{
		BatchConfig: b.BatchConfig, RuntimeConfig: &dataprocpb.RuntimeConfig{Version: "9.9"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("runtime version 9.9: %v", err)
	}

	// Spark SQL with its query variables.
	sqlOp, err := g.CreateBatch(ctx, &dataprocpb.CreateBatchRequest{Parent: parent, Batch: &dataprocpb.Batch{
		BatchConfig: &dataprocpb.Batch_SparkSqlBatch{SparkSqlBatch: &dataprocpb.SparkSqlBatch{
			QueryFileUri: put("q.sql", "SELECT ${n} * 2 AS doubled;"), QueryVariables: map[string]string{"n": "21"}}}}})
	if err != nil {
		t.Fatalf("CreateBatch (Spark SQL): %v", err)
	}
	sb, err := sqlOp.Wait(ctx)
	if err != nil || sb.State != dataprocpb.Batch_SUCCEEDED {
		t.Fatalf("the Spark SQL batch: %v %v", sb, err)
	}
	t.Cleanup(func() { _ = g.DeleteBatch(h.Context(), &dataprocpb.DeleteBatchRequest{Name: sb.GetName()}) })

	// A failing batch.
	failOp, err := g.CreateBatch(ctx, &dataprocpb.CreateBatchRequest{Parent: parent, Batch: &dataprocpb.Batch{
		BatchConfig: &dataprocpb.Batch_PysparkBatch{PysparkBatch: &dataprocpb.PySparkBatch{MainPythonFileUri: put("fail.py", "raise SystemExit(3)\n")}}}})
	if err != nil {
		t.Fatalf("CreateBatch (failing): %v", err)
	}
	if fb, err := failOp.Wait(ctx); err == nil {
		t.Errorf("a failing batch's operation succeeded: %v", fb)
	}
	md, _ = failOp.Metadata()
	fb, err := g.GetBatch(ctx, &dataprocpb.GetBatchRequest{Name: md.GetBatch()})
	if err != nil || fb.State != dataprocpb.Batch_FAILED || fb.StateMessage == "" {
		t.Errorf("the failing batch: %v %v", fb, err)
	}
	_ = g.DeleteBatch(ctx, &dataprocpb.DeleteBatchRequest{Name: md.GetBatch()})

	// A running batch: not deleted, cancelled.
	slowOp, err := g.CreateBatch(ctx, &dataprocpb.CreateBatchRequest{Parent: parent, Batch: &dataprocpb.Batch{
		BatchConfig: &dataprocpb.Batch_PysparkBatch{PysparkBatch: &dataprocpb.PySparkBatch{MainPythonFileUri: put("slow.py", "import time\ntime.sleep(900)\n")}}}})
	if err != nil {
		t.Fatalf("CreateBatch (slow): %v", err)
	}
	md, _ = slowOp.Metadata()
	for deadline := time.Now().Add(10 * time.Minute); ; time.Sleep(2 * time.Second) {
		x, err := g.GetBatch(ctx, &dataprocpb.GetBatchRequest{Name: md.GetBatch()})
		if err != nil {
			t.Fatal(err)
		}
		if x.State == dataprocpb.Batch_RUNNING {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the slow batch is %v", x.State)
		}
	}
	if err := g.DeleteBatch(ctx, &dataprocpb.DeleteBatchRequest{Name: md.GetBatch()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("deleting a running batch: %v", err)
	}
	if err := g.CancelOperation(ctx, &longrunningpb.CancelOperationRequest{Name: slowOp.Name()}); err != nil {
		t.Fatalf("CancelOperation: %v", err)
	}
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(time.Second) {
		x, _ := g.GetBatch(ctx, &dataprocpb.GetBatchRequest{Name: md.GetBatch()})
		if x.GetState() == dataprocpb.Batch_CANCELLED {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cancelled batch is %v", x.GetState())
		}
	}
	if err := g.DeleteBatch(ctx, &dataprocpb.DeleteBatchRequest{Name: md.GetBatch()}); err != nil {
		t.Errorf("deleting a cancelled batch: %v", err)
	}
}

// sparkConnectRequest is a Spark Connect ExecutePlanRequest (Spark 4.0.1's
// spark/connect/base.proto: session_id 1, user_context 2 with user_id 1,
// plan 3 with root 1) whose root is rel.
func sparkConnectRequest(session string, rel []byte) []byte {
	user := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "compat")
	plan := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), rel)
	b := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), session)
	b = protowire.AppendBytes(protowire.AppendTag(b, 2, protowire.BytesType), user)
	return protowire.AppendBytes(protowire.AppendTag(b, 3, protowire.BytesType), plan)
}

// sqlRelation is a Relation (relations.proto) of sql 10: SQL query 1.
func sqlRelation(q string) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, 10, protowire.BytesType),
		protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), q))
}

// bigqueryRelation is a Relation of read 2: Read's data_source 2 with
// format 1 "bigquery" and the option table (options 3).
func bigqueryRelation(table string) []byte {
	entry := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "table")
	entry = protowire.AppendString(protowire.AppendTag(entry, 2, protowire.BytesType), table)
	src := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "bigquery")
	src = protowire.AppendBytes(protowire.AppendTag(src, 3, protowire.BytesType), entry)
	read := protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), src)
	return protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), read)
}

// sparkConnectRows runs req through ExecutePlan on conn and sums the
// arrow batches' row_count (ExecutePlanResponse's arrow_batch 2, its
// row_count 1).
func sparkConnectRows(ctx context.Context, conn *grpc.ClientConn, req []byte) (int64, error) {
	in := &emptypb.Empty{}
	in.ProtoReflect().SetUnknown(req)
	cs, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/spark.connect.SparkConnectService/ExecutePlan")
	if err != nil {
		return 0, err
	}
	if err := cs.SendMsg(in); err != nil {
		return 0, err
	}
	_ = cs.CloseSend()
	var rows int64
	for {
		m := &emptypb.Empty{}
		if err := cs.RecvMsg(m); err == io.EOF {
			return rows, nil
		} else if err != nil {
			return rows, err
		}
		b := []byte(m.ProtoReflect().GetUnknown())
		for len(b) > 0 {
			num, typ, n := protowire.ConsumeTag(b)
			b = b[n:]
			if num == 2 && typ == protowire.BytesType {
				v, m := protowire.ConsumeBytes(b)
				for len(v) > 0 {
					fn, ft, k := protowire.ConsumeTag(v)
					v = v[k:]
					if fn == 1 && ft == protowire.VarintType {
						c, k := protowire.ConsumeVarint(v)
						rows += int64(c)
						v = v[k:]
						continue
					}
					v = v[protowire.ConsumeFieldValue(fn, ft, v):]
				}
				b = b[m:]
				continue
			}
			k := protowire.ConsumeFieldValue(num, typ, b)
			if k < 0 {
				break
			}
			b = b[k:]
		}
	}
}

// TestDataprocServerlessSessions (#2156): interactive sessions, as
// https://docs.cloud.google.com/dataproc-serverless/docs/guides/create-serverless-sessions-templates
// and the v1 reference document them, through the official
// cloud.google.com/go/dataproc/v2 clients: a session template of a Spark
// Connect session on runtime 3.0; a session created from it goes ACTIVE
// with its "Spark Connect Server" endpoint, which runs a Spark Connect
// query, SQL and a read of a BigQuery table through Google's connector;
// sessions.list filters it; it terminates, and is deleted; a Jupyter
// session is refused (501), and a session ID of capitals.
//
// covers: google.cloud.dataproc.v1.SessionTemplateController/CreateSessionTemplate, google.cloud.dataproc.v1.SessionTemplateController/GetSessionTemplate, google.cloud.dataproc.v1.SessionTemplateController/ListSessionTemplates, google.cloud.dataproc.v1.SessionTemplateController/UpdateSessionTemplate, google.cloud.dataproc.v1.SessionTemplateController/DeleteSessionTemplate, google.cloud.dataproc.v1.SessionController/CreateSession, google.cloud.dataproc.v1.SessionController/GetSession, google.cloud.dataproc.v1.SessionController/ListSessions, google.cloud.dataproc.v1.SessionController/TerminateSession, google.cloud.dataproc.v1.SessionController/DeleteSession
func TestDataprocServerlessSessions(t *testing.T) {
	h, addr, project := dataprocEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	tc, err := dataproc.NewSessionTemplateControllerClient(ctx, append(dataprocGRPC(), option.WithEndpoint(addr))...)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	sc, err := dataproc.NewSessionControllerClient(ctx, append(dataprocGRPC(), option.WithEndpoint(addr))...)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	rc, err := dataproc.NewSessionControllerRESTClient(ctx, option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	bq, _ := bigqueryClient(t, h)
	ds := "compat_dpsess_" + strings.ReplaceAll(h.Project(), "-", "_")
	if err := bq.Dataset(ds).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = bq.Dataset(ds).DeleteWithContents(h.Context()) })
	if err := mlRunIn(ctx, bq, fmt.Sprintf("CREATE TABLE `%s.t` AS SELECT x FROM UNNEST(GENERATE_ARRAY(1, 7)) AS x", ds)); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	parent := "projects/" + project + "/locations/us-central1"
	suffix := strings.ToLower(strings.ReplaceAll(h.Project(), "_", "-"))
	if len(suffix) > 40 {
		suffix = suffix[:40]
	}

	tmpl, err := tc.CreateSessionTemplate(ctx, &dataprocpb.CreateSessionTemplateRequest{Parent: parent, SessionTemplate: &dataprocpb.SessionTemplate{
		Name: parent + "/sessionTemplates/connect-" + suffix, Description: "Spark Connect on 3.0",
		SessionConfig: &dataprocpb.SessionTemplate_SparkConnectSession{SparkConnectSession: &dataprocpb.SparkConnectConfig{}},
		RuntimeConfig: &dataprocpb.RuntimeConfig{Version: "3.0"}, Labels: map[string]string{"suite": "compat"}}})
	if err != nil {
		t.Fatalf("CreateSessionTemplate: %v", err)
	}
	t.Cleanup(func() {
		_ = tc.DeleteSessionTemplate(h.Context(), &dataprocpb.DeleteSessionTemplateRequest{Name: tmpl.Name})
	})
	if tmpl.Uuid == "" || tmpl.CreateTime == nil || tmpl.Creator == "" {
		t.Errorf("the template: %v", tmpl)
	}
	tmpl.Description = "Spark Connect, runtime 3.0"
	if up, err := tc.UpdateSessionTemplate(ctx, &dataprocpb.UpdateSessionTemplateRequest{SessionTemplate: tmpl}); err != nil || up.Description != tmpl.Description {
		t.Errorf("UpdateSessionTemplate: %v %v", up, err)
	}
	if got, err := tc.GetSessionTemplate(ctx, &dataprocpb.GetSessionTemplateRequest{Name: tmpl.Name}); err != nil || got.GetRuntimeConfig().GetVersion() != "3.0" {
		t.Errorf("GetSessionTemplate: %v %v", got, err)
	}
	listed := false
	for tit := tc.ListSessionTemplates(ctx, &dataprocpb.ListSessionTemplatesRequest{Parent: parent}); ; {
		x, err := tit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListSessionTemplates: %v", err)
		}
		listed = listed || x.Name == tmpl.Name
	}
	if !listed {
		t.Error("ListSessionTemplates does not list the template")
	}

	if _, err := sc.CreateSession(ctx, &dataprocpb.CreateSessionRequest{Parent: parent, SessionId: "Capitals", Session: &dataprocpb.Session{SessionTemplate: tmpl.Name}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a session ID of capitals: %v", err)
	}
	if _, err := sc.CreateSession(ctx, &dataprocpb.CreateSessionRequest{Parent: parent, SessionId: "jupyter-" + suffix, Session: &dataprocpb.Session{
		SessionConfig: &dataprocpb.Session_JupyterSession{JupyterSession: &dataprocpb.JupyterConfig{}}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("a Jupyter session: %v", err)
	}

	op, err := sc.CreateSession(ctx, &dataprocpb.CreateSessionRequest{Parent: parent, SessionId: "sess-" + suffix, Session: &dataprocpb.Session{SessionTemplate: tmpl.Name}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	x, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("the session's operation: %v", err)
	}
	t.Cleanup(func() {
		if dop, err := sc.DeleteSession(h.Context(), &dataprocpb.DeleteSessionRequest{Name: x.Name}); err == nil {
			_, _ = dop.Wait(h.Context())
		}
	})
	x, err = rc.GetSession(ctx, &dataprocpb.GetSessionRequest{Name: x.Name})
	endpoint := x.GetRuntimeInfo().GetEndpoints()["Spark Connect Server"]
	if err != nil || x.State != dataprocpb.Session_ACTIVE || !strings.HasPrefix(endpoint, "sc://") || x.Labels["suite"] != "compat" || x.RuntimeConfig.GetVersion() != "3.0" {
		t.Fatalf("the session: %v %v", x, err)
	}

	// A Spark Connect query, on the endpoint's host and port.
	conn, err := grpc.NewClient("passthrough:///"+strings.TrimPrefix(endpoint, "sc://"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if n, err := sparkConnectRows(ctx, conn, sparkConnectRequest(x.Uuid, sqlRelation("SELECT id FROM range(5)"))); err != nil || n != 5 {
		t.Errorf("Spark Connect SELECT of range(5): %d rows, %v", n, err)
	}
	if n, err := sparkConnectRows(ctx, conn, sparkConnectRequest(x.Uuid, bigqueryRelation(project+"."+ds+".t"))); err != nil || n != 7 {
		t.Errorf("Spark Connect read of the BigQuery table: %d rows, %v", n, err)
	}
	if _, err := sparkConnectRows(ctx, conn, sparkConnectRequest("00000000-0000-4000-8000-000000000000", sqlRelation("SELECT 1"))); status.Code(err) != codes.NotFound {
		t.Errorf("Spark Connect with no session's UUID: %v", err)
	}

	seen := false
	for lit := sc.ListSessions(ctx, &dataprocpb.ListSessionsRequest{Parent: parent, Filter: `state = ACTIVE`}); ; {
		s, err := lit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListSessions: %v", err)
		}
		seen = seen || s.Name == x.Name
	}
	if !seen {
		t.Error("ListSessions state = ACTIVE does not list the session")
	}

	top, err := sc.TerminateSession(ctx, &dataprocpb.TerminateSessionRequest{Name: x.Name})
	if err != nil {
		t.Fatalf("TerminateSession: %v", err)
	}
	if _, err := top.Wait(ctx); err != nil {
		t.Fatalf("the terminate operation: %v", err)
	}
	if x, err = sc.GetSession(ctx, &dataprocpb.GetSessionRequest{Name: x.Name}); err != nil || x.State != dataprocpb.Session_TERMINATED {
		t.Errorf("the terminated session: %v %v", x, err)
	}
	if _, err := sparkConnectRows(ctx, conn, sparkConnectRequest(x.GetUuid(), sqlRelation("SELECT 1"))); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Spark Connect to a terminated session: %v", err)
	}
	dop, err := sc.DeleteSession(ctx, &dataprocpb.DeleteSessionRequest{Name: x.Name})
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := dop.Wait(ctx); err != nil {
		t.Fatalf("the delete operation: %v", err)
	}
	if _, err := sc.GetSession(ctx, &dataprocpb.GetSessionRequest{Name: x.Name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSession after delete: %v", err)
	}
}
