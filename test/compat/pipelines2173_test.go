//go:build compat

package compat

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/dataform/apiv1"
	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
)

// pipelineImage is the image the pipelines' tasks run: one already on
// this machine's Docker, since CloudBurrow never pulls a task's image.
func pipelineImage(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required")
	}
	img := os.Getenv("CLOUDBURROW_TEST_PIPELINE_IMAGE")
	if img == "" {
		img = "python:3-alpine"
	}
	if err := exec.Command("docker", "image", "inspect", img).Run(); err != nil {
		t.Skipf("the pipeline task image %s is not on this machine's Docker (CLOUDBURROW_TEST_PIPELINE_IMAGE names another)", img)
	}
	return img
}

// pipelineSpec2173 is a compiled KFP pipeline (IR, as kfp 2's compiler
// writes it): produce writes an output parameter and a Dataset artifact,
// consume reads both and writes its result artifact; fail exits 3 when
// asked to.
func pipelineSpec2173(t *testing.T, image string) *structpb.Struct {
	t.Helper()
	raw := `{
 "pipelineInfo": {"name": "hello-pipeline"},
 "schemaVersion": "2.1.0", "sdkVersion": "kfp-2.7.0",
 "root": {
  "inputDefinitions": {"parameters": {"greeting": {"parameterType": "STRING"}, "fail": {"parameterType": "BOOLEAN", "defaultValue": false}}},
  "dag": {"tasks": {
   "produce": {"taskInfo": {"name": "produce"}, "componentRef": {"name": "comp-produce"},
     "inputs": {"parameters": {"msg": {"componentInputParameter": "greeting"}, "fail": {"componentInputParameter": "fail"}}}},
   "consume": {"taskInfo": {"name": "consume"}, "componentRef": {"name": "comp-consume"}, "dependentTasks": ["produce"],
     "inputs": {"parameters": {"word": {"taskOutputParameter": {"producerTask": "produce", "outputParameterKey": "word"}}},
       "artifacts": {"data": {"taskOutputArtifact": {"producerTask": "produce", "outputArtifactKey": "data"}}}}}
  }}
 },
 "components": {
  "comp-produce": {"executorLabel": "exec-produce",
    "inputDefinitions": {"parameters": {"msg": {"parameterType": "STRING"}, "fail": {"parameterType": "BOOLEAN"}}},
    "outputDefinitions": {"parameters": {"word": {"parameterType": "STRING"}}, "artifacts": {"data": {"artifactType": {"schemaTitle": "system.Dataset", "schemaVersion": "0.0.1"}}}}},
  "comp-consume": {"executorLabel": "exec-consume",
    "inputDefinitions": {"parameters": {"word": {"parameterType": "STRING"}}, "artifacts": {"data": {"artifactType": {"schemaTitle": "system.Dataset", "schemaVersion": "0.0.1"}}}},
    "outputDefinitions": {"artifacts": {"result": {"artifactType": {"schemaTitle": "system.Artifact", "schemaVersion": "0.0.1"}}}}}
 },
 "deploymentSpec": {"executors": {
  "exec-produce": {"container": {"image": "IMAGE", "command": ["sh", "-c",
    "test \"$1\" = true && exit 3; mkdir -p \"$(dirname \"$2\")\" \"$(dirname \"$3\")\" && printf '%s' \"$0\" > \"$2\" && printf 'rows' > \"$3\""],
    "args": ["{{$.inputs.parameters['msg']}}", "{{$.inputs.parameters['fail']}}", "{{$.outputs.parameters['word'].output_file}}", "{{$.outputs.artifacts['data'].path}}"]}},
  "exec-consume": {"container": {"image": "IMAGE", "command": ["sh", "-c",
    "test \"$(cat \"$1\")\" = rows && mkdir -p \"$(dirname \"$2\")\" && printf '%s!' \"$0\" > \"$2\""],
    "args": ["{{$.inputs.parameters['word']}}", "{{$.inputs.artifacts['data'].path}}", "{{$.outputs.artifacts['result'].path}}"]}}
 }}
}`
	s := &structpb.Struct{}
	if err := protojson.Unmarshal([]byte(strings.ReplaceAll(raw, "IMAGE", image)), s); err != nil {
		t.Fatal(err)
	}
	return s
}

