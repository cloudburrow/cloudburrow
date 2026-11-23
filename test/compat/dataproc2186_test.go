//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	dataproc "cloud.google.com/go/dataproc/v2/apiv1"
	"cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestDataprocHDFS (#2186): a Dataproc cluster's HDFS, as a Dataproc image
// runs it, through the official Go client: the NameNode on the master
// (fs.defaultFS hdfs://CLUSTER-m) and a DataNode on each primary worker,
// each DataNode's capacity the workers' boot disk (diskConfig), with the
// cluster's hdfs: properties in hdfs-site.xml (dfs.blocksize) and
// dfs.replication 2. A Hadoop MapReduce job writes its word count to
// HDFS, Hadoop's FsShell (a Hadoop job's main class) reads it back, a
// PySpark job writes and reads Parquet in HDFS, and a Hive table is in
// the HDFS warehouse (/user/hive/warehouse). The Component Gateway serves
// the NameNode's web UI ("HDFS NameNode"). HDFS keeps its data across
// StopCluster and StartCluster and across a scale down (the removed
// worker's DataNode decommissioned first), and loses it when the cluster
// is deleted: a cluster made again under the same name has none of it.
//
// covers: google.cloud.dataproc.v1.ClusterController/CreateCluster, google.cloud.dataproc.v1.ClusterController/StopCluster, google.cloud.dataproc.v1.ClusterController/StartCluster, google.cloud.dataproc.v1.ClusterController/UpdateCluster, google.cloud.dataproc.v1.ClusterController/DeleteCluster, google.cloud.dataproc.v1.JobController/SubmitJob
func TestDataprocHDFS(t *testing.T) {
	h, addr, project := dataprocEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
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
	t.Cleanup(func() { _ = jc.Close() })
	sc := storageClient(t, h)
	bh := bucket(t, h, sc)
	const region = "us-central1"
	suffix := dataprocSuffix(h, 30)
	name := "hdfs-" + suffix
	dataprocClusterUp(t, ctx, h, cc, project, region, &dataprocpb.Cluster{ProjectId: project, ClusterName: name,
		Config: &dataprocpb.ClusterConfig{
			SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.2", Properties: map[string]string{"hdfs:dfs.blocksize": "64m"}},
			WorkerConfig: &dataprocpb.InstanceGroupConfig{NumInstances: 2,
				DiskConfig: &dataprocpb.DiskConfig{BootDiskSizeGb: 100}},
			EndpointConfig: &dataprocpb.EndpointConfig{EnableHttpPortAccess: true}}})
	place := &dataprocpb.JobPlacement{ClusterName: name}
	n := 0
	output := func(j *dataprocpb.Job) string { return dataprocGSText(t, ctx, sc, j.GetDriverOutputResourceUri()) }
	run := func(want dataprocpb.JobStatus_State, j *dataprocpb.Job) string {
		t.Helper()
		n++
		j.Placement = place
		j.Reference = &dataprocpb.JobReference{JobId: fmt.Sprintf("hdfs%d-%s", n, suffix)}
		done := dataprocRunJob(t, ctx, jc, project, region, j)
		out := output(done)
		if done.GetStatus().GetState() != want {
			t.Fatalf("job %s: %v; want %v\n%s", j.Reference.JobId, done.GetStatus(), want, out)
		}
		return out
	}
	fsShell := func(want dataprocpb.JobStatus_State, files []string, args ...string) string {
		t.Helper()
		return run(want, &dataprocpb.Job{TypeJob: &dataprocpb.Job_HadoopJob{HadoopJob: &dataprocpb.HadoopJob{
			Driver: &dataprocpb.HadoopJob_MainClass{MainClass: "org.apache.hadoop.fs.FsShell"}, Args: args, FileUris: files}}})
	}

	// A MapReduce job reads Cloud Storage and writes HDFS (a path without
	// a scheme is fs.defaultFS's); FsShell reads it back by hdfs:///.
	in := dataprocPut(t, ctx, bh, "in/words.txt", dataprocWords)
	run(dataprocpb.JobStatus_DONE, &dataprocpb.Job{TypeJob: &dataprocpb.Job_HadoopJob{HadoopJob: &dataprocpb.HadoopJob{
		Driver: &dataprocpb.HadoopJob_MainJarFileUri{MainJarFileUri: "file:///usr/lib/hadoop-mapreduce/hadoop-mapreduce-examples.jar"},
		Args:   []string{"wordcount", strings.TrimSuffix(in, "words.txt"), "/data/wc-out"}}}})
	if out := fsShell(dataprocpb.JobStatus_DONE, nil, "-cat", "hdfs:///data/wc-out/part-r-00000"); !strings.Contains(out, "apple\t3") || !strings.Contains(out, "banana\t2") {
		t.Errorf("the word count in HDFS: %s", out)
	}
	if out := fsShell(dataprocpb.JobStatus_DONE, nil, "-ls", "hdfs://"+name+"-m/data/wc-out"); !strings.Contains(out, "/data/wc-out/_SUCCESS") {
		t.Errorf("hdfs://%s-m/data/wc-out: %s", name, out)
	}
	// Two DataNodes of 100 GB (the workers' boot disks), replication 2,
	// and the hdfs: property's block size.
	if out := fsShell(dataprocpb.JobStatus_DONE, nil, "-df", "/"); !strings.Contains(out, "hdfs://"+name+"-m") || !strings.Contains(out, "214748364800") {
		t.Errorf("hdfs dfs -df: %s", out)
	}
	fsShell(dataprocpb.JobStatus_DONE, []string{in}, "-put", "-f", "words.txt", "/data/keep.txt")
	if out := fsShell(dataprocpb.JobStatus_DONE, nil, "-stat", "%r %o %u", "/data/keep.txt"); !strings.Contains(out, "2 67108864 root") {
		t.Errorf("hdfs dfs -stat of a file a job put: %s", out)
	}

	// Spark: a PySpark job writes Parquet to HDFS and reads it, and the
	// MapReduce job's output.
	py := dataprocPut(t, ctx, bh, "hdfs.py", "from pyspark.sql import SparkSession\n"+
		"s = SparkSession.builder.getOrCreate()\n"+
		"s.createDataFrame([('apple', 3), ('cherry', 1)], ['name', 'n']).write.mode('overwrite').parquet('hdfs:///data/spark/fruit')\n"+
		"rows = s.read.parquet('/data/spark/fruit').orderBy('name').collect()\n"+
		"print('SPARK-HDFS', [(r['name'], r['n']) for r in rows])\n"+
		"print('SPARK-WC', sorted(r.value for r in s.read.text('hdfs:///data/wc-out').collect()))\n")
	out := run(dataprocpb.JobStatus_DONE, &dataprocpb.Job{TypeJob: &dataprocpb.Job_PysparkJob{PysparkJob: &dataprocpb.PySparkJob{MainPythonFileUri: py}}})
	if !strings.Contains(out, "SPARK-HDFS [('apple', 3), ('cherry', 1)]") || !strings.Contains(out, `'apple\t3'`) {
		t.Errorf("the PySpark job's output: %s", out)
	}
	if out := fsShell(dataprocpb.JobStatus_DONE, nil, "-ls", "/data/spark/fruit"); !strings.Contains(out, "/data/spark/fruit/_SUCCESS") {
		t.Errorf("the PySpark job's Parquet in HDFS: %s", out)
	}

	// Hive: a managed table is in the HDFS warehouse.
	hive := run(dataprocpb.JobStatus_DONE, &dataprocpb.Job{TypeJob: &dataprocpb.Job_HiveJob{HiveJob: &dataprocpb.HiveJob{
		Queries: &dataprocpb.HiveJob_QueryList{QueryList: &dataprocpb.QueryList{Queries: []string{
			"CREATE TABLE fruit (name STRING, n INT)", "INSERT INTO fruit VALUES ('apple', 3), ('cherry', 1)",
			"DESCRIBE FORMATTED fruit", "SELECT name, n * 10 AS tens FROM fruit ORDER BY name"}}}}}})
	if !strings.Contains(hive, "hdfs://"+name+"-m/user/hive/warehouse/fruit") || !strings.Contains(hive, "30") {
		t.Errorf("the Hive job's output: %s", hive)
	}
	if out := fsShell(dataprocpb.JobStatus_DONE, nil, "-ls", "/user/hive/warehouse/fruit"); !strings.Contains(out, "/user/hive/warehouse/fruit/") {
		t.Errorf("the Hive table in HDFS: %s", out)
	}

	// The NameNode's web UI through the Component Gateway.
	up, err := cc.GetCluster(ctx, &dataprocpb.GetClusterRequest{ProjectId: project, Region: region, ClusterName: name})
	if err != nil {
		t.Fatal(err)
	}
	nn := up.GetConfig().GetEndpointConfig().GetHttpPorts()["HDFS NameNode"]
	if !strings.HasSuffix(nn, "/hdfs/dfshealth.html") {
		t.Fatalf("httpPorts: %v", up.GetConfig().GetEndpointConfig().GetHttpPorts())
	}
	gw := strings.TrimSuffix(nn, "hdfs/dfshealth.html")
	gwPath := gw[strings.Index(gw, "/component-gateway/"):]
	if page := dataprocHTTPGet(t, ctx, nn); !strings.Contains(page, `src="`+gwPath+`hdfs/static/`) || !strings.Contains(page, "dfshealth.js") {
		t.Errorf("the NameNode's page (%s):\n%.1500s", nn, page)
	}
	if js := dataprocHTTPGet(t, ctx, gw+"hdfs/dfshealth.js"); !strings.Contains(js, gwPath+"hdfs/jmx?qry=") {
		t.Errorf("the NameNode page's script reads its JMX at the root:\n%.800s", js)
	}
	info := dataprocHTTPGet(t, ctx, gw+"hdfs/jmx?qry=Hadoop:service=NameNode,name=NameNodeInfo")
	for _, w := range []string{name + "-w-0:9866", name + "-w-1:9866"} {
		if !strings.Contains(info, w) {
			t.Errorf("the NameNode's live DataNodes have no %s:\n%.1500s", w, info)
		}
	}

	// Stop and start: HDFS keeps its data.
	sop, err := cc.StopCluster(ctx, &dataprocpb.StopClusterRequest{ProjectId: project, Region: region, ClusterName: name})
	if err != nil {
		t.Fatalf("StopCluster: %v", err)
	}
	if _, err := sop.Wait(ctx); err != nil {
		t.Fatalf("StopCluster: %v", err)
	}
	stop, err := cc.StartCluster(ctx, &dataprocpb.StartClusterRequest{ProjectId: project, Region: region, ClusterName: name})
	if err != nil {
		t.Fatalf("StartCluster: %v", err)
	}
	if _, err := stop.Wait(ctx); err != nil {
		t.Fatalf("StartCluster: %v", err)
	}
	if out := fsShell(dataprocpb.JobStatus_DONE, nil, "-cat", "/data/keep.txt"); !strings.Contains(out, "cherry apple banana") {
		t.Errorf("/data/keep.txt after stop and start: %s", out)
	}

	// Scale down to one worker: w-1's DataNode is decommissioned, and
	// the data stays readable.
	uop, err := cc.UpdateCluster(ctx, &dataprocpb.UpdateClusterRequest{ProjectId: project, Region: region, ClusterName: name,
		Cluster:    &dataprocpb.Cluster{Config: &dataprocpb.ClusterConfig{WorkerConfig: &dataprocpb.InstanceGroupConfig{NumInstances: 1}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"config.worker_config.num_instances"}}})
	if err != nil {
		t.Fatalf("UpdateCluster: %v", err)
	}
	if _, err := uop.Wait(ctx); err != nil {
		t.Fatalf("UpdateCluster: %v", err)
	}
	if out := fsShell(dataprocpb.JobStatus_DONE, nil, "-cat", "/data/keep.txt", "/data/wc-out/part-r-00000"); !strings.Contains(out, "cherry apple banana") || !strings.Contains(out, "apple\t3") {
		t.Errorf("HDFS after the scale down: %s", out)
	}

	// Delete: the cluster's HDFS goes with it.
	dop, err := cc.DeleteCluster(ctx, &dataprocpb.DeleteClusterRequest{ProjectId: project, Region: region, ClusterName: name})
	if err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	dataprocClusterUp(t, ctx, h, cc, project, region, &dataprocpb.Cluster{ProjectId: project, ClusterName: name,
		Config: &dataprocpb.ClusterConfig{SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.2",
			Properties: map[string]string{"dataproc:dataproc.allow.zero.workers": "true"}}}})
	if out := fsShell(dataprocpb.JobStatus_DONE, nil, "-ls", "/"); strings.Contains(out, "/data") || !strings.Contains(out, "/user") {
		t.Errorf("a new cluster of the same name's HDFS: %s", out)
	}
	fsShell(dataprocpb.JobStatus_ERROR, nil, "-test", "-e", "/data/keep.txt")
}
