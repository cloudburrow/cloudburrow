//go:build compat

package compat

import (
	"context"
	"encoding/json"
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

// dataprocYarnPi is a PySpark job's main file: Pi by sampling, and the
// master it ran on, which a Dataproc cluster's Spark reports as yarn.
const dataprocYarnPi = `import random
from pyspark.sql import SparkSession

spark = SparkSession.builder.appName("pi").getOrCreate()
sc = spark.sparkContext
n = 20000
inside = sc.parallelize(range(n), 4).filter(lambda _: random.random() ** 2 + random.random() ** 2 < 1).count()
print("master=%s mode=%s" % (sc.master, sc.getConf().get("spark.submit.deployMode")))
print("Pi is roughly %f" % (4.0 * inside / n))
print("executors=%d" % sc._jsc.sc().getExecutorMemoryStatus().size())
spark.stop()
`

// TestDataprocSparkAndHiveOnYarn (#2162): a cluster's Spark and Hive
// jobs run on YARN, as on a Dataproc cluster
// (https://docs.cloud.google.com/dataproc/docs/concepts/configuring-clusters/cluster-properties
// and the spark.submit.deployMode of https://docs.cloud.google.com/dataproc/docs/guides/submit-job):
// a PySpark job in the default client mode reports master yarn and fills
// yarn_applications; the same job with spark.submit.deployMode=cluster
// runs its driver in YARN and ends DONE; a Spark job (SparkPi from the
// examples jar) runs on YARN; and Hive queries run on Tez on YARN, an
// INSERT then a SELECT of the table returning its rows.
//
// covers: google.cloud.dataproc.v1.JobController/SubmitJob, google.cloud.dataproc.v1.JobController/GetJob
func TestDataprocSparkAndHiveOnYarn(t *testing.T) {
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
	name := "yn-" + suffix
	dataprocClusterUp(t, ctx, h, cc, project, region, &dataprocpb.Cluster{ProjectId: project, ClusterName: name,
		Config: &dataprocpb.ClusterConfig{SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.2"},
			WorkerConfig:   &dataprocpb.InstanceGroupConfig{NumInstances: 2},
			EndpointConfig: &dataprocpb.EndpointConfig{EnableHttpPortAccess: true}}})
	place := &dataprocpb.JobPlacement{ClusterName: name}
	pi := dataprocPut(t, ctx, bh, "yarn/pi.py", dataprocYarnPi)

	pyspark := func(id string, props map[string]string) (*dataprocpb.Job, string) {
		j := dataprocRunJob(t, ctx, jc, project, region, &dataprocpb.Job{Placement: place, Reference: &dataprocpb.JobReference{JobId: id + "-" + suffix},
			TypeJob: &dataprocpb.Job_PysparkJob{PysparkJob: &dataprocpb.PySparkJob{MainPythonFileUri: pi, Properties: props}}})
		return j, dataprocGSText(t, ctx, sc, j.GetDriverOutputResourceUri())
	}
	client, out := pyspark("yclient", nil)
	if client.GetStatus().GetState() != dataprocpb.JobStatus_DONE {
		t.Fatalf("the client mode PySpark job: %v\n%s", client.GetStatus(), out)
	}
	for _, want := range []string{"master=yarn mode=client", "Pi is roughly 3."} {
		if !strings.Contains(out, want) {
			t.Errorf("the client mode job's output has no %q:\n%s", want, out)
		}
	}
	apps := client.GetYarnApplications()
	if len(apps) != 1 || apps[0].GetState() != dataprocpb.YarnApplication_FINISHED || !strings.Contains(apps[0].GetTrackingUrl(), "/application_") {
		t.Errorf("the client mode job's YARN applications: %v", apps)
	}

	cluster, out := pyspark("ycluster", map[string]string{"spark.submit.deployMode": "cluster"})
	if cluster.GetStatus().GetState() != dataprocpb.JobStatus_DONE {
		t.Fatalf("the cluster mode PySpark job: %v\n%s", cluster.GetStatus(), out)
	}
	if !strings.Contains(out, "final status: SUCCEEDED") {
		t.Errorf("the cluster mode job's driver output (the YARN client's) has no final status SUCCEEDED:\n%s", out)
	}
	if apps := cluster.GetYarnApplications(); len(apps) != 1 || apps[0].GetState() != dataprocpb.YarnApplication_FINISHED {
		t.Errorf("the cluster mode job's YARN applications: %v", apps)
	}

	spark := dataprocRunJob(t, ctx, jc, project, region, &dataprocpb.Job{Placement: place, Reference: &dataprocpb.JobReference{JobId: "ypi-" + suffix},
		TypeJob: &dataprocpb.Job_SparkJob{SparkJob: &dataprocpb.SparkJob{
			Driver:      &dataprocpb.SparkJob_MainClass{MainClass: "org.apache.spark.examples.SparkPi"},
			JarFileUris: []string{"file:///usr/lib/spark/examples/jars/spark-examples.jar"}, Args: []string{"20"}}}})
	if out := dataprocGSText(t, ctx, sc, spark.GetDriverOutputResourceUri()); spark.GetStatus().GetState() != dataprocpb.JobStatus_DONE || !strings.Contains(out, "Pi is roughly 3.") {
		t.Errorf("SparkPi on YARN: %v\n%s", spark.GetStatus(), out)
	}
	if apps := spark.GetYarnApplications(); len(apps) != 1 {
		t.Errorf("SparkPi's YARN applications: %v", apps)
	}

	hive := dataprocRunJob(t, ctx, jc, project, region, &dataprocpb.Job{Placement: place, Reference: &dataprocpb.JobReference{JobId: "yhive-" + suffix},
		TypeJob: &dataprocpb.Job_HiveJob{HiveJob: &dataprocpb.HiveJob{Queries: &dataprocpb.HiveJob_QueryList{QueryList: &dataprocpb.QueryList{Queries: []string{
			"CREATE TABLE tez_fruit (name STRING, n INT)", "INSERT INTO tez_fruit VALUES ('apple', 3), ('cherry', 1)",
			"SELECT name, n * 10 AS tens FROM tez_fruit ORDER BY name"}}}}}})
	out = dataprocGSText(t, ctx, sc, hive.GetDriverOutputResourceUri())
	if hive.GetStatus().GetState() != dataprocpb.JobStatus_DONE {
		t.Fatalf("the Hive job: %v\n%s", hive.GetStatus(), out)
	}
	for _, want := range []string{"apple", "30", "cherry", "10"} {
		if !strings.Contains(out, want) {
			t.Errorf("the Hive job's output has no %q:\n%s", want, out)
		}
	}
}

// dataprocDockerScript uses a node's Docker daemon from a job's driver:
// the daemon's version, then an image imported from a tarball (the
// static docker CLI alone) and a container of it.
const dataprocDockerScript = `import io, subprocess, tarfile
from pyspark.sql import SparkSession
spark = SparkSession.builder.getOrCreate()
print("SERVER", subprocess.run(["docker", "version", "--format", "{{.Server.Version}}"], capture_output=True, text=True, check=True).stdout.strip())
buf = io.BytesIO()
with tarfile.open(fileobj=buf, mode="w") as t:
    t.add("/usr/bin/docker", arcname="docker")
subprocess.run(["docker", "import", "-", "cb/cli:1"], input=buf.getvalue(), check=True, capture_output=True)
r = subprocess.run(["docker", "run", "--rm", "--network", "none", "cb/cli:1", "/docker", "--version"], capture_output=True, text=True, check=True)
print("NESTED", r.stdout.strip())
print("COUNT", spark.range(5).count())
`

// TestDataprocDockerComponent (#2162): the DOCKER optional component
// (https://docs.cloud.google.com/dataproc/docs/concepts/components/docker),
// through the official Go client: a cluster with Docker runs a PySpark
// job whose driver reaches the node's Docker daemon (Docker 28.1, image
// 2.3's), imports an image and runs a container of it.
//
// covers: google.cloud.dataproc.v1.ClusterController/CreateCluster, google.cloud.dataproc.v1.JobController/SubmitJob
func TestDataprocDockerComponent(t *testing.T) {
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
	sc := storageClient(t, h)
	bh := bucket(t, h, sc)
	const region = "us-central1"
	suffix := dataprocSuffix(h, 30)
	name := "dk-" + suffix
	cluster := &dataprocpb.Cluster{ProjectId: project, ClusterName: name,
		Config: &dataprocpb.ClusterConfig{SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.3",
			Properties:         map[string]string{"dataproc:dataproc.allow.zero.workers": "true"},
			OptionalComponents: []dataprocpb.Component{dataprocpb.Component_DOCKER}}}}
	if !dataprocDockerAllowed(t, ctx, h, cc, project, region, cluster) {
		t.Skip("the instance runs without " + dataprocUnconfinedFlag + "; TestDataprocDockerComponentOff covers it")
	}
	main := dataprocPut(t, ctx, bh, "docker/job.py", dataprocDockerScript)
	j := dataprocRunJob(t, ctx, jc, project, region, &dataprocpb.Job{Placement: &dataprocpb.JobPlacement{ClusterName: name},
		Reference: &dataprocpb.JobReference{JobId: "docker-" + suffix},
		TypeJob:   &dataprocpb.Job_PysparkJob{PysparkJob: &dataprocpb.PySparkJob{MainPythonFileUri: main}}})
	out := dataprocGSText(t, ctx, sc, j.GetDriverOutputResourceUri())
	if j.GetStatus().GetState() != dataprocpb.JobStatus_DONE {
		t.Fatalf("the Docker job: %v\n%s", j.GetStatus(), out)
	}
	for _, want := range []string{"SERVER 28.1.", "NESTED Docker version 28.1.", "COUNT 5"} {
		if !strings.Contains(out, want) {
			t.Errorf("the Docker job's output has no %q:\n%s", want, out)
		}
	}
}

