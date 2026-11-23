package dataproc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	groups  map[string][]string
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
func (n *fakeNodes) SetNodeGroup(_ context.Context, id string, instances []string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.groups == nil {
		n.groups = map[string][]string{}
	}
	n.groups[id] = instances
	return nil
}
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
		Placement: &dataprocpb.JobPlacement{ClusterName: "c1"}, TypeJob: &dataprocpb.Job_HiveJob{HiveJob: &dataprocpb.HiveJob{}}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("hive with no queries: %v", err)
	}
	// A Trino job needs the Trino component, which c1 does not enable.
	if _, err := s.SubmitJob(ctx, &dataprocpb.SubmitJobRequest{ProjectId: "p", Region: "us-central1", Job: &dataprocpb.Job{
		Placement: &dataprocpb.JobPlacement{ClusterName: "c1"}, TypeJob: &dataprocpb.Job_TrinoJob{TrinoJob: &dataprocpb.TrinoJob{
			Queries: &dataprocpb.TrinoJob_QueryList{QueryList: &dataprocpb.QueryList{Queries: []string{"SELECT 1"}}}}}}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("trino without the component: %v", err)
	}
	// A Hive job runs its query list, each query ended with ";", and its
	// script variables.
	hj, err := s.SubmitJob(ctx, &dataprocpb.SubmitJobRequest{ProjectId: "p", Region: "us-central1", Job: &dataprocpb.Job{
		Placement: &dataprocpb.JobPlacement{ClusterName: "c1"}, TypeJob: &dataprocpb.Job_HiveJob{HiveJob: &dataprocpb.HiveJob{
			ScriptVariables: map[string]string{"n": "3"},
			Queries:         &dataprocpb.HiveJob_QueryList{QueryList: &dataprocpb.QueryList{Queries: []string{"SHOW TABLES", "SELECT ${n};"}}}}}}})
	if err != nil {
		t.Fatalf("hive: %v", err)
	}
	waitJob(t, s, hj.GetReference().GetJobId())
	last := eng.clusters[0].jobs[len(eng.clusters[0].jobs)-1]
	if last.Kind != sparkproc.KindHive || string(last.Main.Data) != "SHOW TABLES;\nSELECT ${n};\n" || last.QueryVariables["n"] != "3" {
		t.Errorf("hive job %+v", last)
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
// parameters (or first), magics kept for IPython.
func TestNotebookScript(t *testing.T) {
	cells := func(script string) []map[string]string {
		t.Helper()
		i := strings.Index(script, "_cb_json.loads(")
		j := strings.Index(script[i:], "):\n")
		var lit string
		if i < 0 || j < 0 || json.Unmarshal([]byte(script[i+len("_cb_json.loads("):i+j]), &lit) != nil {
			t.Fatalf("no cells in %q", script)
		}
		var out []map[string]string
		if err := json.Unmarshal([]byte(lit), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	nb := `{"cells":[{"cell_type":"markdown","source":["# T"]},
	{"cell_type":"code","metadata":{"tags":["parameters"]},"source":["n = 1\n"]},
	{"cell_type":"code","metadata":{},"source":"%time print(n)"}]}`
	got, err := notebookScript([]byte(nb), map[string]string{"n": "5"})
	if err != nil {
		t.Fatal(err)
	}
	if c := cells(got); len(c) != 3 || c[0]["source"] != "n = 1\n" || c[1]["source"] != "# Parameters\nn = \"5\"\n" ||
		c[2]["label"] != "In[2]" || c[2]["source"] != "%time print(n)" {
		t.Errorf("cells %v", c)
	}
	if !strings.Contains(got, "InteractiveShell") {
		t.Errorf("script %q runs no IPython", got)
	}
	got, _ = notebookScript([]byte(`{"cells":[{"cell_type":"code","source":"print(n)"}]}`), map[string]string{"n": "5"})
	if c := cells(got); len(c) != 2 || c[0]["label"] != "injected parameters" || c[1]["source"] != "print(n)" {
		t.Errorf("untagged %v", c)
	}
}

func TestMachineShape(t *testing.T) {
	for uri, want := range map[string]string{
		"n4-standard-4": "4 16g", "https://www.googleapis.com/compute/v1/projects/p/zones/us-central1-a/machineTypes/n2-highmem-2": "2 16g",
		"e2-highcpu-8": "8 8g", "e2-medium": "0 ", "": "0 ",
	} {
		c, m := machineShape(uri)
		if got := fmt.Sprint(c) + " " + m; got != want {
			t.Errorf("%s: %q, want %q", uri, got, want)
		}
	}
}

// waitJob waits for the job id of project p to end.
func waitJob(t *testing.T, s *Server, id string) *dataprocpb.Job {
	t.Helper()
	var j *dataprocpb.Job
	wait(t, func() bool {
		var err error
		j, err = s.GetJob(context.Background(), &dataprocpb.GetJobRequest{ProjectId: "p", Region: "us-central1", JobId: id})
		return err == nil && j.GetDone()
	})
	return j
}

func TestCheckComponents(t *testing.T) {
	for _, c := range []struct {
		mm    string
		comps []dataprocpb.Component
		code  codes.Code
	}{
		{"2.2", []dataprocpb.Component{dataprocpb.Component_TRINO, dataprocpb.Component_JUPYTER, dataprocpb.Component_FLINK}, codes.OK},
		{"2.2", []dataprocpb.Component{dataprocpb.Component_PRESTO}, codes.InvalidArgument},
		{"2.3", []dataprocpb.Component{dataprocpb.Component_HBASE}, codes.InvalidArgument},
		{"2.1", []dataprocpb.Component{dataprocpb.Component_ICEBERG}, codes.InvalidArgument},
		{"2.2", []dataprocpb.Component{dataprocpb.Component_ZOOKEEPER, dataprocpb.Component_ZOOKEEPER}, codes.InvalidArgument},
		{"2.2", []dataprocpb.Component{dataprocpb.Component_DELTA, dataprocpb.Component_ICEBERG, dataprocpb.Component_HUDI}, codes.OK},
		{"2.1", []dataprocpb.Component{dataprocpb.Component_HUDI}, codes.OK},
		{"3.0", []dataprocpb.Component{dataprocpb.Component_ICEBERG, dataprocpb.Component_ZEPPELIN, dataprocpb.Component_DOCKER}, codes.OK},
		{"2.2", []dataprocpb.Component{dataprocpb.Component_DOCKER}, codes.OK},
		{"3.0", []dataprocpb.Component{dataprocpb.Component_RANGER, dataprocpb.Component_SOLR}, codes.Unimplemented},
		{"3.0", []dataprocpb.Component{dataprocpb.Component_DELTA, dataprocpb.Component_HUDI}, codes.OK},
	} {
		if err := checkComponents(c.mm, c.comps, false); c.code == codes.OK && hasDocker(c.comps) && status.Code(err) != codes.FailedPrecondition {
			t.Errorf("%s %v without the opt-in: %v, want FAILED_PRECONDITION", c.mm, c.comps, err)
		}
		if err := checkComponents(c.mm, c.comps, true); status.Code(err) != c.code {
			t.Errorf("%s %v: %v, want %v", c.mm, c.comps, err, c.code)
		}
	}
}

// TestDockerComponentNeedsOptIn (#2162): without `up
// --allow-unconfined-containers` a cluster with the DOCKER component is
// refused FAILED_PRECONDITION naming the flag, in Google's error shape;
// with it the cluster runs; a stopped DOCKER cluster is not started again
// by an instance without it.
func TestDockerComponentNeedsOptIn(t *testing.T) {
	s, _, _ := newClusterServer(t)
	ctx := context.Background()
	docker := func(name string) *dataprocpb.CreateClusterRequest {
		return &dataprocpb.CreateClusterRequest{ProjectId: "p", Region: "us-central1", Cluster: &dataprocpb.Cluster{ClusterName: name,
			Config: &dataprocpb.ClusterConfig{SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.3",
				OptionalComponents: []dataprocpb.Component{dataprocpb.Component_DOCKER}}}}}
	}
	_, err := s.CreateCluster(ctx, docker("off"))
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition || !strings.Contains(st.Message(), "cloudburrow up --allow-unconfined-containers") ||
		!strings.Contains(st.Message(), "seccomp, AppArmor") {
		t.Fatalf("create with DOCKER, no opt-in = %v; want FAILED_PRECONDITION naming the flag", err)
	}
	// The JSON API answers Google's error shape: 400, status FAILED_PRECONDITION.
	rec := httptest.NewRecorder()
	NewRESTHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/projects/p/regions/us-central1/clusters",
		strings.NewReader(`{"clusterName":"off","config":{"softwareConfig":{"imageVersion":"2.3","optionalComponents":["DOCKER"]}}}`)))
	var body struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusBadRequest || body.Error.Code != 400 ||
		body.Error.Status != "FAILED_PRECONDITION" || !strings.Contains(body.Error.Message, "--allow-unconfined-containers") {
		t.Errorf("REST create with DOCKER = %d %s; want Google's 400 FAILED_PRECONDITION", rec.Code, rec.Body)
	}
	if _, err := s.GetCluster(ctx, &dataprocpb.GetClusterRequest{ProjectId: "p", Region: "us-central1", ClusterName: "off"}); status.Code(err) != codes.NotFound {
		t.Errorf("the refused cluster exists: %v", err)
	}

	s.opts.AllowUnconfinedContainers = true
	op, err := s.CreateCluster(ctx, docker("on"))
	if err != nil {
		t.Fatal(err)
	}
	var c dataprocpb.Cluster
	if done := waitOp(t, s, op.Name); done.GetResponse().UnmarshalTo(&c) != nil || c.GetStatus().GetState() != dataprocpb.ClusterStatus_RUNNING {
		t.Fatalf("create with DOCKER, opted in: %v", done)
	}
	op, err = s.StopCluster(ctx, &dataprocpb.StopClusterRequest{ProjectId: "p", Region: "us-central1", ClusterName: "on"})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, s, op.Name)
	s.opts.AllowUnconfinedContainers = false
	if _, err := s.StartCluster(ctx, &dataprocpb.StartClusterRequest{ProjectId: "p", Region: "us-central1", ClusterName: "on"}); status.Code(err) != codes.FailedPrecondition ||
		!strings.Contains(err.Error(), "--allow-unconfined-containers") {
		t.Errorf("start a DOCKER cluster, no opt-in = %v; want FAILED_PRECONDITION", err)
	}
}

