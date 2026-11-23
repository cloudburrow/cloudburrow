//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	dataproc "cloud.google.com/go/dataproc/v2/apiv1"
	"cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// dataprocLakeScript writes and reads Delta Lake and Iceberg tables in
// Cloud Storage (argv[1]), as the components' pages' examples do.
const dataprocLakeScript = `import sys
from pyspark.sql import SparkSession
spark = SparkSession.builder.getOrCreate()
base = sys.argv[1]
spark.createDataFrame([(1, "a"), (2, "b")], ["id", "v"]).write.format("delta").mode("overwrite").save(base + "/delta")
spark.createDataFrame([(3, "c")], ["id", "v"]).write.format("delta").mode("append").save(base + "/delta")
print("DELTA_ROWS", spark.read.format("delta").load(base + "/delta").count())
print("DELTA_VERSIONS", spark.sql("DESCRIBE HISTORY delta.` + "`" + `%s/delta` + "`" + `" % base).count())
spark.sql("CREATE TABLE ice.db.t (id BIGINT, v STRING) USING iceberg")
spark.sql("INSERT INTO ice.db.t VALUES (1, 'x'), (2, 'y')")
spark.sql("INSERT INTO ice.db.t VALUES (3, 'z')")
print("ICEBERG_ROWS", spark.sql("SELECT count(*) FROM ice.db.t").first()[0])
print("ICEBERG_SNAPSHOTS", spark.sql("SELECT count(*) FROM ice.db.t.snapshots").first()[0])
print("ICEBERG_VERSION", spark._jvm.org.apache.iceberg.IcebergBuild.version())
print("EXTENSIONS", spark.conf.get("spark.sql.extensions"))
`

// dataprocHudiScript upserts a Hudi table in Cloud Storage twice and
// reads it, at its latest commit and at its first, as the Hudi page's
// example does.
const dataprocHudiScript = `import sys
from pyspark.sql import SparkSession
spark = SparkSession.builder.getOrCreate()
path = sys.argv[1] + "/hudi"
opts = {"hoodie.table.name": "fruit", "hoodie.datasource.write.recordkey.field": "id",
        "hoodie.datasource.write.precombine.field": "ts", "hoodie.datasource.write.partitionpath.field": "",
        "hoodie.datasource.write.operation": "upsert"}
spark.createDataFrame([(1, "apple", 1), (2, "banana", 1)], ["id", "name", "ts"]).write.format("hudi").options(**opts).mode("overwrite").save(path)
spark.createDataFrame([(2, "blueberry", 2), (3, "cherry", 2)], ["id", "name", "ts"]).write.format("hudi").options(**opts).mode("append").save(path)
df = spark.read.format("hudi").load(path)
print("HUDI_ROWS", df.count())
print("HUDI_NAMES", ",".join(sorted(r["name"] for r in df.collect())))
commits = [r[0] for r in df.select("_hoodie_commit_time").distinct().collect()]
print("HUDI_COMMITS", len(commits))
first = sorted(commits)[0]
print("HUDI_FIRST", spark.read.format("hudi").option("as.of.instant", first).load(path).count())
print("SERIALIZER", spark.conf.get("spark.serializer"))
`

