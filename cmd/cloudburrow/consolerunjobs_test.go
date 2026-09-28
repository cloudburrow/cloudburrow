package main

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// fakeRunJobs is a Cloud Run Jobs, Executions and Revisions API held in
// memory. Its operations are returned already done, except RunJob's, which
// carries the new execution as its metadata as the adapter's does.
type fakeRunJobs struct {
	runpb.UnimplementedJobsServer
	runpb.UnimplementedExecutionsServer
	runpb.UnimplementedRevisionsServer

	mu         sync.Mutex
	jobs       map[string]*runpb.Job
	executions map[string]*runpb.Execution
	order      []string // execution names, oldest first
	creates    []*runpb.CreateJobRequest
	updates    []*runpb.UpdateJobRequest
	runs       []string
	overrides  []*runpb.RunJobRequest_Overrides // each RunJob's, nil when it had none
	cancels    []string
	deletes    []string
	revisions  []string
	n          int
}

func newFakeRunJobs() *fakeRunJobs {
	return &fakeRunJobs{jobs: map[string]*runpb.Job{}, executions: map[string]*runpb.Execution{}}
}

func doneOp(name string, res proto.Message) (*longrunningpb.Operation, error) {
	a, err := anypb.New(res)
	if err != nil {
		return nil, err
	}
	return &longrunningpb.Operation{Name: "operations/" + name, Done: true,
		Result: &longrunningpb.Operation_Response{Response: a}}, nil
}

func (f *fakeRunJobs) CreateJob(_ context.Context, req *runpb.CreateJobRequest) (*longrunningpb.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, proto.Clone(req).(*runpb.CreateJobRequest))
	name := req.GetParent() + "/jobs/" + req.GetJobId()
	if _, ok := f.jobs[name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "job %s already exists", name)
	}
	job := proto.Clone(req.GetJob()).(*runpb.Job)
	job.Name, job.Generation, job.CreateTime = name, 1, timestamppb.Now()
	f.jobs[name] = job
	return doneOp("create", job)
}

func (f *fakeRunJobs) GetJob(_ context.Context, req *runpb.GetJobRequest) (*runpb.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "job %s not found", req.GetName())
	}
	return f.withExecutions(job), nil
}

// withExecutions fills a job's output-only execution fields.
func (f *fakeRunJobs) withExecutions(job *runpb.Job) *runpb.Job {
	out := proto.Clone(job).(*runpb.Job)
	for _, n := range f.order {
		e := f.executions[n]
		if e == nil || e.GetJob() != job.GetName() {
			continue
		}
		out.ExecutionCount++
		ref := &runpb.ExecutionReference{Name: n, CreateTime: e.GetCreateTime(),
			CompletionStatus: runpb.ExecutionReference_EXECUTION_PENDING}
		switch executionStatus(e) {
		case "Succeeded":
			ref.CompletionStatus = runpb.ExecutionReference_EXECUTION_SUCCEEDED
		case "Failed":
			ref.CompletionStatus = runpb.ExecutionReference_EXECUTION_FAILED
		case "Cancelled":
			ref.CompletionStatus = runpb.ExecutionReference_EXECUTION_CANCELLED
		case "Running":
			ref.CompletionStatus = runpb.ExecutionReference_EXECUTION_RUNNING
		}
		out.LatestCreatedExecution = ref
	}
	return out
}

func (f *fakeRunJobs) ListJobs(_ context.Context, req *runpb.ListJobsRequest) (*runpb.ListJobsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := &runpb.ListJobsResponse{}
	for name, job := range f.jobs {
		if strings.HasPrefix(name, req.GetParent()+"/jobs/") {
			resp.Jobs = append(resp.Jobs, f.withExecutions(job))
		}
	}
	return resp, nil
}