func hasDocker(comps []dataprocpb.Component) bool {
	for _, c := range comps {
		if c == dataprocpb.Component_DOCKER {
			return true
		}
	}
	return false
}

// TestCheckRanger (#2162): RANGER needs SOLR and the KMS-encrypted admin
// password's two properties, as the Ranger component page documents.
func TestCheckRanger(t *testing.T) {
	key := "projects/p/locations/global/keyRings/r/cryptoKeys/k"
	both := []dataprocpb.Component{dataprocpb.Component_SOLR, dataprocpb.Component_RANGER}
	for _, c := range []struct {
		comps []dataprocpb.Component
		props map[string]string
		want  string // "" is OK
	}{
		{[]dataprocpb.Component{dataprocpb.Component_SOLR}, nil, ""},
		{[]dataprocpb.Component{dataprocpb.Component_RANGER}, map[string]string{"dataproc:ranger.kms.key.uri": key, "dataproc:ranger.admin.password.uri": "gs://b/pw"}, "requires the installation of the Solr component"},
		{both, map[string]string{"dataproc:ranger.admin.password.uri": "gs://b/pw"}, "dataproc:ranger.kms.key.uri"},
		{both, map[string]string{"dataproc:ranger.kms.key.uri": "k", "dataproc:ranger.admin.password.uri": "gs://b/pw"}, "Invalid value"},
		{both, map[string]string{"dataproc:ranger.kms.key.uri": key}, "dataproc:ranger.admin.password.uri"},
		{both, map[string]string{"dataproc:ranger.kms.key.uri": key, "dataproc:ranger.admin.password.uri": "/tmp/pw"}, "gs://"},
		{both, map[string]string{"dataproc:ranger.kms.key.uri": key, "dataproc:ranger.admin.password.uri": "gs://b/pw"}, ""},
	} {
		err := checkRanger(&dataprocpb.SoftwareConfig{OptionalComponents: c.comps, Properties: c.props})
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%v %v: %v", c.comps, c.props, err)
		case c.want != "" && (status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%v %v: %v, want INVALID_ARGUMENT with %q", c.comps, c.props, err, c.want)
		}
	}
}