// TestDataprocTableFormats (#2162): the DELTA, ICEBERG and HUDI optional
// components (https://docs.cloud.google.com/dataproc/docs/concepts/components/delta,
// .../iceberg, .../hudi), through the official Go client: a cluster with
// Delta Lake and Iceberg 1.10 (dataproc:dataproc.iceberg.version) runs a
// PySpark job that writes a Delta table and an Iceberg table (a hadoop
// catalog of the job's properties) in Cloud Storage and reads their
// history and snapshots; a cluster with Hudi upserts a Hudi table and
// reads it at two commits; an invalid Iceberg version and ICEBERG on
// image 3.0 are refused.
//
// covers: google.cloud.dataproc.v1.ClusterController/CreateCluster, google.cloud.dataproc.v1.JobController/SubmitJob
func TestDataprocTableFormats(t *testing.T) {
	h, addr, project := dataprocEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	grpcOpts := append(dataprocGRPC(), option.WithEndpoint(addr))
	cc, err := dataproc.NewClusterControllerClient(ctx, grpcOpts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	jc, err := dataproc.NewJobControllerClient(ctx, grpcOpts...)
	if err != nil {
		t.Fatal(err)
	}
	defer jc.Close()
	sc := storageClient(t, h)
	bh := bucket(t, h, sc)
	const region = "us-central1"
	suffix := dataprocSuffix(h, 30)
	bucketURI := "gs://" + bh.BucketName()

	// The refusals.
	for _, c := range []struct {
		version string
		props   map[string]string
		code    codes.Code
	}{
		{"2.2", map[string]string{"dataproc:dataproc.iceberg.version": "0.9"}, codes.InvalidArgument},
		{"3.0", nil, codes.Unimplemented},
	} {
		_, err := cc.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: project, Region: region, Cluster: &dataprocpb.Cluster{
			ProjectId: project, ClusterName: "lake-bad-" + suffix, Config: &dataprocpb.ClusterConfig{SoftwareConfig: &dataprocpb.SoftwareConfig{
				ImageVersion: c.version, Properties: c.props, OptionalComponents: []dataprocpb.Component{dataprocpb.Component_ICEBERG}}}}})
		if status.Code(err) != c.code {
			t.Errorf("ICEBERG on %s with %v: %v, want %v", c.version, c.props, err, c.code)
		}
	}

	single := map[string]string{"dataproc:dataproc.allow.zero.workers": "true"}
	run := func(cluster, id, script string, props map[string]string, args ...string) string {
		main := dataprocPut(t, ctx, bh, id+".py", script)
		j := dataprocRunJob(t, ctx, jc, project, region, &dataprocpb.Job{Placement: &dataprocpb.JobPlacement{ClusterName: cluster},
			Reference: &dataprocpb.JobReference{JobId: id + "-" + suffix},
			TypeJob:   &dataprocpb.Job_PysparkJob{PysparkJob: &dataprocpb.PySparkJob{MainPythonFileUri: main, Args: args, Properties: props}}})
		out := dataprocGSText(t, ctx, sc, j.GetDriverOutputResourceUri())
		if j.GetStatus().GetState() != dataprocpb.JobStatus_DONE {
			t.Fatalf("job %s: %v\n%s", id, j.GetStatus(), out)
		}
		return out
	}

	// Delta Lake and Iceberg 1.10.
	lake := "lake-" + suffix
	props := map[string]string{"dataproc:dataproc.iceberg.version": "1.10"}
	for k, v := range single {
		props[k] = v
	}
	dataprocClusterUp(t, ctx, h, cc, project, region, &dataprocpb.Cluster{ProjectId: project, ClusterName: lake,
		Config: &dataprocpb.ClusterConfig{SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.2", Properties: props,
			OptionalComponents: []dataprocpb.Component{dataprocpb.Component_DELTA, dataprocpb.Component_ICEBERG}}}})
	out := run(lake, "lake", dataprocLakeScript, map[string]string{"spark.sql.catalog.ice": "org.apache.iceberg.spark.SparkCatalog",
		"spark.sql.catalog.ice.type": "hadoop", "spark.sql.catalog.ice.warehouse": bucketURI + "/iceberg"}, bucketURI+"/tables")
	for _, want := range []string{"DELTA_ROWS 3", "DELTA_VERSIONS 2", "ICEBERG_ROWS 3", "ICEBERG_SNAPSHOTS 2", "ICEBERG_VERSION 1.10.0",
		"EXTENSIONS io.delta.sql.DeltaSparkSessionExtension,org.apache.iceberg.spark.extensions.IcebergSparkSessionExtensions"} {
		if !strings.Contains(out, want) {
			t.Errorf("the Delta Lake and Iceberg job's output has no %q:\n%s", want, out)
		}
	}
	if logs := dataprocGSText(t, ctx, sc, bucketURI+"/tables/delta/_delta_log/00000000000000000001.json"); !strings.Contains(logs, `"commitInfo"`) {
		t.Errorf("the Delta table's log in Cloud Storage: %q", logs)
	}
	if meta := dataprocGSText(t, ctx, sc, bucketURI+"/iceberg/db/t/metadata/version-hint.text"); strings.TrimSpace(meta) == "" {
		t.Error("the Iceberg table has no metadata in Cloud Storage")
	}

	// Hudi.
	hudi := "hudi-" + suffix
	dataprocClusterUp(t, ctx, h, cc, project, region, &dataprocpb.Cluster{ProjectId: project, ClusterName: hudi,
		Config: &dataprocpb.ClusterConfig{SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.2", Properties: single,
			OptionalComponents: []dataprocpb.Component{dataprocpb.Component_HUDI}}}})
	out = run(hudi, "hudi", dataprocHudiScript, nil, bucketURI)
	for _, want := range []string{"HUDI_ROWS 3", "HUDI_NAMES apple,blueberry,cherry", "HUDI_COMMITS 2", "HUDI_FIRST 2",
		"SERIALIZER org.apache.spark.serializer.KryoSerializer"} {
		if !strings.Contains(out, want) {
			t.Errorf("the Hudi job's output has no %q:\n%s", want, out)
		}
	}
}