func (f *fakeRunJobs) UpdateJob(_ context.Context, req *runpb.UpdateJobRequest) (*longrunningpb.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, proto.Clone(req).(*runpb.UpdateJobRequest))
	prev, ok := f.jobs[req.GetJob().GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "job %s not found", req.GetJob().GetName())
	}
	job := proto.Clone(req.GetJob()).(*runpb.Job)
	job.Generation = prev.GetGeneration() + 1
	f.jobs[job.GetName()] = job
	return doneOp("update", job)
}

func (f *fakeRunJobs) DeleteJob(_ context.Context, req *runpb.DeleteJobRequest) (*longrunningpb.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "job %s not found", req.GetName())
	}
	delete(f.jobs, req.GetName())
	for n, e := range f.executions {
		if e.GetJob() == req.GetName() {
			delete(f.executions, n)
		}
	}
	return doneOp("delete", job)
}

func (f *fakeRunJobs) RunJob(_ context.Context, req *runpb.RunJobRequest) (*longrunningpb.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "job %s not found", req.GetName())
	}
	f.runs = append(f.runs, req.GetName())
	f.overrides = append(f.overrides, req.GetOverrides())
	f.n++
	name := fmt.Sprintf("%s/executions/%s-x%d", job.GetName(), lastSegment(job.GetName()), f.n)
	e := &runpb.Execution{Name: name, Job: job.GetName(), TaskCount: job.GetTemplate().GetTaskCount(),
		Parallelism: job.GetTemplate().GetTaskCount(), RunningCount: job.GetTemplate().GetTaskCount(),
		CreateTime: timestamppb.Now(), StartTime: timestamppb.Now(), Template: job.GetTemplate().GetTemplate(),
		Conditions: []*runpb.Condition{{Type: "Completed", State: runpb.Condition_CONDITION_PENDING}}}
	f.executions[name] = e
	f.order = append(f.order, name)
	md, err := anypb.New(e)
	if err != nil {
		return nil, err
	}
	return &longrunningpb.Operation{Name: "operations/run", Metadata: md}, nil
}

func (f *fakeRunJobs) GetExecution(_ context.Context, req *runpb.GetExecutionRequest) (*runpb.Execution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.executions[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "execution %s not found", req.GetName())
	}
	return proto.Clone(e).(*runpb.Execution), nil
}

func (f *fakeRunJobs) ListExecutions(_ context.Context, req *runpb.ListExecutionsRequest) (*runpb.ListExecutionsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.jobs[req.GetParent()]; !ok {
		return nil, status.Errorf(codes.NotFound, "job %s not found", req.GetParent())
	}
	resp := &runpb.ListExecutionsResponse{}
	for i := len(f.order) - 1; i >= 0; i-- {
		if e := f.executions[f.order[i]]; e != nil && e.GetJob() == req.GetParent() {
			resp.Executions = append(resp.Executions, proto.Clone(e).(*runpb.Execution))
		}
	}
	return resp, nil
}

func (f *fakeRunJobs) CancelExecution(_ context.Context, req *runpb.CancelExecutionRequest) (*longrunningpb.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.executions[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "execution %s not found", req.GetName())
	}
	if e.GetCompletionTime() != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "execution %s has already completed and cannot be cancelled", req.GetName())
	}
	f.cancels = append(f.cancels, req.GetName())
	e.CancelledCount, e.RunningCount, e.CompletionTime = e.GetTaskCount(), 0, timestamppb.Now()
	e.Conditions = []*runpb.Condition{{Type: "Completed", State: runpb.Condition_CONDITION_FAILED,
		Reasons: &runpb.Condition_ExecutionReason_{ExecutionReason: runpb.Condition_CANCELLED},
		Message: "The execution was cancelled."}}
	return doneOp("cancel", e)
}

func (f *fakeRunJobs) DeleteExecution(_ context.Context, req *runpb.DeleteExecutionRequest) (*longrunningpb.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.executions[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "execution %s not found", req.GetName())
	}
	f.deletes = append(f.deletes, req.GetName())
	delete(f.executions, req.GetName())
	return doneOp("delete-execution", e)
}

