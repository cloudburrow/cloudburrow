//go:build compat

package compat

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	containerpb "cloud.google.com/go/container/apiv1/containerpb"
	dataproc "cloud.google.com/go/dataproc/v2/apiv1"
	"cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// dataprocGKEScript is a PySpark job for a Dataproc on GKE cluster: its
// master and a sum its executors compute.
const dataprocGKEScript = `from pyspark.sql import SparkSession
spark = SparkSession.builder.getOrCreate()
sc = spark.sparkContext
print("MASTER", sc.master)
print("SUM", sc.parallelize(range(1, 101), 4).sum())
print("NAMESPACE", sc.getConf().get("spark.kubernetes.namespace"))
spark.stop()
`

// TestDataprocOnGKE (#2162): Dataproc on GKE
// (https://docs.cloud.google.com/dataproc/docs/guides/dpgke/quickstarts/dataproc-gke-quickstart-create-cluster),
// with the official Kubernetes Engine and Dataproc clients: a virtual
// cluster on a GKE cluster of the instance, its node pool target the
// GKE cluster's default-pool with the DEFAULT role, Spark 3.5; its
// namespace defaults to the cluster's name and is made on the GKE
// cluster; a PySpark job and SparkPi run with executors in pods there;
// a Hive job, a target without a DEFAULT pool and an unknown Spark
// version are INVALID_ARGUMENT; stopping it is refused.
//
// covers: google.cloud.dataproc.v1.ClusterController/CreateCluster, google.cloud.dataproc.v1.JobController/SubmitJob
func TestDataprocOnGKE(t *testing.T) {
	h, addr, project := dataprocEnv(t)
	g, _ := gkeClients(t, h)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required to read the GKE cluster")
	}
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
	bh := bucket(t, h, sc)
	const region, zone = "us-central1", "us-central1-a"
	suffix := dataprocSuffix(h, 20)

	gkeName := "dpgke-" + suffix
	gkeParent := "projects/" + project + "/locations/" + zone
	target := gkeParent + "/clusters/" + gkeName
	op, err := g.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: gkeParent, Cluster: &containerpb.Cluster{Name: gkeName, InitialNodeCount: 1,
		MasterAuth: &containerpb.MasterAuth{ClientCertificateConfig: &containerpb.ClientCertificateConfig{IssueClientCertificate: true}}}})
	if err != nil {
		t.Fatalf("CreateCluster (GKE): %v", err)
	}
	t.Cleanup(func() {
		if op, err := g.DeleteCluster(h.Context(), &containerpb.DeleteClusterRequest{Name: target}); err == nil {
			gkeWait(t, g, project, zone, op)
		}
	})
	gkeWait(t, g, project, zone, op)

	vc := func(name string, roles []dataprocpb.GkeNodePoolTarget_Role, spark string) *dataprocpb.Cluster {
		return &dataprocpb.Cluster{ProjectId: project, ClusterName: name, VirtualClusterConfig: &dataprocpb.VirtualClusterConfig{
			InfrastructureConfig: &dataprocpb.VirtualClusterConfig_KubernetesClusterConfig{KubernetesClusterConfig: &dataprocpb.KubernetesClusterConfig{
				Config: &dataprocpb.KubernetesClusterConfig_GkeClusterConfig{GkeClusterConfig: &dataprocpb.GkeClusterConfig{GkeClusterTarget: target,
					NodePoolTarget: []*dataprocpb.GkeNodePoolTarget{{NodePool: target + "/nodePools/default-pool", Roles: roles}}}},
				KubernetesSoftwareConfig: &dataprocpb.KubernetesSoftwareConfig{ComponentVersion: map[string]string{"SPARK": spark}}}}}}
	}
	for _, bad := range []struct {
		what string
		c    *dataprocpb.Cluster
	}{{"no DEFAULT pool", vc("vbad1-"+suffix, []dataprocpb.GkeNodePoolTarget_Role{dataprocpb.GkeNodePoolTarget_SPARK_DRIVER}, "3.5-dataproc-28")},
		{"an unknown Spark", vc("vbad2-"+suffix, []dataprocpb.GkeNodePoolTarget_Role{dataprocpb.GkeNodePoolTarget_DEFAULT}, "2.4-dataproc-1")}} {
		if _, err := cc.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: project, Region: region, Cluster: bad.c}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("a virtual cluster with %s: %v", bad.what, err)
		}
	}

	name := "vgke-" + suffix
	got := dataprocClusterUp(t, ctx, h, cc, project, region, vc(name, []dataprocpb.GkeNodePoolTarget_Role{dataprocpb.GkeNodePoolTarget_DEFAULT}, "3.5-dataproc-28"))
	if got.GetConfig() != nil || got.GetVirtualClusterConfig().GetKubernetesClusterConfig().GetKubernetesNamespace() != name ||
		got.GetVirtualClusterConfig().GetStagingBucket() == "" {
		t.Errorf("the virtual cluster: %v", got)
	}
	gc, err := g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: target})
	if err != nil {
		t.Fatal(err)
	}
	kc := gkeKubeconfig(t, gc)
	if out, err := exec.Command("kubectl", "--kubeconfig", kc, "get", "namespace", name, "-o", "jsonpath={.metadata.labels}").CombinedOutput(); err != nil ||
		!strings.Contains(string(out), got.GetClusterUuid()) {
		t.Errorf("the cluster's namespace on GKE: %v %s", err, out)
	}

	place := &dataprocpb.JobPlacement{ClusterName: name}
	if _, err := jc.SubmitJob(ctx, &dataprocpb.SubmitJobRequest{ProjectId: project, Region: region, Job: &dataprocpb.Job{Placement: place,
		TypeJob: &dataprocpb.Job_HiveJob{HiveJob: &dataprocpb.HiveJob{Queries: &dataprocpb.HiveJob_QueryList{QueryList: &dataprocpb.QueryList{Queries: []string{"SHOW TABLES"}}}}}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a Hive job on a virtual cluster: %v", err)
	}
	if _, err := cc.StopCluster(ctx, &dataprocpb.StopClusterRequest{ProjectId: project, Region: region, ClusterName: name}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("stopping a virtual cluster: %v", err)
	}

	main := dataprocPut(t, ctx, bh, "gke/job.py", dataprocGKEScript)
	py := dataprocRunJob(t, ctx, jc, project, region, &dataprocpb.Job{Placement: place, Reference: &dataprocpb.JobReference{JobId: "gkepy-" + suffix},
		TypeJob: &dataprocpb.Job_PysparkJob{PysparkJob: &dataprocpb.PySparkJob{MainPythonFileUri: main}}})
	out := dataprocGSText(t, ctx, sc, py.GetDriverOutputResourceUri())
	if py.GetStatus().GetState() != dataprocpb.JobStatus_DONE {
		t.Fatalf("the PySpark job: %v\n%s", py.GetStatus(), out)
	}
	for _, want := range []string{"MASTER k8s://https://kubernetes.default.svc", "SUM 5050", "NAMESPACE " + name} {
		if !strings.Contains(out, want) {
			t.Errorf("the PySpark job's output has no %q:\n%s", want, out)
		}
	}
	pi := dataprocRunJob(t, ctx, jc, project, region, &dataprocpb.Job{Placement: place, Reference: &dataprocpb.JobReference{JobId: "gkepi-" + suffix},
		TypeJob: &dataprocpb.Job_SparkJob{SparkJob: &dataprocpb.SparkJob{
			Driver:      &dataprocpb.SparkJob_MainClass{MainClass: "org.apache.spark.examples.SparkPi"},
			JarFileUris: []string{"file:///usr/lib/spark/examples/jars/spark-examples.jar"}, Args: []string{"10"}}}})
	if out := dataprocGSText(t, ctx, sc, pi.GetDriverOutputResourceUri()); pi.GetStatus().GetState() != dataprocpb.JobStatus_DONE || !strings.Contains(out, "Pi is roughly 3.") {
		t.Errorf("SparkPi on GKE: %v\n%s", pi.GetStatus(), out)
	}
}