// dataprocMagicsNotebook's cells use IPython's line and cell magics, a
// shell escape and a displayed last expression.
const dataprocMagicsNotebook = `{
 "nbformat": 4, "nbformat_minor": 5, "metadata": {},
 "cells": [
  {"cell_type": "code", "metadata": {"tags": ["parameters"]}, "source": ["n = \"3\"\n"], "outputs": [], "execution_count": null},
  {"cell_type": "code", "metadata": {}, "source": ["%env CB_GREETING=hello\n", "import os\n", "print(\"env\", os.environ[\"CB_GREETING\"])"], "outputs": [], "execution_count": null},
  {"cell_type": "code", "metadata": {}, "source": ["%%writefile /tmp/cb-cell.txt\n", "written by a cell magic\n"], "outputs": [], "execution_count": null},
  {"cell_type": "code", "metadata": {}, "source": ["print(\"file\", open(\"/tmp/cb-cell.txt\").read().strip())\n", "!echo shell-$((2+3))"], "outputs": [], "execution_count": null},
  {"cell_type": "code", "metadata": {}, "source": ["from pyspark.sql import SparkSession\n", "spark = SparkSession.builder.getOrCreate()\n",
    "%time total = spark.range(int(n)).count()\n", "total * 100"], "outputs": [], "execution_count": null}
 ]
}`

// TestDataprocNotebookMagics (#2162): a pyspark_notebook_batch's cells
// run in IPython, as a notebook's kernel runs them, through the official
// Go client: %env, %%writefile, a shell escape, %time and a displayed
// last expression run; a cell that raises fails the batch, naming it.
//
// covers: google.cloud.dataproc.v1.BatchController/CreateBatch, google.cloud.dataproc.v1.BatchController/GetBatch
func TestDataprocNotebookMagics(t *testing.T) {
	h, addr, project := dataprocEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	bc, err := dataproc.NewBatchControllerClient(ctx, append(dataprocGRPC(), option.WithEndpoint(addr))...)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Close()
	sc := storageClient(t, h)
	bh := bucket(t, h, sc)
	suffix := dataprocSuffix(h, 30)
	parent := "projects/" + project + "/locations/us-central1"
	batch := func(id, nb string) *dataprocpb.Batch {
		op, err := bc.CreateBatch(ctx, &dataprocpb.CreateBatchRequest{Parent: parent, BatchId: id + "-" + suffix, Batch: &dataprocpb.Batch{
			BatchConfig: &dataprocpb.Batch_PysparkNotebookBatch{PysparkNotebookBatch: &dataprocpb.PySparkNotebookBatch{
				NotebookFileUri: dataprocPut(t, ctx, bh, id+".ipynb", nb), Params: map[string]string{"n": "7"}}},
			RuntimeConfig: &dataprocpb.RuntimeConfig{Version: "2.2"}}})
		if err != nil {
			t.Fatalf("CreateBatch %s: %v", id, err)
		}
		b, _ := op.Wait(ctx)
		if b == nil {
			b, err = bc.GetBatch(ctx, &dataprocpb.GetBatchRequest{Name: parent + "/batches/" + id + "-" + suffix})
			if err != nil {
				t.Fatalf("GetBatch %s: %v", id, err)
			}
		}
		t.Cleanup(func() { _ = bc.DeleteBatch(h.Context(), &dataprocpb.DeleteBatchRequest{Name: b.Name}) })
		return b
	}
	b := batch("magics", dataprocMagicsNotebook)
	out := dataprocGSText(t, ctx, sc, b.GetRuntimeInfo().GetOutputUri())
	if b.GetState() != dataprocpb.Batch_SUCCEEDED {
		t.Fatalf("the notebook batch: %v %s\n%s", b.GetState(), b.GetStateMessage(), out)
	}
	for _, want := range []string{"env hello", "file written by a cell magic", "shell-5", "Wall time:", "Out[6]: 700"} {
		if !strings.Contains(out, want) {
			t.Errorf("the notebook batch's output has no %q:\n%s", want, out)
		}
	}
	bad := strings.Replace(dataprocMagicsNotebook, `"total * 100"`, `"1 / 0"`, 1)
	if b := batch("raises", bad); b.GetState() != dataprocpb.Batch_FAILED || !strings.Contains(b.GetStateMessage(), "ZeroDivisionError: division by zero (in the notebook's cell In[5])") {
		t.Errorf("a notebook whose cell raises: %v %q", b.GetState(), b.GetStateMessage())
	}
}

