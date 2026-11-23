//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

const colabNotebook = `{"cells":[{"cell_type":"code","metadata":{},"execution_count":null,"outputs":[],"source":"print(6 * 7)"}],"metadata":{},"nbformat":4,"nbformat_minor":5}`

// colabTemplate makes a runtime template for a test and deletes it after.
func colabTemplate(ctx context.Context, t *testing.T, c *aiplatform.NotebookClient, parent, id string) *aiplatformpb.NotebookRuntimeTemplate {
	t.Helper()
	op, err := c.CreateNotebookRuntimeTemplate(ctx, &aiplatformpb.CreateNotebookRuntimeTemplateRequest{Parent: parent, NotebookRuntimeTemplateId: id,
		NotebookRuntimeTemplate: &aiplatformpb.NotebookRuntimeTemplate{DisplayName: "Executions " + id}})
	if err != nil {
		t.Fatalf("CreateNotebookRuntimeTemplate: %v", err)
	}
	tpl, err := op.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteNotebookRuntimeTemplate(context.Background(), &aiplatformpb.DeleteNotebookRuntimeTemplateRequest{Name: tpl.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	return tpl
}

func colabPut(ctx context.Context, t *testing.T, gcs *storage.Client, bucket, object, data string) {
	t.Helper()
	w := gcs.Bucket(bucket).Object(object).NewWriter(ctx)
	if _, err := w.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// waitExecution polls a notebook execution until it ends.
func waitExecution(ctx context.Context, t *testing.T, c *aiplatform.NotebookClient, name string) *aiplatformpb.NotebookExecutionJob {
	t.Helper()
	deadline := time.Now().Add(20 * time.Minute)
	for {
		j, err := c.GetNotebookExecutionJob(ctx, &aiplatformpb.GetNotebookExecutionJobRequest{Name: name})
		if err != nil {
			t.Fatalf("GetNotebookExecutionJob: %v", err)
		}
		switch j.GetJobState() {
		case aiplatformpb.JobState_JOB_STATE_SUCCEEDED, aiplatformpb.JobState_JOB_STATE_FAILED, aiplatformpb.JobState_JOB_STATE_CANCELLED:
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("execution %s still %v", name, j.GetJobState())
		}
		time.Sleep(2 * time.Second)
	}
}

// covers: google.cloud.aiplatform.v1.NotebookService/CreateNotebookExecutionJob, google.cloud.aiplatform.v1.NotebookService/GetNotebookExecutionJob, google.cloud.aiplatform.v1.NotebookService/ListNotebookExecutionJobs, google.cloud.aiplatform.v1.NotebookService/DeleteNotebookExecutionJob
//
// TestColabEnterpriseNotebookExecutions (#2105): with the official
// aiplatform NotebookClient (gRPC) and the JSON surface, a notebook in
// Cloud Storage, one in a Dataform single-file repository (read at HEAD)
// and one given as contents each run on a runtime made from a runtime
// template; each is SUCCEEDED with the executed notebook, its output in
// it, written to gs://BUCKET/JOB_ID/FILE; a notebook whose cell raises is
// FAILED, its notebook still written; the list filters by ID, displayName
// regex and schedule and orders by display_name; view BASIC leaves out
// the direct contents; missing source, sink and identity are
// INVALID_ARGUMENT; delete removes the execution and keeps its result.
func TestColabEnterpriseNotebookExecutions(t *testing.T) {
	h := New(t)
	addr, clients := notebookClients(t, h)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_BIGQUERY", EnvBigQuery)
	gcs := dataformStorage(t, h)
	df := dataformClients(t, h)["grpc"]
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	t.Cleanup(cancel)
	c := clients["grpc"]
	project := h.Project()
	parent := fmt.Sprintf("projects/%s/locations/us-central1", project)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	tpl := colabTemplate(ctx, t, c, parent, "exec-"+suffix)
	in, out := "colab-in-"+suffix, "colab-out-"+suffix
	dataformBucket(ctx, t, gcs, project, in)
	dataformBucket(ctx, t, gcs, project, out)
	colabPut(ctx, t, gcs, in, "nb/run.ipynb", colabNotebook)
	colabPut(ctx, t, gcs, in, "nb/fails.ipynb", strings.Replace(colabNotebook, "print(6 * 7)", "raise ValueError('boom')", 1))

	repo, err := df.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "nb" + suffix,
		Repository: &dfpb.Repository{Labels: map[string]string{"single-file-asset-type": "notebook"}}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	t.Cleanup(func() {
		_ = df.DeleteRepository(context.Background(), &dfpb.DeleteRepositoryRequest{Name: repo.GetName(), Force: true})
	})
	if _, err := df.CommitRepositoryChanges(ctx, &dfpb.CommitRepositoryChangesRequest{Name: repo.GetName(),
		CommitMetadata: &dfpb.CommitMetadata{Author: &dfpb.CommitAuthor{Name: "Dev", EmailAddress: "dev@example.com"}, CommitMessage: "notebook"},
		FileOperations: map[string]*dfpb.CommitRepositoryChangesRequest_FileOperation{"content.ipynb": {Operation: &dfpb.CommitRepositoryChangesRequest_FileOperation_WriteFile_{
			WriteFile: &dfpb.CommitRepositoryChangesRequest_FileOperation_WriteFile{Contents: []byte(colabNotebook)}}}}}); err != nil {
		t.Fatalf("CommitRepositoryChanges: %v", err)
	}

	base := func(name string) *aiplatformpb.NotebookExecutionJob {
		return &aiplatformpb.NotebookExecutionJob{DisplayName: name,
			EnvironmentSpec:   &aiplatformpb.NotebookExecutionJob_NotebookRuntimeTemplateResourceName{NotebookRuntimeTemplateResourceName: tpl.GetName()},
			ExecutionSink:     &aiplatformpb.NotebookExecutionJob_GcsOutputUri{GcsOutputUri: "gs://" + out},
			ExecutionIdentity: &aiplatformpb.NotebookExecutionJob_ExecutionUser{ExecutionUser: "dev@example.com"}}
	}
	gcsSource := func(obj string) *aiplatformpb.NotebookExecutionJob_GcsNotebookSource_ {
		return &aiplatformpb.NotebookExecutionJob_GcsNotebookSource_{GcsNotebookSource: &aiplatformpb.NotebookExecutionJob_GcsNotebookSource{Uri: "gs://" + in + "/" + obj}}
	}
	noSource := base("x")
	noSink := base("x")
	noSink.NotebookSource, noSink.ExecutionSink = gcsSource("nb/run.ipynb"), nil
	noIdentity := base("x")
	noIdentity.NotebookSource, noIdentity.ExecutionIdentity = gcsSource("nb/run.ipynb"), nil
	for what, bad := range map[string]*aiplatformpb.NotebookExecutionJob{"no source": noSource, "no sink": noSink, "no identity": noIdentity} {
		if _, err := c.CreateNotebookExecutionJob(ctx, &aiplatformpb.CreateNotebookExecutionJobRequest{Parent: parent, NotebookExecutionJob: bad}); quotaCode(err) != codes.InvalidArgument {
			t.Errorf("create with %s = %v, want INVALID_ARGUMENT", what, err)
		}
	}

	type run struct {
		id, file string
		job      *aiplatformpb.NotebookExecutionJob
	}
	fromGCS := base("from-gcs " + suffix)
	fromGCS.NotebookSource = gcsSource("nb/run.ipynb")
	fromRepo := base("from-dataform " + suffix)
	fromRepo.NotebookSource = &aiplatformpb.NotebookExecutionJob_DataformRepositorySource_{DataformRepositorySource: &aiplatformpb.NotebookExecutionJob_DataformRepositorySource{
		DataformRepositoryResourceName: repo.GetName()}}
	direct := base("direct " + suffix)
	direct.NotebookSource = &aiplatformpb.NotebookExecutionJob_DirectNotebookSource_{DirectNotebookSource: &aiplatformpb.NotebookExecutionJob_DirectNotebookSource{Content: []byte(colabNotebook)}}
	fails := base("fails " + suffix)
	fails.NotebookSource = gcsSource("nb/fails.ipynb")
	runs := []run{{"gcs-" + suffix, "run.ipynb", fromGCS}, {"df-" + suffix, "content.ipynb", fromRepo}, {"direct-" + suffix, "notebook.ipynb", direct}, {"fails-" + suffix, "fails.ipynb", fails}}
	for _, r := range runs {
		op, err := c.CreateNotebookExecutionJob(ctx, &aiplatformpb.CreateNotebookExecutionJobRequest{Parent: parent, NotebookExecutionJobId: r.id, NotebookExecutionJob: r.job})
		if err != nil {
			t.Fatalf("CreateNotebookExecutionJob %s: %v", r.id, err)
		}
		j, err := op.Wait(ctx)
		if err != nil || j.GetName() != parent+"/notebookExecutionJobs/"+r.id {
			t.Fatalf("the create operation of %s = %v, %v", r.id, j, err)
		}
	}
	for _, r := range runs {
		j := waitExecution(ctx, t, c, parent+"/notebookExecutionJobs/"+r.id)
		got := string(dfReadObject(ctx, t, gcs, out, r.id+"/"+r.file))
		if strings.HasPrefix(r.id, "fails-") {
			if j.GetJobState() != aiplatformpb.JobState_JOB_STATE_FAILED || !strings.Contains(j.GetStatus().GetMessage(), "boom") || !strings.Contains(got, "ValueError") {
				t.Errorf("the raising notebook = %v; output %q", j, got)
			}
			continue
		}
		if j.GetJobState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED || j.GetExecutionTimeout().AsDuration() != 24*time.Hour {
			t.Errorf("%s = %v", r.id, j)
		}
		if !strings.Contains(got, "42") {
			t.Errorf("%s's executed notebook = %q, want its output 42", r.id, got)
		}
	}
	basic, err := c.GetNotebookExecutionJob(ctx, &aiplatformpb.GetNotebookExecutionJobRequest{Name: parent + "/notebookExecutionJobs/direct-" + suffix})
	full, err2 := c.GetNotebookExecutionJob(ctx, &aiplatformpb.GetNotebookExecutionJobRequest{Name: parent + "/notebookExecutionJobs/direct-" + suffix,
		View: aiplatformpb.NotebookExecutionJobView_NOTEBOOK_EXECUTION_JOB_VIEW_FULL})
	if err != nil || err2 != nil || len(basic.GetDirectNotebookSource().GetContent()) != 0 || string(full.GetDirectNotebookSource().GetContent()) != colabNotebook {
		t.Errorf("views: BASIC %v (%v), FULL %v (%v)", basic, err, full, err2)
	}
	var names []string
	for it := c.ListNotebookExecutionJobs(ctx, &aiplatformpb.ListNotebookExecutionJobsRequest{Parent: parent,
		Filter: fmt.Sprintf(`displayName=~"^from-.* %s$"`, suffix), OrderBy: "display_name desc"}); ; {
		j, err := it.Next()
		if err != nil {
			break
		}
		names = append(names, j.GetDisplayName())
	}
	if strings.Join(names, ",") != "from-gcs "+suffix+",from-dataform "+suffix {
		t.Errorf("filtered list = %v", names)
	}
	if j, err := c.ListNotebookExecutionJobs(ctx, &aiplatformpb.ListNotebookExecutionJobsRequest{Parent: parent, Filter: `notebookExecutionJob="gcs-` + suffix + `"`}).Next(); err != nil || j.GetDisplayName() != "from-gcs "+suffix {
		t.Errorf("by ID = %v, %v", j, err)
	}
	if _, err := c.ListNotebookExecutionJobs(ctx, &aiplatformpb.ListNotebookExecutionJobsRequest{Parent: parent, Filter: `machineType="x"`}).Next(); quotaCode(err) != codes.InvalidArgument {
		t.Errorf("an undocumented filter field = %v", err)
	}
	// JSON: the list and a get.
	if code, body := notebookJSON(t, http.MethodGet, "http://"+addr+"/v1/"+parent+"/notebookExecutionJobs/gcs-"+suffix, ""); code != 200 || !strings.Contains(body, `"jobState":"JOB_STATE_SUCCEEDED"`) {
		t.Errorf("JSON get = %d %s", code, body)
	}
	op, err := c.DeleteNotebookExecutionJob(ctx, &aiplatformpb.DeleteNotebookExecutionJobRequest{Name: parent + "/notebookExecutionJobs/fails-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetNotebookExecutionJob(ctx, &aiplatformpb.GetNotebookExecutionJobRequest{Name: parent + "/notebookExecutionJobs/fails-" + suffix}); quotaCode(err) != codes.NotFound {
		t.Errorf("a deleted execution = %v", err)
	}
	for _, r := range runs[:3] {
		if op, err := c.DeleteNotebookExecutionJob(ctx, &aiplatformpb.DeleteNotebookExecutionJobRequest{Name: parent + "/notebookExecutionJobs/" + r.id}); err == nil {
			_ = op.Wait(ctx)
		}
	}
}

// scheduleClient is the official ScheduleClient at the run shard's local
// Vertex AI endpoint.
func scheduleClient(t *testing.T, h *Harness) (string, *aiplatform.ScheduleClient) {
	t.Helper()
	addr := runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	c, err := aiplatform.NewScheduleClient(h.Context(), option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("aiplatform.NewScheduleClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return addr, c
}

// covers: google.cloud.aiplatform.v1.ScheduleService/CreateSchedule, google.cloud.aiplatform.v1.ScheduleService/GetSchedule, google.cloud.aiplatform.v1.ScheduleService/ListSchedules, google.cloud.aiplatform.v1.ScheduleService/PauseSchedule, google.cloud.aiplatform.v1.ScheduleService/ResumeSchedule, google.cloud.aiplatform.v1.ScheduleService/UpdateSchedule, google.cloud.aiplatform.v1.ScheduleService/DeleteSchedule
//
// TestColabEnterpriseSchedules (#2105): with the official ScheduleClient,
// a schedule of a notebook execution every minute with max_run_count 1
// starts one execution, named after the schedule and carrying its
// schedule_resource_name, then is COMPLETED (a completed schedule cannot
// be paused); another is paused, resumed and renamed over the update
// mask, and a mask naming state is INVALID_ARGUMENT; the list filters by
// state and request presence; a pipeline schedule is UNIMPLEMENTED; a
// missing cron, display name and max_concurrent_run_count are
// INVALID_ARGUMENT; delete keeps the runs it made; JSON :pause works.
func TestColabEnterpriseSchedules(t *testing.T) {
	h := New(t)
	_, clients := notebookClients(t, h)
	addr, sc := scheduleClient(t, h)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_BIGQUERY", EnvBigQuery)
	gcs := dataformStorage(t, h)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	c := clients["grpc"]
	project := h.Project()
	parent := fmt.Sprintf("projects/%s/locations/us-central1", project)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	tpl := colabTemplate(ctx, t, c, parent, "sched-"+suffix)
	in, out := "colab-sin-"+suffix, "colab-sout-"+suffix
	dataformBucket(ctx, t, gcs, project, in)
	dataformBucket(ctx, t, gcs, project, out)
	colabPut(ctx, t, gcs, in, "run.ipynb", colabNotebook)
	request := &aiplatformpb.Schedule_CreateNotebookExecutionJobRequest{CreateNotebookExecutionJobRequest: &aiplatformpb.CreateNotebookExecutionJobRequest{
		Parent: parent, NotebookExecutionJob: &aiplatformpb.NotebookExecutionJob{
			NotebookSource:    &aiplatformpb.NotebookExecutionJob_GcsNotebookSource_{GcsNotebookSource: &aiplatformpb.NotebookExecutionJob_GcsNotebookSource{Uri: "gs://" + in + "/run.ipynb"}},
			EnvironmentSpec:   &aiplatformpb.NotebookExecutionJob_NotebookRuntimeTemplateResourceName{NotebookRuntimeTemplateResourceName: tpl.GetName()},
			ExecutionSink:     &aiplatformpb.NotebookExecutionJob_GcsOutputUri{GcsOutputUri: "gs://" + out},
			ExecutionIdentity: &aiplatformpb.NotebookExecutionJob_ExecutionUser{ExecutionUser: "dev@example.com"}}}}
	every := &aiplatformpb.Schedule_Cron{Cron: "TZ=America/New_York * * * * *"}
	for what, bad := range map[string]*aiplatformpb.Schedule{
		"no display name":             {TimeSpecification: every, MaxConcurrentRunCount: 1, Request: request},
		"no cron":                     {DisplayName: "x", MaxConcurrentRunCount: 1, Request: request},
		"a bad cron":                  {DisplayName: "x", TimeSpecification: &aiplatformpb.Schedule_Cron{Cron: "every minute"}, MaxConcurrentRunCount: 1, Request: request},
		"no max_concurrent_run_count": {DisplayName: "x", TimeSpecification: every, Request: request},
		"no request":                  {DisplayName: "x", TimeSpecification: every, MaxConcurrentRunCount: 1},
	} {
		if _, err := sc.CreateSchedule(ctx, &aiplatformpb.CreateScheduleRequest{Parent: parent, Schedule: bad}); quotaCode(err) != codes.InvalidArgument {
			t.Errorf("create with %s = %v, want INVALID_ARGUMENT", what, err)
		}
	}
	if _, err := sc.CreateSchedule(ctx, &aiplatformpb.CreateScheduleRequest{Parent: parent, Schedule: &aiplatformpb.Schedule{DisplayName: "pipeline", TimeSpecification: every,
		MaxConcurrentRunCount: 1, Request: &aiplatformpb.Schedule_CreatePipelineJobRequest{CreatePipelineJobRequest: &aiplatformpb.CreatePipelineJobRequest{Parent: parent}}}}); quotaCode(err) != codes.Unimplemented {
		t.Errorf("a pipeline schedule = %v, want UNIMPLEMENTED", err)
	}
	once, err := sc.CreateSchedule(ctx, &aiplatformpb.CreateScheduleRequest{Parent: parent, Schedule: &aiplatformpb.Schedule{DisplayName: "Once " + suffix,
		TimeSpecification: every, MaxConcurrentRunCount: 1, MaxRunCount: 1, Request: request}})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	t.Cleanup(func() {
		if op, err := sc.DeleteSchedule(context.Background(), &aiplatformpb.DeleteScheduleRequest{Name: once.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if once.GetState() != aiplatformpb.Schedule_ACTIVE || once.GetNextRunTime() == nil || once.GetStartTime() == nil || once.GetCreateTime() == nil {
		t.Errorf("created schedule = %v", once)
	}
	paused, err := sc.CreateSchedule(ctx, &aiplatformpb.CreateScheduleRequest{Parent: parent, Schedule: &aiplatformpb.Schedule{DisplayName: "Paused " + suffix,
		TimeSpecification: &aiplatformpb.Schedule_Cron{Cron: "0 3 * * MON"}, MaxConcurrentRunCount: 2, Request: request}})
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.PauseSchedule(ctx, &aiplatformpb.PauseScheduleRequest{Name: paused.GetName()}); err != nil {
		t.Fatalf("PauseSchedule: %v", err)
	}
	if g, err := sc.GetSchedule(ctx, &aiplatformpb.GetScheduleRequest{Name: paused.GetName()}); err != nil || g.GetState() != aiplatformpb.Schedule_PAUSED || g.GetLastPauseTime() == nil {
		t.Errorf("paused = %v, %v", g, err)
	}
	if code, body := notebookJSON(t, http.MethodPost, "http://"+addr+"/v1/"+paused.GetName()+":resume", `{"catchUp":false}`); code != 200 {
		t.Errorf("JSON resume = %d %s", code, body)
	}
	if g, _ := sc.GetSchedule(ctx, &aiplatformpb.GetScheduleRequest{Name: paused.GetName()}); g.GetState() != aiplatformpb.Schedule_ACTIVE || g.GetLastResumeTime() == nil {
		t.Errorf("resumed = %v", g)
	}
	up, err := sc.UpdateSchedule(ctx, &aiplatformpb.UpdateScheduleRequest{Schedule: &aiplatformpb.Schedule{Name: paused.GetName(), DisplayName: "Renamed " + suffix},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	if err != nil || up.GetDisplayName() != "Renamed "+suffix || up.GetCron() != "0 3 * * MON" {
		t.Errorf("UpdateSchedule = %v, %v", up, err)
	}
	if _, err := sc.UpdateSchedule(ctx, &aiplatformpb.UpdateScheduleRequest{Schedule: &aiplatformpb.Schedule{Name: paused.GetName()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); quotaCode(err) != codes.InvalidArgument {
		t.Errorf("an update of state = %v, want INVALID_ARGUMENT", err)
	}

	// The every-minute schedule's one run.
	var job *aiplatformpb.NotebookExecutionJob
	deadline := time.Now().Add(3 * time.Minute)
	for job == nil {
		it := c.ListNotebookExecutionJobs(ctx, &aiplatformpb.ListNotebookExecutionJobsRequest{Parent: parent, Filter: `schedule="` + once.GetName() + `"`})
		if j, err := it.Next(); err == nil {
			job = j
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the every-minute schedule started no execution in 3 minutes")
		}
		time.Sleep(3 * time.Second)
	}
	if job.GetDisplayName() != "Once "+suffix || job.GetScheduleResourceName() != once.GetName() {
		t.Errorf("the scheduled execution = %v", job)
	}
	if j := waitExecution(ctx, t, c, job.GetName()); j.GetJobState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED {
		t.Errorf("the scheduled execution ended %v: %v", j.GetJobState(), j.GetStatus())
	}
	if !strings.Contains(string(dfReadObject(ctx, t, gcs, out, job.GetName()[strings.LastIndex(job.GetName(), "/")+1:]+"/run.ipynb")), "42") {
		t.Error("the scheduled execution's notebook has no output 42")
	}
	g, err := sc.GetSchedule(ctx, &aiplatformpb.GetScheduleRequest{Name: once.GetName()})
	if err != nil || g.GetState() != aiplatformpb.Schedule_COMPLETED || g.GetStartedRunCount() != 1 ||
		g.GetLastScheduledRunResponse().GetRunResponse() != job.GetName() {
		t.Errorf("after its run the schedule = %v, %v", g, err)
	}
	if err := sc.PauseSchedule(ctx, &aiplatformpb.PauseScheduleRequest{Name: once.GetName()}); quotaCode(err) != codes.FailedPrecondition {
		t.Errorf("pausing a completed schedule = %v, want FAILED_PRECONDITION", err)
	}
	var listed []string
	for it := sc.ListSchedules(ctx, &aiplatformpb.ListSchedulesRequest{Parent: parent, Filter: `state="COMPLETED" AND create_notebook_execution_job_request:*`}); ; {
		s, err := it.Next()
		if err != nil {
			break
		}
		listed = append(listed, s.GetName())
	}
	if len(listed) == 0 || !strings.Contains(strings.Join(listed, ","), once.GetName()) || strings.Contains(strings.Join(listed, ","), paused.GetName()) {
		t.Errorf("COMPLETED schedules = %v", listed)
	}
	op, err := sc.DeleteSchedule(ctx, &aiplatformpb.DeleteScheduleRequest{Name: once.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetNotebookExecutionJob(ctx, &aiplatformpb.GetNotebookExecutionJobRequest{Name: job.GetName()}); err != nil {
		t.Errorf("the deleted schedule's run: %v", err)
	}
	if op, err := sc.DeleteSchedule(ctx, &aiplatformpb.DeleteScheduleRequest{Name: paused.GetName()}); err == nil {
		_ = op.Wait(ctx)
	}
	if op, err := c.DeleteNotebookExecutionJob(ctx, &aiplatformpb.DeleteNotebookExecutionJobRequest{Name: job.GetName()}); err == nil {
		_ = op.Wait(ctx)
	}
}

// covers: aiplatform.projects.locations.notebookRuntimeTemplates.getIamPolicy, aiplatform.projects.locations.notebookRuntimeTemplates.setIamPolicy, aiplatform.projects.locations.notebookRuntimeTemplates.testIamPermissions
//
// TestColabEnterpriseRuntimeTemplateIAM (#2105): with the official
// NotebookClient's IAM methods and the JSON surface (POST
// :getIamPolicy and :testIamPermissions, no body; :setIamPolicy), a new
// template's policy is empty, a binding of the Notebook Runtime User role
// is kept and read back with its etag, a stale etag is refused, and a
// template that does not exist is NOT_FOUND.
func TestColabEnterpriseRuntimeTemplateIAM(t *testing.T) {
	h := New(t)
	addr, clients := notebookClients(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	c := clients["grpc"]
	parent := fmt.Sprintf("projects/%s/locations/us-central1", h.Project())
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	tpl := colabTemplate(ctx, t, c, parent, "iam-"+suffix)
	p, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: tpl.GetName()})
	if err != nil || len(p.GetBindings()) != 0 {
		t.Fatalf("a new template's policy = %v, %v", p, err)
	}
	set, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: tpl.GetName(), Policy: &iampb.Policy{Etag: p.GetEtag(), Bindings: []*iampb.Binding{
		{Role: "roles/aiplatform.notebookRuntimeUser", Members: []string{"user:ana@example.com"}}}}})
	if err != nil || len(set.GetBindings()) != 1 || len(set.GetEtag()) == 0 {
		t.Fatalf("SetIamPolicy = %v, %v", set, err)
	}
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: tpl.GetName(), Policy: &iampb.Policy{Etag: p.GetEtag()}}); err == nil {
		t.Error("a SetIamPolicy with a stale etag succeeded")
	}
	got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: tpl.GetName()})
	if err != nil || len(got.GetBindings()) != 1 || got.GetBindings()[0].GetMembers()[0] != "user:ana@example.com" {
		t.Errorf("GetIamPolicy = %v, %v", got, err)
	}
	perms, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: tpl.GetName(), Permissions: []string{"aiplatform.notebookRuntimeTemplates.apply"}})
	if err != nil || len(perms.GetPermissions()) != 1 {
		t.Errorf("TestIamPermissions = %v, %v", perms, err)
	}
	if _, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: parent + "/notebookRuntimeTemplates/missing-" + suffix}); quotaCode(err) != codes.NotFound {
		t.Errorf("a missing template's policy = %v, want NOT_FOUND", err)
	}
	code, body := notebookJSON(t, http.MethodPost, "http://"+addr+"/v1/"+tpl.GetName()+":getIamPolicy?options.requestedPolicyVersion=3", "")
	var jp struct {
		Bindings []struct {
			Role string `json:"role"`
		} `json:"bindings"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &jp) != nil || len(jp.Bindings) != 1 || jp.Bindings[0].Role != "roles/aiplatform.notebookRuntimeUser" {
		t.Errorf("JSON getIamPolicy = %d %s", code, body)
	}
	if code, body := notebookJSON(t, http.MethodPost, "http://"+addr+"/v1/"+tpl.GetName()+":testIamPermissions?permissions=aiplatform.notebookRuntimeTemplates.get", ""); code != 200 ||
		!strings.Contains(body, "aiplatform.notebookRuntimeTemplates.get") {
		t.Errorf("JSON testIamPermissions = %d %s", code, body)
	}
	if code, body := notebookJSON(t, http.MethodPost, "http://"+addr+"/v1/"+tpl.GetName()+":setIamPolicy", `{"policy":{"bindings":[]}}`); code != 200 {
		t.Errorf("JSON setIamPolicy = %d %s", code, body)
	}
}

// TestIAMEnforcementColabEnterprise (#2105), with --iam-enforcement=on and
// the official NotebookClient: without a role, ListNotebookRuntimeTemplates,
// GetNotebookRuntimeTemplate, AssignNotebookRuntime,
// CreateNotebookExecutionJob and GetIamPolicy are refused naming their
// permissions; Vertex AI Viewer granted on the template's own policy (set
// with SetIamPolicy) lets the principal read that template while the
// project's list and assign stay refused; Colab Enterprise User on the
// project gets assign and create past their checks (to the requests' own
// INVALID_ARGUMENT, so no runtime is started).
func TestIAMEnforcementColabEnterprise(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvLocalAI)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iecolab-")
	p := newIAMPrincipal(t, h, project, "iecolab")
	owner, err := aiplatform.NewNotebookClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	as, err := aiplatform.NewNotebookClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	parent := "projects/" + project + "/locations/us-central1"
	tpl := colabTemplate(ctx, t, owner, parent, "enf")
	assign := func() error {
		// No display name: past the checks this is the request's own
		// INVALID_ARGUMENT, and no runtime starts.
		op, err := as.AssignNotebookRuntime(ctx, &aiplatformpb.AssignNotebookRuntimeRequest{Parent: parent, NotebookRuntimeTemplate: tpl.GetName(),
			NotebookRuntime: &aiplatformpb.NotebookRuntime{RuntimeUser: p.email}})
		if err == nil {
			_, err = op.Wait(ctx)
		}
		return err
	}
	execute := func() error {
		_, err := as.CreateNotebookExecutionJob(ctx, &aiplatformpb.CreateNotebookExecutionJobRequest{Parent: parent, NotebookExecutionJob: &aiplatformpb.NotebookExecutionJob{
			EnvironmentSpec: &aiplatformpb.NotebookExecutionJob_NotebookRuntimeTemplateResourceName{NotebookRuntimeTemplateResourceName: tpl.GetName()}}})
		return err
	}
	list := func() error {
		_, err := as.ListNotebookRuntimeTemplates(ctx, &aiplatformpb.ListNotebookRuntimeTemplatesRequest{Parent: parent}).Next()
		return first(err)
	}
	get := func() error {
		_, err := as.GetNotebookRuntimeTemplate(ctx, &aiplatformpb.GetNotebookRuntimeTemplateRequest{Name: tpl.GetName()})
		return err
	}
	wantDenied(t, "ListNotebookRuntimeTemplates without a role", list(), newerDenied("aiplatform.notebookRuntimeTemplates.list", parent), "aiplatform.notebookRuntimeTemplates.list", true)
	wantDenied(t, "GetNotebookRuntimeTemplate without a role", get(), newerDenied("aiplatform.notebookRuntimeTemplates.get", tpl.GetName()), "aiplatform.notebookRuntimeTemplates.get", true)
	wantDenied(t, "AssignNotebookRuntime without a role", assign(), newerDenied("aiplatform.notebookRuntimes.assign", parent), "aiplatform.notebookRuntimes.assign", true)
	wantDenied(t, "CreateNotebookExecutionJob without a role", execute(), newerDenied("aiplatform.notebookExecutionJobs.create", parent), "aiplatform.notebookExecutionJobs.create", true)
	_, err = as.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: tpl.GetName()})
	wantDenied(t, "GetIamPolicy without a role", err, newerDenied("aiplatform.notebookRuntimeTemplates.getIamPolicy", tpl.GetName()), "aiplatform.notebookRuntimeTemplates.getIamPolicy", true)

	// The template's own policy.
	undo := bindOn(t, owner.GetIamPolicy, owner.SetIamPolicy, tpl.GetName(),
		&iampb.Binding{Role: "roles/aiplatform.viewer", Members: []string{"serviceAccount:" + p.email}})
	t.Cleanup(undo)
	waitGranted(t, "GetNotebookRuntimeTemplate with Vertex AI Viewer on the template", get)
	wantDenied(t, "ListNotebookRuntimeTemplates with the template's role only", list(), newerDenied("aiplatform.notebookRuntimeTemplates.list", parent), "aiplatform.notebookRuntimeTemplates.list", true)
	wantDenied(t, "AssignNotebookRuntime with the template's role only", assign(), newerDenied("aiplatform.notebookRuntimes.assign", parent), "aiplatform.notebookRuntimes.assign", true)

	p.grant("roles/aiplatform.colabEnterpriseUser")
	waitGranted(t, "AssignNotebookRuntime as a Colab Enterprise User", func() error {
		if err := assign(); quotaCode(err) != codes.InvalidArgument {
			return err
		}
		return nil
	})
	if err := execute(); quotaCode(err) != codes.InvalidArgument {
		t.Errorf("CreateNotebookExecutionJob past the checks = %v, want the request's INVALID_ARGUMENT", err)
	}
}
