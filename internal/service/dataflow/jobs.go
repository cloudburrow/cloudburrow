package dataflow

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// jobNameRE and its length bound are a job name, as projects.locations.jobs
// documents name: [a-z]([-a-z0-9]{0,1022}[a-z0-9])? (Go's regexp repeats at
// most 1000 times, so the length is checked apart).
var jobNameRE = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

func validName(n string) bool { return len(n) <= 1024 && jobNameRE.MatchString(n) }

var regionRE = regexp.MustCompile(`^[a-z]+(-[a-z]+)*[0-9]+$`)

func checkProject(p string) error {
	if p == "" {
		return apierror.InvalidArgument("projectId must be set")
	}
	return nil
}

// location is the request's region: us-central1 for projects.jobs.*.
func location(loc string) (string, error) {
	if loc == "" {
		return DefaultLocation, nil
	}
	if !regionRE.MatchString(loc) {
		// UNVERIFIED: Google's text for a malformed regional endpoint (docs re-read 2026-11-24).
		return "", apierror.InvalidArgument("The location %q is not a valid Dataflow regional endpoint.", loc)
	}
	return loc, nil
}

func notFound(id string) error {
	return apierror.NotFound("Information about job %s could not be found in our system. Please double check that the API being used is projects.locations.jobs.get (https://cloud.google.com/dataflow/docs/reference/rest/v1b3/projects.locations.jobs/get). If the API being used is projects.locations.jobs.get, please double check the id (%s) is correct.", id, id)
}

// CreateJob creates a job and starts it.
func (s *Server) CreateJob(ctx context.Context, req *dataflowpb.CreateJobRequest) (*dataflowpb.Job, error) {
	if err := checkProject(req.GetProjectId()); err != nil {
		return nil, err
	}
	loc, err := location(req.GetLocation())
	if err != nil {
		return nil, err
	}
	in := req.GetJob()
	if in == nil {
		return nil, apierror.InvalidArgument("job must be set")
	}
	if in.GetLocation() != "" && in.GetLocation() != loc {
		// UNVERIFIED (docs re-read 2026-11-24)
		return nil, apierror.InvalidArgument("The job's location %q does not match the request's location %q.", in.GetLocation(), loc)
	}
	return s.create(req.GetProjectId(), loc, proto.Clone(in).(*dataflowpb.Job), firstNonEmpty(req.GetReplaceJobId(), in.GetReplaceJobId()), req.GetView())
}

func firstNonEmpty(v ...string) string {
	for _, x := range v {
		if x != "" {
			return x
		}
	}
	return ""
}