// zeppelinCall is a Zeppelin REST call's body (method, base+path,
// request JSON or nil).
func zeppelinCall(t *testing.T, ctx context.Context, method, u string, body any) map[string]any {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Minute}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &out) != nil {
		t.Fatalf("%s %s: %s %.1000s", method, u, resp.Status, b)
	}
	return out
}

// TestDataprocZeppelin (#2162): the ZEPPELIN optional component
// (https://docs.cloud.google.com/dataproc/docs/concepts/components/zeppelin),
// through the official Go client and Zeppelin's REST API at its Component
// Gateway URL: a note's %spark paragraph runs Scala on the cluster's Spark
// and a %md paragraph renders; ZEPPELIN on image 3.0 is refused. Zeppelin's
// image is linux/amd64 only: on arm64 it runs under emulation, and the
// test logs how long it took to start and to run.
//
// covers: google.cloud.dataproc.v1.ClusterController/CreateCluster, google.cloud.dataproc.v1.ClusterController/GetCluster
func TestDataprocZeppelin(t *testing.T) {
	h, addr, project := dataprocEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	cc, err := dataproc.NewClusterControllerClient(ctx, append(dataprocGRPC(), option.WithEndpoint(addr))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	const region = "us-central1"
	suffix := dataprocSuffix(h, 30)
	if _, err := cc.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: project, Region: region, Cluster: &dataprocpb.Cluster{
		ProjectId: project, ClusterName: "zp-bad-" + suffix, Config: &dataprocpb.ClusterConfig{SoftwareConfig: &dataprocpb.SoftwareConfig{
			ImageVersion: "3.0", OptionalComponents: []dataprocpb.Component{dataprocpb.Component_ZEPPELIN}}}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("ZEPPELIN on 3.0: %v", err)
	}
	name := "zp-" + suffix
	start := time.Now()
	dataprocClusterUp(t, ctx, h, cc, project, region, &dataprocpb.Cluster{ProjectId: project, ClusterName: name,
		Config: &dataprocpb.ClusterConfig{SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.2",
			Properties: map[string]string{"dataproc:dataproc.allow.zero.workers": "true"}, OptionalComponents: []dataprocpb.Component{dataprocpb.Component_ZEPPELIN}},
			EndpointConfig: &dataprocpb.EndpointConfig{EnableHttpPortAccess: true}}})
	t.Logf("the cluster with Zeppelin was RUNNING after %s", time.Since(start).Round(time.Second))
	got, err := cc.GetCluster(ctx, &dataprocpb.GetClusterRequest{ProjectId: project, Region: region, ClusterName: name})
	if err != nil {
		t.Fatal(err)
	}
	base := got.GetConfig().GetEndpointConfig().GetHttpPorts()["Zeppelin"]
	if !strings.HasSuffix(base, "/zeppelin/") || !strings.Contains(base, "/component-gateway/") {
		t.Fatalf("httpPorts: %v", got.GetConfig().GetEndpointConfig().GetHttpPorts())
	}
	if page := dataprocHTTPGet(t, ctx, base); !strings.Contains(page, "Zeppelin") {
		t.Errorf("Zeppelin's page: %.500s", page)
	}
	api := base + "api/"
	note := zeppelinCall(t, ctx, http.MethodPost, api+"notebook", map[string]any{"name": "cb-" + suffix})
	id, _ := note["body"].(string)
	para := func(text string) string {
		p := zeppelinCall(t, ctx, http.MethodPost, api+"notebook/"+id+"/paragraph", map[string]any{"title": "", "text": text})
		s, _ := p["body"].(string)
		return s
	}
	run := func(pid string) string {
		began := time.Now()
		res := zeppelinCall(t, ctx, http.MethodPost, api+"notebook/run/"+id+"/"+pid, nil)
		t.Logf("paragraph %s ran in %s", pid, time.Since(began).Round(time.Second))
		var b strings.Builder
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(res)
		return b.String()
	}
	if out := run(para("%spark\nval n = sc.parallelize(1 to 100, 4).map(_ * 2).sum\nprintln(s\"sum=$n workers=${sc.master}\")")); !strings.Contains(out, "sum=10100.0") ||
		!strings.Contains(out, "spark://"+name+"-m:7077") {
		t.Errorf("the %%spark paragraph: %s", out)
	}
	if out := run(para("%md\n## Hello from Zeppelin")); !strings.Contains(out, "<h2>Hello from Zeppelin</h2>") {
		t.Errorf("the %%md paragraph: %s", out)
	}
}

