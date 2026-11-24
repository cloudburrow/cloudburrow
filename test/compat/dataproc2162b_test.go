//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	dataproc "cloud.google.com/go/dataproc/v2/apiv1"
	"cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	"golang.org/x/net/websocket"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// dataprocClusterUp creates a cluster and waits for it to run, deleting
// it when the test ends.
func dataprocClusterUp(t *testing.T, ctx context.Context, h *Harness, cc *dataproc.ClusterControllerClient, project, region string, c *dataprocpb.Cluster) *dataprocpb.Cluster {
	t.Helper()
	op, err := cc.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: project, Region: region, Cluster: c})
	if err != nil {
		t.Fatalf("CreateCluster %s: %v", c.ClusterName, err)
	}
	t.Cleanup(func() {
		if dop, err := cc.DeleteCluster(h.Context(), &dataprocpb.DeleteClusterRequest{ProjectId: project, Region: region, ClusterName: c.ClusterName}); err == nil {
			_ = dop.Wait(h.Context())
		}
	})
	got, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateCluster %s: %v", c.ClusterName, err)
	}
	if got.GetStatus().GetState() != dataprocpb.ClusterStatus_RUNNING {
		t.Fatalf("cluster %s is %v", c.ClusterName, got.GetStatus())
	}
	return got
}

// dataprocRunJob submits a job and waits for it to end.
func dataprocRunJob(t *testing.T, ctx context.Context, jc *dataproc.JobControllerClient, project, region string, j *dataprocpb.Job) *dataprocpb.Job {
	t.Helper()
	sub, err := jc.SubmitJob(ctx, &dataprocpb.SubmitJobRequest{ProjectId: project, Region: region, Job: j})
	if err != nil {
		t.Fatalf("SubmitJob %s: %v", j.GetReference().GetJobId(), err)
	}
	done, err := dataprocWaitJob(ctx, jc, project, region, sub.GetReference().GetJobId())
	if err != nil {
		t.Fatalf("job %s: %v", sub.GetReference().GetJobId(), err)
	}
	return done
}

const dataprocWords = "apple banana apple\ncherry apple banana\n"

