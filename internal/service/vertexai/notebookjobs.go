package vertexai

// Colab Enterprise notebook executions (#2105): NotebookService's
// NotebookExecutionJob methods over gRPC and REST. An execution runs its
// notebook on a runtime of its own made from its runtime template (or its
// custom environment, or the default specifications) on the instance's
// notebook runtime (internal/nbruntime), and writes the executed notebook
// to Cloud Storage under its gcs_output_uri.
//
// Documented behaviour (the v1 discovery document, revision 20261114;
// https://docs.cloud.google.com/colab/docs/schedule-notebook-run;
// https://docs.cloud.google.com/gemini-enterprise-agent-platform/reference/rest/v1/projects.locations.notebookExecutionJobs/create;
// read 2026-11-22):
//   - the source is one of dataform_repository_source ("a single file
//     notebook repository", read "at HEAD" when commit_sha is unset),
//     gcs_notebook_source (gs://bucket/notebook_file.ipynb, at an optional
//     generation) or direct_notebook_source (the notebook's contents);
//   - gcs_output_uri is gs://bucket-name; "Results are saved as notebook
//     (IPYNB) files in the bucket";
//   - execution_timeout defaults to 86400s (24 hours);
//   - display_name is up to 128 characters; labels as everywhere in
//     Vertex AI;
//   - the list filters notebookExecutionJob (the ID, = and !=),
//     displayName (= != and regex) and schedule (= != and regex), and
//     orders by display_name, create_time and update_time; view BASIC (the
//     default) leaves out direct notebook inputs, FULL includes them;
//   - create answers an Operation; "Deleting a schedule doesn't delete the
//     runs it generated."
//
// UNVERIFIED (re-read 2026-11-22: https://docs.cloud.google.com/colab/docs/schedule-notebook-run):
// the executed notebook's object is JOB_ID/FILE.ipynb under the output URI
// (FILE the source's file name: content.ipynb for a Dataform single-file
// repository, the object's base name for Cloud Storage, notebook.ipynb for
// direct contents), as for Dataform's notebook actions; the create
// operation is done once the job is recorded (PENDING), its run going on
// after; an execution identity (execution_user or service_account) is
// required, as gcloud's --user-email and --service-account are; with no
// environment the template's defaults apply; kernel_name may be empty or
// python3, the runtime's one kernel.
//
// Local differences: a Workbench runtime (workbench_runtime) is not served
// (notebooks.googleapis.com is not run), FAILED_PRECONDITION; a job
// running when the instance stopped is FAILED when it starts again. The
// job runs as the identity it names (runas.go, #2173): its Cloud Storage,
// Dataform and BigQuery calls carry that identity's token.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// NotebookExecutor runs notebook executions: it reads their notebooks,
// runs them on a fresh runtime and writes the results, adapted in
// cmd/cloudburrow over the instance's Cloud Storage, Dataform and notebook
// runtime.
type NotebookExecutor interface {
	// ReadObject reads gs://bucket/object at generation (0: the live one).
	ReadObject(ctx context.Context, bucket, object string, generation int64) ([]byte, error)
	// WriteObject writes an object.
	WriteObject(ctx context.Context, bucket, object, contentType string, data []byte) error
	// ReadRepositoryFile reads a file of a Dataform repository at commit
	// ("" for HEAD).
	ReadRepositoryFile(ctx context.Context, repository, commit, file string) ([]byte, error)
	// Run runs the notebook's code cells on a new runtime named key in
	// project and returns the executed notebook; when a cell raises it
	// returns the notebook up to that cell with the error.
	Run(ctx context.Context, key, project string, notebook []byte, o NotebookRuntimeOptions) ([]byte, error)
}

const (
	defaultExecutionTimeout = 24 * time.Hour
	// dataformNotebookFile is a single-file notebook repository's file, as
	// BigQuery Studio and Colab Enterprise write it.
	dataformNotebookFile = "content.ipynb"
)