func pipelineClient(t *testing.T, h *Harness, endpoint, token string) *aiplatform.PipelineClient {
	t.Helper()
	opts := []option.ClientOption{option.WithEndpoint(endpoint), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	if token != "" {
		opts = grpcAs(endpoint, token)
	}
	c, err := aiplatform.NewPipelineClient(h.Context(), opts...)
	if err != nil {
		t.Fatalf("aiplatform.NewPipelineClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// waitPipelineJob waits for a run to end.
func waitPipelineJob(ctx context.Context, t *testing.T, c *aiplatform.PipelineClient, name string) *aiplatformpb.PipelineJob {
	t.Helper()
	for {
		j, err := c.GetPipelineJob(ctx, &aiplatformpb.GetPipelineJobRequest{Name: name})
		if err != nil {
			t.Fatalf("GetPipelineJob: %v", err)
		}
		switch j.GetState() {
		case aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED, aiplatformpb.PipelineState_PIPELINE_STATE_FAILED, aiplatformpb.PipelineState_PIPELINE_STATE_CANCELLED:
			return j
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s did not end: %v", name, j.GetState())
		case <-time.After(time.Second):
		}
	}
}

func pipelineTasks(j *aiplatformpb.PipelineJob) map[string]*aiplatformpb.PipelineTaskDetail {
	out := map[string]*aiplatformpb.PipelineTaskDetail{}
	for _, d := range j.GetJobDetail().GetTaskDetails() {
		out[d.GetTaskName()] = d
	}
	return out
}

func readGCSURI(ctx context.Context, t *testing.T, c *storage.Client, uri string) string {
	t.Helper()
	bucket, object, _ := strings.Cut(strings.TrimPrefix(uri, "gs://"), "/")
	r, err := c.Bucket(bucket).Object(object).NewReader(ctx)
	if err != nil {
		t.Fatalf("read %s: %v", uri, err)
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

// covers: google.cloud.aiplatform.v1.PipelineService/CreatePipelineJob, google.cloud.aiplatform.v1.PipelineService/GetPipelineJob, google.cloud.aiplatform.v1.PipelineService/ListPipelineJobs, google.cloud.aiplatform.v1.PipelineService/DeletePipelineJob, google.cloud.aiplatform.v1.PipelineService/CancelPipelineJob, google.cloud.aiplatform.v1.PipelineService/BatchDeletePipelineJobs, google.cloud.aiplatform.v1.PipelineService/BatchCancelPipelineJobs
//
// TestVertexAIPipelineJobs (#2173), with the official aiplatform
// PipelineClient: a compiled KFP pipeline runs its two tasks as
// containers, the second after the first, taking its output parameter
// and its Dataset artifact, which is written under the run's Cloud
// Storage location; the run and its tasks are SUCCEEDED; a task that
// exits non-zero FAILS the run, with Google's message naming it, and its
// downstream task is SKIPPED; a missing required parameter, a missing
// output directory and a bad ID are INVALID_ARGUMENT; the list filters
// by display name and state; a running run cannot be deleted, an ended
// one cannot be cancelled; BatchDelete removes runs.
func TestVertexAIPipelineJobs(t *testing.T) {
	h := New(t)
	endpoint := runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	gcs := dataformStorage(t, h)
	image := pipelineImage(t)
	c := pipelineClient(t, h, endpoint, "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)
	project := h.Project()
	parent := fmt.Sprintf("projects/%s/locations/us-central1", project)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	bucket := "pl-root-" + suffix
	dataformBucket(ctx, t, gcs, project, bucket)
	spec := pipelineSpec2173(t, image)
	job := func(name string, params map[string]any, out string) *aiplatformpb.PipelineJob {
		pv := map[string]*structpb.Value{}
		for k, v := range params {
			pv[k], _ = structpb.NewValue(v)
		}
		return &aiplatformpb.PipelineJob{DisplayName: name, PipelineSpec: spec, Labels: map[string]string{"suite": "compat"},
			RuntimeConfig: &aiplatformpb.PipelineJob_RuntimeConfig{GcsOutputDirectory: out, ParameterValues: pv}}
	}
	root := "gs://" + bucket + "/runs"
	for what, req := range map[string]*aiplatformpb.CreatePipelineJobRequest{
		"no greeting":         {Parent: parent, PipelineJob: job("x", nil, root)},
		"no output directory": {Parent: parent, PipelineJob: job("x", map[string]any{"greeting": "hi"}, "")},
		"a bad ID":            {Parent: parent, PipelineJobId: "Bad_ID", PipelineJob: job("x", map[string]any{"greeting": "hi"}, root)},
	} {
		if _, err := c.CreatePipelineJob(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreatePipelineJob with %s = %v, want INVALID_ARGUMENT", what, err)
		}
	}

	ok, err := c.CreatePipelineJob(ctx, &aiplatformpb.CreatePipelineJobRequest{Parent: parent, PipelineJobId: "hello-" + suffix, PipelineJob: job("hello "+suffix, map[string]any{"greeting": "hi"}, root)})
	if err != nil {
		t.Fatalf("CreatePipelineJob: %v", err)
	}
	if ok.GetName() != parent+"/pipelineJobs/hello-"+suffix {
		t.Errorf("name = %s", ok.GetName())
	}
	if op, err := c.DeletePipelineJob(ctx, &aiplatformpb.DeletePipelineJobRequest{Name: ok.GetName()}); err == nil {
		if err = op.Wait(ctx); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("DeletePipelineJob of a running run = %v, want FAILED_PRECONDITION", err)
		}
	} else if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeletePipelineJob of a running run = %v, want FAILED_PRECONDITION", err)
	}
	done := waitPipelineJob(ctx, t, c, ok.GetName())
	if done.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED {
		t.Fatalf("the run = %v %v", done.GetState(), done.GetError())
	}
	tasks := pipelineTasks(done)
	for _, n := range []string{"produce", "consume"} {
		if tasks[n].GetState() != aiplatformpb.PipelineTaskDetail_SUCCEEDED {
			t.Errorf("task %s = %v", n, tasks[n])
		}
	}
	data := tasks["produce"].GetOutputs()["data"].GetArtifacts()
	result := tasks["consume"].GetOutputs()["result"].GetArtifacts()
	if len(data) != 1 || !strings.HasPrefix(data[0].GetUri(), root+"/hello-"+suffix+"/") || len(result) != 1 {
		t.Fatalf("artifacts = %v, %v", data, result)
	}
	if got := readGCSURI(ctx, t, gcs, data[0].GetUri()); got != "rows" {
		t.Errorf("produce's Dataset = %q", got)
	}
	if got := readGCSURI(ctx, t, gcs, result[0].GetUri()); got != "hi!" {
		t.Errorf("consume's result = %q, want the parameter it took and its suffix", got)
	}
	if err := c.CancelPipelineJob(ctx, &aiplatformpb.CancelPipelineJobRequest{Name: ok.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("CancelPipelineJob of an ended run = %v, want FAILED_PRECONDITION", err)
	}

	bad, err := c.CreatePipelineJob(ctx, &aiplatformpb.CreatePipelineJobRequest{Parent: parent, PipelineJob: job("fails "+suffix, map[string]any{"greeting": "hi", "fail": true}, root)})
	if err != nil {
		t.Fatalf("CreatePipelineJob: %v", err)
	}
	failed := waitPipelineJob(ctx, t, c, bad.GetName())
	ft := pipelineTasks(failed)
	if failed.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_FAILED || ft["produce"].GetState() != aiplatformpb.PipelineTaskDetail_FAILED ||
		ft["consume"].GetState() != aiplatformpb.PipelineTaskDetail_SKIPPED {
		t.Errorf("the failing run = %v, tasks %v", failed.GetState(), failed.GetJobDetail())
	}
	if msg := failed.GetError().GetMessage(); !strings.Contains(msg, "The DAG failed because some tasks failed. The failed tasks are: [produce].") {
		t.Errorf("the failing run's error = %q", msg)
	}

	var names []string
	for it := c.ListPipelineJobs(ctx, &aiplatformpb.ListPipelineJobsRequest{Parent: parent, Filter: `display_name="hello ` + suffix + `" AND state="PIPELINE_STATE_SUCCEEDED"`}); ; {
		j, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListPipelineJobs: %v", err)
		}
		names = append(names, j.GetName())
	}
	if len(names) != 1 || names[0] != ok.GetName() {
		t.Errorf("the filtered list = %v", names)
	}
	op, err := c.BatchDeletePipelineJobs(ctx, &aiplatformpb.BatchDeletePipelineJobsRequest{Parent: parent, Names: []string{ok.GetName(), bad.GetName()}})
	if err != nil {
		t.Fatalf("BatchDeletePipelineJobs: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("BatchDeletePipelineJobs: %v", err)
	}
	if _, err := c.GetPipelineJob(ctx, &aiplatformpb.GetPipelineJobRequest{Name: ok.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("a deleted run = %v, want NOT_FOUND", err)
	}
}

// covers: google.cloud.aiplatform.v1.ScheduleService/CreateSchedule
//
// TestVertexAIPipelineSchedules (#2173), with the official ScheduleClient
// and PipelineClient: a schedule of a create_pipeline_job_request starts a
// run every minute, the run naming its schedule; a pipeline_job_id in the
// template is INVALID_ARGUMENT ("User-specified ID is not supported in the
// request template", as Google says the scheduler refuses it).
func TestVertexAIPipelineSchedules(t *testing.T) {
	h := New(t)
	endpoint, sc := scheduleClient(t, h)
	gcs := dataformStorage(t, h)
	image := pipelineImage(t)
	c := pipelineClient(t, h, endpoint, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	project := h.Project()
	parent := fmt.Sprintf("projects/%s/locations/us-central1", project)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	bucket := "pl-sched-" + suffix
	dataformBucket(ctx, t, gcs, project, bucket)
	pv := map[string]*structpb.Value{"greeting": structpb.NewStringValue("hey")}
	req := func(id string) *aiplatformpb.Schedule_CreatePipelineJobRequest {
		return &aiplatformpb.Schedule_CreatePipelineJobRequest{CreatePipelineJobRequest: &aiplatformpb.CreatePipelineJobRequest{Parent: parent, PipelineJobId: id,
			PipelineJob: &aiplatformpb.PipelineJob{DisplayName: "scheduled", PipelineSpec: pipelineSpec2173(t, image),
				RuntimeConfig: &aiplatformpb.PipelineJob_RuntimeConfig{GcsOutputDirectory: "gs://" + bucket, ParameterValues: pv}}}}
	}
	every := &aiplatformpb.Schedule_Cron{Cron: "* * * * *"}
	if _, err := sc.CreateSchedule(ctx, &aiplatformpb.CreateScheduleRequest{Parent: parent, Schedule: &aiplatformpb.Schedule{DisplayName: "x", TimeSpecification: every,
		MaxConcurrentRunCount: 1, Request: req("mine")}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a template with a pipeline_job_id = %v, want INVALID_ARGUMENT", err)
	}
	s, err := sc.CreateSchedule(ctx, &aiplatformpb.CreateScheduleRequest{Parent: parent, Schedule: &aiplatformpb.Schedule{DisplayName: "pipelines " + suffix,
		TimeSpecification: every, MaxConcurrentRunCount: 1, MaxRunCount: 1, Request: req("")}})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	t.Cleanup(func() {
		if op, err := sc.DeleteSchedule(context.Background(), &aiplatformpb.DeleteScheduleRequest{Name: s.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	var run *aiplatformpb.PipelineJob
	for run == nil {
		it := c.ListPipelineJobs(ctx, &aiplatformpb.ListPipelineJobsRequest{Parent: parent})
		for {
			j, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("ListPipelineJobs: %v", err)
			}
			if j.GetScheduleName() == s.GetName() {
				run = j
			}
		}
		if run == nil {
			select {
			case <-ctx.Done():
				t.Fatal("the schedule started no run")
			case <-time.After(2 * time.Second):
			}
		}
	}
	if done := waitPipelineJob(ctx, t, c, run.GetName()); done.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED {
		t.Errorf("the scheduled run = %v %v", done.GetState(), done.GetError())
	}
	got, err := sc.GetSchedule(ctx, &aiplatformpb.GetScheduleRequest{Name: s.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetStartedRunCount() != 1 || got.GetLastScheduledRunResponse().GetRunResponse() != run.GetName() {
		t.Errorf("the schedule after its run = %d runs, last %v", got.GetStartedRunCount(), got.GetLastScheduledRunResponse())
	}
}

// covers: google.cloud.aiplatform.v1.NotebookService/StopNotebookRuntime
//
// TestIAMEnforcementVertexAIRunIdentity (#2173), with --iam-enforcement=on
// and the official PipelineClient and NotebookClient: StopNotebookRuntime
// is refused naming aiplatform.notebookRuntimes.update; a pipeline run
// naming a service account is refused, even to a Vertex AI User, until the
// caller holds the Service Account User role on that account (actAs); the
// run then runs as the account: with no Cloud Storage role its first task
// cannot write its artifact and the run FAILS, and once the account is
// granted Storage Object Admin the next run SUCCEEDS.
func TestIAMEnforcementVertexAIRunIdentity(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvLocalAI)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	image := pipelineImage(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)
	project := iamProject(t, h, "ierunas-")
	p := newIAMPrincipal(t, h, project, "ierunas")
	nb, err := aiplatform.NewNotebookClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nb.Close() })
	parent := "projects/" + project + "/locations/us-central1"
	stop := parent + "/notebookRuntimes/none"
	_, err = nb.StopNotebookRuntime(ctx, &aiplatformpb.StopNotebookRuntimeRequest{Name: stop})
	wantDenied(t, "StopNotebookRuntime without a role", err, newerDenied("aiplatform.notebookRuntimes.update", stop), "aiplatform.notebookRuntimes.update", true)

	gcs, err := storage.NewClient(ctx, option.WithEndpoint("http://"+stripScheme(h.Endpoint(EnvStorage))+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gcs.Close() })
	bucket := fmt.Sprintf("ierunas-%d", time.Now().UnixNano()%1e8)
	dataformBucket(ctx, t, gcs, project, bucket)
	runner := newServiceAccount(t, h, project, "plrunner")
	as := pipelineClient(t, h, endpoint, p.token)
	create := func() (*aiplatformpb.PipelineJob, error) {
		return as.CreatePipelineJob(ctx, &aiplatformpb.CreatePipelineJobRequest{Parent: parent, PipelineJob: &aiplatformpb.PipelineJob{
			DisplayName: "as the runner", PipelineSpec: pipelineSpec2173(t, image), ServiceAccount: runner.GetEmail(),
			RuntimeConfig: &aiplatformpb.PipelineJob_RuntimeConfig{GcsOutputDirectory: "gs://" + bucket,
				ParameterValues: map[string]*structpb.Value{"greeting": structpb.NewStringValue("hi")}}}})
	}
	_, err = create()
	wantDenied(t, "CreatePipelineJob without a role", err, newerDenied("aiplatform.pipelineJobs.create", parent), "aiplatform.pipelineJobs.create", true)
	p.grant("roles/aiplatform.user")
	var lastErr error
	for i := 0; i < 20; i++ {
		if _, lastErr = create(); status.Code(lastErr) == codes.PermissionDenied && strings.Contains(status.Convert(lastErr).Message(), "iam.serviceAccounts.actAs") {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if status.Code(lastErr) != codes.PermissionDenied || !strings.Contains(status.Convert(lastErr).Message(), "iam.serviceAccounts.actAs") ||
		!strings.Contains(status.Convert(lastErr).Message(), runner.GetEmail()) {
		t.Fatalf("CreatePipelineJob as a Vertex AI User without actAs = %v, want PERMISSION_DENIED on iam.serviceAccounts.actAs", lastErr)
	}
	grantOn(t, h, runner.GetName(), "roles/iam.serviceAccountUser", "serviceAccount:"+p.email)
	var job *aiplatformpb.PipelineJob
	waitGranted(t, "CreatePipelineJob with actAs on the runner", func() (err error) { job, err = create(); return err })
	failed := waitPipelineJob(ctx, t, as, job.GetName())
	if failed.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_FAILED || pipelineTasks(failed)["produce"].GetState() != aiplatformpb.PipelineTaskDetail_FAILED {
		t.Errorf("a run as an account with no Cloud Storage role = %v %v, want FAILED at produce", failed.GetState(), failed.GetJobDetail())
	}
	if msg := pipelineTasks(failed)["produce"].GetError().GetMessage(); !strings.Contains(msg, "storage.objects.create") {
		t.Errorf("produce's error = %q, want Cloud Storage's refusal of the runner", msg)
	}
	change := projectRoles(t, h, project, "serviceAccount:"+runner.GetEmail())
	change("roles/storage.objectAdmin", true)
	t.Cleanup(func() { change("roles/storage.objectAdmin", false) })
	var done *aiplatformpb.PipelineJob
	for i := 0; i < 5; i++ {
		job, err = create()
		if err != nil {
			t.Fatalf("CreatePipelineJob: %v", err)
		}
		if done = waitPipelineJob(ctx, t, as, job.GetName()); done.GetState() == aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED {
			break
		}
		time.Sleep(time.Second)
	}
	if done.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED {
		t.Errorf("a run as the runner granted Storage Object Admin = %v %v", done.GetState(), done.GetError())
	}
}

// TestIAMEnforcementDataformStrictActAs (#2173), with --iam-enforcement=on
// and the official Dataform client: a repository naming a custom service
// account is refused until the caller holds iam.serviceAccounts.actAs on it
// (the Service Account User role), as Dataform's strict act-as mode checks
// it on create and update ("Use strict act-as mode",
// https://docs.cloud.google.com/dataform/docs/strict-act-as-mode).
func TestIAMEnforcementDataformStrictActAs(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvDataform)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iedfas-")
	p := newIAMPrincipal(t, h, project, "iedfas")
	runner := newServiceAccount(t, h, project, "dfrunner")
	df, err := dataform.NewClient(ctx, grpcAs(addr, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = df.Close() })
	parent := "projects/" + project + "/locations/us-central1"
	id := fmt.Sprintf("asrepo%d", time.Now().UnixNano()%1e8)
	create := func() error {
		_, err := df.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: id, Repository: &dfpb.Repository{ServiceAccount: runner.GetEmail()}})
		return err
	}
	if err := create(); status.Code(err) != codes.PermissionDenied || !strings.Contains(status.Convert(err).Message(), "iam.serviceAccounts.actAs") {
		t.Fatalf("CreateRepository without actAs on its service account = %v, want PERMISSION_DENIED on iam.serviceAccounts.actAs", err)
	}
	grantOn(t, h, runner.GetName(), "roles/iam.serviceAccountUser", "serviceAccount:"+p.email)
	waitGranted(t, "CreateRepository with actAs", create)
	t.Cleanup(func() {
		_ = df.DeleteRepository(context.Background(), &dfpb.DeleteRepositoryRequest{Name: parent + "/repositories/" + id, Force: true})
	})
	other := newServiceAccount(t, h, project, "dfother")
	_, err = df.UpdateRepository(ctx, &dfpb.UpdateRepositoryRequest{Repository: &dfpb.Repository{Name: parent + "/repositories/" + id, ServiceAccount: other.GetEmail()}})
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(status.Convert(err).Message(), "iam.serviceAccounts.actAs") {
		t.Errorf("UpdateRepository to an account without actAs = %v, want PERMISSION_DENIED", err)
	}
}