// TestRangerPassword (#2162): a RANGER cluster's admin password is read
// from dataproc:ranger.admin.password.uri, decrypted with
// dataproc:ranger.kms.key.uri and given to the engine; one that breaks the
// image version's rules, or a key that does not decrypt it, fails the
// cluster with the reason.
func TestRangerPassword(t *testing.T) {
	// RANGER is refused until its Admin image builds (unservedComponents);
	// the path it takes once served is tested here.
	servedComponents[dataprocpb.Component_RANGER] = true
	t.Cleanup(func() { delete(servedComponents, dataprocpb.Component_RANGER) })
	s, eng, st := newClusterServer(t)
	key := "projects/p/locations/global/keyRings/r/cryptoKeys/k"
	s.opts.Decrypt = func(_ context.Context, k string, ct []byte) ([]byte, error) {
		if k != key {
			return nil, fmt.Errorf("NOT_FOUND: %s", k)
		}
		return []byte(strings.TrimPrefix(string(ct), "enc:")), nil
	}
	st.objects["gs://b/good"] = []byte("enc:Ranger2026\n")
	st.objects["gs://b/weak"] = []byte("enc:ranger2026")
	ctx := context.Background()
	create := func(name, uri, k string) *dataprocpb.Cluster {
		t.Helper()
		op, err := s.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: "p", Region: "us-central1", Cluster: &dataprocpb.Cluster{ClusterName: name,
			Config: &dataprocpb.ClusterConfig{SoftwareConfig: &dataprocpb.SoftwareConfig{ImageVersion: "2.3",
				OptionalComponents: []dataprocpb.Component{dataprocpb.Component_SOLR, dataprocpb.Component_RANGER},
				Properties:         map[string]string{"dataproc:ranger.kms.key.uri": k, "dataproc:ranger.admin.password.uri": uri}}}}})
		if err != nil {
			t.Fatal(err)
		}
		waitOp(t, s, op.Name)
		c, err := s.GetCluster(ctx, &dataprocpb.GetClusterRequest{ProjectId: "p", Region: "us-central1", ClusterName: name})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := create("good", "gs://b/good", key); c.GetStatus().GetState() != dataprocpb.ClusterStatus_RUNNING {
		t.Fatalf("good: %v", c.GetStatus())
	}
	if got := eng.clusters[len(eng.clusters)-1].spec.RangerPassword; got != "Ranger2026" {
		t.Errorf("the engine got password %q", got)
	}
	for _, c := range []struct{ name, uri, key, want string }{
		{"weak", "gs://b/weak", key, "one uppercase letter"},
		{"nokey", "gs://b/good", key + "x", "NOT_FOUND"},
		{"noobject", "gs://b/none", key, "gs://b/none"},
	} {
		got := create(c.name, c.uri, c.key)
		if got.GetStatus().GetState() != dataprocpb.ClusterStatus_ERROR || !strings.Contains(got.GetStatus().GetDetail(), c.want) {
			t.Errorf("%s: %v; want ERROR with %q", c.name, got.GetStatus(), c.want)
		}
	}
	if msg := rangerPasswordRule("2.1", "ranger2026"); msg != "" {
		t.Errorf("2.1 lowercase and digits: %s", msg)
	}
	if msg := rangerPasswordRule("2.1", "rangerrr"); msg == "" {
		t.Errorf("2.1 no digit accepted")
	}
}