var (
	repositoryRE = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/repositories/[^/]+$`)
	jobFilter    = append([]aipfilter.Option{
		aipfilter.Fields("notebook_execution_job", "display_name", "schedule", "schedule_resource_name", "job_state", "labels", "create_time", "update_time"),
		aipfilter.Regex("displayName", jobDisplayName), aipfilter.Regex("display_name", jobDisplayName),
		aipfilter.Regex("schedule", jobSchedule), aipfilter.Regex("scheduleResourceName", jobSchedule), aipfilter.Regex("schedule_resource_name", jobSchedule),
	}, bothCases(map[string]func(proto.Message) string{
		"notebook_execution_job": func(m proto.Message) string { return lastSegment(m.(*aiplatformpb.NotebookExecutionJob).GetName()) },
	}, map[string]string{"schedule": "schedule_resource_name"})...)
)

func jobDisplayName(m proto.Message) string {
	return m.(*aiplatformpb.NotebookExecutionJob).GetDisplayName()
}
func jobSchedule(m proto.Message) string {
	return m.(*aiplatformpb.NotebookExecutionJob).GetScheduleResourceName()
}

// jobEntry is an execution and what its run needs.
type jobEntry struct {
	Job    *aiplatformpb.NotebookExecutionJob
	cancel context.CancelFunc
	// runAs is the account the run executes as, "" for the instance's
	// own credentials (runas.go).
	runAs string
}

// SetNotebookExecutor runs notebook executions and schedules on x, the
// instance's Cloud Storage, Dataform and notebook runtime.
func (s *Server) SetNotebookExecutor(x NotebookExecutor) {
	st := s.notebooks()
	st.mu.Lock()
	st.exec = x
	st.mu.Unlock()
	s.startScheduler()
}

// splitNotebookGCS splits gs://bucket/object; object may be empty when allowed.
func splitNotebookGCS(field, uri string, needObject bool) (string, string, error) {
	rest, ok := strings.CutPrefix(uri, "gs://")
	bucket, object, _ := strings.Cut(rest, "/")
	if !ok || bucket == "" || (needObject && object == "") {
		if needObject {
			return "", "", apierror.InvalidArgument("%s %q must be gs://bucket/notebook_file.ipynb", field, uri)
		}
		return "", "", apierror.InvalidArgument("%s %q must be gs://bucket-name", field, uri)
	}
	return bucket, strings.TrimSuffix(object, "/"), nil
}

// checkJob validates an execution to create (prefix names its field in
// errors) and fills its defaults; the caller holds st.mu.
func (st *notebookStore) checkJob(prefix, parent string, j *aiplatformpb.NotebookExecutionJob) error {
	if utf8.RuneCountInString(j.GetDisplayName()) > 128 {
		return apierror.InvalidArgument("%sdisplay_name can be up to 128 characters long", prefix)
	}
	if err := checkNotebookLabels(j.GetLabels()); err != nil {
		return err
	}
	switch src := j.GetNotebookSource().(type) {
	case nil:
		return apierror.InvalidArgument("%snotebook_source is required: one of dataform_repository_source, gcs_notebook_source or direct_notebook_source", prefix)
	case *aiplatformpb.NotebookExecutionJob_DataformRepositorySource_:
		if !repositoryRE.MatchString(src.DataformRepositorySource.GetDataformRepositoryResourceName()) {
			return apierror.InvalidArgument("%sdataform_repository_source.dataform_repository_resource_name %q must be projects/{project_id}/locations/{location}/repositories/{repository_id}",
				prefix, src.DataformRepositorySource.GetDataformRepositoryResourceName())
		}
	case *aiplatformpb.NotebookExecutionJob_GcsNotebookSource_:
		if _, _, err := splitNotebookGCS(prefix+"gcs_notebook_source.uri", src.GcsNotebookSource.GetUri(), true); err != nil {
			return err
		}
		if g := src.GcsNotebookSource.GetGeneration(); g != "" {
			if _, err := strconv.ParseInt(g, 10, 64); err != nil {
				return apierror.InvalidArgument("%sgcs_notebook_source.generation %q is not a generation number", prefix, g)
			}
		}
	case *aiplatformpb.NotebookExecutionJob_DirectNotebookSource_:
		var nb map[string]any
		if json.Unmarshal(src.DirectNotebookSource.GetContent(), &nb) != nil {
			return apierror.InvalidArgument("%sdirect_notebook_source.content is not an IPYNB notebook", prefix)
		}
	}
	if _, ok := j.GetExecutionSink().(*aiplatformpb.NotebookExecutionJob_GcsOutputUri); !ok {
		return apierror.InvalidArgument("%sgcs_output_uri is required", prefix)
	}
	if _, _, err := splitNotebookGCS(prefix+"gcs_output_uri", j.GetGcsOutputUri(), false); err != nil {
		return err
	}
	switch id := j.GetExecutionIdentity().(type) {
	case nil:
		return apierror.InvalidArgument("%sexecution_identity is required: execution_user or service_account", prefix)
	case *aiplatformpb.NotebookExecutionJob_ExecutionUser:
		if !strings.Contains(id.ExecutionUser, "@") {
			return apierror.InvalidArgument("%sexecution_user %q is not an email address", prefix, id.ExecutionUser)
		}
	case *aiplatformpb.NotebookExecutionJob_ServiceAccount:
		if !strings.Contains(id.ServiceAccount, "@") {
			return apierror.InvalidArgument("%sservice_account %q is not a service account email", prefix, id.ServiceAccount)
		}
	}
	if j.GetWorkbenchRuntime() != nil {
		return apierror.FailedPrecondition("%sworkbench_runtime: Vertex AI Workbench instances are not served on this instance; run the notebook on a Colab Enterprise runtime template", prefix)
	}
	if k := j.GetKernelName(); k != "" && k != "python3" {
		return apierror.InvalidArgument("%skernel_name %q: the runtime's kernel is python3", prefix, k)
	}
	if t := j.GetExecutionTimeout(); t != nil && t.AsDuration() <= 0 {
		return apierror.InvalidArgument("%sexecution_timeout must be positive", prefix)
	}
	if j.ExecutionTimeout == nil {
		j.ExecutionTimeout = durationpb.New(defaultExecutionTimeout)
	}
	switch env := j.GetEnvironmentSpec().(type) {
	case *aiplatformpb.NotebookExecutionJob_NotebookRuntimeTemplateResourceName:
		t, err := st.template(env.NotebookRuntimeTemplateResourceName)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(t.GetName(), parent+"/") {
			return apierror.InvalidArgument("%snotebook_runtime_template_resource_name %s must be in %s", prefix, t.GetName(), parent)
		}
		if err := runnable(t.GetMachineSpec()); err != nil {
			return err
		}
		if u := t.GetSoftwareConfig().GetPostStartupScriptConfig().GetPostStartupScriptUrl(); u != "" {
			return scriptURLError(u)
		}
		if j.EncryptionSpec == nil && t.EncryptionSpec != nil {
			j.EncryptionSpec = proto.Clone(t.EncryptionSpec).(*aiplatformpb.EncryptionSpec)
		}
	case *aiplatformpb.NotebookExecutionJob_CustomEnvironmentSpec_:
		if err := runnable(env.CustomEnvironmentSpec.GetMachineSpec()); err != nil {
			return err
		}
	}
	return nil
}

// jobView is a job as the API shows it: BASIC leaves out direct inputs.
func jobView(j *aiplatformpb.NotebookExecutionJob, view aiplatformpb.NotebookExecutionJobView) *aiplatformpb.NotebookExecutionJob {
	out := proto.Clone(j).(*aiplatformpb.NotebookExecutionJob)
	if view != aiplatformpb.NotebookExecutionJobView_NOTEBOOK_EXECUTION_JOB_VIEW_FULL {
		if d, ok := out.NotebookSource.(*aiplatformpb.NotebookExecutionJob_DirectNotebookSource_); ok {
			d.DirectNotebookSource = &aiplatformpb.NotebookExecutionJob_DirectNotebookSource{}
		}
	}
	return out
}

func (n *notebookService) CreateNotebookExecutionJob(ctx context.Context, req *aiplatformpb.CreateNotebookExecutionJobRequest) (*longrunningpb.Operation, error) {
	in := req.GetNotebookExecutionJob()
	j, err := n.s.createJob(req.GetParent(), req.GetNotebookExecutionJobId(), in, "",
		jobIdentity(in.GetServiceAccount(), in.GetExecutionUser(), n.s.callerOf(ctx)))
	if err != nil {
		return nil, grpcError(err)
	}
	return n.s.finished(j.GetName(), &aiplatformpb.CreateNotebookExecutionJobOperationMetadata{GenericMetadata: generic(), ProgressMessage: "The execution is queued"},
		jobView(j, aiplatformpb.NotebookExecutionJobView_NOTEBOOK_EXECUTION_JOB_VIEW_FULL), nil)
}

// createJob records an execution and starts it; schedule names the
// schedule that made it, if one did, and runAs the account it runs as.
func (s *Server) createJob(parent, id string, in *aiplatformpb.NotebookExecutionJob, schedule, runAs string) (*aiplatformpb.NotebookExecutionJob, error) {
	project, _, err := parseParent("parent", parent)
	if err != nil {
		return nil, err
	}
	if in == nil {
		return nil, apierror.InvalidArgument("notebook_execution_job is required")
	}
	if id != "" && !notebookIDRE.MatchString(id) {
		return nil, apierror.InvalidArgument("notebook_execution_job_id %q must be 1 to 63 lowercase letters, digits and hyphens, starting with a letter", id)
	}
	j := proto.Clone(in).(*aiplatformpb.NotebookExecutionJob)
	st := s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.checkJob("notebook_execution_job.", parent, j); err != nil {
		return nil, err
	}
	if st.exec == nil || st.host == nil {
		return nil, runtimesUnavailable()
	}
	if id == "" {
		id = numericID()
	}
	name := parent + "/notebookExecutionJobs/" + id
	if _, ok := st.jobs[name]; ok {
		return nil, apierror.AlreadyExists("NotebookExecutionJob %s already exists", name)
	}
	now := timestamppb.New(st.now())
	j.Name, j.CreateTime, j.UpdateTime, j.JobState, j.Status = name, now, now, aiplatformpb.JobState_JOB_STATE_PENDING, nil
	if schedule != "" {
		j.ScheduleResourceName = schedule
	}
	ctx, cancel := context.WithTimeout(context.Background(), j.GetExecutionTimeout().AsDuration())
	e := &jobEntry{Job: j, cancel: cancel, runAs: runAs}
	st.jobs[name] = e
	st.save()
	go s.runJob(ctx, e, project)
	return proto.Clone(j).(*aiplatformpb.NotebookExecutionJob), nil
}

// setJob updates e's state unless it was deleted.
func (st *notebookStore) setJob(e *jobEntry, state aiplatformpb.JobState, err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.jobs[e.Job.GetName()] != e {
		return
	}
	e.Job.JobState, e.Job.UpdateTime = state, timestamppb.New(st.now())
	if err != nil {
		e.Job.Status = &status.Status{Code: int32(apierror.From(err).Code), Message: apierror.From(err).Message}
	} else if state == aiplatformpb.JobState_JOB_STATE_SUCCEEDED {
		e.Job.Status = &status.Status{Code: int32(codes.OK)}
	}
	st.save()
}

// runJob reads e's notebook, runs it and writes the executed notebook.
func (s *Server) runJob(ctx context.Context, e *jobEntry, project string) {
	defer e.cancel()
	st := s.notebooks()
	st.setJob(e, aiplatformpb.JobState_JOB_STATE_RUNNING, nil)
	st.mu.Lock()
	j := proto.Clone(e.Job).(*aiplatformpb.NotebookExecutionJob)
	x := st.exec
	opts := NotebookRuntimeOptions{IdleTimeout: defaultIdleTimeout}
	if t, ok := st.templates[j.GetNotebookRuntimeTemplateResourceName()]; ok {
		opts = optionsOf(t.GetIdleShutdownConfig(), t.GetSoftwareConfig())
	}
	st.mu.Unlock()
	// The run's identity (runas.go): a service account that does not
	// exist or is disabled fails the run, as Google's would.
	tok, err := s.runAsToken(e.runAs)
	if err != nil {
		st.setJob(e, aiplatformpb.JobState_JOB_STATE_FAILED, apierror.FailedPrecondition("the execution's identity %s: %v", e.runAs, err))
		return
	}
	ctx = WithRunAsToken(ctx, tok)
	opts.Token = tok
	nb, file, err := readJobNotebook(ctx, x, j)
	if err != nil {
		st.setJob(e, aiplatformpb.JobState_JOB_STATE_FAILED, apierror.FailedPrecondition("reading the notebook: %v", err))
		return
	}
	out, runErr := x.Run(ctx, "colab-exec/"+j.GetName(), project, nb, opts)
	if ctx.Err() == context.DeadlineExceeded {
		runErr = &apierror.Error{Code: codes.DeadlineExceeded, Message: fmt.Sprintf("the execution ran longer than its execution_timeout of %s", j.GetExecutionTimeout().AsDuration())}
	} else if ctx.Err() != nil {
		st.setJob(e, aiplatformpb.JobState_JOB_STATE_CANCELLED, &apierror.Error{Code: codes.Canceled, Message: "the execution was deleted"})
		return
	}
	if len(out) > 0 {
		bucket, prefix, _ := splitNotebookGCS("gcs_output_uri", j.GetGcsOutputUri(), false)
		object := path.Join(prefix, lastSegment(j.GetName()), file)
		wctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		werr := x.WriteObject(wctx, bucket, object, "application/x-ipynb+json", out)
		cancel()
		if werr != nil && runErr == nil {
			runErr = apierror.FailedPrecondition("writing the result to gs://%s/%s: %v", bucket, object, werr)
		}
	}
	if runErr != nil {
		var ae *apierror.Error
		if !errors.As(runErr, &ae) {
			// A cell that raised, or the runtime that did not start.
			runErr = apierror.FailedPrecondition("the notebook execution failed: %v", runErr)
		}
		st.setJob(e, aiplatformpb.JobState_JOB_STATE_FAILED, runErr)
		return
	}
	st.setJob(e, aiplatformpb.JobState_JOB_STATE_SUCCEEDED, nil)
}

// readJobNotebook is j's input notebook and the file name its result
// takes.
func readJobNotebook(ctx context.Context, x NotebookExecutor, j *aiplatformpb.NotebookExecutionJob) ([]byte, string, error) {
	switch src := j.GetNotebookSource().(type) {
	case *aiplatformpb.NotebookExecutionJob_DataformRepositorySource_:
		r := src.DataformRepositorySource
		b, err := x.ReadRepositoryFile(ctx, r.GetDataformRepositoryResourceName(), r.GetCommitSha(), dataformNotebookFile)
		return b, dataformNotebookFile, err
	case *aiplatformpb.NotebookExecutionJob_GcsNotebookSource_:
		bucket, object, _ := splitNotebookGCS("gcs_notebook_source.uri", src.GcsNotebookSource.GetUri(), true)
		gen, _ := strconv.ParseInt(src.GcsNotebookSource.GetGeneration(), 10, 64)
		b, err := x.ReadObject(ctx, bucket, object, gen)
		return b, path.Base(object), err
	case *aiplatformpb.NotebookExecutionJob_DirectNotebookSource_:
		return src.DirectNotebookSource.GetContent(), "notebook.ipynb", nil
	}
	return nil, "", fmt.Errorf("the execution has no notebook source")
}

func (st *notebookStore) job(name string) (*jobEntry, error) {
	if _, _, _, err := parseChild("name", "notebookExecutionJobs", name); err != nil {
		return nil, err
	}
	e, ok := st.jobs[name]
	if !ok {
		return nil, apierror.NotFound("NotebookExecutionJob %s not found", name)
	}
	return e, nil
}

func (n *notebookService) GetNotebookExecutionJob(_ context.Context, req *aiplatformpb.GetNotebookExecutionJobRequest) (*aiplatformpb.NotebookExecutionJob, error) {
	st := n.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	e, err := st.job(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	return jobView(e.Job, req.GetView()), nil
}

func (n *notebookService) ListNotebookExecutionJobs(_ context.Context, req *aiplatformpb.ListNotebookExecutionJobsRequest) (*aiplatformpb.ListNotebookExecutionJobsResponse, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	st := n.s.notebooks()
	st.mu.Lock()
	var all []*aiplatformpb.NotebookExecutionJob
	for _, k := range sortedNames(st.jobs) {
		if strings.HasPrefix(k, req.GetParent()+"/") {
			all = append(all, jobView(st.jobs[k].Job, req.GetView()))
		}
	}
	st.mu.Unlock()
	all, err := aipfilter.Apply(req.GetFilter(), all, jobFilter...)
	if err != nil {
		return nil, grpcError(err)
	}
	if err := aipfilter.SortBy(req.GetOrderBy(), all, notebookOrder); err != nil {
		return nil, grpcError(err)
	}
	items, next, err := aipfilter.Page("notebook-jobs:"+req.GetParent()+"|"+req.GetFilter()+"|"+req.GetOrderBy(), all, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, grpcError(err)
	}
	return &aiplatformpb.ListNotebookExecutionJobsResponse{NotebookExecutionJobs: items, NextPageToken: next}, nil
}

func (n *notebookService) DeleteNotebookExecutionJob(_ context.Context, req *aiplatformpb.DeleteNotebookExecutionJobRequest) (*longrunningpb.Operation, error) {
	st := n.s.notebooks()
	st.mu.Lock()
	e, err := st.job(req.GetName())
	if err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	delete(st.jobs, req.GetName())
	st.save()
	st.mu.Unlock()
	if e.cancel != nil {
		e.cancel()
	}
	return n.s.finished(req.GetName(), &aiplatformpb.DeleteOperationMetadata{GenericMetadata: generic()}, &emptypb.Empty{}, nil)
}