// TestDataprocHadoopHivePigJobs (#2162): a cluster's Hadoop MapReduce,
// Hive and Pig jobs, as the jobs reference documents them
// (https://docs.cloud.google.com/dataproc/docs/reference/rest/v1/HadoopJob,
// HiveJob and PigJob), through the official Go client: Hadoop's word count
// from the image's examples jar (file:///usr/lib/hadoop-mapreduce/
// hadoop-mapreduce-examples.jar) reads and writes Cloud Storage; a Hive
// job makes a table that a later Hive job reads, and an external table on
// Cloud Storage with its script variables; a Pig script counts the words
// and stores them to Cloud Storage; a failing Hive query ends the job in
// ERROR; a Trino job is refused on a cluster without the Trino component.
//
// covers: google.cloud.dataproc.v1.JobController/SubmitJob, google.cloud.dataproc.v1.JobController/GetJob
func TestDataprocHadoopHivePigJobs(t *testing.T) {
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
	name := "hh-" + suffix
	dataprocClusterUp(t, ctx, h, cc, project, region, &dataprocpb.Cluster{ProjectId: project, ClusterName: name,
		Config: &dataprocpb.ClusterConfig{SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.2",
			Properties: map[string]string{"dataproc:dataproc.allow.zero.workers": "true"}},
			EndpointConfig: &dataprocpb.EndpointConfig{EnableHttpPortAccess: true}}})
	place := &dataprocpb.JobPlacement{ClusterName: name}
	in := dataprocPut(t, ctx, bh, "in/words.txt", dataprocWords)
	inDir := strings.TrimSuffix(in, "words.txt")
	bucketURI := "gs://" + bh.BucketName()

	// Hadoop MapReduce: the examples jar's word count, gs:// in and out.
	hj := dataprocRunJob(t, ctx, jc, project, region, &dataprocpb.Job{Placement: place, Reference: &dataprocpb.JobReference{JobId: "hadoop-" + suffix},
		TypeJob: &dataprocpb.Job_HadoopJob{HadoopJob: &dataprocpb.HadoopJob{
			Driver: &dataprocpb.HadoopJob_MainJarFileUri{MainJarFileUri: "file:///usr/lib/hadoop-mapreduce/hadoop-mapreduce-examples.jar"},
			Args:   []string{"wordcount", inDir, bucketURI + "/wc-out"}}}})
	if hj.GetStatus().GetState() != dataprocpb.JobStatus_DONE {
		t.Fatalf("hadoop job: %v\n%s", hj.GetStatus(), dataprocGSText(t, ctx, sc, hj.GetDriverOutputResourceUri()))
	}
	if out := dataprocGSText(t, ctx, sc, bucketURI+"/wc-out/part-r-"); !strings.Contains(out, "apple\t3") || !strings.Contains(out, "banana\t2") {
		t.Errorf("word count's output %q", out)
	}
	// It ran on the cluster's YARN: its application, as the
	// ResourceManager reports it, and the ResourceManager's and the
	// JobHistory Server's pages through the Component Gateway.
	apps := hj.GetYarnApplications()
	if len(apps) != 1 || apps[0].GetName() != "word count" || apps[0].GetState() != dataprocpb.YarnApplication_FINISHED ||
		apps[0].GetProgress() != 1 || !strings.HasPrefix(apps[0].GetTrackingUrl(), "http://"+name+"-m:8088/proxy/application_") {
		t.Fatalf("the hadoop job's YARN applications: %v", apps)
	}
	appID := strings.TrimSuffix(strings.TrimPrefix(apps[0].GetTrackingUrl(), "http://"+name+"-m:8088/proxy/"), "/")
	up, err := cc.GetCluster(ctx, &dataprocpb.GetClusterRequest{ProjectId: project, Region: region, ClusterName: name})
	if err != nil {
		t.Fatal(err)
	}
	ports := up.GetConfig().GetEndpointConfig().GetHttpPorts()
	rm, jhs := ports["YARN ResourceManager"], ports["MapReduce Job History"]
	if !strings.HasSuffix(rm, "/yarn/") || !strings.HasSuffix(jhs, "/jobhistory/") || !strings.HasSuffix(ports["YARN Application Timeline"], "/apphistory/") {
		t.Fatalf("the Component Gateway's Hadoop interfaces: %v", ports)
	}
	gw := strings.TrimSuffix(rm, "yarn/")
	gwPath := gw[strings.Index(gw, "/component-gateway/"):]
	page := dataprocHTTPGet(t, ctx, rm+"cluster/apps")
	if !strings.Contains(page, appID) || !strings.Contains(page, `href="`+gwPath+`yarn/static/`) || strings.Contains(page, name+"-m:8088") {
		t.Errorf("the ResourceManager's applications page (%s) has no %s or unrewritten links:\n%.2000s", rm, appID, page)
	}
	if nodes := dataprocHTTPGet(t, ctx, rm+"ws/v1/cluster/nodes"); !strings.Contains(nodes, `"nodeHostName":"`+name+`-m"`) {
		t.Errorf("the ResourceManager's nodes: %.500s", nodes)
	}
	jobID := strings.Replace(appID, "application_", "job_", 1)
	if page := dataprocHTTPGet(t, ctx, jhs+"jobhistory/job/"+jobID); !strings.Contains(page, "word count") || !strings.Contains(page, "SUCCEEDED") {
		t.Errorf("the JobHistory Server's page of %s:\n%.2000s", jobID, page)
	}

	// Hive: a managed table, then a second job reads it; an external
	// table over Cloud Storage.
	hive := func(id string, vars map[string]string, queries ...string) *dataprocpb.Job {
		return dataprocRunJob(t, ctx, jc, project, region, &dataprocpb.Job{Placement: place, Reference: &dataprocpb.JobReference{JobId: id + "-" + suffix},
			TypeJob: &dataprocpb.Job_HiveJob{HiveJob: &dataprocpb.HiveJob{ScriptVariables: vars,
				Queries: &dataprocpb.HiveJob_QueryList{QueryList: &dataprocpb.QueryList{Queries: queries}}}}})
	}
	j1 := hive("hive1", nil, "CREATE TABLE fruit (name STRING, n INT)", "INSERT INTO fruit VALUES ('apple', 3), ('cherry', 1)")
	if j1.GetStatus().GetState() != dataprocpb.JobStatus_DONE {
		t.Fatalf("hive job 1: %v\n%s", j1.GetStatus(), dataprocGSText(t, ctx, sc, j1.GetDriverOutputResourceUri()))
	}
	j2 := hive("hive2", map[string]string{"loc": inDir}, "SELECT name, n * 10 AS tens FROM fruit ORDER BY name",
		"CREATE EXTERNAL TABLE lines (line STRING) LOCATION '${loc}'",
		"SELECT word, COUNT(*) AS c FROM lines LATERAL VIEW explode(split(line, ' ')) t AS word GROUP BY word ORDER BY word")
	out2 := dataprocGSText(t, ctx, sc, j2.GetDriverOutputResourceUri())
	if j2.GetStatus().GetState() != dataprocpb.JobStatus_DONE {
		t.Fatalf("hive job 2: %v\n%s", j2.GetStatus(), out2)
	}
	for _, want := range []string{"apple", "30", "cherry", "10", "banana"} {
		if !strings.Contains(out2, want) {
			t.Errorf("hive job 2's output has no %q:\n%s", want, out2)
		}
	}
	if bad := hive("hivebad", nil, "SELECT * FROM no_such_table"); bad.GetStatus().GetState() != dataprocpb.JobStatus_ERROR {
		t.Errorf("a failing Hive query: %v", bad.GetStatus())
	}

	// Pig: count the words, DUMP them and STORE them to Cloud Storage.
	script := dataprocPut(t, ctx, bh, "count.pig", "lines = LOAD '$in' AS (line:chararray);\n"+
		"words = FOREACH lines GENERATE FLATTEN(TOKENIZE(line)) AS word;\n"+
		"grouped = GROUP words BY word;\ncounts = FOREACH grouped GENERATE group, COUNT(words);\n"+
		"STORE counts INTO '$out';\nDUMP counts;\n")
	pj := dataprocRunJob(t, ctx, jc, project, region, &dataprocpb.Job{Placement: place, Reference: &dataprocpb.JobReference{JobId: "pig-" + suffix},
		TypeJob: &dataprocpb.Job_PigJob{PigJob: &dataprocpb.PigJob{Queries: &dataprocpb.PigJob_QueryFileUri{QueryFileUri: script},
			ScriptVariables: map[string]string{"in": in, "out": bucketURI + "/pig-out"}}}})
	pout := dataprocGSText(t, ctx, sc, pj.GetDriverOutputResourceUri())
	if pj.GetStatus().GetState() != dataprocpb.JobStatus_DONE {
		t.Fatalf("pig job: %v\n%s", pj.GetStatus(), pout)
	}
	if len(pj.GetYarnApplications()) == 0 {
		t.Errorf("the pig job ran no YARN application: %v", pj)
	}
	if !strings.Contains(pout, "(apple,3)") {
		t.Errorf("pig's DUMP: %s", pout)
	}
	if out := dataprocGSText(t, ctx, sc, bucketURI+"/pig-out/part-"); !strings.Contains(out, "apple\t3") {
		t.Errorf("pig's STORE: %q", out)
	}

	// A Trino job needs the Trino component.
	if _, err := jc.SubmitJob(ctx, &dataprocpb.SubmitJobRequest{ProjectId: project, Region: region, Job: &dataprocpb.Job{Placement: place,
		TypeJob: &dataprocpb.Job_TrinoJob{TrinoJob: &dataprocpb.TrinoJob{Queries: &dataprocpb.TrinoJob_QueryList{QueryList: &dataprocpb.QueryList{Queries: []string{"SELECT 1"}}}}}}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a Trino job without the component: %v", err)
	}
}

