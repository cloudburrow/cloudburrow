package vertexai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// fakeExecutor is a NotebookExecutor over maps.
type fakeExecutor struct {
	mu      sync.Mutex
	objects map[string][]byte
	repos   map[string][]byte
	runs    int
	fail    bool
}

func (x *fakeExecutor) ReadObject(_ context.Context, bucket, object string, _ int64) ([]byte, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	b, ok := x.objects[bucket+"/"+object]
	if !ok {
		return nil, errors.New("no such object")
	}
	return b, nil
}

func (x *fakeExecutor) WriteObject(_ context.Context, bucket, object, _ string, data []byte) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.objects[bucket+"/"+object] = data
	return nil
}

func (x *fakeExecutor) ReadRepositoryFile(_ context.Context, repo, _, file string) ([]byte, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	b, ok := x.repos[repo+"/"+file]
	if !ok {
		return nil, errors.New("no such file")
	}
	return b, nil
}

func (x *fakeExecutor) Run(_ context.Context, _, _ string, nb []byte, _ NotebookRuntimeOptions) ([]byte, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.runs++
	out := []byte(strings.Replace(string(nb), `"outputs":[]`, `"outputs":[{"output_type":"stream","name":"stdout","text":"42"}]`, 1))
	if x.fail {
		return out, errors.New("cell 1 raised ValueError: boom")
	}
	return out, nil
}

func codeOf(err error) codes.Code { return status.Code(err) }

const testNotebook = `{"cells":[{"cell_type":"code","source":"print(42)","outputs":[]}],"metadata":{},"nbformat":4,"nbformat_minor":5}`

func newJobServer(t *testing.T) (*Server, *fakeExecutor) {
	t.Helper()
	s := NewServer(nil)
	s.SetNotebookRuntimes(newFakeHost(), filepath.Join(t.TempDir(), "nb.json"))
	x := &fakeExecutor{objects: map[string][]byte{"in/nb/run.ipynb": []byte(testNotebook)},
		repos: map[string][]byte{"projects/p/locations/us-central1/repositories/r/content.ipynb": []byte(testNotebook)}}
	s.SetNotebookExecutor(x)
	t.Cleanup(s.stopScheduler)
	return s, x
}

