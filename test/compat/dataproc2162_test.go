//go:build compat

package compat

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	dataproc "cloud.google.com/go/dataproc/v2/apiv1"
	"cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// dataprocSuffix is a name part unique to the test's harness project.
func dataprocSuffix(h *Harness, n int) string {
	s := strings.ToLower(strings.ReplaceAll(h.Project(), "_", "-"))
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return strings.Trim(s, "-")
}

// dataprocPut writes body to the bucket and returns its gs:// URI.
func dataprocPut(t *testing.T, ctx context.Context, bh *storage.BucketHandle, name, body string) string {
	t.Helper()
	w := bh.Object(name).NewWriter(ctx)
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return "gs://" + bh.BucketName() + "/" + name
}

// dataprocGSText is every object under the gs:// prefix uri, concatenated
// in name order: a driver output's parts.
func dataprocGSText(t *testing.T, ctx context.Context, sc *storage.Client, uri string) string {
	t.Helper()
	ob, ok := strings.CutPrefix(uri, "gs://")
	if !ok {
		t.Fatalf("%q is not a gs:// URI", uri)
	}
	bucketName, prefix, _ := strings.Cut(ob, "/")
	var out strings.Builder
	it := sc.Bucket(bucketName).Objects(ctx, &storage.Query{Prefix: prefix})
	for {
		a, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("listing %s: %v", uri, err)
		}
		rd, err := sc.Bucket(bucketName).Object(a.Name).NewReader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(&out, rd)
		_ = rd.Close()
	}
	return out.String()
}

// dataprocGet polls url until it answers 200, and returns its body.
func dataprocGet(ctx context.Context, url string) (string, error) {
	var last error
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			last = err
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return string(b), nil
		}
		last = &httpStatusError{resp.StatusCode, string(b)}
	}
	return "", last
}

type httpStatusError struct {
	code int
	body string
}

func (e *httpStatusError) Error() string {
	if len(e.body) > 300 {
		e.body = e.body[:300]
	}
	return http.StatusText(e.code) + ": " + e.body
}

