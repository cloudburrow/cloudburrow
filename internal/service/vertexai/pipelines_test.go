package vertexai

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakePipelineRunner "runs" a container by acting out its args: an arg
// "write:PATH=TEXT" writes TEXT to PATH under the mounts, "fail" fails.
type fakePipelineRunner struct {
	fakeExecutor
	mu   sync.Mutex
	runs []PipelineContainer
	key  string
}

func (r *fakePipelineRunner) RunContainer(_ context.Context, c PipelineContainer) error {
	r.mu.Lock()
	r.runs = append(r.runs, c)
	r.mu.Unlock()
	for _, a := range append(append([]string{}, c.Command...), c.Args...) {
		if a == "fail" {
			return errors.New("exit status 1: boom")
		}
		if rest, ok := strings.CutPrefix(a, "write:"); ok {
			p, text, _ := strings.Cut(rest, "=")
			for mount, dir := range c.Mounts {
				if rel, ok := strings.CutPrefix(p, mount+"/"); ok {
					full := filepath.Join(dir, filepath.FromSlash(rel))
					_ = os.MkdirAll(filepath.Dir(full), 0o755)
					if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (r *fakePipelineRunner) ListObjects(_ context.Context, bucket, prefix string) ([]string, error) {
	r.fakeExecutor.mu.Lock()
	defer r.fakeExecutor.mu.Unlock()
	var out []string
	for k := range r.objects {
		if rest, ok := strings.CutPrefix(k, bucket+"/"); ok && strings.HasPrefix(rest, prefix) {
			out = append(out, rest)
		}
	}
	return out, nil
}

// testPipelineSpec is a KFP IR: produce writes an output parameter and an
// artifact; consume reads both, after it.
const testPipelineSpec = `{
 "pipelineInfo": {"name": "hello-pipeline"},
 "schemaVersion": "2.1.0", "sdkVersion": "kfp-2.7.0",
 "root": {
  "inputDefinitions": {"parameters": {"greeting": {"parameterType": "STRING"}, "times": {"parameterType": "NUMBER_INTEGER", "defaultValue": 2}}},
  "dag": {"tasks": {
   "produce": {"taskInfo": {"name": "produce"}, "componentRef": {"name": "comp-produce"},
     "inputs": {"parameters": {"msg": {"componentInputParameter": "greeting"}, "n": {"componentInputParameter": "times"}}}},
   "consume": {"taskInfo": {"name": "consume"}, "componentRef": {"name": "comp-consume"}, "dependentTasks": ["produce"],
     "inputs": {"parameters": {"word": {"taskOutputParameter": {"producerTask": "produce", "outputParameterKey": "word"}}, "suffix": {"runtimeValue": {"constant": "!"}}},
       "artifacts": {"data": {"taskOutputArtifact": {"producerTask": "produce", "outputArtifactKey": "data"}}}}}
  }}
 },
 "components": {
  "comp-produce": {"executorLabel": "exec-produce",
    "inputDefinitions": {"parameters": {"msg": {"parameterType": "STRING"}, "n": {"parameterType": "NUMBER_INTEGER"}}},
    "outputDefinitions": {"parameters": {"word": {"parameterType": "STRING"}}, "artifacts": {"data": {"artifactType": {"schemaTitle": "system.Dataset", "schemaVersion": "0.0.1"}}}}},
  "comp-consume": {"executorLabel": "exec-consume",
    "inputDefinitions": {"parameters": {"word": {"parameterType": "STRING"}, "suffix": {"parameterType": "STRING"}}, "artifacts": {"data": {"artifactType": {"schemaTitle": "system.Dataset"}}}},
    "outputDefinitions": {"parameters": {"n": {"parameterType": "NUMBER_INTEGER"}}}}
 },
 "deploymentSpec": {"executors": {
  "exec-produce": {"container": {"image": "local/tool:1", "command": ["sh", "-c"],
    "args": ["write:{{$.outputs.parameters['word'].output_file}}={{$.inputs.parameters['msg']}}x{{$.inputs.parameters['n']}}", "write:{{$.outputs.artifacts['data'].path}}=rows", "{{$.pipeline_job_name}}"]}},
  "exec-consume": {"container": {"image": "local/tool:1",
    "args": ["{{$.inputs.parameters['word']}}{{$.inputs.parameters['suffix']}}", "{{$.inputs.artifacts['data'].uri}}", "write:{{$.outputs.parameters['n'].output_file}}=7", "{{$}}"]}}
 }}
}`

func testSpec(t *testing.T, raw string) *structpb.Struct {
	t.Helper()
	s := &structpb.Struct{}
	if err := protojson.Unmarshal([]byte(raw), s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newPipelineServer(t *testing.T) (*Server, *fakePipelineRunner) {
	t.Helper()
	s := NewServer(nil)
	s.SetNotebookRuntimes(newFakeHost(), filepath.Join(t.TempDir(), "nb.json"))
	r := &fakePipelineRunner{fakeExecutor: fakeExecutor{objects: map[string][]byte{}, repos: map[string][]byte{}}}
	s.SetPipelineRunner(r)
	t.Cleanup(s.stopScheduler)
	return s, r
}

func waitPipeline(t *testing.T, p *pipelineService, name string) *aiplatformpb.PipelineJob {
	t.Helper()
	for i := 0; i < 500; i++ {
		j, err := p.GetPipelineJob(context.Background(), &aiplatformpb.GetPipelineJobRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if !pipelineRunning(j) {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pipeline %s did not end", name)
	return nil
}

func TestPipelineJobs(t *testing.T) {
	ctx := context.Background()
	s, r := newPipelineServer(t)
	p := &pipelineService{s: s}
	parent := "projects/p/locations/us-central1"
	spec := testSpec(t, testPipelineSpec)
	rc := func(params map[string]*structpb.Value) *aiplatformpb.PipelineJob_RuntimeConfig {
		return &aiplatformpb.PipelineJob_RuntimeConfig{GcsOutputDirectory: "gs://root/runs", ParameterValues: params}
	}
	for name, bad := range map[string]*aiplatformpb.PipelineJob{
		"no spec":           {RuntimeConfig: rc(nil)},
		"no root":           {PipelineSpec: spec},
		"required missing":  {PipelineSpec: spec, RuntimeConfig: rc(nil)},
		"unknown parameter": {PipelineSpec: spec, RuntimeConfig: rc(map[string]*structpb.Value{"greeting": structpb.NewStringValue("hi"), "x": structpb.NewStringValue("y")})},
		"template uri":      {TemplateUri: "https://us-central1-kfp.pkg.dev/p/r/t/v1", RuntimeConfig: rc(nil)},
		"loop": {PipelineSpec: testSpec(t, strings.Replace(testPipelineSpec, `"taskInfo": {"name": "produce"},`, `"taskInfo": {"name": "produce"}, "parameterIterator": {"items": {"raw": "[1]"}},`, 1)),
			RuntimeConfig: rc(map[string]*structpb.Value{"greeting": structpb.NewStringValue("hi")})},
	} {
		if _, err := p.CreatePipelineJob(ctx, &aiplatformpb.CreatePipelineJobRequest{Parent: parent, PipelineJob: bad}); codeOf(err) != codes.InvalidArgument {
			t.Errorf("create %s = %v, want INVALID_ARGUMENT", name, err)
		}
	}
	if _, err := p.CreatePipelineJob(ctx, &aiplatformpb.CreatePipelineJobRequest{Parent: parent, PipelineJobId: "Bad_ID",
		PipelineJob: &aiplatformpb.PipelineJob{PipelineSpec: spec, RuntimeConfig: rc(map[string]*structpb.Value{"greeting": structpb.NewStringValue("hi")})}}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("a bad pipeline_job_id = %v", err)
	}

	j, err := p.CreatePipelineJob(ctx, &aiplatformpb.CreatePipelineJobRequest{Parent: parent,
		PipelineJob: &aiplatformpb.PipelineJob{DisplayName: "hello", PipelineSpec: spec, Labels: map[string]string{"env": "test"},
			RuntimeConfig: rc(map[string]*structpb.Value{"greeting": structpb.NewStringValue("hi")})}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(j.GetName(), parent+"/pipelineJobs/hello-pipeline-") || j.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_PENDING {
		t.Errorf("created %s %v", j.GetName(), j.GetState())
	}
	done := waitPipeline(t, p, j.GetName())
	if done.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED {
		t.Fatalf("the pipeline = %v %v", done.GetState(), done.GetError())
	}
	tasks := map[string]*aiplatformpb.PipelineTaskDetail{}
	for _, d := range done.GetJobDetail().GetTaskDetails() {
		tasks[d.GetTaskName()] = d
	}
	if tasks["hello-pipeline"].GetState() != aiplatformpb.PipelineTaskDetail_SUCCEEDED || tasks["produce"].GetState() != aiplatformpb.PipelineTaskDetail_SUCCEEDED ||
		tasks["consume"].GetState() != aiplatformpb.PipelineTaskDetail_SUCCEEDED {
		t.Errorf("task details = %v", done.GetJobDetail())
	}
	art := tasks["produce"].GetOutputs()["data"].GetArtifacts()
	if len(art) != 1 || !strings.HasPrefix(art[0].GetUri(), "gs://root/runs/"+lastSegment(j.GetName())+"/produce_") {
		t.Errorf("produce's data artifact = %v", art)
	}
	if b, _ := r.ReadObject(ctx, "root", strings.TrimPrefix(art[0].GetUri(), "gs://root/"), 0); string(b) != "rows" {
		t.Errorf("the artifact's object = %q", b)
	}
	if len(r.runs) != 2 {
		t.Fatalf("containers run = %d", len(r.runs))
	}
	consume := r.runs[1]
	if consume.Args[0] != "hix2!" || consume.Args[1] != art[0].GetUri() || !strings.Contains(consume.Args[3], `"parameterValues"`) {
		t.Errorf("consume's args = %q", consume.Args)
	}
	if r.runs[0].Args[2] != lastSegment(j.GetName()) || r.runs[0].Image != "local/tool:1" {
		t.Errorf("produce's run = %+v", r.runs[0])
	}

	// The list: filter and default order.
	l, err := p.ListPipelineJobs(ctx, &aiplatformpb.ListPipelineJobsRequest{Parent: parent, Filter: `pipeline_name="hello-pipeline" AND labels.env="test"`})
	if err != nil || len(l.GetPipelineJobs()) != 1 {
		t.Errorf("filtered list = %v %v", l, err)
	}
	if l, _ := p.ListPipelineJobs(ctx, &aiplatformpb.ListPipelineJobsRequest{Parent: parent, Filter: `state="PIPELINE_STATE_FAILED"`}); len(l.GetPipelineJobs()) != 0 {
		t.Errorf("failed list = %v", l)
	}

	// A failing task fails the job; its dependents are skipped.
	failing := testSpec(t, strings.Replace(testPipelineSpec, `"command": ["sh", "-c"]`, `"command": ["fail"]`, 1))
	f, err := p.CreatePipelineJob(ctx, &aiplatformpb.CreatePipelineJobRequest{Parent: parent, PipelineJobId: "failing",
		PipelineJob: &aiplatformpb.PipelineJob{PipelineSpec: failing, RuntimeConfig: rc(map[string]*structpb.Value{"greeting": structpb.NewStringValue("hi")})}})
	if err != nil {
		t.Fatal(err)
	}
	fd := waitPipeline(t, p, f.GetName())
	if fd.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_FAILED || !strings.Contains(fd.GetError().GetMessage(), "produce") {
		t.Errorf("the failing pipeline = %v %v", fd.GetState(), fd.GetError())
	}
	for _, d := range fd.GetJobDetail().GetTaskDetails() {
		if d.GetTaskName() == "consume" && d.GetState() != aiplatformpb.PipelineTaskDetail_SKIPPED {
			t.Errorf("consume after a failed produce = %v", d.GetState())
		}
	}
	if _, err := p.CancelPipelineJob(ctx, &aiplatformpb.CancelPipelineJobRequest{Name: f.GetName()}); codeOf(err) != codes.FailedPrecondition {
		t.Errorf("cancel an ended job = %v", err)
	}
	op, err := p.BatchDeletePipelineJobs(ctx, &aiplatformpb.BatchDeletePipelineJobsRequest{Parent: parent, Names: []string{f.GetName(), j.GetName()}})
	if err != nil || !op.GetDone() {
		t.Fatalf("batch delete = %v %v", op, err)
	}
	if _, err := p.GetPipelineJob(ctx, &aiplatformpb.GetPipelineJobRequest{Name: j.GetName()}); codeOf(err) != codes.NotFound {
		t.Errorf("a deleted job = %v", err)
	}
}

func TestPipelineSchedule(t *testing.T) {
	ctx := context.Background()
	s, _ := newPipelineServer(t)
	sch := &scheduleService{s: s}
	p := &pipelineService{s: s}
	parent := "projects/p/locations/us-central1"
	job := &aiplatformpb.PipelineJob{PipelineSpec: testSpec(t, testPipelineSpec),
		RuntimeConfig: &aiplatformpb.PipelineJob_RuntimeConfig{GcsOutputDirectory: "gs://root", ParameterValues: map[string]*structpb.Value{"greeting": structpb.NewStringValue("hi")}}}
	if _, err := sch.CreateSchedule(ctx, &aiplatformpb.CreateScheduleRequest{Parent: parent, Schedule: &aiplatformpb.Schedule{DisplayName: "p",
		TimeSpecification: &aiplatformpb.Schedule_Cron{Cron: "* * * * *"}, MaxConcurrentRunCount: 1,
		Request: &aiplatformpb.Schedule_CreatePipelineJobRequest{CreatePipelineJobRequest: &aiplatformpb.CreatePipelineJobRequest{Parent: parent, PipelineJobId: "x", PipelineJob: job}}}}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("a template with pipeline_job_id = %v", err)
	}
	sc, err := sch.CreateSchedule(ctx, &aiplatformpb.CreateScheduleRequest{Parent: parent, Schedule: &aiplatformpb.Schedule{DisplayName: "nightly",
		TimeSpecification: &aiplatformpb.Schedule_Cron{Cron: "* * * * *"}, MaxConcurrentRunCount: 1, MaxRunCount: 1,
		Request: &aiplatformpb.Schedule_CreatePipelineJobRequest{CreatePipelineJobRequest: &aiplatformpb.CreatePipelineJobRequest{Parent: parent, PipelineJob: job}}}})
	if err != nil {
		t.Fatal(err)
	}
	// Run it now.
	st := s.notebooks()
	st.mu.Lock()
	st.schedules[sc.GetName()].NextRunTime = sc.GetCreateTime()
	st.mu.Unlock()
	s.runDueSchedules()
	got, _ := sch.GetSchedule(ctx, &aiplatformpb.GetScheduleRequest{Name: sc.GetName()})
	run := got.GetLastScheduledRunResponse().GetRunResponse()
	if !strings.HasPrefix(run, parent+"/pipelineJobs/") || got.GetStartedRunCount() != 1 || got.GetState() != aiplatformpb.Schedule_COMPLETED {
		t.Fatalf("after its run the schedule = %v", got)
	}
	j := waitPipeline(t, p, run)
	if j.GetScheduleName() != sc.GetName() || j.GetDisplayName() != "nightly" || j.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED {
		t.Errorf("the scheduled run = %s %q %v %v", j.GetScheduleName(), j.GetDisplayName(), j.GetState(), j.GetError())
	}
}

func TestScheduleActAs(t *testing.T) {
	ctx := context.Background()
	s, _ := newJobServer(t)
	sch := &scheduleService{s: s}
	var checked []string
	caller := "ana@example.com"
	s.SetIdentity(Identity{
		ActAs: func(_ context.Context, project, account string) error {
			checked = append(checked, project+" "+account)
			return errors.New("denied")
		},
		Caller: func(context.Context) string { return caller },
	})
	parent := "projects/p/locations/us-central1"
	mk := func(id isExecutionIdentity) *aiplatformpb.Schedule {
		j := &aiplatformpb.NotebookExecutionJob{DisplayName: "x", ExecutionSink: &aiplatformpb.NotebookExecutionJob_GcsOutputUri{GcsOutputUri: "gs://out"},
			NotebookSource: &aiplatformpb.NotebookExecutionJob_GcsNotebookSource_{GcsNotebookSource: &aiplatformpb.NotebookExecutionJob_GcsNotebookSource{Uri: "gs://in/nb/run.ipynb"}}}
		switch v := id.(type) {
		case *aiplatformpb.NotebookExecutionJob_ServiceAccount:
			j.ExecutionIdentity = v
		case *aiplatformpb.NotebookExecutionJob_ExecutionUser:
			j.ExecutionIdentity = v
		}
		return &aiplatformpb.Schedule{DisplayName: "s", TimeSpecification: &aiplatformpb.Schedule_Cron{Cron: "0 9 * * *"}, MaxConcurrentRunCount: 1,
			Request: &aiplatformpb.Schedule_CreateNotebookExecutionJobRequest{CreateNotebookExecutionJobRequest: &aiplatformpb.CreateNotebookExecutionJobRequest{Parent: parent, NotebookExecutionJob: j}}}
	}
	sa, err := sch.CreateSchedule(ctx, &aiplatformpb.CreateScheduleRequest{Parent: parent, Schedule: mk(&aiplatformpb.NotebookExecutionJob_ServiceAccount{ServiceAccount: "run@q.iam.gserviceaccount.com"})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sch.PauseSchedule(ctx, &aiplatformpb.PauseScheduleRequest{Name: sa.GetName()}); err == nil || len(checked) != 1 || checked[0] != "p run@q.iam.gserviceaccount.com" {
		t.Errorf("pause without actAs = %v (checked %v)", err, checked)
	}
	user, err := sch.CreateSchedule(ctx, &aiplatformpb.CreateScheduleRequest{Parent: parent, Schedule: mk(&aiplatformpb.NotebookExecutionJob_ExecutionUser{ExecutionUser: caller})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sch.PauseSchedule(ctx, &aiplatformpb.PauseScheduleRequest{Name: user.GetName()}); err != nil {
		t.Errorf("its own user's pause = %v", err)
	}
	caller = "bo@example.com"
	if _, err := sch.ResumeSchedule(ctx, &aiplatformpb.ResumeScheduleRequest{Name: user.GetName()}); codeOf(err) != codes.PermissionDenied {
		t.Errorf("another user's resume = %v, want PERMISSION_DENIED", err)
	}
}

// isExecutionIdentity is the execution identity oneof's interface.
type isExecutionIdentity interface{}

func TestJobRunsAsItsIdentity(t *testing.T) {
	ctx := context.Background()
	s, x := newJobServer(t)
	n := &notebookService{s: s}
	var minted []string
	s.SetIdentity(Identity{Caller: func(context.Context) string { return "ana@example.com" },
		Mint: func(account string) (string, error) {
			minted = append(minted, account)
			if account == "gone@p.iam.gserviceaccount.com" {
				return "", errors.New("Service account gone@p.iam.gserviceaccount.com does not exist")
			}
			return "tok-" + account, nil
		}})
	parent := "projects/p/locations/us-central1"
	mk := func(sa string) *aiplatformpb.NotebookExecutionJob {
		return &aiplatformpb.NotebookExecutionJob{DisplayName: "x", ExecutionSink: &aiplatformpb.NotebookExecutionJob_GcsOutputUri{GcsOutputUri: "gs://out"},
			NotebookSource:    &aiplatformpb.NotebookExecutionJob_GcsNotebookSource_{GcsNotebookSource: &aiplatformpb.NotebookExecutionJob_GcsNotebookSource{Uri: "gs://in/nb/run.ipynb"}},
			ExecutionIdentity: &aiplatformpb.NotebookExecutionJob_ServiceAccount{ServiceAccount: sa}}
	}
	op, err := n.CreateNotebookExecutionJob(ctx, &aiplatformpb.CreateNotebookExecutionJobRequest{Parent: parent, NotebookExecutionJob: mk("run@p.iam.gserviceaccount.com")})
	if err != nil {
		t.Fatal(err)
	}
	j := &aiplatformpb.NotebookExecutionJob{}
	_ = op.GetResponse().UnmarshalTo(j)
	if got := waitJob(t, n, j.GetName()); got.GetJobState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("the job = %v", got)
	}
	x.mu.Lock()
	runs := x.runs
	x.mu.Unlock()
	if len(minted) != 1 || minted[0] != "run@p.iam.gserviceaccount.com" || runs != 1 {
		t.Errorf("minted %v, runs %d", minted, runs)
	}
	op, err = n.CreateNotebookExecutionJob(ctx, &aiplatformpb.CreateNotebookExecutionJobRequest{Parent: parent, NotebookExecutionJob: mk("gone@p.iam.gserviceaccount.com")})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.GetResponse().UnmarshalTo(j)
	if got := waitJob(t, n, j.GetName()); got.GetJobState() != aiplatformpb.JobState_JOB_STATE_FAILED || !strings.Contains(got.GetStatus().GetMessage(), "does not exist") {
		t.Errorf("a missing service account's job = %v", got)
	}
}