// create stores job in project's loc and starts it; replace is the job it
// replaces ("" none).
func (s *Server) create(project, loc string, job *dataflowpb.Job, replace string, view dataflowpb.JobView) (*dataflowpb.Job, error) {
	if !validName(job.GetName()) {
		return nil, apierror.InvalidArgument("(%s): Invalid job name: %q. Job names must match the regular expression [a-z]([-a-z0-9]{0,1022}[a-z0-9])?", project, job.GetName())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var old, adopt *jobRec
	for _, r := range s.jobs {
		j := r.job
		if j.ProjectId != project || j.Location != loc || j.Name != job.Name || !active(j.CurrentState) {
			continue
		}
		if r.flex && j.CurrentState == dataflowpb.JobState_JOB_STATE_QUEUED {
			// The Flex Template launch this job is the pipeline of.
			adopt = r
			continue
		}
		if job.ClientRequestId != "" && j.ClientRequestId == job.ClientRequestId {
			// The same request again: its job.
			return viewOf(j, view), nil
		}
		if replace != "" && j.Id == replace {
			old = r
			continue
		}
		// An active job of the name: Google answers it, whose
		// clientRequestId is not the request's (the SDKs' DataflowRunner
		// reads that as "There is already active job named ...").
		return viewOf(j, view), nil
	}
	if replace != "" {
		if old == nil {
			r := s.jobs[jobKey(project, loc, replace)]
			if r == nil {
				return nil, apierror.NotFound("The workflow could not be created. Causes: Could not find a running job with id %s to update.", replace) // UNVERIFIED (docs re-read 2026-11-24)
			}
			return nil, apierror.FailedPrecondition("The workflow could not be created. Causes: The job %s to update is %s, not running, or is named %q, not %q.", replace, r.job.CurrentState, r.job.Name, job.Name) // UNVERIFIED (docs re-read 2026-11-24)
		}
		if old.job.Type != dataflowpb.JobType_JOB_TYPE_STREAMING {
			return nil, apierror.FailedPrecondition("The workflow could not be created. Causes: Updating a batch job is not supported: only streaming jobs can be updated.") // UNVERIFIED (docs re-read 2026-11-24)
		}
	}
	if err := s.restoreSnapshotLocked(project, loc, job); err != nil {
		return nil, err
	}
	now := s.now()
	job.Id = newJobID(now)
	job.ProjectId, job.Location = project, loc
	job.CreateTime, job.StartTime = timestamppb.New(now), timestamppb.New(now)
	if adopt != nil {
		job.Id, job.CreateTime = adopt.job.Id, adopt.job.CreateTime
		for k, v := range adopt.job.Labels {
			if job.Labels == nil {
				job.Labels = map[string]string{}
			}
			job.Labels[k] = v
		}
	}
	job.CurrentState, job.CurrentStateTime = dataflowpb.JobState_JOB_STATE_PENDING, timestamppb.New(now)
	job.RequestedState = dataflowpb.JobState_JOB_STATE_UNKNOWN
	job.ReplacedByJobId = ""
	job.ReplaceJobId = replace
	if job.Type == dataflowpb.JobType_JOB_TYPE_UNKNOWN {
		job.Type = dataflowpb.JobType_JOB_TYPE_BATCH
	}
	if job.JobMetadata == nil {
		job.JobMetadata = &dataflowpb.JobMetadata{}
	}
	camelOptions(job)
	if adopt != nil && adopt.flexTemp != "" {
		if o := job.GetEnvironment().GetSdkPipelineOptions().GetFields()["options"].GetStructValue(); o != nil {
			o.Fields["tempLocation"] = structpb.NewStringValue(adopt.flexTemp)
		}
	}
	if sdk, ver := sdkOf(job); ver != "" {
		job.JobMetadata.SdkVersion = &dataflowpb.SdkVersion{Version: ver, VersionDisplayName: sdk,
			SdkSupportStatus: dataflowpb.SdkVersion_SUPPORTED}
	}
	if job.Type == dataflowpb.JobType_JOB_TYPE_STREAMING {
		max := int32(maxWorkers(job))
		min := int32(1)
		job.RuntimeUpdatableParams = &dataflowpb.RuntimeUpdatableParams{MaxNumWorkers: &max, MinNumWorkers: &min}
	}
	f := false
	job.SatisfiesPzi = &f
	r := &jobRec{job: job}
	if adopt != nil {
		r.messages, r.msgSeq, r.flex = adopt.messages, adopt.msgSeq, true
	}
	key := jobKey(project, loc, job.Id)
	s.jobs[key] = r
	if old != nil {
		// The replaced job stops reading; this one starts when it has
		// stopped (guides/updating-a-pipeline: the replacement takes over
		// its sources).
		old.replacement = key
		old.job.ReplacedByJobId = job.Id
		s.addMessageLocked(old, dataflowpb.JobMessageImportance_JOB_MESSAGE_BASIC, "Workflow update is in progress: the job is being replaced by job "+job.Id+".")
		s.addMessageLocked(r, dataflowpb.JobMessageImportance_JOB_MESSAGE_BASIC, "This job is replacing job "+old.job.Id+".")
		s.signalDrainLocked(old)
	} else {
		s.startLocked(key, r)
	}
	s.save()
	// The response is the job as created, before it has a state, as
	// Google's (currentStateTime the epoch).
	out := viewOf(job, view)
	out.CurrentState, out.CurrentStateTime = dataflowpb.JobState_JOB_STATE_UNKNOWN, timestamppb.New(epoch)
	return out, nil
}

// sdkOf is the job's SDK, from its user agent: ("Apache Beam Python 3.12
// SDK", "2.78.0").
func sdkOf(job *dataflowpb.Job) (string, string) {
	ua := job.GetEnvironment().GetUserAgent().GetFields()
	return ua["name"].GetStringValue(), ua["version"].GetStringValue()
}

// maxWorkers is the job's worker bound: its pool's maxNumWorkers, else
// numWorkers, else 1.
func maxWorkers(job *dataflowpb.Job) int {
	for _, p := range job.GetEnvironment().GetWorkerPools() {
		if n := p.GetAutoscalingSettings().GetMaxNumWorkers(); n > 0 {
			return int(n)
		}
		if p.GetNumWorkers() > 0 {
			return int(p.GetNumWorkers())
		}
	}
	if opts := pipelineOptions(job); opts != nil {
		for _, k := range []string{"max_num_workers", "maxNumWorkers", "num_workers", "numWorkers"} {
			if v, ok := opts[k].(float64); ok && v > 0 {
				return int(v)
			}
		}
	}
	return 1
}

// pipelineOptions are the job's environment.sdkPipelineOptions.options.
func pipelineOptions(job *dataflowpb.Job) map[string]any {
	o := job.GetEnvironment().GetSdkPipelineOptions().GetFields()["options"].GetStructValue()
	if o == nil {
		return nil
	}
	return o.AsMap()
}

// GetJob answers a job.
func (s *Server) GetJob(_ context.Context, req *dataflowpb.GetJobRequest) (*dataflowpb.Job, error) {
	r, err := s.lookup(req.GetProjectId(), req.GetLocation(), req.GetJobId())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return viewOf(r.job, req.GetView()), nil
}

func (s *Server) lookup(project, loc, id string) (*jobRec, error) {
	if err := checkProject(project); err != nil {
		return nil, err
	}
	l, err := location(loc)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.jobs[jobKey(project, l, id)]
	if r == nil {
		return nil, notFound(id)
	}
	return r, nil
}

// viewOf is a copy of job in view: SUMMARY (the default) has the job's
// identity, type, state, times, SDK and labels; DESCRIPTION adds its
// pipeline description, steps and environment; ALL is everything.
func viewOf(job *dataflowpb.Job, view dataflowpb.JobView) *dataflowpb.Job {
	j := proto.Clone(job).(*dataflowpb.Job)
	switch view {
	case dataflowpb.JobView_JOB_VIEW_ALL:
		return j
	case dataflowpb.JobView_JOB_VIEW_DESCRIPTION:
		j.ExecutionInfo, j.TempFiles = nil, nil
		return j
	}
	j.Environment, j.Steps, j.PipelineDescription, j.ExecutionInfo = nil, nil, nil, nil
	j.TempFiles, j.TransformNameMapping, j.StageStates = nil, nil, nil
	return j
}

// UpdateJob changes a job's requested state (cancel, drain) or, by
// update_mask, its runtime-updatable parameters.
func (s *Server) UpdateJob(_ context.Context, req *dataflowpb.UpdateJobRequest) (*dataflowpb.Job, error) {
	r, err := s.lookup(req.GetProjectId(), req.GetLocation(), req.GetJobId())
	if err != nil {
		return nil, err
	}
	in := req.GetJob()
	if in == nil {
		return nil, apierror.InvalidArgument("job must be set")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j := r.job
	if paths := req.GetUpdateMask().GetPaths(); len(paths) > 0 {
		for _, p := range paths {
			switch p {
			case "runtime_updatable_params.max_num_workers", "runtime_updatable_params.min_num_workers",
				"runtimeUpdatableParams.maxNumWorkers", "runtimeUpdatableParams.minNumWorkers":
			case "requested_state", "requestedState":
			default:
				return nil, apierror.InvalidArgument("The update mask path %q is not supported. Only runtime_updatable_params.max_num_workers, runtime_updatable_params.min_num_workers and requested_state may be updated.", p) // UNVERIFIED (docs re-read 2026-11-24)
			}
		}
		if j.Type != dataflowpb.JobType_JOB_TYPE_STREAMING || !active(j.CurrentState) {
			if in.GetRequestedState() == dataflowpb.JobState_JOB_STATE_UNKNOWN {
				return nil, apierror.FailedPrecondition("Workflow modification failed. Causes: Runtime updatable parameters can only be changed for a running streaming job.") // UNVERIFIED (docs re-read 2026-11-24)
			}
		}
		if p := in.GetRuntimeUpdatableParams(); p != nil {
			if j.RuntimeUpdatableParams == nil {
				j.RuntimeUpdatableParams = &dataflowpb.RuntimeUpdatableParams{}
			}
			max, min := p.MaxNumWorkers, p.MinNumWorkers
			if max == nil {
				max = j.RuntimeUpdatableParams.MaxNumWorkers
			}
			if min == nil {
				min = j.RuntimeUpdatableParams.MinNumWorkers
			}
			if max != nil && min != nil && *min > *max {
				return nil, apierror.InvalidArgument("min_num_workers (%d) must not exceed max_num_workers (%d).", *min, *max) // UNVERIFIED (docs re-read 2026-11-24)
			}
			if max != nil && (*max < 1 || *max > 1000) {
				return nil, apierror.InvalidArgument("max_num_workers must be between 1 and 1000, got %d.", *max) // UNVERIFIED (docs re-read 2026-11-24)
			}
			j.RuntimeUpdatableParams.MaxNumWorkers, j.RuntimeUpdatableParams.MinNumWorkers = max, min
			s.addMessageLocked(r, dataflowpb.JobMessageImportance_JOB_MESSAGE_BASIC,
				fmt.Sprintf("Autoscaling: the worker pool's bounds are now %d to %d workers.", deref(min), deref(max)))
		}
	}
	switch want := in.GetRequestedState(); want {
	case dataflowpb.JobState_JOB_STATE_UNKNOWN:
	case dataflowpb.JobState_JOB_STATE_CANCELLED:
		if terminal(j.CurrentState) {
			return nil, apierror.FailedPrecondition("Workflow modification failed. Causes: Cannot cancel job %s: it is in the terminal state %s.", j.Id, j.CurrentState) // UNVERIFIED (docs re-read 2026-11-24)
		}
		j.RequestedState = want
		j.CurrentState, j.CurrentStateTime = dataflowpb.JobState_JOB_STATE_CANCELLING, timestamppb.New(s.now())
		s.addMessageLocked(r, dataflowpb.JobMessageImportance_JOB_MESSAGE_BASIC, "Cancel request is committed for workflow job: "+j.Id+".")
		if r.cancel != nil {
			r.cancel()
		} else {
			s.finishLocked(r, Outcome{State: dataflowpb.JobState_JOB_STATE_CANCELLED})
		}
	case dataflowpb.JobState_JOB_STATE_DRAINED, dataflowpb.JobState_JOB_STATE_DRAINING:
		// gcloud asks for JOB_STATE_DRAINED and the Terraform provider
		// (google_dataflow_job's on_delete = "drain") for
		// JOB_STATE_DRAINING; Google drains on either.
		want = dataflowpb.JobState_JOB_STATE_DRAINED
		if j.Type != dataflowpb.JobType_JOB_TYPE_STREAMING {
			return nil, apierror.FailedPrecondition("Workflow modification failed. Causes: Draining is supported for streaming jobs only: job %s is a batch job.", j.Id) // UNVERIFIED (docs re-read 2026-11-24)
		}
		if terminal(j.CurrentState) || j.CurrentState == dataflowpb.JobState_JOB_STATE_CANCELLING {
			return nil, apierror.FailedPrecondition("Workflow modification failed. Causes: Cannot drain job %s: it is in the state %s.", j.Id, j.CurrentState) // UNVERIFIED (docs re-read 2026-11-24)
		}
		j.RequestedState = want
		j.CurrentState, j.CurrentStateTime = dataflowpb.JobState_JOB_STATE_DRAINING, timestamppb.New(s.now())
		s.addMessageLocked(r, dataflowpb.JobMessageImportance_JOB_MESSAGE_BASIC, "Drain request is committed for workflow job: "+j.Id+".")
		s.signalDrainLocked(r)
	default:
		return nil, apierror.InvalidArgument("requestedState %s is not supported: a job's requested state may be JOB_STATE_CANCELLED, JOB_STATE_DRAINED or JOB_STATE_DRAINING.", want) // UNVERIFIED (docs re-read 2026-11-24)
	}
	s.save()
	return viewOf(j, dataflowpb.JobView_JOB_VIEW_SUMMARY), nil
}

func deref(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

func (s *Server) signalDrainLocked(r *jobRec) {
	if r.drain != nil && !r.drained {
		r.drained = true
		close(r.drain)
	}
}

// ListJobs lists a region's jobs, newest first.
func (s *Server) ListJobs(_ context.Context, req *dataflowpb.ListJobsRequest) (*dataflowpb.ListJobsResponse, error) {
	if err := checkProject(req.GetProjectId()); err != nil {
		return nil, err
	}
	loc, err := location(req.GetLocation())
	if err != nil {
		return nil, err
	}
	return s.list(req, loc)
}

// AggregatedListJobs lists the project's jobs in every region.
func (s *Server) AggregatedListJobs(_ context.Context, req *dataflowpb.ListJobsRequest) (*dataflowpb.ListJobsResponse, error) {
	if err := checkProject(req.GetProjectId()); err != nil {
		return nil, err
	}
	if req.Name != nil {
		return nil, apierror.InvalidArgument("Filtering by name is not supported by projects.jobs.aggregated.") // UNVERIFIED (docs re-read 2026-11-24)
	}
	return s.list(req, "")
}

func (s *Server) list(req *dataflowpb.ListJobsRequest, loc string) (*dataflowpb.ListJobsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	byKey := map[string]*dataflowpb.Job{}
	var keys []string
	for _, r := range s.jobs {
		j := r.job
		if j.ProjectId != req.GetProjectId() || (loc != "" && j.Location != loc) {
			continue
		}
		if req.Name != nil && j.Name != req.GetName() {
			continue
		}
		switch req.GetFilter() {
		case dataflowpb.ListJobsRequest_TERMINATED:
			if !terminal(j.CurrentState) {
				continue
			}
		case dataflowpb.ListJobsRequest_ACTIVE:
			if !active(j.CurrentState) {
				continue
			}
		}
		// Newest first: the key orders by the inverted create time.
		k := fmt.Sprintf("%020d/%s/%s", math.MaxInt64-j.GetCreateTime().AsTime().UnixNano(), j.Location, j.Id)
		byKey[k] = j
		keys = append(keys, k)
	}
	sort.Strings(keys)
	scope := fmt.Sprintf("jobs:%s/%s/%d/%v", req.GetProjectId(), loc, req.GetFilter(), req.GetName())
	page, next, err := paging.Page(scope, keys, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token")
	}
	out := &dataflowpb.ListJobsResponse{NextPageToken: next}
	view := req.GetView()
	for _, k := range page {
		out.Jobs = append(out.Jobs, viewOf(byKey[k], view))
	}
	return out, nil
}

// CheckActiveJobs answers whether the project has an active job.
func (s *Server) CheckActiveJobs(_ context.Context, req *dataflowpb.CheckActiveJobsRequest) (*dataflowpb.CheckActiveJobsResponse, error) {
	if err := checkProject(req.GetProjectId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.jobs {
		if r.job.ProjectId == req.GetProjectId() && active(r.job.CurrentState) {
			return &dataflowpb.CheckActiveJobsResponse{ActiveJobsExist: true}, nil
		}
	}
	return &dataflowpb.CheckActiveJobsResponse{}, nil
}

func optString(opts map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := opts[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// structOf is v as a Struct value, or nil.
func structOf(v map[string]any) *structpb.Struct {
	s, err := structpb.NewStruct(v)
	if err != nil {
		return nil
	}
	return s
}

func lowerSDK(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "python"):
		return "Python"
	case strings.Contains(n, "java"):
		return "Java"
	case strings.Contains(n, "go"):
		return "Go"
	}
	return name
}

// camelOptions gives each of a job's snake_case pipeline options (the
// Python SDK's spelling, temp_location) its camelCase name too
// (tempLocation), the spelling Google's service shows a job's options in
// and the Terraform provider reads google_dataflow_flex_template_job's
// temp_location back from.
func camelOptions(job *dataflowpb.Job) {
	o := job.GetEnvironment().GetSdkPipelineOptions().GetFields()["options"].GetStructValue()
	if o == nil {
		return
	}
	for k, v := range o.Fields {
		if !strings.Contains(k, "_") {
			continue
		}
		parts := strings.Split(k, "_")
		for i := 1; i < len(parts); i++ {
			if parts[i] != "" {
				parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
			}
		}
		if c := strings.Join(parts, ""); c != k {
			if _, ok := o.Fields[c]; !ok {
				o.Fields[c] = v
			}
		}
	}
}