// jupyterRun runs code in a new kernel of kernelName through Jupyter's
// REST API and kernel websocket, and returns its stream output.
func jupyterRun(ctx context.Context, base, kernelName, code string) (string, error) {
	body := strings.NewReader(`{"name":"` + kernelName + `"}`)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"api/kernels", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	var k struct {
		ID string `json:"id"`
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err := json.Unmarshal(b, &k); err != nil || k.ID == "" {
		return "", fmt.Errorf("start a %s kernel: %s %s", kernelName, resp.Status, b)
	}
	defer func() {
		r, _ := http.NewRequest(http.MethodDelete, base+"api/kernels/"+k.ID, nil)
		if resp, err := http.DefaultClient.Do(r); err == nil {
			_ = resp.Body.Close()
		}
	}()
	wsURL := "ws" + strings.TrimPrefix(base, "http") + "api/kernels/" + k.ID + "/channels"
	cfg, err := websocket.NewConfig(wsURL, base)
	if err != nil {
		return "", err
	}
	ws, err := websocket.DialConfig(cfg)
	if err != nil {
		return "", err
	}
	defer ws.Close()
	msgID := fmt.Sprintf("cb-%d", time.Now().UnixNano())
	msg := map[string]any{"header": map[string]any{"msg_id": msgID, "username": "compat", "session": "compat", "msg_type": "execute_request", "version": "5.3"},
		"parent_header": map[string]any{}, "metadata": map[string]any{}, "channel": "shell",
		"content": map[string]any{"code": code, "silent": false, "store_history": false, "user_expressions": map[string]any{}, "allow_stdin": false}}
	if err := websocket.JSON.Send(ws, msg); err != nil {
		return "", err
	}
	var out strings.Builder
	deadline := time.Now().Add(8 * time.Minute)
	for time.Now().Before(deadline) {
		_ = ws.SetReadDeadline(time.Now().Add(time.Minute))
		var m struct {
			Header struct {
				MsgType string `json:"msg_type"`
			} `json:"header"`
			ParentHeader struct {
				MsgID string `json:"msg_id"`
			} `json:"parent_header"`
			Content map[string]any `json:"content"`
		}
		if err := websocket.JSON.Receive(ws, &m); err != nil {
			return out.String(), err
		}
		if m.ParentHeader.MsgID != msgID {
			continue
		}
		switch m.Header.MsgType {
		case "stream":
			fmt.Fprint(&out, m.Content["text"])
		case "error":
			return out.String(), fmt.Errorf("%v: %v", m.Content["ename"], m.Content["evalue"])
		case "status":
			if m.Content["execution_state"] == "idle" {
				return out.String(), nil
			}
		}
	}
	return out.String(), fmt.Errorf("the cell did not finish")
}

// TestDataprocOptionalComponents (#2162): a cluster's optional
// components, as the components overview lists them
// (https://docs.cloud.google.com/dataproc/docs/concepts/components/overview),
// run as their projects' official images: Trino (a Trino job's query of
// the tpch catalog, and of the cluster's Hive metastore), Flink (the
// image's batch word count example, file:///usr/lib/flink/examples/batch/
// WordCount.jar), Jupyter (a PySpark kernel's `spark` on the cluster, and
// the Python 3 kernel), ZooKeeper, Solr and Hive WebHCat, their web
// interfaces through the Component Gateway's endpointConfig.httpPorts.
// And the documented refusals: Presto on image 2.2, a component listed
// twice.
//
// covers: google.cloud.dataproc.v1.ClusterController/CreateCluster, google.cloud.dataproc.v1.ClusterController/GetCluster
func TestDataprocOptionalComponents(t *testing.T) {
	h, addr, project := dataprocEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
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
	const region = "us-central1"
	suffix := dataprocSuffix(h, 30)
	for _, bad := range [][]dataprocpb.Component{{dataprocpb.Component_PRESTO}, {dataprocpb.Component_TRINO, dataprocpb.Component_TRINO}} {
		if _, err := cc.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: project, Region: region, Cluster: &dataprocpb.Cluster{
			ProjectId: project, ClusterName: "bad-" + suffix, Config: &dataprocpb.ClusterConfig{
				SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.2", OptionalComponents: bad}}}}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("components %v: %v", bad, err)
		}
	}
	name := "oc-" + suffix
	c := dataprocClusterUp(t, ctx, h, cc, project, region, &dataprocpb.Cluster{ProjectId: project, ClusterName: name,
		Config: &dataprocpb.ClusterConfig{
			SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.2", Properties: map[string]string{"dataproc:dataproc.allow.zero.workers": "true"},
				OptionalComponents: []dataprocpb.Component{dataprocpb.Component_TRINO, dataprocpb.Component_FLINK, dataprocpb.Component_JUPYTER,
					dataprocpb.Component_ZOOKEEPER, dataprocpb.Component_SOLR, dataprocpb.Component_HIVE_WEBHCAT}},
			EndpointConfig: &dataprocpb.EndpointConfig{EnableHttpPortAccess: true}}})
	// GetCluster's http_ports are URLs at the caller's host.
	gc, err := cc.GetCluster(ctx, &dataprocpb.GetClusterRequest{ProjectId: project, Region: region, ClusterName: name})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.GetConfig().GetEndpointConfig().GetHttpPorts()) != len(gc.GetConfig().GetEndpointConfig().GetHttpPorts()) {
		t.Errorf("http_ports: %v, then %v", c.GetConfig().GetEndpointConfig().GetHttpPorts(), gc.GetConfig().GetEndpointConfig().GetHttpPorts())
	}
	ports := gc.GetConfig().GetEndpointConfig().GetHttpPorts()
	for key, want := range map[string]string{"Trino": "Trino", "Flink": "Apache Flink", "JupyterLab": "JupyterLab", "Solr": "Solr", "WebHCat": `"ok"`} {
		u := ports[key]
		if u == "" {
			t.Errorf("httpPorts has no %q: %v", key, ports)
			continue
		}
		body, err := dataprocGet(ctx, u)
		if err != nil || !strings.Contains(body, want) {
			t.Errorf("%s at %s: %v %.300s", key, u, err, body)
		}
	}
	place := &dataprocpb.JobPlacement{ClusterName: name}

	// Trino: the tpch catalog, and a table of the Hive metastore.
	tj := dataprocRunJob(t, ctx, jc, project, region, &dataprocpb.Job{Placement: place, Reference: &dataprocpb.JobReference{JobId: "trino-" + suffix},
		TypeJob: &dataprocpb.Job_TrinoJob{TrinoJob: &dataprocpb.TrinoJob{OutputFormat: "CSV", ClientTags: []string{"compat"},
			Queries: &dataprocpb.TrinoJob_QueryList{QueryList: &dataprocpb.QueryList{Queries: []string{
				"SELECT count(*) FROM tpch.tiny.nation",
				"CREATE TABLE hive.default.colours (name varchar)",
				"INSERT INTO hive.default.colours VALUES ('red'), ('blue')",
				"SELECT name FROM hive.default.colours ORDER BY name"}}}}}})
	tout := dataprocGSText(t, ctx, sc, tj.GetDriverOutputResourceUri())
	if tj.GetStatus().GetState() != dataprocpb.JobStatus_DONE {
		t.Fatalf("trino job: %v\n%s", tj.GetStatus(), tout)
	}
	if !strings.Contains(tout, `"25"`) || !strings.Contains(tout, `"blue"`) || !strings.Contains(tout, `"red"`) {
		t.Errorf("trino's output: %s", tout)
	}

	// Flink: the image's batch word count, on its built-in text.
	fj := dataprocRunJob(t, ctx, jc, project, region, &dataprocpb.Job{Placement: place, Reference: &dataprocpb.JobReference{JobId: "flink-" + suffix},
		TypeJob: &dataprocpb.Job_FlinkJob{FlinkJob: &dataprocpb.FlinkJob{
			Driver: &dataprocpb.FlinkJob_MainJarFileUri{MainJarFileUri: "file:///usr/lib/flink/examples/batch/WordCount.jar"}}}})
	fout := dataprocGSText(t, ctx, sc, fj.GetDriverOutputResourceUri())
	if fj.GetStatus().GetState() != dataprocpb.JobStatus_DONE {
		t.Fatalf("flink job: %v\n%s", fj.GetStatus(), fout)
	}
	if !strings.Contains(fout, "(to,") || !strings.Contains(fout, "Job has been submitted") {
		t.Errorf("flink's output: %s", fout)
	}

	// Jupyter: the PySpark kernel's session runs on the cluster.
	base := strings.TrimSuffix(ports["JupyterLab"], "lab")
	got, err := jupyterRun(ctx, base, "pyspark", "print(spark.range(100).selectExpr('sum(id) AS s').first()['s'], sc.master)")
	if err != nil || !strings.Contains(got, "4950") || !strings.Contains(got, "spark://"+name+"-m:7077") {
		t.Errorf("the PySpark kernel: %q %v", got, err)
	}
	if got, err := jupyterRun(ctx, base, "python3", "print(6 * 7)"); err != nil || !strings.Contains(got, "42") {
		t.Errorf("the Python 3 kernel: %q %v", got, err)
	}
}