// waitJob polls a job until it is done.
func dataprocWaitJob(ctx context.Context, jc *dataproc.JobControllerClient, project, region, id string) (*dataprocpb.Job, error) {
	for {
		j, err := jc.GetJob(ctx, &dataprocpb.GetJobRequest{ProjectId: project, Region: region, JobId: id})
		if err != nil || j.GetDone() {
			return j, err
		}
		select {
		case <-ctx.Done():
			return j, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

const dataprocPi = `
import sys
from operator import add
from random import random
from pyspark.sql import SparkSession

spark = SparkSession.builder.appName("pi").getOrCreate()
n = 1000 * int(sys.argv[1])
def inside(_):
    x, y = random() * 2 - 1, random() * 2 - 1
    return 1 if x * x + y * y <= 1 else 0
count = spark.sparkContext.parallelize(range(n), 4).map(inside).reduce(add)
print("Pi is roughly %f" % (4.0 * count / n))
print("args", " ".join(sys.argv[1:]))
`

const dataprocSparkR = `
library(SparkR)
sparkR.session(appName = "sparkr-compat")
df <- createDataFrame(data.frame(x = 1:4, y = c("a", "b", "a", "b")))
cat("rows", count(df), "\n")
cat("groups", nrow(collect(count(groupBy(df, "y")))), "\n")
`

// TestDataprocClusters (#2162): Dataproc on Compute Engine's clusters, as
// https://docs.cloud.google.com/dataproc/docs/guides/create-cluster and the
// v1 reference document them, through the official
// cloud.google.com/go/dataproc/v2 clients over gRPC and REST, each cluster
// Apache Spark's standalone master and workers in containers: an
// autoscaling policy is created, read and listed; a cluster on image 2.2
// with 2 workers and the component gateway is created by a long-running
// operation (ClusterOperationMetadata CREATE) and is RUNNING with its
// UUID, its staging and temp buckets, its instance names and Dataproc's
// labels; a duplicate is ALREADY_EXISTS and a name of capitals
// INVALID_ARGUMENT; get and a filtered list over REST; its Spark History
// Server answers at its component gateway URL. A PySpark job runs on it
// with its arguments, DONE, its driver output in Cloud Storage at
// driverOutputResourceUri, its Spark application's tracking URL the
// History Server's page of it; a SparkR job runs as an operation; a failing
// job ends ERROR with the driver's error; a Hive job is UNIMPLEMENTED (no
// YARN or Hive). The cluster is resized to 3 workers over REST, stopped,
// started, diagnosed (a tarball in the staging bucket) and deleted.
//
// covers: google.cloud.dataproc.v1.ClusterController/CreateCluster, google.cloud.dataproc.v1.ClusterController/GetCluster, google.cloud.dataproc.v1.ClusterController/ListClusters, google.cloud.dataproc.v1.ClusterController/UpdateCluster, google.cloud.dataproc.v1.ClusterController/StopCluster, google.cloud.dataproc.v1.ClusterController/StartCluster, google.cloud.dataproc.v1.ClusterController/DiagnoseCluster, google.cloud.dataproc.v1.ClusterController/DeleteCluster, google.cloud.dataproc.v1.JobController/SubmitJob, google.cloud.dataproc.v1.JobController/SubmitJobAsOperation, google.cloud.dataproc.v1.JobController/GetJob, google.cloud.dataproc.v1.JobController/ListJobs, google.cloud.dataproc.v1.JobController/UpdateJob, google.cloud.dataproc.v1.JobController/DeleteJob, google.cloud.dataproc.v1.AutoscalingPolicyService/CreateAutoscalingPolicy, google.cloud.dataproc.v1.AutoscalingPolicyService/GetAutoscalingPolicy, google.cloud.dataproc.v1.AutoscalingPolicyService/ListAutoscalingPolicies, google.cloud.dataproc.v1.AutoscalingPolicyService/UpdateAutoscalingPolicy, google.cloud.dataproc.v1.AutoscalingPolicyService/DeleteAutoscalingPolicy
func TestDataprocClusters(t *testing.T) {
	h, addr, project := dataprocEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	grpcOpts := append(dataprocGRPC(), option.WithEndpoint(addr))
	restOpts := []option.ClientOption{option.WithEndpoint("http://" + addr), option.WithoutAuthentication()}
	cc, err := dataproc.NewClusterControllerClient(ctx, grpcOpts...)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	cr, err := dataproc.NewClusterControllerRESTClient(ctx, restOpts...)
	if err != nil {
		t.Fatal(err)
	}
	defer cr.Close()
	jc, err := dataproc.NewJobControllerClient(ctx, grpcOpts...)
	if err != nil {
		t.Fatal(err)
	}
	defer jc.Close()
	jr, err := dataproc.NewJobControllerRESTClient(ctx, restOpts...)
	if err != nil {
		t.Fatal(err)
	}
	defer jr.Close()
	pc, err := dataproc.NewAutoscalingPolicyClient(ctx, grpcOpts...)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	sc := storageClient(t, h)
	bh := bucket(t, h, sc)
	const region = "us-central1"
	suffix := dataprocSuffix(h, 30)

	// An autoscaling policy.
	parent := "projects/" + project + "/regions/" + region
	pol, err := pc.CreateAutoscalingPolicy(ctx, &dataprocpb.CreateAutoscalingPolicyRequest{Parent: parent, Policy: &dataprocpb.AutoscalingPolicy{
		Id: "pol-" + suffix, WorkerConfig: &dataprocpb.InstanceGroupAutoscalingPolicyConfig{MinInstances: 2, MaxInstances: 4},
		Algorithm: &dataprocpb.AutoscalingPolicy_BasicAlgorithm{BasicAlgorithm: &dataprocpb.BasicAutoscalingAlgorithm{
			Config: &dataprocpb.BasicAutoscalingAlgorithm_YarnConfig{YarnConfig: &dataprocpb.BasicYarnAutoscalingConfig{
				GracefulDecommissionTimeout: durationpb.New(time.Hour), ScaleUpFactor: 0.5, ScaleDownFactor: 1}}}}}})
	if err != nil {
		t.Fatalf("CreateAutoscalingPolicy: %v", err)
	}
	t.Cleanup(func() {
		_ = pc.DeleteAutoscalingPolicy(h.Context(), &dataprocpb.DeleteAutoscalingPolicyRequest{Name: pol.Name})
	})
	if pol.Name != parent+"/autoscalingPolicies/pol-"+suffix {
		t.Errorf("policy name %q", pol.Name)
	}
	if _, err := pc.CreateAutoscalingPolicy(ctx, &dataprocpb.CreateAutoscalingPolicyRequest{Parent: parent, Policy: &dataprocpb.AutoscalingPolicy{
		Id: "bad-" + suffix, WorkerConfig: &dataprocpb.InstanceGroupAutoscalingPolicyConfig{MaxInstances: 4},
		Algorithm: &dataprocpb.AutoscalingPolicy_BasicAlgorithm{BasicAlgorithm: &dataprocpb.BasicAutoscalingAlgorithm{
			Config: &dataprocpb.BasicAutoscalingAlgorithm_YarnConfig{YarnConfig: &dataprocpb.BasicYarnAutoscalingConfig{
				GracefulDecommissionTimeout: durationpb.New(time.Hour), ScaleUpFactor: 2, ScaleDownFactor: 1}}}}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a scale up factor of 2: %v", err)
	}
	pol.WorkerConfig.MaxInstances = 5
	if up, err := pc.UpdateAutoscalingPolicy(ctx, &dataprocpb.UpdateAutoscalingPolicyRequest{Policy: pol}); err != nil || up.GetWorkerConfig().GetMaxInstances() != 5 {
		t.Errorf("UpdateAutoscalingPolicy: %v %v", up, err)
	}
	if got, err := pc.GetAutoscalingPolicy(ctx, &dataprocpb.GetAutoscalingPolicyRequest{Name: pol.Name}); err != nil || got.GetBasicAlgorithm().GetYarnConfig().GetScaleUpFactor() != 0.5 {
		t.Errorf("GetAutoscalingPolicy: %v %v", got, err)
	}
	listed := false
	for it := pc.ListAutoscalingPolicies(ctx, &dataprocpb.ListAutoscalingPoliciesRequest{Parent: parent}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListAutoscalingPolicies: %v", err)
		}
		listed = listed || x.Name == pol.Name
	}
	if !listed {
		t.Error("ListAutoscalingPolicies does not list the policy")
	}

	// The cluster.
	name := "cc-" + suffix
	if _, err := cr.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: project, Region: region, Cluster: &dataprocpb.Cluster{ProjectId: project, ClusterName: "Capitals"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a cluster name of capitals: %v", err)
	}
	op, err := cc.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: project, Region: region, Cluster: &dataprocpb.Cluster{
		ProjectId: project, ClusterName: name, Labels: map[string]string{"suite": "compat"},
		Config: &dataprocpb.ClusterConfig{
			SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.2", Properties: map[string]string{"spark:spark.executor.memory": "1g"}},
			WorkerConfig:   &dataprocpb.InstanceGroupConfig{NumInstances: 2},
			EndpointConfig: &dataprocpb.EndpointConfig{EnableHttpPortAccess: true}}}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	t.Cleanup(func() {
		if dop, err := cc.DeleteCluster(h.Context(), &dataprocpb.DeleteClusterRequest{ProjectId: project, Region: region, ClusterName: name}); err == nil {
			_ = dop.Wait(h.Context())
		}
	})
	if md, err := op.Metadata(); err != nil || md.GetClusterName() != name || md.GetOperationType() != "CREATE" {
		t.Errorf("ClusterOperationMetadata: %v %v", md, err)
	}
	c, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("the create operation: %v", err)
	}
	if c.GetStatus().GetState() != dataprocpb.ClusterStatus_RUNNING || c.ClusterUuid == "" || c.Labels["goog-dataproc-cluster-name"] != name || c.Labels["suite"] != "compat" ||
		!strings.HasPrefix(c.Config.GetConfigBucket(), "dataproc-staging-") || !strings.HasPrefix(c.Config.GetTempBucket(), "dataproc-temp-") ||
		len(c.Config.GetWorkerConfig().GetInstanceNames()) != 2 || c.Config.GetMasterConfig().GetInstanceNames()[0] != name+"-m" ||
		!strings.HasPrefix(c.Config.GetSoftwareConfig().GetImageVersion(), "2.2") {
		t.Errorf("the cluster: %v", c)
	}
	if _, err := cc.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: project, Region: region, Cluster: &dataprocpb.Cluster{ProjectId: project, ClusterName: name}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a duplicate cluster: %v", err)
	}
	g, err := cr.GetCluster(ctx, &dataprocpb.GetClusterRequest{ProjectId: project, Region: region, ClusterName: name})
	if err != nil || g.ClusterUuid != c.ClusterUuid {
		t.Errorf("GetCluster over REST: %v %v", g, err)
	}
	found := false
	for it := cr.ListClusters(ctx, &dataprocpb.ListClustersRequest{ProjectId: project, Region: region, Filter: "status.state = ACTIVE AND labels.suite = compat"}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListClusters: %v", err)
		}
		found = found || x.ClusterName == name
	}
	if !found {
		t.Error("ListClusters with a filter does not list the cluster")
	}
	history := g.GetConfig().GetEndpointConfig().GetHttpPorts()["Spark History Server"]
	if !strings.HasPrefix(history, "http://") {
		t.Fatalf("httpPorts: %v", g.GetConfig().GetEndpointConfig().GetHttpPorts())
	}
	if body, err := dataprocGet(ctx, history); err != nil || !strings.Contains(body, "History Server") {
		t.Errorf("the Spark History Server at %s: %v %.200s", history, err, body)
	}

	// A PySpark job.
	pi := dataprocPut(t, ctx, bh, "pi.py", dataprocPi)
	jobID := "pi-" + suffix
	j, err := jc.SubmitJob(ctx, &dataprocpb.SubmitJobRequest{ProjectId: project, Region: region, Job: &dataprocpb.Job{
		Reference: &dataprocpb.JobReference{JobId: jobID}, Placement: &dataprocpb.JobPlacement{ClusterName: name}, Labels: map[string]string{"suite": "compat"},
		TypeJob: &dataprocpb.Job_PysparkJob{PysparkJob: &dataprocpb.PySparkJob{MainPythonFileUri: pi, Args: []string{"20", "compat"}}}}})
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	if j.GetPlacement().GetClusterUuid() != c.ClusterUuid || j.DriverOutputResourceUri == "" {
		t.Errorf("the submitted job: %v", j)
	}
	j, err = dataprocWaitJob(ctx, jc, project, region, jobID)
	if err != nil || j.GetStatus().GetState() != dataprocpb.JobStatus_DONE {
		t.Fatalf("the job: %v %v", j, err)
	}
	if out := dataprocGSText(t, ctx, sc, j.DriverOutputResourceUri); !strings.Contains(out, "Pi is roughly 3.") || !strings.Contains(out, "args 20 compat") {
		t.Errorf("driver output at %s: %q", j.DriverOutputResourceUri, out)
	}
	if apps := j.GetYarnApplications(); len(apps) != 1 || !strings.Contains(apps[0].GetTrackingUrl(), "/sparkhistory/history/") {
		t.Errorf("the job's Spark application: %v", apps)
	} else if body, err := dataprocGet(ctx, apps[0].TrackingUrl); err != nil || !strings.Contains(body, "pi") {
		t.Errorf("the application's page at %s: %v", apps[0].TrackingUrl, err)
	}
	found = false
	for it := jr.ListJobs(ctx, &dataprocpb.ListJobsRequest{ProjectId: project, Region: region, ClusterName: name, Filter: "status.state = NON_ACTIVE AND labels.suite = compat"}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListJobs: %v", err)
		}
		found = found || x.GetReference().GetJobId() == jobID
	}
	if !found {
		t.Error("ListJobs does not list the job")
	}
	if up, err := jr.UpdateJob(ctx, &dataprocpb.UpdateJobRequest{ProjectId: project, Region: region, JobId: jobID,
		Job: &dataprocpb.Job{Labels: map[string]string{"suite": "compat", "seen": "yes"}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}}); err != nil || up.Labels["seen"] != "yes" {
		t.Errorf("UpdateJob: %v %v", up, err)
	}

	// A SparkR job, as an operation.
	rop, err := jr.SubmitJobAsOperation(ctx, &dataprocpb.SubmitJobRequest{ProjectId: project, Region: region, Job: &dataprocpb.Job{
		Placement: &dataprocpb.JobPlacement{ClusterName: name},
		TypeJob:   &dataprocpb.Job_SparkRJob{SparkRJob: &dataprocpb.SparkRJob{MainRFileUri: dataprocPut(t, ctx, bh, "count.R", dataprocSparkR)}}}})
	if err != nil {
		t.Fatalf("SubmitJobAsOperation: %v", err)
	}
	rj, err := rop.Wait(ctx)
	if err != nil || rj.GetStatus().GetState() != dataprocpb.JobStatus_DONE {
		t.Errorf("the SparkR job: %v %v", rj, err)
	} else if out := dataprocGSText(t, ctx, sc, rj.DriverOutputResourceUri); !strings.Contains(out, "rows 4") || !strings.Contains(out, "groups 2") {
		t.Errorf("SparkR driver output: %q", out)
	}

	// A failing job, and a job type this instance does not run.
	bad, err := jc.SubmitJob(ctx, &dataprocpb.SubmitJobRequest{ProjectId: project, Region: region, Job: &dataprocpb.Job{
		Placement: &dataprocpb.JobPlacement{ClusterName: name},
		TypeJob:   &dataprocpb.Job_PysparkJob{PysparkJob: &dataprocpb.PySparkJob{MainPythonFileUri: dataprocPut(t, ctx, bh, "fail.py", "raise ValueError('compat kaboom')\n")}}}})
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	if bad, err = dataprocWaitJob(ctx, jc, project, region, bad.GetReference().GetJobId()); err != nil || bad.GetStatus().GetState() != dataprocpb.JobStatus_ERROR {
		t.Errorf("the failing job: %v %v", bad, err)
	}
	if _, err := jc.SubmitJob(ctx, &dataprocpb.SubmitJobRequest{ProjectId: project, Region: region, Job: &dataprocpb.Job{
		Placement: &dataprocpb.JobPlacement{ClusterName: name},
		TypeJob:   &dataprocpb.Job_HiveJob{HiveJob: &dataprocpb.HiveJob{Queries: &dataprocpb.HiveJob_QueryList{QueryList: &dataprocpb.QueryList{Queries: []string{"SHOW TABLES"}}}}}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("a Hive job: %v", err)
	}
	if err := jc.DeleteJob(ctx, &dataprocpb.DeleteJobRequest{ProjectId: project, Region: region, JobId: jobID}); err != nil {
		t.Errorf("DeleteJob: %v", err)
	}

	// Resize, stop, start, diagnose.
	uop, err := cr.UpdateCluster(ctx, &dataprocpb.UpdateClusterRequest{ProjectId: project, Region: region, ClusterName: name,
		Cluster:    &dataprocpb.Cluster{Config: &dataprocpb.ClusterConfig{WorkerConfig: &dataprocpb.InstanceGroupConfig{NumInstances: 3}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"config.worker_config.num_instances"}}})
	if err != nil {
		t.Fatalf("UpdateCluster: %v", err)
	}
	if u, err := uop.Wait(ctx); err != nil || u.GetConfig().GetWorkerConfig().GetNumInstances() != 3 || len(u.GetConfig().GetWorkerConfig().GetInstanceNames()) != 3 {
		t.Errorf("the resized cluster: %v %v", u, err)
	}
	sop, err := cc.StopCluster(ctx, &dataprocpb.StopClusterRequest{ProjectId: project, Region: region, ClusterName: name})
	if err != nil {
		t.Fatalf("StopCluster: %v", err)
	}
	if s, err := sop.Wait(ctx); err != nil || s.GetStatus().GetState() != dataprocpb.ClusterStatus_STOPPED {
		t.Errorf("the stopped cluster: %v %v", s, err)
	}
	stop, err := cc.StartCluster(ctx, &dataprocpb.StartClusterRequest{ProjectId: project, Region: region, ClusterName: name})
	if err != nil {
		t.Fatalf("StartCluster: %v", err)
	}
	if s, err := stop.Wait(ctx); err != nil || s.GetStatus().GetState() != dataprocpb.ClusterStatus_RUNNING {
		t.Errorf("the started cluster: %v %v", s, err)
	}
	dop, err := cc.DiagnoseCluster(ctx, &dataprocpb.DiagnoseClusterRequest{ProjectId: project, Region: region, ClusterName: name})
	if err != nil {
		t.Fatalf("DiagnoseCluster: %v", err)
	}
	dr, err := dop.Wait(ctx)
	if err != nil || !strings.HasPrefix(dr.GetOutputUri(), "gs://"+c.Config.GetConfigBucket()+"/") {
		t.Errorf("DiagnoseClusterResults: %v %v", dr, err)
	} else if tarball := dataprocGSText(t, ctx, sc, dr.OutputUri); len(tarball) < 100 || tarball[0] != 0x1f {
		t.Errorf("the diagnostic tarball at %s is %d bytes", dr.OutputUri, len(tarball))
	}

	del, err := cc.DeleteCluster(ctx, &dataprocpb.DeleteClusterRequest{ProjectId: project, Region: region, ClusterName: name})
	if err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	if err := del.Wait(ctx); err != nil {
		t.Errorf("the delete operation: %v", err)
	}
	if _, err := cc.GetCluster(ctx, &dataprocpb.GetClusterRequest{ProjectId: project, Region: region, ClusterName: name}); status.Code(err) != codes.NotFound {
		t.Errorf("the deleted cluster: %v", err)
	}
	if err := pc.DeleteAutoscalingPolicy(ctx, &dataprocpb.DeleteAutoscalingPolicyRequest{Name: pol.Name}); err != nil {
		t.Errorf("DeleteAutoscalingPolicy: %v", err)
	}
}

// TestDataprocWorkflowTemplates (#2162): workflow templates, as
// https://docs.cloud.google.com/dataproc/docs/concepts/workflows/overview
// and .../using-workflows document them, through the official Go clients
// over gRPC and REST: a template whose managed cluster is a single node
// cluster, two PySpark steps the second of which needs the first, and a
// parameter substituted into the first's arguments; created, read over
// REST, listed, updated (version 2; a stale version FAILED_PRECONDITION);
// a parameter value its regex refuses is INVALID_ARGUMENT; instantiated,
// its operation's WorkflowMetadata DONE with both nodes COMPLETED and the
// managed cluster created and deleted, its jobs labelled with the
// template and step and their driver output the parameter's value; an
// inline template instantiated over REST; the template deleted.
//
// covers: google.cloud.dataproc.v1.WorkflowTemplateService/CreateWorkflowTemplate, google.cloud.dataproc.v1.WorkflowTemplateService/GetWorkflowTemplate, google.cloud.dataproc.v1.WorkflowTemplateService/ListWorkflowTemplates, google.cloud.dataproc.v1.WorkflowTemplateService/UpdateWorkflowTemplate, google.cloud.dataproc.v1.WorkflowTemplateService/InstantiateWorkflowTemplate, google.cloud.dataproc.v1.WorkflowTemplateService/InstantiateInlineWorkflowTemplate, google.cloud.dataproc.v1.WorkflowTemplateService/DeleteWorkflowTemplate
func TestDataprocWorkflowTemplates(t *testing.T) {
	h, addr, project := dataprocEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	wc, err := dataproc.NewWorkflowTemplateClient(ctx, append(dataprocGRPC(), option.WithEndpoint(addr))...)
	if err != nil {
		t.Fatal(err)
	}
	defer wc.Close()
	wr, err := dataproc.NewWorkflowTemplateRESTClient(ctx, option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer wr.Close()
	jc, err := dataproc.NewJobControllerClient(ctx, append(dataprocGRPC(), option.WithEndpoint(addr))...)
	if err != nil {
		t.Fatal(err)
	}
	defer jc.Close()
	sc := storageClient(t, h)
	bh := bucket(t, h, sc)
	const region = "us-central1"
	parent := "projects/" + project + "/regions/" + region
	suffix := dataprocSuffix(h, 30)
	pi := dataprocPut(t, ctx, bh, "pi.py", dataprocPi)
	singleNode := &dataprocpb.ClusterConfig{SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.2",
		Properties: map[string]string{"dataproc:dataproc.allow.zero.workers": "true"}}}

	tmpl := &dataprocpb.WorkflowTemplate{Id: "wf-" + suffix, Labels: map[string]string{"suite": "compat"},
		Placement: &dataprocpb.WorkflowTemplatePlacement{Placement: &dataprocpb.WorkflowTemplatePlacement_ManagedCluster{
			ManagedCluster: &dataprocpb.ManagedCluster{ClusterName: "wf", Config: singleNode}}},
		Jobs: []*dataprocpb.OrderedJob{
			{StepId: "first", JobType: &dataprocpb.OrderedJob_PysparkJob{PysparkJob: &dataprocpb.PySparkJob{MainPythonFileUri: pi, Args: []string{"5", "MSG"}}}},
			{StepId: "second", PrerequisiteStepIds: []string{"first"}, JobType: &dataprocpb.OrderedJob_PysparkJob{PysparkJob: &dataprocpb.PySparkJob{MainPythonFileUri: pi, Args: []string{"5", "after"}}}}},
		Parameters: []*dataprocpb.TemplateParameter{{Name: "MSG", Fields: []string{"jobs['first'].pysparkJob.args[1]"},
			Validation: &dataprocpb.ParameterValidation{ValidationType: &dataprocpb.ParameterValidation_Regex{Regex: &dataprocpb.RegexValidation{Regexes: []string{"[a-z]+"}}}}}}}
	created, err := wc.CreateWorkflowTemplate(ctx, &dataprocpb.CreateWorkflowTemplateRequest{Parent: parent, Template: tmpl})
	if err != nil {
		t.Fatalf("CreateWorkflowTemplate: %v", err)
	}
	t.Cleanup(func() {
		_ = wc.DeleteWorkflowTemplate(h.Context(), &dataprocpb.DeleteWorkflowTemplateRequest{Name: created.Name})
	})
	if created.Name != parent+"/workflowTemplates/"+tmpl.Id || created.Version != 1 || created.CreateTime == nil {
		t.Errorf("the template: %v", created)
	}
	got, err := wr.GetWorkflowTemplate(ctx, &dataprocpb.GetWorkflowTemplateRequest{Name: created.Name})
	if err != nil || len(got.GetJobs()) != 2 || got.GetJobs()[1].GetPrerequisiteStepIds()[0] != "first" {
		t.Errorf("GetWorkflowTemplate over REST: %v %v", got, err)
	}
	listed := false
	for it := wc.ListWorkflowTemplates(ctx, &dataprocpb.ListWorkflowTemplatesRequest{Parent: parent}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListWorkflowTemplates: %v", err)
		}
		listed = listed || x.Name == created.Name
	}
	if !listed {
		t.Error("ListWorkflowTemplates does not list the template")
	}
	created.Labels["round"] = "two"
	updated, err := wr.UpdateWorkflowTemplate(ctx, &dataprocpb.UpdateWorkflowTemplateRequest{Template: created})
	if err != nil || updated.Version != 2 {
		t.Fatalf("UpdateWorkflowTemplate: %v %v", updated, err)
	}
	if _, err := wc.UpdateWorkflowTemplate(ctx, &dataprocpb.UpdateWorkflowTemplateRequest{Template: created}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("an update of version 1: %v", err)
	}
	if _, err := wc.InstantiateWorkflowTemplate(ctx, &dataprocpb.InstantiateWorkflowTemplateRequest{Name: created.Name, Parameters: map[string]string{"MSG": "NOT-OK"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a parameter its regex refuses: %v", err)
	}

	op, err := wc.InstantiateWorkflowTemplate(ctx, &dataprocpb.InstantiateWorkflowTemplateRequest{Name: created.Name, Parameters: map[string]string{"MSG": "hello"}})
	if err != nil {
		t.Fatalf("InstantiateWorkflowTemplate: %v", err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatalf("the workflow: %v", err)
	}
	md, err := op.Metadata()
	if err != nil || md.GetState() != dataprocpb.WorkflowMetadata_DONE || md.GetTemplate() != created.Name || md.GetVersion() != 2 ||
		!strings.HasPrefix(md.GetClusterName(), "wf-") || !md.GetCreateCluster().GetDone() || !md.GetDeleteCluster().GetDone() || len(md.GetGraph().GetNodes()) != 2 {
		t.Fatalf("WorkflowMetadata: %v %v", md, err)
	}
	for _, n := range md.GetGraph().GetNodes() {
		if n.GetState() != dataprocpb.WorkflowNode_COMPLETED || n.GetJobId() == "" {
			t.Errorf("node %v", n)
		}
		j, err := jc.GetJob(ctx, &dataprocpb.GetJobRequest{ProjectId: project, Region: region, JobId: n.GetJobId()})
		if err != nil || j.Labels["goog-dataproc-workflow-template-id"] != tmpl.Id || j.Labels["goog-dataproc-workflow-step-id"] != n.GetStepId() {
			t.Errorf("the step's job: %v %v", j, err)
			continue
		}
		want := map[string]string{"first": "args 5 hello", "second": "args 5 after"}[n.GetStepId()]
		if out := dataprocGSText(t, ctx, sc, j.DriverOutputResourceUri); !strings.Contains(out, want) {
			t.Errorf("step %s's driver output: %q", n.GetStepId(), out)
		}
	}

	// An inline template, over REST.
	iop, err := wr.InstantiateInlineWorkflowTemplate(ctx, &dataprocpb.InstantiateInlineWorkflowTemplateRequest{Parent: parent, Template: &dataprocpb.WorkflowTemplate{
		Placement: &dataprocpb.WorkflowTemplatePlacement{Placement: &dataprocpb.WorkflowTemplatePlacement_ManagedCluster{
			ManagedCluster: &dataprocpb.ManagedCluster{ClusterName: "inline", Config: singleNode}}},
		Jobs: []*dataprocpb.OrderedJob{{StepId: "only", JobType: &dataprocpb.OrderedJob_PysparkJob{PysparkJob: &dataprocpb.PySparkJob{MainPythonFileUri: pi, Args: []string{"5", "inline"}}}}}}})
	if err != nil {
		t.Fatalf("InstantiateInlineWorkflowTemplate: %v", err)
	}
	if err := iop.Wait(ctx); err != nil {
		t.Errorf("the inline workflow: %v", err)
	}
	if imd, err := iop.Metadata(); err != nil || imd.GetState() != dataprocpb.WorkflowMetadata_DONE {
		t.Errorf("the inline workflow's metadata: %v %v", imd, err)
	}
	if err := wc.DeleteWorkflowTemplate(ctx, &dataprocpb.DeleteWorkflowTemplateRequest{Name: created.Name}); err != nil {
		t.Errorf("DeleteWorkflowTemplate: %v", err)
	}
	if _, err := wc.GetWorkflowTemplate(ctx, &dataprocpb.GetWorkflowTemplateRequest{Name: created.Name}); status.Code(err) != codes.NotFound {
		t.Errorf("the deleted template: %v", err)
	}
}

const dataprocNotebook = `{
 "nbformat": 4, "nbformat_minor": 5, "metadata": {},
 "cells": [
  {"cell_type": "markdown", "metadata": {}, "source": ["# A notebook batch"]},
  {"cell_type": "code", "metadata": {"tags": ["parameters"]}, "source": ["n = \"3\"\n"], "outputs": [], "execution_count": null},
  {"cell_type": "code", "metadata": {}, "source": ["from pyspark.sql import SparkSession\n", "spark = SparkSession.builder.getOrCreate()\n",
    "print(\"n\", n, \"count\", spark.range(int(n)).count())"], "outputs": [], "execution_count": null}
 ]
}`

// TestDataprocJupyterNotebookAndSparkR (#2162): the rest of Serverless
// for Apache Spark's workloads, through the official Go clients: a
// Jupyter session (kernel PYTHON) is ACTIVE and answers Spark Connect on
// this API's port by its UUID, then is terminated; a pyspark_notebook_batch
// runs the notebook's code cells with its params set after the cell
// tagged parameters; a SparkR batch runs on runtime 2.2; the notebook
// batch's Spark UI is its application in the instance's Spark History
// Server.
//
// covers: google.cloud.dataproc.v1.SessionController/CreateSession, google.cloud.dataproc.v1.SessionController/TerminateSession, google.cloud.dataproc.v1.BatchController/CreateBatch, google.cloud.dataproc.v1.BatchController/GetBatch
func TestDataprocJupyterNotebookAndSparkR(t *testing.T) {
	h, addr, project := dataprocEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	sess, err := dataproc.NewSessionControllerClient(ctx, append(dataprocGRPC(), option.WithEndpoint(addr))...)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	bc, err := dataproc.NewBatchControllerClient(ctx, append(dataprocGRPC(), option.WithEndpoint(addr))...)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Close()
	sc := storageClient(t, h)
	bh := bucket(t, h, sc)
	parent := "projects/" + project + "/locations/us-central1"
	suffix := dataprocSuffix(h, 30)

	// A Jupyter session.
	op, err := sess.CreateSession(ctx, &dataprocpb.CreateSessionRequest{Parent: parent, SessionId: "jup-" + suffix, Session: &dataprocpb.Session{
		SessionConfig: &dataprocpb.Session_JupyterSession{JupyterSession: &dataprocpb.JupyterConfig{DisplayName: "compat"}},
		RuntimeConfig: &dataprocpb.RuntimeConfig{Version: "3.0"}}})
	if err != nil {
		t.Fatalf("CreateSession (Jupyter): %v", err)
	}
	x, err := op.Wait(ctx)
	if err != nil || x.GetState() != dataprocpb.Session_ACTIVE || x.GetJupyterSession().GetKernel() != dataprocpb.JupyterConfig_PYTHON {
		t.Fatalf("the Jupyter session: %v %v", x, err)
	}
	t.Cleanup(func() {
		if dop, err := sess.DeleteSession(h.Context(), &dataprocpb.DeleteSessionRequest{Name: x.Name}); err == nil {
			_, _ = dop.Wait(h.Context())
		}
	})
	conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if n, err := sparkConnectRows(ctx, conn, sparkConnectRequest(x.Uuid, sqlRelation("SELECT id FROM range(3)"))); err != nil || n != 3 {
		t.Errorf("Spark Connect on the Jupyter session: %d rows, %v", n, err)
	}
	top, err := sess.TerminateSession(ctx, &dataprocpb.TerminateSessionRequest{Name: x.Name})
	if err != nil {
		t.Fatalf("TerminateSession: %v", err)
	}
	if y, err := top.Wait(ctx); err != nil || y.GetState() != dataprocpb.Session_TERMINATED {
		t.Errorf("the terminated session: %v %v", y, err)
	}

	// A notebook batch.
	bop, err := bc.CreateBatch(ctx, &dataprocpb.CreateBatchRequest{Parent: parent, BatchId: "nb-" + suffix, Batch: &dataprocpb.Batch{
		BatchConfig: &dataprocpb.Batch_PysparkNotebookBatch{PysparkNotebookBatch: &dataprocpb.PySparkNotebookBatch{
			NotebookFileUri: dataprocPut(t, ctx, bh, "count.ipynb", dataprocNotebook), Params: map[string]string{"n": "7"}}},
		RuntimeConfig: &dataprocpb.RuntimeConfig{Version: "2.2"}}})
	if err != nil {
		t.Fatalf("CreateBatch (notebook): %v", err)
	}
	nb, err := bop.Wait(ctx)
	if err != nil || nb.GetState() != dataprocpb.Batch_SUCCEEDED {
		t.Fatalf("the notebook batch: %v %v", nb, err)
	}
	t.Cleanup(func() { _ = bc.DeleteBatch(h.Context(), &dataprocpb.DeleteBatchRequest{Name: nb.Name}) })
	if out := dataprocGSText(t, ctx, sc, nb.GetRuntimeInfo().GetOutputUri()); !strings.Contains(out, "n 7 count 7") {
		t.Errorf("the notebook batch's output: %q", out)
	}
	ui := "http://" + addr + "/sparkhistory/history/app-" + strings.ReplaceAll(nb.Uuid, "-", "") + "/jobs/"
	if body, err := dataprocGet(ctx, ui); err != nil || !strings.Contains(body, "Spark Jobs") {
		t.Errorf("the notebook batch's Spark UI at %s: %v", ui, err)
	}

	// A SparkR batch.
	rop, err := bc.CreateBatch(ctx, &dataprocpb.CreateBatchRequest{Parent: parent, BatchId: "r-" + suffix, Batch: &dataprocpb.Batch{
		BatchConfig:   &dataprocpb.Batch_SparkRBatch{SparkRBatch: &dataprocpb.SparkRBatch{MainRFileUri: dataprocPut(t, ctx, bh, "count.R", dataprocSparkR)}},
		RuntimeConfig: &dataprocpb.RuntimeConfig{Version: "2.2"}}})
	if err != nil {
		t.Fatalf("CreateBatch (SparkR): %v", err)
	}
	rb, err := rop.Wait(ctx)
	if err != nil || rb.GetState() != dataprocpb.Batch_SUCCEEDED {
		t.Fatalf("the SparkR batch: %v %v", rb, err)
	}
	t.Cleanup(func() { _ = bc.DeleteBatch(h.Context(), &dataprocpb.DeleteBatchRequest{Name: rb.Name}) })
	if out := dataprocGSText(t, ctx, sc, rb.GetRuntimeInfo().GetOutputUri()); !strings.Contains(out, "rows 4") {
		t.Errorf("the SparkR batch's output: %q", out)
	}
}