func waitJob(t *testing.T, n *notebookService, name string) *aiplatformpb.NotebookExecutionJob {
	t.Helper()
	for i := 0; i < 200; i++ {
		j, err := n.GetNotebookExecutionJob(context.Background(), &aiplatformpb.GetNotebookExecutionJobRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		switch j.GetJobState() {
		case aiplatformpb.JobState_JOB_STATE_SUCCEEDED, aiplatformpb.JobState_JOB_STATE_FAILED, aiplatformpb.JobState_JOB_STATE_CANCELLED:
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not end", name)
	return nil
}

func TestNotebookExecutionJobs(t *testing.T) {
	ctx := context.Background()
	s, x := newJobServer(t)
	n := &notebookService{s: s}
	parent := "projects/p/locations/us-central1"
	for _, bad := range []*aiplatformpb.NotebookExecutionJob{
		{DisplayName: "no source", ExecutionSink: &aiplatformpb.NotebookExecutionJob_GcsOutputUri{GcsOutputUri: "gs://out"},
			ExecutionIdentity: &aiplatformpb.NotebookExecutionJob_ExecutionUser{ExecutionUser: "a@b.c"}},
		{DisplayName: "no sink", NotebookSource: &aiplatformpb.NotebookExecutionJob_GcsNotebookSource_{GcsNotebookSource: &aiplatformpb.NotebookExecutionJob_GcsNotebookSource{Uri: "gs://in/nb/run.ipynb"}},
			ExecutionIdentity: &aiplatformpb.NotebookExecutionJob_ExecutionUser{ExecutionUser: "a@b.c"}},
		{DisplayName: "no identity", NotebookSource: &aiplatformpb.NotebookExecutionJob_GcsNotebookSource_{GcsNotebookSource: &aiplatformpb.NotebookExecutionJob_GcsNotebookSource{Uri: "gs://in/nb/run.ipynb"}},
			ExecutionSink: &aiplatformpb.NotebookExecutionJob_GcsOutputUri{GcsOutputUri: "gs://out"}},
		{DisplayName: "bad uri", NotebookSource: &aiplatformpb.NotebookExecutionJob_GcsNotebookSource_{GcsNotebookSource: &aiplatformpb.NotebookExecutionJob_GcsNotebookSource{Uri: "gs://in"}},
			ExecutionSink: &aiplatformpb.NotebookExecutionJob_GcsOutputUri{GcsOutputUri: "gs://out"}, ExecutionIdentity: &aiplatformpb.NotebookExecutionJob_ExecutionUser{ExecutionUser: "a@b.c"}},
	} {
		if _, err := n.CreateNotebookExecutionJob(ctx, &aiplatformpb.CreateNotebookExecutionJobRequest{Parent: parent, NotebookExecutionJob: bad}); codeOf(err) != codes.InvalidArgument {
			t.Errorf("create %q = %v, want INVALID_ARGUMENT", bad.GetDisplayName(), err)
		}
	}
	job := func(name string, src any) *aiplatformpb.NotebookExecutionJob {
		j := &aiplatformpb.NotebookExecutionJob{DisplayName: name, ExecutionSink: &aiplatformpb.NotebookExecutionJob_GcsOutputUri{GcsOutputUri: "gs://out/results"},
			ExecutionIdentity: &aiplatformpb.NotebookExecutionJob_ServiceAccount{ServiceAccount: "sa@p.iam.gserviceaccount.com"}}
		switch v := src.(type) {
		case *aiplatformpb.NotebookExecutionJob_GcsNotebookSource_:
			j.NotebookSource = v
		case *aiplatformpb.NotebookExecutionJob_DataformRepositorySource_:
			j.NotebookSource = v
		case *aiplatformpb.NotebookExecutionJob_DirectNotebookSource_:
			j.NotebookSource = v
		}
		return j
	}
	cases := []struct {
		id, file string
		src      any
	}{
		{"from-gcs", "run.ipynb", &aiplatformpb.NotebookExecutionJob_GcsNotebookSource_{GcsNotebookSource: &aiplatformpb.NotebookExecutionJob_GcsNotebookSource{Uri: "gs://in/nb/run.ipynb"}}},
		{"from-dataform", "content.ipynb", &aiplatformpb.NotebookExecutionJob_DataformRepositorySource_{DataformRepositorySource: &aiplatformpb.NotebookExecutionJob_DataformRepositorySource{
			DataformRepositoryResourceName: parent + "/repositories/r"}}},
		{"direct", "notebook.ipynb", &aiplatformpb.NotebookExecutionJob_DirectNotebookSource_{DirectNotebookSource: &aiplatformpb.NotebookExecutionJob_DirectNotebookSource{Content: []byte(testNotebook)}}},
	}
	for _, c := range cases {
		op, err := n.CreateNotebookExecutionJob(ctx, &aiplatformpb.CreateNotebookExecutionJobRequest{Parent: parent, NotebookExecutionJobId: c.id, NotebookExecutionJob: job(c.id, c.src)})
		if err != nil || !op.GetDone() {
			t.Fatalf("create %s = %v, %v", c.id, op, err)
		}
		j := waitJob(t, n, parent+"/notebookExecutionJobs/"+c.id)
		if j.GetJobState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED || j.GetExecutionTimeout().AsDuration() != 24*time.Hour {
			t.Errorf("%s = %v", c.id, j)
		}
		if out := string(x.objects["out/results/"+c.id+"/"+c.file]); !strings.Contains(out, `"42"`) {
			t.Errorf("%s output = %q", c.id, out)
		}
	}
	// BASIC leaves out direct contents, FULL keeps them.
	basic, _ := n.GetNotebookExecutionJob(ctx, &aiplatformpb.GetNotebookExecutionJobRequest{Name: parent + "/notebookExecutionJobs/direct"})
	full, _ := n.GetNotebookExecutionJob(ctx, &aiplatformpb.GetNotebookExecutionJobRequest{Name: parent + "/notebookExecutionJobs/direct", View: aiplatformpb.NotebookExecutionJobView_NOTEBOOK_EXECUTION_JOB_VIEW_FULL})
	if len(basic.GetDirectNotebookSource().GetContent()) != 0 || len(full.GetDirectNotebookSource().GetContent()) == 0 {
		t.Errorf("views: basic %v, full %v", basic, full)
	}
	list, err := n.ListNotebookExecutionJobs(ctx, &aiplatformpb.ListNotebookExecutionJobsRequest{Parent: parent, Filter: `displayName=~"^from-"`, OrderBy: "display_name desc"})
	if err != nil || len(list.GetNotebookExecutionJobs()) != 2 || list.GetNotebookExecutionJobs()[0].GetDisplayName() != "from-gcs" {
		t.Errorf("list = %v, %v", list, err)
	}
	// A cell that raises fails the job, and the notebook is still written.
	x.fail = true
	if _, err := n.CreateNotebookExecutionJob(ctx, &aiplatformpb.CreateNotebookExecutionJobRequest{Parent: parent, NotebookExecutionJobId: "fails", NotebookExecutionJob: job("fails", cases[0].src)}); err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, n, parent+"/notebookExecutionJobs/fails"); j.GetJobState() != aiplatformpb.JobState_JOB_STATE_FAILED ||
		j.GetStatus().GetCode() != int32(codes.FailedPrecondition) || !strings.Contains(j.GetStatus().GetMessage(), "boom") {
		t.Errorf("failing job = %v", j)
	}
	if _, ok := x.objects["out/results/fails/run.ipynb"]; !ok {
		t.Error("the failing job's notebook was not written")
	}
	if _, err := n.DeleteNotebookExecutionJob(ctx, &aiplatformpb.DeleteNotebookExecutionJobRequest{Name: parent + "/notebookExecutionJobs/fails"}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.GetNotebookExecutionJob(ctx, &aiplatformpb.GetNotebookExecutionJobRequest{Name: parent + "/notebookExecutionJobs/fails"}); codeOf(err) != codes.NotFound {
		t.Errorf("get deleted = %v", err)
	}
}

func TestSchedulesRunNotebookExecutions(t *testing.T) {
	ctx := context.Background()
	old := scheduleTick
	scheduleTick = 20 * time.Millisecond
	t.Cleanup(func() { scheduleTick = old })
	s, x := newJobServer(t)
	clock := time.Now()
	var mu sync.Mutex
	st := s.notebooks()
	st.mu.Lock()
	st.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	st.mu.Unlock()
	sch := &scheduleService{s: s}
	parent := "projects/p/locations/us-central1"
	req := &aiplatformpb.CreateNotebookExecutionJobRequest{Parent: parent, NotebookExecutionJob: &aiplatformpb.NotebookExecutionJob{
		NotebookSource: &aiplatformpb.NotebookExecutionJob_GcsNotebookSource_{GcsNotebookSource: &aiplatformpb.NotebookExecutionJob_GcsNotebookSource{Uri: "gs://in/nb/run.ipynb"}},
		ExecutionSink:  &aiplatformpb.NotebookExecutionJob_GcsOutputUri{GcsOutputUri: "gs://out"}, ExecutionIdentity: &aiplatformpb.NotebookExecutionJob_ExecutionUser{ExecutionUser: "a@b.c"}}}
	for _, bad := range []*aiplatformpb.Schedule{
		{DisplayName: "", TimeSpecification: &aiplatformpb.Schedule_Cron{Cron: "* * * * *"}, MaxConcurrentRunCount: 1, Request: &aiplatformpb.Schedule_CreateNotebookExecutionJobRequest{CreateNotebookExecutionJobRequest: req}},
		{DisplayName: "x", TimeSpecification: &aiplatformpb.Schedule_Cron{Cron: "nope"}, MaxConcurrentRunCount: 1, Request: &aiplatformpb.Schedule_CreateNotebookExecutionJobRequest{CreateNotebookExecutionJobRequest: req}},
		{DisplayName: "x", TimeSpecification: &aiplatformpb.Schedule_Cron{Cron: "* * * * *"}, Request: &aiplatformpb.Schedule_CreateNotebookExecutionJobRequest{CreateNotebookExecutionJobRequest: req}},
	} {
		if _, err := sch.CreateSchedule(ctx, &aiplatformpb.CreateScheduleRequest{Parent: parent, Schedule: bad}); codeOf(err) != codes.InvalidArgument {
			t.Errorf("create %v = %v", bad, err)
		}
	}
	if _, err := sch.CreateSchedule(ctx, &aiplatformpb.CreateScheduleRequest{Parent: parent, Schedule: &aiplatformpb.Schedule{DisplayName: "p",
		TimeSpecification: &aiplatformpb.Schedule_Cron{Cron: "* * * * *"}, MaxConcurrentRunCount: 1,
		Request: &aiplatformpb.Schedule_CreatePipelineJobRequest{CreatePipelineJobRequest: &aiplatformpb.CreatePipelineJobRequest{Parent: parent}}}}); codeOf(err) != codes.Unimplemented {
		t.Errorf("a pipeline schedule = %v, want UNIMPLEMENTED", err)
	}
	sc, err := sch.CreateSchedule(ctx, &aiplatformpb.CreateScheduleRequest{Parent: parent, Schedule: &aiplatformpb.Schedule{DisplayName: "Every minute",
		TimeSpecification: &aiplatformpb.Schedule_Cron{Cron: "TZ=America/New_York * * * * *"}, MaxConcurrentRunCount: 1, MaxRunCount: 2,
		Request: &aiplatformpb.Schedule_CreateNotebookExecutionJobRequest{CreateNotebookExecutionJobRequest: req}}})
	if err != nil || sc.GetState() != aiplatformpb.Schedule_ACTIVE || sc.GetNextRunTime() == nil || sc.GetStartTime() == nil {
		t.Fatalf("create = %v, %v", sc, err)
	}
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }
	waitRuns := func(want int64) *aiplatformpb.Schedule {
		t.Helper()
		for i := 0; i < 300; i++ {
			got, _ := sch.GetSchedule(ctx, &aiplatformpb.GetScheduleRequest{Name: sc.GetName()})
			if got.GetStartedRunCount() >= want {
				return got
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("the schedule did not start %d runs", want)
		return nil
	}
	advance(61 * time.Second)
	got := waitRuns(1)
	if !strings.Contains(got.GetLastScheduledRunResponse().GetRunResponse(), "/notebookExecutionJobs/") {
		t.Errorf("last run = %v", got.GetLastScheduledRunResponse())
	}
	n := &notebookService{s: s}
	jobs, _ := n.ListNotebookExecutionJobs(ctx, &aiplatformpb.ListNotebookExecutionJobsRequest{Parent: parent, Filter: `schedule="` + sc.GetName() + `"`})
	if len(jobs.GetNotebookExecutionJobs()) != 1 || jobs.GetNotebookExecutionJobs()[0].GetDisplayName() != "Every minute" {
		t.Fatalf("jobs = %v", jobs)
	}
	waitJob(t, n, jobs.GetNotebookExecutionJobs()[0].GetName())
	// Paused: no runs.
	if _, err := sch.PauseSchedule(ctx, &aiplatformpb.PauseScheduleRequest{Name: sc.GetName()}); err != nil {
		t.Fatal(err)
	}
	advance(61 * time.Second)
	time.Sleep(100 * time.Millisecond)
	if g, _ := sch.GetSchedule(ctx, &aiplatformpb.GetScheduleRequest{Name: sc.GetName()}); g.GetStartedRunCount() != 1 || g.GetState() != aiplatformpb.Schedule_PAUSED || g.GetLastPauseTime() == nil {
		t.Errorf("paused = %v", g)
	}
	if _, err := sch.ResumeSchedule(ctx, &aiplatformpb.ResumeScheduleRequest{Name: sc.GetName()}); err != nil {
		t.Fatal(err)
	}
	advance(61 * time.Second)
	if g := waitRuns(2); g.GetState() != aiplatformpb.Schedule_COMPLETED {
		t.Errorf("after max_run_count = %v", g)
	}
	if _, err := sch.PauseSchedule(ctx, &aiplatformpb.PauseScheduleRequest{Name: sc.GetName()}); codeOf(err) != codes.FailedPrecondition {
		t.Errorf("pause completed = %v", err)
	}
	up, err := sch.UpdateSchedule(ctx, &aiplatformpb.UpdateScheduleRequest{Schedule: &aiplatformpb.Schedule{Name: sc.GetName(), DisplayName: "Renamed"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"displayName"}}})
	if err != nil || up.GetDisplayName() != "Renamed" {
		t.Errorf("update = %v, %v", up, err)
	}
	if _, err := sch.UpdateSchedule(ctx, &aiplatformpb.UpdateScheduleRequest{Schedule: &aiplatformpb.Schedule{Name: sc.GetName()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("update state = %v", err)
	}
	list, err := sch.ListSchedules(ctx, &aiplatformpb.ListSchedulesRequest{Parent: parent, Filter: `state="COMPLETED" AND create_notebook_execution_job_request:*`})
	if err != nil || len(list.GetSchedules()) != 1 {
		t.Errorf("list = %v, %v", list, err)
	}
	if _, err := sch.DeleteSchedule(ctx, &aiplatformpb.DeleteScheduleRequest{Name: sc.GetName()}); err != nil {
		t.Fatal(err)
	}
	// Its runs stay.
	if jobs, _ := n.ListNotebookExecutionJobs(ctx, &aiplatformpb.ListNotebookExecutionJobsRequest{Parent: parent}); len(jobs.GetNotebookExecutionJobs()) != 2 {
		t.Errorf("jobs after delete = %v", jobs)
	}
	_ = x
}

func TestNotebookTemplateIAM(t *testing.T) {
	ctx := context.Background()
	s, _ := newJobServer(t)
	n := &notebookService{s: s}
	op, err := n.CreateNotebookRuntimeTemplate(ctx, &aiplatformpb.CreateNotebookRuntimeTemplateRequest{Parent: "projects/p/locations/us-central1", NotebookRuntimeTemplateId: "t",
		NotebookRuntimeTemplate: &aiplatformpb.NotebookRuntimeTemplate{DisplayName: "T"}})
	if err != nil || !op.GetDone() {
		t.Fatal(err)
	}
	name := "projects/p/locations/us-central1/notebookRuntimeTemplates/t"
	x := &notebookIAM{s: s}
	p, err := x.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
	if err != nil || len(p.GetBindings()) != 0 {
		t.Fatalf("empty policy = %v, %v", p, err)
	}
	if _, err := x.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name + "x"}); codeOf(err) != codes.NotFound {
		t.Errorf("missing template = %v", err)
	}
	set, err := x.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{Bindings: []*iampb.Binding{
		{Role: "roles/aiplatform.notebookRuntimeUser", Members: []string{"user:ana@example.com"}}}}})
	if err != nil || len(set.GetEtag()) == 0 {
		t.Fatalf("set = %v, %v", set, err)
	}
	if got := s.NotebookTemplatePolicy(name); got == nil || got.Bindings[0].Members[0] != "user:ana@example.com" {
		t.Errorf("stored = %v", got)
	}
	// REST: POST :getIamPolicy and :testIamPermissions with no body.
	h := httptest.NewServer(http.HandlerFunc(s.serveNotebookREST))
	defer h.Close()
	resp, err := http.Post(h.URL+"/v1/"+name+":getIamPolicy?options.requestedPolicyVersion=3", "application/json", nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("REST getIamPolicy = %v, %v", resp, err)
	}
	resp.Body.Close()
	resp, err = http.Post(h.URL+"/v1/"+name+":testIamPermissions?permissions=aiplatform.notebookRuntimeTemplates.apply", "application/json", nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("REST testIamPermissions = %v, %v", resp, err)
	}
	resp.Body.Close()
}