const dataprocHostScript = `
import socket
from pyspark.sql import SparkSession
spark = SparkSession.builder.getOrCreate()
n = spark.sparkContext.parallelize(range(10), 2).sum()
print("driver-host", socket.gethostname(), "sum", n)
spark.stop()
`

// TestDataprocDriverNodeGroups (#2162): a cluster with a driver node
// group, as the driver node groups page documents it
// (https://docs.cloud.google.com/dataproc/docs/guides/node-groups/dataproc-driver-node-groups):
// its nodes are named CLUSTER-np-XXXX; a job without
// driver_scheduling_config, or with a zero value in it, is refused with
// the page's errors; a job with it runs its driver on the group's node;
// the group is read and resized with projects.regions.clusters.nodeGroups;
// and the cluster cannot be stopped. Its one secondary worker is
// PREEMPTIBLE, the default.
//
// covers: google.cloud.dataproc.v1.NodeGroupController/GetNodeGroup, google.cloud.dataproc.v1.NodeGroupController/ResizeNodeGroup, google.cloud.dataproc.v1.NodeGroupController/CreateNodeGroup
func TestDataprocDriverNodeGroups(t *testing.T) {
	h, addr, project := dataprocEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
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
	ng, err := dataproc.NewNodeGroupControllerClient(ctx, grpcOpts...)
	if err != nil {
		t.Fatal(err)
	}
	defer ng.Close()
	sc := storageClient(t, h)
	bh := bucket(t, h, sc)
	const region = "us-central1"
	suffix := dataprocSuffix(h, 30)
	name := "ng-" + suffix

	bad := &dataprocpb.Cluster{ProjectId: project, ClusterName: name + "x", Config: &dataprocpb.ClusterConfig{
		AuxiliaryNodeGroups: []*dataprocpb.AuxiliaryNodeGroup{{NodeGroupId: "-bad", NodeGroup: &dataprocpb.NodeGroup{Roles: []dataprocpb.NodeGroup_Role{dataprocpb.NodeGroup_DRIVER}}}}}}
	if _, err := cc.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: project, Region: region, Cluster: bad}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a node group ID beginning with a hyphen: %v", err)
	}

	c := dataprocClusterUp(t, ctx, h, cc, project, region, &dataprocpb.Cluster{ProjectId: project, ClusterName: name,
		Config: &dataprocpb.ClusterConfig{SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.2"},
			WorkerConfig:          &dataprocpb.InstanceGroupConfig{NumInstances: 2},
			SecondaryWorkerConfig: &dataprocpb.InstanceGroupConfig{NumInstances: 1},
			AuxiliaryNodeGroups: []*dataprocpb.AuxiliaryNodeGroup{{NodeGroupId: "drivers", NodeGroup: &dataprocpb.NodeGroup{
				Roles: []dataprocpb.NodeGroup_Role{dataprocpb.NodeGroup_DRIVER}, NodeGroupConfig: &dataprocpb.InstanceGroupConfig{NumInstances: 1}}}}}})
	// Secondary workers are preemptible unless said otherwise.
	if sw := c.GetConfig().GetSecondaryWorkerConfig(); sw.GetPreemptibility() != dataprocpb.InstanceGroupConfig_PREEMPTIBLE || !sw.GetIsPreemptible() ||
		len(sw.GetInstanceNames()) != 1 || !strings.HasPrefix(sw.GetInstanceNames()[0], name+"-sw-") {
		t.Errorf("the secondary workers: %v", sw)
	}
	groups := c.GetConfig().GetAuxiliaryNodeGroups()
	if len(groups) != 1 {
		t.Fatalf("auxiliary node groups: %v", groups)
	}
	insts := groups[0].GetNodeGroup().GetNodeGroupConfig().GetInstanceNames()
	if len(insts) != 1 || !strings.HasPrefix(insts[0], name+"-np-") || len(insts[0]) != len(name)+8 {
		t.Errorf("the node group's instances: %v", insts)
	}
	groupName := "projects/" + project + "/regions/" + region + "/clusters/" + name + "/nodeGroups/drivers"
	if g, err := ng.GetNodeGroup(ctx, &dataprocpb.GetNodeGroupRequest{Name: groupName}); err != nil || g.GetName() != groupName ||
		len(g.GetRoles()) != 1 || g.GetRoles()[0] != dataprocpb.NodeGroup_DRIVER {
		t.Errorf("GetNodeGroup: %v %v", g, err)
	}

	script := dataprocPut(t, ctx, bh, "host.py", dataprocHostScript)
	place := &dataprocpb.JobPlacement{ClusterName: name}
	py := &dataprocpb.Job_PysparkJob{PysparkJob: &dataprocpb.PySparkJob{MainPythonFileUri: script}}
	_, err = jc.SubmitJob(ctx, &dataprocpb.SubmitJobRequest{ProjectId: project, Region: region, Job: &dataprocpb.Job{Placement: place, TypeJob: py}})
	want := "Cluster " + name + " requires driver scheduling config to run PYSPARK job because it contains a node pool with role DRIVER."
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != want {
		t.Errorf("a job without driver_scheduling_config: %v", err)
	}
	_, err = jc.SubmitJob(ctx, &dataprocpb.SubmitJobRequest{ProjectId: project, Region: region, Job: &dataprocpb.Job{Placement: place, TypeJob: py,
		DriverSchedulingConfig: &dataprocpb.DriverSchedulingConfig{MemoryMb: 1024}}})
	if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "Positive values are required for all driver scheduling config values." {
		t.Errorf("driver_scheduling_config with zero vcores: %v", err)
	}
	j := dataprocRunJob(t, ctx, jc, project, region, &dataprocpb.Job{Placement: place, TypeJob: py,
		DriverSchedulingConfig: &dataprocpb.DriverSchedulingConfig{MemoryMb: 1024, Vcores: 1}})
	out := dataprocGSText(t, ctx, sc, j.GetDriverOutputResourceUri())
	if j.GetStatus().GetState() != dataprocpb.JobStatus_DONE || !strings.Contains(out, "driver-host "+insts[0]+" sum 45") {
		t.Errorf("a job on the driver node group: %v\n%s", j.GetStatus(), out)
	}

	op, err := ng.ResizeNodeGroup(ctx, &dataprocpb.ResizeNodeGroupRequest{Name: groupName, Size: 2})
	if err != nil {
		t.Fatalf("ResizeNodeGroup: %v", err)
	}
	if md, err := op.Metadata(); err != nil || md.GetNodeGroupId() != "drivers" || md.GetClusterUuid() != c.ClusterUuid ||
		md.GetOperationType() != dataprocpb.NodeGroupOperationMetadata_RESIZE {
		t.Errorf("NodeGroupOperationMetadata: %v %v", md, err)
	}
	g, err := op.Wait(ctx)
	if err != nil || len(g.GetNodeGroupConfig().GetInstanceNames()) != 2 || g.GetNodeGroupConfig().GetInstanceNames()[0] != insts[0] {
		t.Errorf("the resized node group: %v %v", g, err)
	}
	if _, err := ng.CreateNodeGroup(ctx, &dataprocpb.CreateNodeGroupRequest{Parent: "projects/" + project + "/regions/" + region + "/clusters/" + name,
		NodeGroupId: "drivers", NodeGroup: &dataprocpb.NodeGroup{Roles: []dataprocpb.NodeGroup_Role{dataprocpb.NodeGroup_DRIVER}}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second node group drivers: %v", err)
	}
	if _, err := cc.StopCluster(ctx, &dataprocpb.StopClusterRequest{ProjectId: project, Region: region, ClusterName: name}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("stopping a cluster with node groups: %v", err)
	}
}

// dataprocHTTPGet is the body of a GET of a Component Gateway URL (on the
// instance's Dataproc port), after its redirects.
func dataprocHTTPGet(t *testing.T, ctx context.Context, u string) string {
	t.Helper()
	if !strings.Contains(u, "/component-gateway/") {
		t.Fatalf("%s is not a Component Gateway URL", u)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET %s: %s %.500s", u, resp.Status, b)
	}
	return string(b)
}