// dataprocUnconfinedFlag is the `up` flag the DOCKER component needs.
const dataprocUnconfinedFlag = "--allow-unconfined-containers"

// dataprocDockerAllowed creates cluster, a DOCKER cluster, and waits for
// it to run: false when the instance refuses it, FAILED_PRECONDITION
// naming dataprocUnconfinedFlag, as one started without it does.
func dataprocDockerAllowed(t *testing.T, ctx context.Context, h *Harness, cc *dataproc.ClusterControllerClient, project, region string, cluster *dataprocpb.Cluster) bool {
	t.Helper()
	op, err := cc.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: project, Region: region, Cluster: cluster})
	if status.Code(err) == codes.FailedPrecondition && strings.Contains(err.Error(), dataprocUnconfinedFlag) {
		return false
	}
	if err != nil {
		t.Fatalf("CreateCluster %s: %v", cluster.ClusterName, err)
	}
	t.Cleanup(func() {
		if dop, err := cc.DeleteCluster(h.Context(), &dataprocpb.DeleteClusterRequest{ProjectId: project, Region: region, ClusterName: cluster.ClusterName}); err == nil {
			_ = dop.Wait(h.Context())
		}
	})
	got, err := op.Wait(ctx)
	if err != nil || got.GetStatus().GetState() != dataprocpb.ClusterStatus_RUNNING {
		t.Fatalf("CreateCluster %s: %v %v", cluster.ClusterName, got.GetStatus(), err)
	}
	return true
}