// TestDataprocKernelGateway (#2162): Jupyter kernels on Serverless for
// Apache Spark sessions through the kernel gateway, as Google's
// dataproc-jupyter-plugin uses its GatewayClient URL
// (https://PROJECT_NUMBER-dot-REGION.kernels.googleusercontent.com; here
// /kernels/PROJECT-dot-REGION/ on the Dataproc port), with the official
// Go client for the session templates: the region's Jupyter session
// templates are its kernelspecs; a PYTHON template's kernel runs PySpark
// on its session's Spark Connect server, and a SCALA template's kernel
// (Almond) runs Scala on its own; deleting a kernel terminates its session.
//
// covers: google.cloud.dataproc.v1.SessionTemplateController/CreateSessionTemplate, google.cloud.dataproc.v1.SessionController/ListSessions
func TestDataprocKernelGateway(t *testing.T) {
	h, addr, project := dataprocEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	opts := append(dataprocGRPC(), option.WithEndpoint(addr))
	tc, err := dataproc.NewSessionTemplateControllerClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	sc, err := dataproc.NewSessionControllerClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	suffix := dataprocSuffix(h, 20)
	parent := "projects/" + project + "/locations/us-central1"
	tpl := func(id, display string, kernel dataprocpb.JupyterConfig_Kernel) {
		t.Helper()
		x, err := tc.CreateSessionTemplate(ctx, &dataprocpb.CreateSessionTemplateRequest{Parent: parent, SessionTemplate: &dataprocpb.SessionTemplate{
			Name:          parent + "/sessionTemplates/" + id,
			SessionConfig: &dataprocpb.SessionTemplate_JupyterSession{JupyterSession: &dataprocpb.JupyterConfig{Kernel: kernel, DisplayName: display}},
			RuntimeConfig: &dataprocpb.RuntimeConfig{Version: "2.2"}}})
		if err != nil {
			t.Fatalf("CreateSessionTemplate %s: %v", id, err)
		}
		t.Cleanup(func() {
			_ = tc.DeleteSessionTemplate(h.Context(), &dataprocpb.DeleteSessionTemplateRequest{Name: x.Name})
		})
	}
	py, scala := "py-"+suffix, "scala-"+suffix
	tpl(py, "PySpark on Serverless Spark", dataprocpb.JupyterConfig_PYTHON)
	tpl(scala, "Scala on Serverless Spark", dataprocpb.JupyterConfig_SCALA)

	gw := "http://" + addr + "/kernels/" + project + "-dot-us-central1/"
	var specs struct {
		Kernelspecs map[string]struct {
			Spec struct {
				DisplayName string `json:"display_name"`
				Language    string `json:"language"`
			} `json:"spec"`
		} `json:"kernelspecs"`
	}
	resp, err := http.Get(gw + "api/kernelspecs")
	if err != nil {
		t.Fatal(err)
	}
	err = json.NewDecoder(resp.Body).Decode(&specs)
	resp.Body.Close()
	if err != nil || specs.Kernelspecs[py].Spec.DisplayName != "PySpark on Serverless Spark" || specs.Kernelspecs[py].Spec.Language != "python" ||
		specs.Kernelspecs[scala].Spec.Language != "scala" {
		t.Fatalf("kernelspecs: %+v %v", specs, err)
	}

	out, err := jupyterRun(ctx, gw, py, `print("count", spark.range(5).count(), type(spark).__module__)`)
	if err != nil || !strings.Contains(out, "count 5 pyspark.sql.connect.session") {
		t.Errorf("the PySpark kernel: %q %v", out, err)
	}
	out, err = jupyterRun(ctx, gw, scala, `println("count " + spark.range(7).count() + " " + spark.getClass.getName)`)
	if err != nil || !strings.Contains(out, "count 7 org.apache.spark.sql.SparkSession") {
		t.Errorf("the Scala kernel: %q %v", out, err)
	}

	// Each kernel ran in a session of its own, terminated with it.
	deadline := time.Now().Add(3 * time.Minute)
	for {
		it := sc.ListSessions(ctx, &dataprocpb.ListSessionsRequest{Parent: parent})
		live, seen := 0, 0
		for {
			x, err := it.Next()
			if err != nil {
				break
			}
			if !strings.Contains(x.Name, "/sessions/kernel-") || x.GetSessionTemplate() == "" ||
				!strings.HasSuffix(x.GetSessionTemplate(), py) && !strings.HasSuffix(x.GetSessionTemplate(), scala) {
				continue
			}
			seen++
			if x.State != dataprocpb.Session_TERMINATED && x.State != dataprocpb.Session_TERMINATING && x.State != dataprocpb.Session_FAILED {
				live++
			}
		}
		if seen == 2 && live == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the kernels' sessions: %d, %d not terminated", seen, live)
		}
		time.Sleep(5 * time.Second)
	}
}
