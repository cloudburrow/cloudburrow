package dataproc

import (
	"context"
	"strings"
	"sync"
	"testing"

	dataprocpb "cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/sparkproc"
)

// fakeClusterEngine is a fakeEngine that starts clusters.
type fakeClusterEngine struct {
	fakeEngine
	mu       sync.Mutex
	clusters []*fakeNodes
	submit   func(sparkproc.Job) sparkproc.Result
}

type fakeNodes struct {
	mu      sync.Mutex
	spec    sparkproc.ClusterSpec
	workers int
	stopped bool
	deleted bool
	jobs    []sparkproc.Job
	submit  func(sparkproc.Job) sparkproc.Result
	dir     string
}

func (f *fakeClusterEngine) StartCluster(_ context.Context, spec sparkproc.ClusterSpec) (Nodes, error) {
	n := &fakeNodes{spec: spec, workers: spec.Workers, submit: f.submit}
	f.mu.Lock()
	f.clusters = append(f.clusters, n)
	f.mu.Unlock()
	return n, nil
}

func (n *fakeNodes) Scale(_ context.Context, w int) error {
	n.mu.Lock()
	n.workers = w
	n.mu.Unlock()
	return nil
}
func (n *fakeNodes) Workers() int { n.mu.Lock(); defer n.mu.Unlock(); return n.workers }
func (n *fakeNodes) Stop(context.Context) error {
	n.mu.Lock()
	n.stopped = true
	n.mu.Unlock()
	return nil
}
func (n *fakeNodes) Start(context.Context) error {
	n.mu.Lock()
	n.stopped = false
	n.mu.Unlock()
	return nil
}
func (n *fakeNodes) Delete() { n.mu.Lock(); n.deleted = true; n.mu.Unlock() }
func (n *fakeNodes) Web(context.Context) map[string]string {
	return map[string]string{}
}
func (n *fakeNodes) HistoryDir() string { return n.dir }
func (n *fakeNodes) Diagnose(_ context.Context, summary string) ([]byte, error) {
	return []byte(summary), nil
}
func (n *fakeNodes) Submit(_ context.Context, job sparkproc.Job) (sparkproc.Result, error) {
	n.mu.Lock()
	n.jobs = append(n.jobs, job)
	n.mu.Unlock()
	if n.submit != nil {
		return n.submit(job), nil
	}
	return sparkproc.Result{DriverOutput: "Pi is roughly 3.14\n"}, nil
}

func waitOp(t *testing.T, s *Server, name string) *longrunningpb.Operation {
	t.Helper()
	ops := &operations{s: s}
	var op *longrunningpb.Operation
	wait(t, func() bool {
		op, _ = ops.GetOperation(context.Background(), &longrunningpb.GetOperationRequest{Name: name})
		return op.GetDone()
	})
	return op
}