func (f *fakeRunJobs) DeleteRevision(_ context.Context, req *runpb.DeleteRevisionRequest) (*longrunningpb.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revisions = append(f.revisions, req.GetName())
	return doneOp("delete-revision", &runpb.Revision{Name: req.GetName()})
}

// serve starts the fake on a loopback port and returns its address.
func (f *fakeRunJobs) serve(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	runpb.RegisterJobsServer(g, f)
	runpb.RegisterExecutionsServer(g, f)
	runpb.RegisterRevisionsServer(g, f)
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	return ln.Addr().String()
}

const jobsParent = "projects/demo/locations/us-central1"

func jobsProviderFor(addr string) runJobsProvider {
	return runJobsProvider{runAddr: fixedAddr(addr), defaultProject: "demo", region: "us-central1",
		kubeconfig: "/instance/kubeconfig", namespace: "default"}
}

func actionIDs(actions []console.Action) []string {
	var out []string
	for _, a := range actions {
		out = append(out, a.ID)
	}
	return out
}

// TestRunJobsCreateExecuteCancelAndDeleteThroughTheAPI: the Jobs page's
// create form becomes one CreateJob carrying every field it maps; the job is
// listed with Execute; Execute is one RunJob; the execution is on the job's
// page with Cancel while it runs, and Cancel is one CancelExecution, after
// which it offers Delete and not Cancel; Delete is DeleteExecution, and the
// row's Delete is DeleteJob.
func TestRunJobsCreateExecuteCancelAndDeleteThroughTheAPI(t *testing.T) {
	ctx := context.Background()
	f := newFakeRunJobs()
	p := jobsProviderFor(f.serve(t))
	useFakeKube(t, `{"items":[]}`)

	name, err := p.Create(ctx, "demo", map[string]string{
		"name": "migrate", "image": "docker.io/library/busybox:1.36",
		"command": "sh -c", "args": `'echo "task $CLOUD_RUN_TASK_INDEX"; exit 0'`,
		"env": `{"B":"2","A":"1"}`, "taskCount": "3", "parallelism": "2", "maxRetries": "0",
		"timeout": "90", "cpu": "500m", "memory": "256Mi",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if name != jobsParent+"/jobs/migrate" || len(f.creates) != 1 {
		t.Fatalf("Create = %q after %d CreateJob calls", name, len(f.creates))
	}
	req := f.creates[0]
	et := req.GetJob().GetTemplate()
	c := et.GetTemplate().GetContainers()[0]
	if req.GetParent() != jobsParent || req.GetJobId() != "migrate" {
		t.Errorf("CreateJob parent %q id %q", req.GetParent(), req.GetJobId())
	}
	if et.GetTaskCount() != 3 || et.GetParallelism() != 2 || et.GetTemplate().GetMaxRetries() != 0 ||
		et.GetTemplate().GetRetries() == nil || et.GetTemplate().GetTimeout().AsDuration() != 90*time.Second {
		t.Errorf("execution template = %v", et)
	}
	if c.GetImage() != "docker.io/library/busybox:1.36" ||
		!reflect.DeepEqual(c.GetCommand(), []string{"sh", "-c"}) ||
		!reflect.DeepEqual(c.GetArgs(), []string{`echo "task $CLOUD_RUN_TASK_INDEX"; exit 0`}) {
		t.Errorf("container = %v", c)
	}
	if len(c.GetEnv()) != 2 || c.GetEnv()[0].GetName() != "A" || c.GetEnv()[1].GetValue() != "2" {
		t.Errorf("env = %v, want A=1 then B=2", c.GetEnv())
	}
	if l := c.GetResources().GetLimits(); l["cpu"] != "500m" || l["memory"] != "256Mi" {
		t.Errorf("limits = %v", l)
	}

	list, err := p.List(ctx, "demo")
	if err != nil || list.Unavailable != "" || len(list.Items) != 1 {
		t.Fatalf("List = %+v, %v", list, err)
	}
	row := list.Items[0]
	if row.Name != "migrate" || row.Status != "Not executed" || !reflect.DeepEqual(actionIDs(row.Actions), []string{"execute"}) {
		t.Errorf("row = %+v", row)
	}

	if err := p.Act(ctx, "demo", "migrate", "execute"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(f.runs) != 1 || f.runs[0] != name {
		t.Fatalf("RunJob calls = %v", f.runs)
	}
	d, err := p.Detail(ctx, "demo", []string{"migrate"})
	if err != nil || d.Unavailable != "" {
		t.Fatalf("job detail = %+v, %v", d, err)
	}
	if d.Edit == nil || d.Edit.Label != "Edit job" {
		t.Errorf("the job page offers edit %+v", d.Edit)
	}
	var execs console.Listing
	for _, s := range d.Sections {
		if s.ID == "executions" {
			execs = s.Listing
		}
	}
	if len(execs.Items) != 1 || !execs.RowsOpenable || execs.Items[0].Status != "Running" ||
		execs.Items[0].Fields["Tasks"] != "0/3 completed" {
		t.Fatalf("executions = %+v", execs)
	}
	exec := execs.Items[0].Name
	if got := actionIDs(execs.Items[0].Actions); !reflect.DeepEqual(got, []string{"cancel"}) {
		t.Errorf("a running execution's row offers %v, want cancel", got)
	}
	path := []string{"migrate", exec}
	if got := actionIDs(p.DetailActions(ctx, "demo", path)); !reflect.DeepEqual(got, []string{"cancel"}) {
		t.Errorf("a running execution's page offers %v, want cancel", got)
	}

	if err := p.ActAt(ctx, "demo", path, "cancel", nil); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if len(f.cancels) != 1 || f.cancels[0] != name+"/executions/"+exec {
		t.Errorf("CancelExecution calls = %v", f.cancels)
	}
	after := p.DetailActions(ctx, "demo", path)
	if got := actionIDs(after); !reflect.DeepEqual(got, []string{"delete"}) || !after[0].Destructive {
		t.Errorf("a cancelled execution offers %+v, want a destructive delete and no cancel", after)
	}
	ed, _ := p.Detail(ctx, "demo", path)
	if ed.Summary[0].Value != "Cancelled" {
		t.Errorf("the cancelled execution's status reads %q", ed.Summary[0].Value)
	}
	// The API refuses a second cancel, in its own words.
	if err := p.ActAt(ctx, "demo", path, "cancel", nil); err == nil || !strings.Contains(apiMessage(err), "FailedPrecondition") {
		t.Errorf("a second cancel = %v, want the API's FailedPrecondition", err)
	}

	if err := p.ActAt(ctx, "demo", path, "delete", nil); err != nil {
		t.Fatalf("Delete execution: %v", err)
	}
	if len(f.deletes) != 1 {
		t.Errorf("DeleteExecution calls = %v", f.deletes)
	}
	if err := p.Delete(ctx, "demo", "migrate"); err != nil {
		t.Fatalf("Delete job: %v", err)
	}
	if list, _ := p.List(ctx, "demo"); len(list.Items) != 0 {
		t.Errorf("the deleted job is still listed: %+v", list.Items)
	}
}

// TestRunJobEditKeepsWhatTheFormDoesNotShow: the edit form is the create form
// prefilled from the job, with the name shown and refused, and its labels and
// secret-backed variable prefilled (#852); saving it sends UpdateJob with the
// form's fields replaced and the job's working directory kept.
func TestRunJobEditKeepsWhatTheFormDoesNotShow(t *testing.T) {
	ctx := context.Background()
	f := newFakeRunJobs()
	p := jobsProviderFor(f.serve(t))
	useFakeKube(t, `{"items":[]}`)
	name := jobsParent + "/jobs/nightly"
	f.jobs[name] = &runpb.Job{Name: name, Labels: map[string]string{"team": "data"},
		Template: &runpb.ExecutionTemplate{TaskCount: 2, Template: &runpb.TaskTemplate{
			Retries: &runpb.TaskTemplate_MaxRetries{MaxRetries: 1}, Timeout: durationpb.New(5 * time.Minute),
			Containers: []*runpb.Container{{
				Image: "example.com/etl:v1", WorkingDir: "/work", Command: []string{"/bin/etl"},
				Args: []string{"--since", "one day"},
				Env: []*runpb.EnvVar{
					{Name: "MODE", Values: &runpb.EnvVar_Value{Value: "full"}},
					{Name: "TOKEN", Values: &runpb.EnvVar_ValueSource{ValueSource: &runpb.EnvVarSource{
						SecretKeyRef: &runpb.SecretKeySelector{Secret: "etl-token", Version: "latest"}}}},
				},
			}},
		}}}

	d, err := p.Detail(ctx, "demo", []string{"nightly"})
	if err != nil || d.Edit == nil {
		t.Fatalf("detail = %+v, %v", d, err)
	}
	values := map[string]string{}
	for _, fld := range d.Edit.Fields {
		if fld.Name == "name" {
			if !fld.Immutable || fld.Default != "nightly" {
				t.Errorf("name field = %+v", fld)
			}
			continue
		}
		values[fld.Name] = fld.Default
	}
	if values["args"] != "--since 'one day'" || values["taskCount"] != "2" || values["maxRetries"] != "1" ||
		values["timeout"] != "300" || values["env"] != `{"MODE":"full"}` {
		t.Errorf("prefilled = %v", values)
	}
	// Since #852 the form holds the labels and the secret-backed variable,
	// prefilled, rather than keeping them unseen.
	if values["secretEnv"] != `{"TOKEN":"etl-token:latest"}` || values["labels"] != `{"team":"data"}` {
		t.Errorf("secret-backed variables %q, labels %q; want TOKEN and team prefilled", values["secretEnv"], values["labels"])
	}

	values["image"], values["taskCount"], values["env"] = "example.com/etl:v2", "4", `{"MODE":"delta"}`
	if err := p.Edit(ctx, "demo", []string{"nightly"}, values); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if len(f.updates) != 1 {
		t.Fatalf("UpdateJob calls = %d", len(f.updates))
	}
	job := f.updates[0].GetJob()
	c := job.GetTemplate().GetTemplate().GetContainers()[0]
	if c.GetImage() != "example.com/etl:v2" || job.GetTemplate().GetTaskCount() != 4 || c.GetWorkingDir() != "/work" ||
		job.GetLabels()["team"] != "data" || !reflect.DeepEqual(c.GetArgs(), []string{"--since", "one day"}) {
		t.Errorf("updated job = %v", job)
	}
	var names []string
	for _, e := range c.GetEnv() {
		names = append(names, e.GetName()+"="+e.GetValue()+e.GetValueSource().GetSecretKeyRef().GetSecret())
	}
	if !reflect.DeepEqual(names, []string{"MODE=delta", "TOKEN=etl-token"}) {
		t.Errorf("env after edit = %v", names)
	}

	values["name"] = "renamed"
	if err := p.Edit(ctx, "demo", []string{"nightly"}, values); err == nil || err.Error() != runJobNameImmutable {
		t.Errorf("a rename = %v, want %q", err, runJobNameImmutable)
	}
}

// routedKube answers kubectl by the verb it was given.
type routedKube struct {
	mu    sync.Mutex
	calls []string
	pods  string
	logs  map[string]string
}

func (k *routedKube) Run(_ context.Context, _ string, args ...string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, strings.Join(args, " "))
	for i, a := range args {
		switch a {
		case "get":
			return k.pods, nil
		case "logs":
			return k.logs[args[i+1]], nil
		}
	}
	return "", fmt.Errorf("unexpected kubectl %v", args)
}

// TestRunJobExecutionPageShowsTheFailureAndItsTasksLogs: a failed
// execution's page says Failed with the pod's own message, lists its
// conditions, says the task list is not offered and why, and its Logs tab is
// each task pod's output, read with kubectl logs.
func TestRunJobExecutionPageShowsTheFailureAndItsTasksLogs(t *testing.T) {
	ctx := context.Background()
	f := newFakeRunJobs()
	p := jobsProviderFor(f.serve(t))
	k := &routedKube{
		pods: `{"items":[
		  {"metadata":{"name":"boom-abcde-0-x1","creationTimestamp":"2026-01-01T00:00:01Z",
		     "annotations":{"batch.kubernetes.io/job-completion-index":"0"}},"status":{"phase":"Failed"}}]}`,
		logs: map[string]string{"boom-abcde-0-x1": "starting\nboom\n"},
	}
	was := kubeInvoker
	kubeInvoker = k
	t.Cleanup(func() { kubeInvoker = was })

	job := jobsParent + "/jobs/boom"
	f.jobs[job] = &runpb.Job{Name: job, Template: &runpb.ExecutionTemplate{TaskCount: 1,
		Template: &runpb.TaskTemplate{Containers: []*runpb.Container{{Image: "busybox:1.36"}}}}}
	exec := job + "/executions/boom-abcde"
	const msg = "Task 0 failed: container boom exited with code 3: boom"
	f.executions[exec] = &runpb.Execution{Name: exec, Job: job, TaskCount: 1, FailedCount: 1,
		CreateTime: timestamppb.Now(), CompletionTime: timestamppb.Now(),
		Conditions: []*runpb.Condition{
			{Type: "Completed", State: runpb.Condition_CONDITION_FAILED, Message: msg,
				Reasons: &runpb.Condition_ExecutionReason_{ExecutionReason: runpb.Condition_NON_ZERO_EXIT_CODE}},
			{Type: "Started", State: runpb.Condition_CONDITION_SUCCEEDED},
		}}
	f.order = append(f.order, exec)

	d, err := p.Detail(ctx, "demo", []string{"boom", "boom-abcde"})
	if err != nil || d.Unavailable != "" {
		t.Fatalf("execution detail = %+v, %v", d, err)
	}
	props := map[string]string{}
	for _, pr := range d.Summary {
		props[pr.Label] = pr.Value
	}
	if props["Status"] != "Failed" || props["Failure"] != msg || props["Failed"] != "1" {
		t.Errorf("summary = %v", props)
	}
	if !strings.HasPrefix(props["Task list"], "Not offered:") {
		t.Errorf("the task list is %q, want it said to be not offered", props["Task list"])
	}
	sections := map[string]console.Section{}
	for _, s := range d.Sections {
		sections[s.ID] = s
	}
	conds := sections["conditions"].Listing.Items
	if len(conds) != 2 || conds[0].Status != "Failed" || conds[0].Fields["Reason"] != "NON_ZERO_EXIT_CODE" {
		t.Errorf("conditions = %+v", conds)
	}
	logs := sections["logs"]
	if logs.Kind != console.KindText || !strings.Contains(logs.Text, "Task 0 · pod boom-abcde-0-x1 · Failed") ||
		!strings.Contains(logs.Text, "boom") {
		t.Errorf("logs tab = %+v", logs)
	}
	if !strings.Contains(strings.Join(k.calls, "\n"), "-l batch.kubernetes.io/job-name=boom-abcde -n default") {
		t.Errorf("the pods were not selected by the execution's batch Job: %v", k.calls)
	}
	if got := actionIDs(p.DetailActions(ctx, "demo", []string{"boom", "boom-abcde"})); !reflect.DeepEqual(got, []string{"delete"}) {
		t.Errorf("a failed execution offers %v, want delete only", got)
	}
}

// TestRunJobsWithNoAdapterSayWhy: with the adapter not running the screen is
// unavailable with the reason, rather than empty.
func TestRunJobsWithNoAdapterSayWhy(t *testing.T) {
	list, err := (runJobsProvider{defaultProject: "demo"}).List(context.Background(), "")
	if err != nil || !strings.Contains(list.Unavailable, "not running") {
		t.Errorf("List with no adapter = %+v, %v", list, err)
	}
}

func TestShellSplitAndJoinRoundTrip(t *testing.T) {
	for in, want := range map[string][]string{
		"":                     nil,
		"sh -c":                {"sh", "-c"},
		`'echo "a b"; exit 3'`: {`echo "a b"; exit 3`},
		`"x \" y" z\ w`:        {`x " y`, "z w"},
		`'it'\''s'`:            {"it's"},
		"  --flag=1   two  ":   {"--flag=1", "two"},
		`a""b ''`:              {"ab", ""},
	} {
		got, err := shellSplit(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("shellSplit(%q) = %q, %v; want %q", in, got, err, want)
		}
		back, err := shellSplit(shellJoin(want))
		if err != nil || !reflect.DeepEqual(back, want) {
			t.Errorf("shellJoin(%q) = %q, which splits to %q", want, shellJoin(want), back)
		}
	}
	for _, bad := range []string{`'open`, `"open`, `trailing\`} {
		if _, err := shellSplit(bad); err == nil {
			t.Errorf("shellSplit(%q) accepted an unterminated quote or escape", bad)
		}
	}
}

// TestRunRevisionDeleteIsOfferedOnlyWhereTheAPIAccepts: a revision that
// serves no traffic, of a service CloudBurrow created, offers Delete
// revision on its row and its page, and it is one DeleteRevision; the
// serving revision, and every revision of a service CloudBurrow does not own,
// offer nothing.
func TestRunRevisionDeleteIsOfferedOnlyWhereTheAPIAccepts(t *testing.T) {
	ctx := context.Background()
	f := newFakeRunJobs()
	addr := f.serve(t)
	useFakeKube(t, `{"items":[
	  {"metadata":{"name":"api","labels":{"cloudburrow.dev/owned":"true"}},
	   "status":{"latestReadyRevisionName":"api-00002",
	     "traffic":[{"revisionName":"api-00002","percent":100,"latestRevision":true}]}},
	  {"metadata":{"name":"foreign"},
	   "status":{"latestReadyRevisionName":"foreign-00002",
	     "traffic":[{"revisionName":"foreign-00002","percent":100}]}}]}`)
	p := runProvider{kubeconfig: "/instance/kubeconfig", namespace: "default", runAddr: fixedAddr(addr),
		defaultProject: "demo", region: "us-central1"}

	for _, c := range []struct {
		path []string
		want []string
	}{
		{[]string{"api", "api-00001"}, []string{runDeleteRevision}},
		{[]string{"api", "api-00002"}, nil},
		{[]string{"foreign", "foreign-00001"}, nil},
		{[]string{"api"}, nil},
	} {
		if got := actionIDs(p.DetailActions(ctx, "demo", c.path)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v offers %v, want %v", c.path, got, c.want)
		}
	}
	svc, err := p.service(ctx, "api")
	if err != nil {
		t.Fatal(err)
	}
	if a := p.revisionActions(svc, "api-00001"); len(a) != 1 || !a[0].Destructive || a[0].Label != "Delete revision" {
		t.Errorf("the row action = %+v, want a destructive Delete revision", a)
	}

	if err := p.ActAt(ctx, "demo", []string{"api", "api-00001"}, runDeleteRevision, nil); err != nil {
		t.Fatalf("Delete revision: %v", err)
	}
	if want := jobsParent + "/services/api/revisions/api-00001"; len(f.revisions) != 1 || f.revisions[0] != want {
		t.Errorf("DeleteRevision calls = %v, want %s", f.revisions, want)
	}
	// With no adapter, nothing is offered: the delete could only fail.
	if got := (runProvider{kubeconfig: "/k"}).DetailActions(ctx, "demo", []string{"api", "api-00001"}); got != nil {
		t.Errorf("with no adapter a revision offers %v", got)
	}
}
