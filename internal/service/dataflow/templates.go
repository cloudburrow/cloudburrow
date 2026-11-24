package dataflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// A classic template is the job file an SDK writes with
// --template_location (the job's JSON, as it would create it), and its
// metadata file beside it, TEMPLATE_metadata (templates/creating-templates).

func (s *Server) readTemplate(ctx context.Context, gcsPath string) (*dataflowpb.Job, error) {
	if !strings.HasPrefix(gcsPath, "gs://") {
		return nil, apierror.InvalidArgument("(%s): The template's gcsPath must be a Cloud Storage path starting with gs://.", gcsPath) // UNVERIFIED
	}
	if s.opts.Storage == nil {
		return nil, apierror.FailedPrecondition("Templates are read from Cloud Storage, which this instance does not serve.")
	}
	b, err := s.opts.Storage.Read(ctx, gcsPath)
	if err != nil {
		return nil, apierror.InvalidArgument("(%s): Unable to open template file: %s.", gcsPath, gcsPath)
	}
	var job dataflowpb.Job
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, &job); err != nil {
		return nil, apierror.InvalidArgument("(%s): The template file %s is not a valid template: %v", gcsPath, gcsPath, err) // UNVERIFIED
	}
	return &job, nil
}

// metadataOf reads the template's metadata file, nil when it has none.
func (s *Server) metadataOf(ctx context.Context, gcsPath string) *dataflowpb.TemplateMetadata {
	if s.opts.Storage == nil {
		return nil
	}
	b, err := s.opts.Storage.Read(ctx, gcsPath+"_metadata")
	if err != nil {
		return nil
	}
	var m dataflowpb.TemplateMetadata
	if (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, &m) != nil {
		return nil
	}
	return &m
}