func newClusterServer(t *testing.T) (*Server, *fakeClusterEngine, *fakeStorage) {
	t.Helper()
	eng := &fakeClusterEngine{}
	st := &fakeStorage{objects: map[string][]byte{"gs://b/pi.py": []byte("print(1)")}}
	s, err := NewServer(Options{Engine: eng, Storage: st, ProjectNumber: func(string) string { return "123" }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, eng, st
}

// TestClusterLifecycle: create (defaults, Dataproc's labels, names),
// list with a filter, resize, stop, start, diagnose and delete, each an
// operation; the documented refusals.
func TestClusterLifecycle(t *testing.T) {
	s, eng, st := newClusterServer(t)
	ctx := context.Background()
	for _, bad := range []*dataprocpb.Cluster{{ClusterName: "Bad"}, {ClusterName: "ends-"}, {ClusterName: "c", Config: &dataprocpb.ClusterConfig{
		WorkerConfig: &dataprocpb.InstanceGroupConfig{NumInstances: 1}}},
		{ClusterName: "c", Config: &dataprocpb.ClusterConfig{SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "1.5-debian10"}}}} {
		if _, err := s.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: "p", Region: "us-central1", Cluster: bad}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("create %v: %v", bad, err)
		}
	}
	op, err := s.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: "p", Region: "us-central1", Cluster: &dataprocpb.Cluster{
		ClusterName: "c1", Labels: map[string]string{"env": "dev"}, Config: &dataprocpb.ClusterConfig{
			SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.2"}, EndpointConfig: &dataprocpb.EndpointConfig{EnableHttpPortAccess: true}}}})
	if err != nil {
		t.Fatal(err)
	}
	var md dataprocpb.ClusterOperationMetadata
	_ = op.GetMetadata().UnmarshalTo(&md)
	if md.OperationType != "CREATE" || md.ClusterName != "c1" || md.Description != "Create cluster with 2 workers" {
		t.Errorf("metadata %v", &md)
	}
	done := waitOp(t, s, op.Name)
	var c dataprocpb.Cluster
	if err := done.GetResponse().UnmarshalTo(&c); err != nil || c.GetStatus().GetState() != dataprocpb.ClusterStatus_RUNNING {
		t.Fatalf("cluster %v %v (%v)", &c, err, done.GetError())
	}
	if c.Labels["goog-dataproc-cluster-name"] != "c1" || c.Config.SoftwareConfig.ImageVersion != "2.2-debian12" ||
		strings.Join(c.Config.WorkerConfig.InstanceNames, ",") != "c1-w-0,c1-w-1" || c.Config.MasterConfig.InstanceNames[0] != "c1-m" ||
		!strings.HasPrefix(c.Config.ConfigBucket, "dataproc-staging-us-central1-123-") || c.Config.EndpointConfig.HttpPorts["Spark History Server"] == "" {
		t.Errorf("cluster %v", &c)
	}
	if eng.clusters[0].spec.Workers != 2 || eng.clusters[0].spec.ImageVersion != "2.2" {
		t.Errorf("spec %+v", eng.clusters[0].spec)
	}
	if _, err := s.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: "p", Region: "us-central1", Cluster: &dataprocpb.Cluster{ClusterName: "c1"}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate: %v", err)
	}
	list, err := s.ListClusters(ctx, &dataprocpb.ListClustersRequest{ProjectId: "p", Region: "us-central1", Filter: "status.state = ACTIVE AND labels.env = dev"})
	if err != nil || len(list.Clusters) != 1 {
		t.Errorf("list %v %v", list, err)
	}
	if list, _ := s.ListClusters(ctx, &dataprocpb.ListClustersRequest{ProjectId: "p", Region: "us-central1", Filter: "status.state = INACTIVE"}); len(list.GetClusters()) != 0 {
		t.Errorf("inactive %v", list)
	}
	// Resize.
	op, err = s.UpdateCluster(ctx, &dataprocpb.UpdateClusterRequest{ProjectId: "p", Region: "us-central1", ClusterName: "c1",
		Cluster:    &dataprocpb.Cluster{Config: &dataprocpb.ClusterConfig{WorkerConfig: &dataprocpb.InstanceGroupConfig{NumInstances: 4}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"config.worker_config.num_instances"}}})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, s, op.Name)
	if eng.clusters[0].Workers() != 4 {
		t.Errorf("workers %d", eng.clusters[0].Workers())
	}
	if _, err := s.UpdateCluster(ctx, &dataprocpb.UpdateClusterRequest{ProjectId: "p", Region: "us-central1", ClusterName: "c1",
		Cluster: &dataprocpb.Cluster{}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"config.master_config.num_instances"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad mask: %v", err)
	}
	// Stop and start.
	op, err = s.StopCluster(ctx, &dataprocpb.StopClusterRequest{ProjectId: "p", Region: "us-central1", ClusterName: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, s, op.Name)
	g, _ := s.GetCluster(ctx, &dataprocpb.GetClusterRequest{ProjectId: "p", Region: "us-central1", ClusterName: "c1"})
	if g.GetStatus().GetState() != dataprocpb.ClusterStatus_STOPPED || !eng.clusters[0].stopped {
		t.Errorf("stopped %v", g.GetStatus())
	}
	if _, err := s.SubmitJob(ctx, &dataprocpb.SubmitJobRequest{ProjectId: "p", Region: "us-central1", Job: &dataprocpb.Job{
		Placement: &dataprocpb.JobPlacement{ClusterName: "c1"}, TypeJob: &dataprocpb.Job_PysparkJob{PysparkJob: &dataprocpb.PySparkJob{MainPythonFileUri: "gs://b/pi.py"}}}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("job on a stopped cluster: %v", err)
	}
	op, err = s.StartCluster(ctx, &dataprocpb.StartClusterRequest{ProjectId: "p", Region: "us-central1", ClusterName: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, s, op.Name)
	// Diagnose.
	op, err = s.DiagnoseCluster(ctx, &dataprocpb.DiagnoseClusterRequest{ProjectId: "p", Region: "us-central1", ClusterName: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	done = waitOp(t, s, op.Name)
	var dr dataprocpb.DiagnoseClusterResults
	if err := done.GetResponse().UnmarshalTo(&dr); err != nil || !strings.HasSuffix(dr.OutputUri, "/diagnostic.tar.gz") || st.objects[dr.OutputUri] == nil {
		t.Errorf("diagnose %v %v", &dr, done.GetError())
	}
	// Delete.
	op, err = s.DeleteCluster(ctx, &dataprocpb.DeleteClusterRequest{ProjectId: "p", Region: "us-central1", ClusterName: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, s, op.Name)
	if _, err := s.GetCluster(ctx, &dataprocpb.GetClusterRequest{ProjectId: "p", Region: "us-central1", ClusterName: "c1"}); status.Code(err) != codes.NotFound || !eng.clusters[0].deleted {
		t.Errorf("deleted: %v", err)
	}
}

// TestJobs: a PySpark job runs on its cluster, its driver output in the
// staging bucket; list, labels, cancel and delete; Hive is refused.
func TestJobs(t *testing.T) {
	s, eng, st := newClusterServer(t)
	ctx := context.Background()
	op, _ := s.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: "p", Region: "us-central1", Cluster: &dataprocpb.Cluster{ClusterName: "c1",
		Config: &dataprocpb.ClusterConfig{SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.2", Properties: map[string]string{"spark:spark.executor.memory": "1g"}}}}})
	waitOp(t, s, op.Name)
	j, err := s.SubmitJob(ctx, &dataprocpb.SubmitJobRequest{ProjectId: "p", Region: "us-central1", Job: &dataprocpb.Job{
		Reference: &dataprocpb.JobReference{JobId: "job-1"}, Placement: &dataprocpb.JobPlacement{ClusterName: "c1"}, Labels: map[string]string{"a": "b"},
		TypeJob: &dataprocpb.Job_PysparkJob{PysparkJob: &dataprocpb.PySparkJob{MainPythonFileUri: "gs://b/pi.py", Args: []string{"10"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if j.GetStatus().GetState() != dataprocpb.JobStatus_PENDING || !strings.HasSuffix(j.DriverOutputResourceUri, "/jobs/job-1/driveroutput") {
		t.Errorf("submitted %v", j)
	}
	wait(t, func() bool {
		g, _ := s.GetJob(ctx, &dataprocpb.GetJobRequest{ProjectId: "p", Region: "us-central1", JobId: "job-1"})
		return g.GetDone()
	})
	g, _ := s.GetJob(ctx, &dataprocpb.GetJobRequest{ProjectId: "p", Region: "us-central1", JobId: "job-1"})
	if g.GetStatus().GetState() != dataprocpb.JobStatus_DONE || string(st.objects[g.DriverOutputResourceUri+".000000000"]) != "Pi is roughly 3.14\n" {
		t.Errorf("job %v", g)
	}
	sub := eng.clusters[0].jobs[0]
	if sub.Kind != sparkproc.KindPySpark || string(sub.Main.Data) != "print(1)" || sub.Args[0] != "10" {
		t.Errorf("submitted job %+v", sub)
	}
	list, _ := s.ListJobs(ctx, &dataprocpb.ListJobsRequest{ProjectId: "p", Region: "us-central1", Filter: "status.state = NON_ACTIVE AND labels.a = b"})
	if len(list.GetJobs()) != 1 {
		t.Errorf("list %v", list)
	}
	if _, err := s.UpdateJob(ctx, &dataprocpb.UpdateJobRequest{ProjectId: "p", Region: "us-central1", JobId: "job-1", Job: &dataprocpb.Job{Labels: map[string]string{"x": "y"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}}); err != nil {
		t.Error(err)
	}
	if _, err := s.CancelJob(ctx, &dataprocpb.CancelJobRequest{ProjectId: "p", Region: "us-central1", JobId: "job-1"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("cancel done: %v", err)
	}
	if _, err := s.SubmitJob(ctx, &dataprocpb.SubmitJobRequest{ProjectId: "p", Region: "us-central1", Job: &dataprocpb.Job{
		Placement: &dataprocpb.JobPlacement{ClusterName: "c1"}, TypeJob: &dataprocpb.Job_HiveJob{HiveJob: &dataprocpb.HiveJob{}}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("hive: %v", err)
	}
	if _, err := s.DeleteJob(ctx, &dataprocpb.DeleteJobRequest{ProjectId: "p", Region: "us-central1", JobId: "job-1"}); err != nil {
		t.Error(err)
	}
	// A failed job's error.
	eng.clusters[0].submit = func(sparkproc.Job) sparkproc.Result {
		return sparkproc.Result{Failed: true, Message: "ValueError: kaboom"}
	}
	op2, err := s.SubmitJobAsOperation(ctx, &dataprocpb.SubmitJobRequest{ProjectId: "p", Region: "us-central1", Job: &dataprocpb.Job{
		Placement: &dataprocpb.JobPlacement{ClusterName: "c1"}, TypeJob: &dataprocpb.Job_PysparkJob{PysparkJob: &dataprocpb.PySparkJob{MainPythonFileUri: "gs://b/pi.py"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if done := waitOp(t, s, op2.Name); !strings.Contains(done.GetError().GetMessage(), "kaboom") {
		t.Errorf("failed job op %v", done)
	}
}

// TestPoliciesAndWorkflows: an autoscaling policy's bounds, a workflow
// template's checks, versions and parameters, and its instantiation on a
// managed cluster, created and deleted, its jobs in order.
func TestPoliciesAndWorkflows(t *testing.T) {
	s, eng, _ := newClusterServer(t)
	ctx := context.Background()
	pol := &dataprocpb.AutoscalingPolicy{Id: "pol-1", Algorithm: &dataprocpb.AutoscalingPolicy_BasicAlgorithm{BasicAlgorithm: &dataprocpb.BasicAutoscalingAlgorithm{
		Config: &dataprocpb.BasicAutoscalingAlgorithm_YarnConfig{YarnConfig: &dataprocpb.BasicYarnAutoscalingConfig{
			GracefulDecommissionTimeout: durationpb.New(0), ScaleUpFactor: 0.5, ScaleDownFactor: 1}}}},
		WorkerConfig: &dataprocpb.InstanceGroupAutoscalingPolicyConfig{MaxInstances: 5}}
	if _, err := s.CreateAutoscalingPolicy(ctx, &dataprocpb.CreateAutoscalingPolicyRequest{Parent: "projects/p/locations/us-central1", Policy: pol}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAutoscalingPolicy(ctx, &dataprocpb.GetAutoscalingPolicyRequest{Name: "projects/p/regions/us-central1/autoscalingPolicies/pol-1"})
	if err != nil || got.GetWorkerConfig().GetMinInstances() != 2 {
		t.Errorf("policy %v %v", got, err)
	}
	bad := clone(pol)
	bad.Id, bad.GetBasicAlgorithm().GetYarnConfig().ScaleUpFactor = "pol-2", 2
	if _, err := s.CreateAutoscalingPolicy(ctx, &dataprocpb.CreateAutoscalingPolicyRequest{Parent: "projects/p/regions/us-central1", Policy: bad}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad factor: %v", err)
	}

	tmpl := &dataprocpb.WorkflowTemplate{Id: "wf-1", Placement: &dataprocpb.WorkflowTemplatePlacement{Placement: &dataprocpb.WorkflowTemplatePlacement_ManagedCluster{
		ManagedCluster: &dataprocpb.ManagedCluster{ClusterName: "wf", Config: &dataprocpb.ClusterConfig{SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.2"}}}}},
		Jobs: []*dataprocpb.OrderedJob{
			{StepId: "first", JobType: &dataprocpb.OrderedJob_PysparkJob{PysparkJob: &dataprocpb.PySparkJob{MainPythonFileUri: "gs://b/pi.py", Args: []string{"1"}}}},
			{StepId: "second", PrerequisiteStepIds: []string{"first"}, JobType: &dataprocpb.OrderedJob_PysparkJob{PysparkJob: &dataprocpb.PySparkJob{MainPythonFileUri: "gs://b/pi.py"}}}},
		Parameters: []*dataprocpb.TemplateParameter{{Name: "ARG", Fields: []string{"jobs['first'].pysparkJob.args[0]"},
			Validation: &dataprocpb.ParameterValidation{ValidationType: &dataprocpb.ParameterValidation_Regex{Regex: &dataprocpb.RegexValidation{Regexes: []string{`\d+`}}}}}}}
	cyclic := clone(tmpl)
	cyclic.Id, cyclic.Jobs[0].PrerequisiteStepIds = "wf-2", []string{"second"}
	if _, err := s.CreateWorkflowTemplate(ctx, &dataprocpb.CreateWorkflowTemplateRequest{Parent: "projects/p/regions/us-central1", Template: cyclic}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("cycle: %v", err)
	}
	created, err := s.CreateWorkflowTemplate(ctx, &dataprocpb.CreateWorkflowTemplateRequest{Parent: "projects/p/regions/us-central1", Template: tmpl})
	if err != nil || created.Version != 1 {
		t.Fatalf("create %v %v", created, err)
	}
	created.Labels = map[string]string{"team": "a"}
	updated, err := s.UpdateWorkflowTemplate(ctx, &dataprocpb.UpdateWorkflowTemplateRequest{Template: created})
	if err != nil || updated.Version != 2 {
		t.Fatalf("update %v %v", updated, err)
	}
	if _, err := s.UpdateWorkflowTemplate(ctx, &dataprocpb.UpdateWorkflowTemplateRequest{Template: created}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("stale version: %v", err)
	}
	if _, err := s.InstantiateWorkflowTemplate(ctx, &dataprocpb.InstantiateWorkflowTemplateRequest{Name: created.Name, Parameters: map[string]string{"ARG": "x"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad parameter: %v", err)
	}
	op, err := s.InstantiateWorkflowTemplate(ctx, &dataprocpb.InstantiateWorkflowTemplateRequest{Name: created.Name, Parameters: map[string]string{"ARG": "42"}})
	if err != nil {
		t.Fatal(err)
	}
	done := waitOp(t, s, op.Name)
	if done.GetError() != nil {
		t.Fatalf("workflow failed: %v", done.GetError())
	}
	var md dataprocpb.WorkflowMetadata
	_ = done.GetMetadata().UnmarshalTo(&md)
	if md.State != dataprocpb.WorkflowMetadata_DONE || !strings.HasPrefix(md.ClusterName, "wf-") || len(md.Graph.Nodes) != 2 ||
		md.Graph.Nodes[0].State != dataprocpb.WorkflowNode_COMPLETED || md.Graph.Nodes[1].State != dataprocpb.WorkflowNode_COMPLETED ||
		!md.CreateCluster.GetDone() || !md.DeleteCluster.GetDone() {
		t.Errorf("metadata %v", &md)
	}
	n := eng.clusters[0]
	if len(n.jobs) != 2 || n.jobs[0].Args[0] != "42" || !n.deleted {
		t.Errorf("cluster jobs %+v deleted %v", n.jobs, n.deleted)
	}
	jobs, _ := s.ListJobs(ctx, &dataprocpb.ListJobsRequest{ProjectId: "p", Region: "us-central1"})
	for _, j := range jobs.GetJobs() {
		if j.Labels["goog-dataproc-workflow-template-id"] != "wf-1" || j.Labels["goog-dataproc-workflow-step-id"] == "" {
			t.Errorf("job labels %v", j.Labels)
		}
	}
	if _, err := s.DeleteWorkflowTemplate(ctx, &dataprocpb.DeleteWorkflowTemplateRequest{Name: created.Name}); err != nil {
		t.Error(err)
	}
}

// TestNotebookScript: code cells in order, params after the cell tagged
// parameters (or first), magics refused.
func TestNotebookScript(t *testing.T) {
	nb := `{"cells":[{"cell_type":"markdown","source":["# T"]},
	{"cell_type":"code","metadata":{"tags":["parameters"]},"source":["n = 1\n"]},
	{"cell_type":"code","metadata":{},"source":"print(n)"}]}`
	got, err := notebookScript([]byte(nb), map[string]string{"n": "5"})
	if err != nil || !strings.Contains(got, "n = 1\n\n# Parameters\nn = \"5\"\n\n# In[2]:\nprint(n)") {
		t.Errorf("script %q %v", got, err)
	}
	if got, _ := notebookScript([]byte(`{"cells":[{"cell_type":"code","source":"print(n)"}]}`), map[string]string{"n": "5"}); !strings.Contains(got, "# Parameters\nn = \"5\"\n\n# In[1]:") {
		t.Errorf("untagged %q", got)
	}
	if _, err := notebookScript([]byte(`{"cells":[{"cell_type":"code","source":"%pip install x"}]}`), nil); err == nil {
		t.Error("magic accepted")
	}
}