// TestDataprocDockerComponentOff (#2162): on an instance started without
// --allow-unconfined-containers, the official Go client's CreateCluster
// of a cluster with DOCKER is refused FAILED_PRECONDITION, before any
// cluster exists, with a message that says how to opt in; the same
// request over JSON is Google's 400 error with status FAILED_PRECONDITION.
// It skips on an instance with the flag, where TestDataprocDockerComponent
// runs.
//
// covers: google.cloud.dataproc.v1.ClusterController/CreateCluster
func TestDataprocDockerComponentOff(t *testing.T) {
	h, addr, project := dataprocEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cc, err := dataproc.NewClusterControllerClient(ctx, append(dataprocGRPC(), option.WithEndpoint(addr))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	const region = "us-central1"
	name := "dkoff-" + dataprocSuffix(h, 30)
	cluster := &dataprocpb.Cluster{ProjectId: project, ClusterName: name,
		Config: &dataprocpb.ClusterConfig{SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.3",
			Properties:         map[string]string{"dataproc:dataproc.allow.zero.workers": "true"},
			OptionalComponents: []dataprocpb.Component{dataprocpb.Component_DOCKER}}}}
	if dataprocDockerAllowed(t, ctx, h, cc, project, region, cluster) {
		t.Skip("the instance runs with " + dataprocUnconfinedFlag + "; TestDataprocDockerComponent covers it")
	}
	_, err = cc.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: project, Region: region, Cluster: cluster})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "cloudburrow up "+dataprocUnconfinedFlag) ||
		!strings.Contains(err.Error(), "seccomp, AppArmor") {
		t.Fatalf("CreateCluster with DOCKER = %v; want FAILED_PRECONDITION saying how to opt in", err)
	}
	if _, err := cc.GetCluster(ctx, &dataprocpb.GetClusterRequest{ProjectId: project, Region: region, ClusterName: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetCluster of the refused cluster = %v; want NOT_FOUND", err)
	}
	body := `{"clusterName":"` + name + `","config":{"softwareConfig":{"imageVersion":"2.3","optionalComponents":["DOCKER"]}}}`
	resp, err := http.Post("http://"+addr+"/v1/projects/"+project+"/regions/"+region+"/clusters", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var e struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil || resp.StatusCode != http.StatusBadRequest || e.Error.Code != 400 ||
		e.Error.Status != "FAILED_PRECONDITION" || !strings.Contains(e.Error.Message, dataprocUnconfinedFlag) {
		t.Errorf("JSON create with DOCKER = %d %+v (%v); want Google's 400 FAILED_PRECONDITION", resp.StatusCode, e, err)
	}
}