// checkParameters checks the launch's parameters against the template's
// metadata: each required one given, none it does not declare.
func checkParameters(meta *dataflowpb.TemplateMetadata, params map[string]string) error {
	if meta == nil {
		return nil
	}
	known := map[string]bool{}
	var missing []string
	for _, p := range meta.GetParameters() {
		known[p.GetName()] = true
		if !p.GetIsOptional() && params[p.GetName()] == "" {
			missing = append(missing, p.GetName())
		}
	}
	var unknown []string
	for k := range params {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	sort.Strings(missing)
	if len(missing) > 0 || len(unknown) > 0 {
		var violations []string
		for _, m := range missing {
			violations = append(violations, fmt.Sprintf("The template parameters are invalid. Missing required parameter: %s", m))
		}
		for _, u := range unknown {
			violations = append(violations, fmt.Sprintf("The template parameters are invalid. Unrecognized parameter: %s", u))
		}
		return apierror.InvalidArgument("%s", strings.Join(violations, "; "))
	}
	return nil
}

// fromTemplate is the job a classic template launches: its file's job
// with the launch's name, parameters (pipeline options, which the
// workers' RuntimeValueProviders read) and runtime environment.
func fromTemplate(job *dataflowpb.Job, name string, params map[string]string, env *dataflowpb.RuntimeEnvironment) *dataflowpb.Job {
	job.Id, job.Name, job.CurrentState, job.ClientRequestId = "", name, 0, ""
	if job.Environment == nil {
		job.Environment = &dataflowpb.Environment{}
	}
	opts := job.Environment.GetSdkPipelineOptions()
	if opts == nil {
		opts = &structpb.Struct{Fields: map[string]*structpb.Value{}}
		job.Environment.SdkPipelineOptions = opts
	}
	o := opts.Fields["options"].GetStructValue()
	if o == nil {
		o = &structpb.Struct{Fields: map[string]*structpb.Value{}}
		opts.Fields["options"] = structpb.NewStructValue(o)
	}
	for k, v := range params {
		o.Fields[k] = structpb.NewStringValue(v)
	}
	o.Fields["job_name"] = structpb.NewStringValue(name)
	o.Fields["template_location"] = structpb.NewNullValue()
	delete(o.Fields, "template_location")
	if env != nil {
		if env.TempLocation != "" {
			o.Fields["temp_location"] = structpb.NewStringValue(env.TempLocation)
			job.Environment.TempStoragePrefix = env.TempLocation
		}
		if env.MaxWorkers > 0 || env.NumWorkers > 0 {
			if len(job.Environment.WorkerPools) == 0 {
				job.Environment.WorkerPools = []*dataflowpb.WorkerPool{{Kind: "harness"}}
			}
			p := job.Environment.WorkerPools[0]
			if env.NumWorkers > 0 {
				p.NumWorkers = env.NumWorkers
			}
			if env.MaxWorkers > 0 {
				if p.AutoscalingSettings == nil {
					p.AutoscalingSettings = &dataflowpb.AutoscalingSettings{}
				}
				p.AutoscalingSettings.MaxNumWorkers = env.MaxWorkers
			}
		}
		if env.ServiceAccountEmail != "" {
			job.Environment.ServiceAccountEmail = env.ServiceAccountEmail
		}
		if len(env.AdditionalUserLabels) > 0 {
			if job.Labels == nil {
				job.Labels = map[string]string{}
			}
			for k, v := range env.AdditionalUserLabels {
				job.Labels[k] = v
			}
		}
		job.Environment.Experiments = append(job.Environment.Experiments, env.AdditionalExperiments...)
	}
	return job
}

// CreateJobFromTemplate launches a classic template (projects.templates.create).
func (s *Server) CreateJobFromTemplate(ctx context.Context, req *dataflowpb.CreateJobFromTemplateRequest) (*dataflowpb.Job, error) {
	if err := checkProject(req.GetProjectId()); err != nil {
		return nil, err
	}
	loc, err := location(req.GetLocation())
	if err != nil {
		return nil, err
	}
	if req.GetGcsPath() == "" {
		return nil, apierror.InvalidArgument("gcsPath must be set")
	}
	job, err := s.readTemplate(ctx, req.GetGcsPath())
	if err != nil {
		return nil, err
	}
	if err := checkParameters(s.metadataOf(ctx, req.GetGcsPath()), req.GetParameters()); err != nil {
		return nil, err
	}
	return s.create(req.GetProjectId(), loc, fromTemplate(job, req.GetJobName(), req.GetParameters(), req.GetEnvironment()), "", dataflowpb.JobView_JOB_VIEW_SUMMARY)
}

// LaunchTemplate launches a classic template (projects.locations.templates.launch).
func (s *Server) LaunchTemplate(ctx context.Context, req *dataflowpb.LaunchTemplateRequest) (*dataflowpb.LaunchTemplateResponse, error) {
	if err := checkProject(req.GetProjectId()); err != nil {
		return nil, err
	}
	loc, err := location(req.GetLocation())
	if err != nil {
		return nil, err
	}
	if req.GetDynamicTemplate() != nil {
		return nil, apierror.Unimplemented("Dynamic templates (dynamicTemplate) are not supported by CloudBurrow; launch the template by gcsPath.")
	}
	if req.GetGcsPath() == "" {
		return nil, apierror.InvalidArgument("gcsPath must be set")
	}
	lp := req.GetLaunchParameters()
	if lp == nil {
		return nil, apierror.InvalidArgument("launchParameters must be set")
	}
	job, err := s.readTemplate(ctx, req.GetGcsPath())
	if err != nil {
		return nil, err
	}
	if err := checkParameters(s.metadataOf(ctx, req.GetGcsPath()), lp.GetParameters()); err != nil {
		return nil, err
	}
	job = fromTemplate(job, lp.GetJobName(), lp.GetParameters(), lp.GetEnvironment())
	job.TransformNameMapping = lp.GetTransformNameMapping()
	if req.GetValidateOnly() {
		if !validName(job.GetName()) {
			return nil, apierror.InvalidArgument("(%s): Invalid job name: %q.", req.GetProjectId(), job.GetName())
		}
		return &dataflowpb.LaunchTemplateResponse{}, nil
	}
	replace := ""
	if lp.GetUpdate() {
		replace = s.activeJobNamed(req.GetProjectId(), loc, job.GetName())
		if replace == "" {
			return nil, apierror.NotFound("The workflow could not be created. Causes: Could not find a running job named %s to update.", job.GetName()) // UNVERIFIED
		}
	}
	out, err := s.create(req.GetProjectId(), loc, job, replace, dataflowpb.JobView_JOB_VIEW_SUMMARY)
	if err != nil {
		return nil, err
	}
	return &dataflowpb.LaunchTemplateResponse{Job: out}, nil
}

func (s *Server) activeJobNamed(project, loc, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.jobs {
		j := r.job
		if j.ProjectId == project && j.Location == loc && j.Name == name && active(j.CurrentState) {
			return j.Id
		}
	}
	return ""
}

// flexSpec is a Flex Template's spec file (templates/configuring-flex-templates).
type flexSpec struct {
	Image    string          `json:"image"`
	Metadata json.RawMessage `json:"metadata"`
	SDKInfo  struct {
		Language string `json:"language"`
	} `json:"sdkInfo"`
	DefaultEnvironment json.RawMessage `json:"defaultEnvironment"`
}

// GetTemplate answers a template's metadata and type.
func (s *Server) GetTemplate(ctx context.Context, req *dataflowpb.GetTemplateRequest) (*dataflowpb.GetTemplateResponse, error) {
	if err := checkProject(req.GetProjectId()); err != nil {
		return nil, err
	}
	if s.opts.Storage == nil {
		return nil, apierror.FailedPrecondition("Templates are read from Cloud Storage, which this instance does not serve.")
	}
	path := req.GetGcsPath()
	b, err := s.opts.Storage.Read(ctx, path)
	if err != nil {
		return nil, apierror.InvalidArgument("(%s): Unable to open template file: %s.", path, path)
	}
	var spec flexSpec
	if json.Unmarshal(b, &spec) == nil && spec.Image != "" {
		out := &dataflowpb.GetTemplateResponse{Status: &status.Status{}, TemplateType: dataflowpb.GetTemplateResponse_FLEX}
		if len(spec.Metadata) > 0 {
			var m dataflowpb.TemplateMetadata
			if (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(spec.Metadata, &m) == nil {
				out.Metadata = &m
			}
		}
		return out, nil
	}
	if _, err := s.readTemplate(ctx, path); err != nil {
		return nil, err
	}
	meta := s.metadataOf(ctx, path)
	if meta == nil {
		meta = &dataflowpb.TemplateMetadata{Name: path[strings.LastIndex(path, "/")+1:]}
	}
	return &dataflowpb.GetTemplateResponse{Status: &status.Status{}, Metadata: meta, TemplateType: dataflowpb.GetTemplateResponse_LEGACY}, nil
}

// LaunchFlexTemplate launches a Flex Template: its job is created QUEUED
// while the template's launcher image runs the pipeline's program, whose
// DataflowRunner then submits it, taking this job's ID.
func (s *Server) LaunchFlexTemplate(ctx context.Context, req *dataflowpb.LaunchFlexTemplateRequest) (*dataflowpb.LaunchFlexTemplateResponse, error) {
	if err := checkProject(req.GetProjectId()); err != nil {
		return nil, err
	}
	loc, err := location(req.GetLocation())
	if err != nil {
		return nil, err
	}
	lp := req.GetLaunchParameter()
	if lp == nil {
		return nil, apierror.InvalidArgument("launchParameter must be set")
	}
	if !validName(lp.GetJobName()) {
		return nil, apierror.InvalidArgument("(%s): Invalid job name: %q. Job names must match the regular expression [a-z]([-a-z0-9]{0,1022}[a-z0-9])?", req.GetProjectId(), lp.GetJobName())
	}
	var image string
	var meta *dataflowpb.TemplateMetadata
	switch {
	case lp.GetContainerSpec() != nil:
		image, meta = lp.GetContainerSpec().GetImage(), lp.GetContainerSpec().GetMetadata()
	case lp.GetContainerSpecGcsPath() != "":
		if s.opts.Storage == nil {
			return nil, apierror.FailedPrecondition("Templates are read from Cloud Storage, which this instance does not serve.")
		}
		b, err := s.opts.Storage.Read(ctx, lp.GetContainerSpecGcsPath())
		if err != nil {
			return nil, apierror.InvalidArgument("(%s): Unable to open template file: %s.", lp.GetContainerSpecGcsPath(), lp.GetContainerSpecGcsPath())
		}
		var spec flexSpec
		if err := json.Unmarshal(b, &spec); err != nil || spec.Image == "" {
			return nil, apierror.InvalidArgument("(%s): The template file is not a valid Flex Template spec: it names no image.", lp.GetContainerSpecGcsPath()) // UNVERIFIED
		}
		image = spec.Image
		if len(spec.Metadata) > 0 {
			var m dataflowpb.TemplateMetadata
			if (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(spec.Metadata, &m) == nil {
				meta = &m
			}
		}
	default:
		return nil, apierror.InvalidArgument("One of containerSpec or containerSpecGcsPath must be set.") // UNVERIFIED
	}
	if image == "" {
		return nil, apierror.InvalidArgument("The Flex Template's container spec names no image.") // UNVERIFIED
	}
	if err := checkParameters(meta, lp.GetParameters()); err != nil {
		return nil, err
	}
	if req.GetValidateOnly() {
		return &dataflowpb.LaunchFlexTemplateResponse{}, nil
	}
	if s.opts.Launcher == nil {
		return nil, apierror.FailedPrecondition("Flex Templates run their launcher image in the instance's Docker, which this instance does not have.")
	}
	env := lp.GetEnvironment()
	now := s.now()
	streaming := meta.GetStreaming()
	job := &dataflowpb.Job{Id: newJobID(now), ProjectId: req.GetProjectId(), Location: loc, Name: lp.GetJobName(),
		Type: dataflowpb.JobType_JOB_TYPE_BATCH, CreateTime: timestamppb.New(now), StartTime: timestamppb.New(now),
		CurrentState: dataflowpb.JobState_JOB_STATE_QUEUED, CurrentStateTime: timestamppb.New(now),
		Labels: env.GetAdditionalUserLabels(), JobMetadata: &dataflowpb.JobMetadata{}}
	if streaming {
		job.Type = dataflowpb.JobType_JOB_TYPE_STREAMING
	}
	s.mu.Lock()
	for _, r := range s.jobs {
		j := r.job
		if j.ProjectId == job.ProjectId && j.Location == loc && j.Name == job.Name && active(j.CurrentState) && !lp.GetUpdate() {
			s.mu.Unlock()
			return nil, apierror.AlreadyExists("There is already an active job named %s. If you want to submit a second job, try again by setting a different name.", job.Name) // UNVERIFIED
		}
	}
	r := &jobRec{job: job, flex: true}
	key := jobKey(job.ProjectId, loc, job.Id)
	s.jobs[key] = r
	s.addMessageLocked(r, dataflowpb.JobMessageImportance_JOB_MESSAGE_BASIC, "Starting the Flex Template launcher for image "+image+".")
	s.save()
	s.mu.Unlock()

	var args []string
	keys := make([]string, 0, len(lp.GetParameters()))
	for k := range lp.GetParameters() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--"+k+"="+lp.GetParameters()[k])
	}
	args = append(args, "--job_name="+job.Name, "--project="+job.ProjectId, "--region="+loc)
	if env.GetTempLocation() != "" {
		args = append(args, "--temp_location="+env.GetTempLocation())
	}
	if env.GetStagingLocation() != "" {
		args = append(args, "--staging_location="+env.GetStagingLocation())
	}
	if env.GetMaxWorkers() > 0 {
		args = append(args, fmt.Sprintf("--max_num_workers=%d", env.GetMaxWorkers()))
	}
	if lp.GetUpdate() {
		args = append(args, "--update")
	}
	if len(lp.GetTransformNameMappings()) > 0 {
		b, _ := json.Marshal(lp.GetTransformNameMappings())
		args = append(args, "--transform_name_mapping="+string(b))
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		err := s.opts.Launcher.Launch(context.Background(), FlexLaunch{Project: job.ProjectId, Location: loc, JobName: job.Name, Image: image, Args: args})
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.job.CurrentState == dataflowpb.JobState_JOB_STATE_QUEUED {
			msg := "Error occurred in the launcher container: the template's program did not submit a job."
			if err != nil {
				msg = "Error occurred in the launcher container: " + err.Error()
			}
			r.job.CurrentState, r.job.CurrentStateTime = dataflowpb.JobState_JOB_STATE_FAILED, timestamppb.New(s.now())
			s.addMessageLocked(r, dataflowpb.JobMessageImportance_JOB_MESSAGE_ERROR, msg)
			s.save()
		}
	}()
	return &dataflowpb.LaunchFlexTemplateResponse{Job: viewOf(job, dataflowpb.JobView_JOB_VIEW_SUMMARY)}, nil
}
