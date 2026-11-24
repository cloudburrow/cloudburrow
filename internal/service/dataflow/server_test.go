package dataflow

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeEngine runs a job until it is drained (DONE, which the server
// reports as DRAINED) or cancelled; a batch job ends at once.
type fakeEngine struct{ runs chan Run }

func (e *fakeEngine) Run(ctx context.Context, run Run, r Reporter) Outcome {
	if e.runs != nil {
		e.runs <- run
	}
	r.Graph([]Transform{{ID: "a", Name: "Read", Kind: "Read", Outputs: []string{"pc"}, Stage: "s1"},
		{ID: "b", Name: "Count", Kind: "ParDo", Inputs: []string{"pc"}, Stage: "s1"}})
	r.Started(2)
	r.Metrics([]Metric{{Name: "ElementCount", Origin: "dataflow/v1b3", Step: "Read", PCollection: "pc", Kind: "Sum", Value: 5},
		{Name: "seen", Origin: "user", Step: "Count", Namespace: "ns", Kind: "Sum", Value: 5}})
	if !run.Streaming {
		return Outcome{State: dataflowpb.JobState_JOB_STATE_DONE}
	}
	select {
	case <-run.Drain:
		return Outcome{State: dataflowpb.JobState_JOB_STATE_DONE}
	case <-ctx.Done():
		return Outcome{State: dataflowpb.JobState_JOB_STATE_CANCELLED}
	}
}

type memStorage map[string]string

func (m memStorage) Read(_ context.Context, uri string) ([]byte, error) {
	if b, ok := m[uri]; ok {
		return []byte(b), nil
	}
	return nil, errors.New("no such object")
}

func newTestServer(t *testing.T, opts Options) *Server {
	t.Helper()
	if opts.Engine == nil {
		opts.Engine = &fakeEngine{}
	}
	s, err := NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func job(name string, streaming bool) *dataflowpb.Job {
	t := dataflowpb.JobType_JOB_TYPE_BATCH
	if streaming {
		t = dataflowpb.JobType_JOB_TYPE_STREAMING
	}
	opts, _ := structpb.NewStruct(map[string]any{"options": map[string]any{"pipelineUrl": "gs://b/p.pb", "streaming": streaming}})
	return &dataflowpb.Job{Name: name, Type: t, Environment: &dataflowpb.Environment{SdkPipelineOptions: opts,
		UserAgent: mustStruct(map[string]any{"name": "Apache Beam Python 3.12 SDK", "version": "2.78.0"})}}
}

func mustStruct(m map[string]any) *structpb.Struct {
	s, err := structpb.NewStruct(m)
	if err != nil {
		panic(err)
	}
	return s
}

func waitState(t *testing.T, s *Server, id string, want dataflowpb.JobState) *dataflowpb.Job {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; {
		j, err := s.GetJob(context.Background(), &dataflowpb.GetJobRequest{ProjectId: "p", Location: DefaultLocation, JobId: id, View: dataflowpb.JobView_JOB_VIEW_ALL})
		if err != nil {
			t.Fatal(err)
		}
		if j.CurrentState == want {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s is %s; want %s", id, j.CurrentState, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCreateValidatesAndRuns(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t, Options{})
	for _, name := range []string{"", "Upper", "ends-", "9lead", strings.Repeat("a", 1025)} {
		if _, err := s.CreateJob(ctx, &dataflowpb.CreateJobRequest{ProjectId: "p", Job: &dataflowpb.Job{Name: name}}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateJob(%q) = %v; want INVALID_ARGUMENT", name, err)
		}
	}
	j, err := s.CreateJob(ctx, &dataflowpb.CreateJobRequest{ProjectId: "p", Job: job("words", false)})
	if err != nil {
		t.Fatal(err)
	}
	if j.Location != DefaultLocation || j.CurrentState != dataflowpb.JobState_JOB_STATE_UNKNOWN || !strings.Contains(j.Id, "_") {
		t.Errorf("CreateJob = %v; want the new job in %s before it has a state", j, DefaultLocation)
	}
	done := waitState(t, s, j.Id, dataflowpb.JobState_JOB_STATE_DONE)
	if done.GetJobMetadata().GetSdkVersion().GetVersion() != "2.78.0" || len(done.GetPipelineDescription().GetOriginalPipelineTransform()) != 2 {
		t.Errorf("the finished job is %v", done)
	}
	m, err := s.GetJobMetrics(ctx, &dataflowpb.GetJobMetricsRequest{ProjectId: "p", Location: DefaultLocation, JobId: j.Id})
	if err != nil {
		t.Fatal(err)
	}
	// ElementCount once; the user counter committed and tentative.
	if len(m.Metrics) != 3 {
		t.Errorf("GetJobMetrics = %v; want 3 updates", m.Metrics)
	}
	msgs, err := s.ListJobMessages(ctx, &dataflowpb.ListJobMessagesRequest{ProjectId: "p", Location: DefaultLocation, JobId: j.Id,
		MinimumImportance: dataflowpb.JobMessageImportance_JOB_MESSAGE_BASIC})
	if err != nil || len(msgs.JobMessages) == 0 || len(msgs.AutoscalingEvents) == 0 {
		t.Errorf("ListJobMessages = %v, %v", msgs, err)
	}
	if _, err := s.UpdateJob(ctx, &dataflowpb.UpdateJobRequest{ProjectId: "p", Location: DefaultLocation, JobId: j.Id,
		Job: &dataflowpb.Job{RequestedState: dataflowpb.JobState_JOB_STATE_CANCELLED}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("cancel a DONE job = %v; want FAILED_PRECONDITION", err)
	}
	if _, err := s.GetJob(ctx, &dataflowpb.GetJobRequest{ProjectId: "p", Location: "europe-west1", JobId: j.Id}); status.Code(err) != codes.NotFound {
		t.Errorf("GetJob in another region = %v; want NOT_FOUND", err)
	}
	noURL := &dataflowpb.Job{Name: "no-url"}
	f, _ := s.CreateJob(ctx, &dataflowpb.CreateJobRequest{ProjectId: "p", Job: noURL})
	waitState(t, s, f.Id, dataflowpb.JobState_JOB_STATE_FAILED)
}

func TestStreamingDrainCancelAndUpdate(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t, Options{})
	a, err := s.CreateJob(ctx, &dataflowpb.CreateJobRequest{ProjectId: "p", Job: job("stream", true)})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, s, a.Id, dataflowpb.JobState_JOB_STATE_RUNNING)
	// A second active job of the name answers the first.
	dup, err := s.CreateJob(ctx, &dataflowpb.CreateJobRequest{ProjectId: "p", Job: job("stream", true)})
	if err != nil || dup.Id != a.Id {
		t.Errorf("a second CreateJob of a running name = %v, %v; want the running job", dup, err)
	}
	if act, _ := s.CheckActiveJobs(ctx, &dataflowpb.CheckActiveJobsRequest{ProjectId: "p"}); !act.GetActiveJobsExist() {
		t.Error("CheckActiveJobs says no job is active")
	}
	// Runtime-updatable parameters.
	if _, err := s.UpdateJob(ctx, &dataflowpb.UpdateJobRequest{ProjectId: "p", Location: DefaultLocation, JobId: a.Id,
		Job:        &dataflowpb.Job{RuntimeUpdatableParams: &dataflowpb.RuntimeUpdatableParams{MaxNumWorkers: ptr(int32(4))}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"runtime_updatable_params.max_num_workers"}}}); err != nil {
		t.Errorf("update max_num_workers: %v", err)
	}
	// An update replaces the job: the old one ends UPDATED.
	repl := job("stream", true)
	b, err := s.CreateJob(ctx, &dataflowpb.CreateJobRequest{ProjectId: "p", Job: repl, ReplaceJobId: a.Id})
	if err != nil {
		t.Fatal(err)
	}
	old := waitState(t, s, a.Id, dataflowpb.JobState_JOB_STATE_UPDATED)
	if old.ReplacedByJobId != b.Id {
		t.Errorf("the replaced job's replacedByJobId = %q; want %q", old.ReplacedByJobId, b.Id)
	}
	waitState(t, s, b.Id, dataflowpb.JobState_JOB_STATE_RUNNING)
	if _, err := s.UpdateJob(ctx, &dataflowpb.UpdateJobRequest{ProjectId: "p", Location: DefaultLocation, JobId: b.Id,
		Job: &dataflowpb.Job{RequestedState: dataflowpb.JobState_JOB_STATE_DRAINED}}); err != nil {
		t.Fatal(err)
	}
	waitState(t, s, b.Id, dataflowpb.JobState_JOB_STATE_DRAINED)
	// Terraform's provider asks for JOB_STATE_DRAINING.
	e, _ := s.CreateJob(ctx, &dataflowpb.CreateJobRequest{ProjectId: "p", Job: job("tf", true)})
	waitState(t, s, e.Id, dataflowpb.JobState_JOB_STATE_RUNNING)
	if _, err := s.UpdateJob(ctx, &dataflowpb.UpdateJobRequest{ProjectId: "p", Location: DefaultLocation, JobId: e.Id,
		Job: &dataflowpb.Job{RequestedState: dataflowpb.JobState_JOB_STATE_DRAINING}}); err != nil {
		t.Fatal(err)
	}
	waitState(t, s, e.Id, dataflowpb.JobState_JOB_STATE_DRAINED)

	c, _ := s.CreateJob(ctx, &dataflowpb.CreateJobRequest{ProjectId: "p", Job: job("other", true)})
	waitState(t, s, c.Id, dataflowpb.JobState_JOB_STATE_RUNNING)
	if _, err := s.UpdateJob(ctx, &dataflowpb.UpdateJobRequest{ProjectId: "p", Location: DefaultLocation, JobId: c.Id,
		Job: &dataflowpb.Job{RequestedState: dataflowpb.JobState_JOB_STATE_CANCELLED}}); err != nil {
		t.Fatal(err)
	}
	waitState(t, s, c.Id, dataflowpb.JobState_JOB_STATE_CANCELLED)

	// A batch job cannot be drained.
	d, _ := s.CreateJob(ctx, &dataflowpb.CreateJobRequest{ProjectId: "p", Job: job("batch", false)})
	waitState(t, s, d.Id, dataflowpb.JobState_JOB_STATE_DONE)

	act, _ := s.ListJobs(ctx, &dataflowpb.ListJobsRequest{ProjectId: "p", Location: DefaultLocation, Filter: dataflowpb.ListJobsRequest_ACTIVE})
	if len(act.Jobs) != 0 {
		t.Errorf("ListJobs(ACTIVE) = %d jobs; want none", len(act.Jobs))
	}
	all, _ := s.ListJobs(ctx, &dataflowpb.ListJobsRequest{ProjectId: "p", Location: DefaultLocation, PageSize: 2})
	if len(all.Jobs) != 2 || all.NextPageToken == "" {
		t.Errorf("ListJobs(page 2) = %d jobs, token %q", len(all.Jobs), all.NextPageToken)
	}
	if all.Jobs[0].Id != d.Id {
		t.Errorf("ListJobs lists %s first; want the newest, %s", all.Jobs[0].Id, d.Id)
	}
}

func ptr[T any](v T) *T { return &v }

func TestTemplates(t *testing.T) {
	ctx := context.Background()
	tmpl := `{"type": "JOB_TYPE_BATCH", "environment": {"sdkPipelineOptions": {"options": {"pipelineUrl": "gs://b/p.pb", "temp_location": "gs://b/t"}}}}`
	meta := `{"name": "words", "parameters": [{"name": "input", "label": "Input"}, {"name": "extra", "label": "Extra", "isOptional": true}]}`
	s := newTestServer(t, Options{Storage: memStorage{"gs://b/tpl": tmpl, "gs://b/tpl_metadata": meta}})
	launch := func(params map[string]string, validate bool) (*dataflowpb.LaunchTemplateResponse, error) {
		return s.LaunchTemplate(ctx, &dataflowpb.LaunchTemplateRequest{ProjectId: "p", ValidateOnly: validate,
			Template:         &dataflowpb.LaunchTemplateRequest_GcsPath{GcsPath: "gs://b/tpl"},
			LaunchParameters: &dataflowpb.LaunchTemplateParameters{JobName: "from-tpl", Parameters: params}})
	}
	if _, err := launch(nil, false); status.Code(err) != codes.InvalidArgument {
		t.Errorf("launch without a required parameter = %v; want INVALID_ARGUMENT", err)
	}
	if _, err := launch(map[string]string{"input": "x", "nope": "y"}, false); status.Code(err) != codes.InvalidArgument {
		t.Errorf("launch with an unknown parameter = %v; want INVALID_ARGUMENT", err)
	}
	if r, err := launch(map[string]string{"input": "x"}, true); err != nil || r.GetJob().GetId() != "" {
		t.Errorf("validateOnly = %v, %v; want no job", r, err)
	}
	r, err := launch(map[string]string{"input": "x"}, false)
	if err != nil {
		t.Fatal(err)
	}
	j := waitState(t, s, r.GetJob().GetId(), dataflowpb.JobState_JOB_STATE_DONE)
	o := j.GetEnvironment().GetSdkPipelineOptions().GetFields()["options"].GetStructValue().GetFields()
	if o["input"].GetStringValue() != "x" || o["templateLocation"].GetStringValue() != "gs://b/tpl" || o["tempLocation"].GetStringValue() != "gs://b/t" {
		t.Errorf("the templated job's options are %v", o)
	}
	g, err := s.GetTemplate(ctx, &dataflowpb.GetTemplateRequest{ProjectId: "p", Template: &dataflowpb.GetTemplateRequest_GcsPath{GcsPath: "gs://b/tpl"}})
	if err != nil || g.GetTemplateType() != dataflowpb.GetTemplateResponse_LEGACY || g.GetMetadata().GetName() != "words" {
		t.Errorf("GetTemplate = %v, %v", g, err)
	}
	if _, err := s.GetTemplate(ctx, &dataflowpb.GetTemplateRequest{ProjectId: "p", Template: &dataflowpb.GetTemplateRequest_GcsPath{GcsPath: "gs://b/none"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("GetTemplate of a missing file = %v; want INVALID_ARGUMENT", err)
	}
	if _, err := s.LaunchFlexTemplate(ctx, &dataflowpb.LaunchFlexTemplateRequest{ProjectId: "p"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("LaunchFlexTemplate without a launcher = %v; want FAILED_PRECONDITION", err)
	}
	if _, err := s.SnapshotJob(ctx, &dataflowpb.SnapshotJobRequest{ProjectId: "p", JobId: j.Id}); status.Code(err) != codes.Unimplemented {
		t.Errorf("SnapshotJob = %v; want UNIMPLEMENTED", err)
	}
}

func TestPersistenceAndREST(t *testing.T) {
	ctx := context.Background()
	state := filepath.Join(t.TempDir(), "dataflow.json")
	runs := make(chan Run, 4)
	s, err := NewServer(Options{State: state, Engine: &fakeEngine{runs: runs}})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.CreateJob(ctx, &dataflowpb.CreateJobRequest{ProjectId: "p", Job: job("kept", true)})
	<-runs
	waitState(t, s, a.Id, dataflowpb.JobState_JOB_STATE_RUNNING)

	// The JSON API, by the v1b3 paths.
	srv := httptest.NewServer(NewRESTHandler(s))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1b3/projects/p/locations/us-central1/jobs/" + a.Id)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET the job over REST: %v %v", resp, err)
	}
	resp.Body.Close()
	resp, err = http.Get(srv.URL + "/v1b3/projects/p/jobs:aggregated")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET jobs:aggregated: %v %v", resp, err)
	}
	resp.Body.Close()
	s.Close()

	// Stopping the server cancels the jobs it runs, which are kept so.
	s2, err := NewServer(Options{State: state, Engine: &fakeEngine{}})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	j, err := s2.GetJob(ctx, &dataflowpb.GetJobRequest{ProjectId: "p", Location: DefaultLocation, JobId: a.Id})
	if err != nil || j.CurrentState != dataflowpb.JobState_JOB_STATE_CANCELLED {
		t.Errorf("after a restart the job is %v, %v; want CANCELLED", j.GetCurrentState(), err)
	}
	if err := s2.Reset("p"); err != nil {
		t.Fatal(err)
	}
	if got := s2.Jobs("p"); len(got) != 0 {
		t.Errorf("after Reset the project has %d jobs", len(got))
	}
}

// fakeLauncher stands for a template's program: it submits the job its
// launch names, as the SDK's DataflowRunner does, or fails.
type fakeLauncher struct {
	s    **Server
	got  chan FlexLaunch
	fail bool
}

func (l fakeLauncher) Launch(ctx context.Context, f FlexLaunch) error {
	l.got <- f
	if l.fail {
		return errors.New("Traceback: ModuleNotFoundError: no module named nope")
	}
	_, err := (*l.s).CreateJob(ctx, &dataflowpb.CreateJobRequest{ProjectId: f.Project, Location: f.Location, Job: job(f.JobName, false)})
	return err
}

func TestFlexTemplates(t *testing.T) {
	ctx := context.Background()
	spec := `{"image": "us-central1-docker.pkg.dev/p/r/flex:1", "sdkInfo": {"language": "PYTHON"},
		"metadata": {"name": "words", "parameters": [{"name": "input", "label": "Input"}]}}`
	var s *Server
	got := make(chan FlexLaunch, 2)
	s = newTestServer(t, Options{Storage: memStorage{"gs://b/flex.json": spec}, Launcher: fakeLauncher{s: &s, got: got}})
	launch := func(name string, params map[string]string) (*dataflowpb.LaunchFlexTemplateResponse, error) {
		return s.LaunchFlexTemplate(ctx, &dataflowpb.LaunchFlexTemplateRequest{ProjectId: "p", Location: DefaultLocation,
			LaunchParameter: &dataflowpb.LaunchFlexTemplateParameter{JobName: name, Parameters: params,
				Template:    &dataflowpb.LaunchFlexTemplateParameter_ContainerSpecGcsPath{ContainerSpecGcsPath: "gs://b/flex.json"},
				Environment: &dataflowpb.FlexTemplateRuntimeEnvironment{TempLocation: "gs://b/tmp", MaxWorkers: 2}}})
	}
	if _, err := launch("flex", nil); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a launch without a required parameter = %v; want INVALID_ARGUMENT", err)
	}
	r, err := launch("flex", map[string]string{"input": "gs://b/in.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if r.GetJob().GetCurrentState() != dataflowpb.JobState_JOB_STATE_QUEUED {
		t.Errorf("the launched job is %s; want QUEUED", r.GetJob().GetCurrentState())
	}
	f := <-got
	if f.Image != "us-central1-docker.pkg.dev/p/r/flex:1" || !strings.Contains(strings.Join(f.Args, " "), "--input=gs://b/in.txt --job_name=flex --project=p --region=us-central1 --temp_location=gs://b/tmp --max_num_workers=2") {
		t.Errorf("the launcher was given %+v", f)
	}
	// The program's job is the launch's: one job, its ID, run to the end.
	j := waitState(t, s, r.GetJob().GetId(), dataflowpb.JobState_JOB_STATE_DONE)
	if o := j.GetEnvironment().GetSdkPipelineOptions().GetFields()["options"].GetStructValue().GetFields(); j.Name != "flex" ||
		o["tempLocation"].GetStringValue() != "gs://b/tmp" || o["pipelineUrl"] == nil {
		t.Errorf("the adopted job is %v", j)
	}
	if l, _ := s.ListJobs(ctx, &dataflowpb.ListJobsRequest{ProjectId: "p", Location: DefaultLocation}); len(l.Jobs) != 1 {
		t.Errorf("ListJobs = %d jobs; want the launch's one", len(l.Jobs))
	}

	// A program that fails fails the launch's job, with its error.
	s2 := newTestServer(t, Options{Storage: memStorage{"gs://b/flex.json": spec}, Launcher: fakeLauncher{s: &s, got: got, fail: true}})
	r2, err := s2.LaunchFlexTemplate(ctx, &dataflowpb.LaunchFlexTemplateRequest{ProjectId: "p",
		LaunchParameter: &dataflowpb.LaunchFlexTemplateParameter{JobName: "broken", Parameters: map[string]string{"input": "x"},
			Template: &dataflowpb.LaunchFlexTemplateParameter_ContainerSpecGcsPath{ContainerSpecGcsPath: "gs://b/flex.json"}}})
	if err != nil {
		t.Fatal(err)
	}
	<-got
	waitState(t, s2, r2.GetJob().GetId(), dataflowpb.JobState_JOB_STATE_FAILED)
	m, _ := s2.ListJobMessages(ctx, &dataflowpb.ListJobMessagesRequest{ProjectId: "p", Location: DefaultLocation, JobId: r2.GetJob().GetId(),
		MinimumImportance: dataflowpb.JobMessageImportance_JOB_MESSAGE_ERROR})
	if len(m.GetJobMessages()) == 0 || !strings.Contains(m.JobMessages[len(m.JobMessages)-1].MessageText, "ModuleNotFoundError") {
		t.Errorf("the failed launch's messages are %v", m.GetJobMessages())
	}
}
