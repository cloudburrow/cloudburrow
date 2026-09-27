package run

import (
	"context"
	"encoding/json"
	"math"
	"math/rand/v2"
	"regexp"
	"sort"
	"strings"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// Jobs (#582): google.cloud.run.v2.Jobs.
//
// A job is configuration: it runs nothing until RunJob. It is kept as a
// ConfigMap the adapter owns, labelled cloudburrow.dev/owned like every
// Knative Service it creates, holding the job as protojson. Each RunJob
// creates one batch/v1 Job, the execution (executions.go), rendered with the
// same container mapping as a service's revision: image localisation, env,
// secretKeyRef and resource limits.

// Labels and annotations on the objects behind a job.
const (
	labelOwned    = "cloudburrow.dev/owned"
	labelInstance = "cloudburrow.dev/instance"
	// labelKind tells a job's ConfigMap from any other the adapter owns.
	labelKind    = "cloudburrow.dev/kind"
	kindRunJob   = "run-job"
	labelRunJob  = "cloudburrow.dev/run-job"
	annRunName   = "cloudburrow.dev/cloud-run-name"
	annRunJob    = "cloudburrow.dev/run-job-name"
	jobDataKey   = "job.json"
	jobCMPrefix  = "run-job-"
	maxJobIDSize = 63
)

// Cloud Run's documented defaults for a task: three retries and a
// ten-minute timeout, and one task per execution. Applied on create so a job
// reads back with them, as Google returns it.
const (
	defaultMaxRetries  = 3
	defaultTaskTimeout = 10 * time.Minute
)

// jobIDRE is the DNS-label rule the ConfigMap's run-job label value must
// satisfy.
var jobIDRE = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)

// validateJobID checks a job ID before anything is written. The 63-character
// bound is the Kubernetes label-value limit the adapter's bookkeeping
// needs; Google's own limit is not measured here.
func validateJobID(id string) error {
	if id == "" {
		return apierror.InvalidArgument("jobId is required")
	}
	if len(id) > maxJobIDSize {
		return apierror.InvalidArgument("job name %q is %d characters; at most %d are allowed", id, len(id), maxJobIDSize)
	}
	if !jobIDRE.MatchString(id) {
		return apierror.InvalidArgument(
			"job name %q is not valid: use lowercase letters, digits and hyphens, "+
				"starting with a letter and not ending with a hyphen", id)
	}
	return nil
}

// parseJobName splits projects/{p}/locations/{l}/jobs/{job}.
func parseJobName(name string) (parent, id string, err error) {
	n, perr := resource.Parse(name)
	if perr != nil || n.Collection != "jobs" || n.ParentCollection != "" {
		return "", "", apierror.InvalidArgument("%q is not a Cloud Run job name", name)
	}
	return n.LocationName(), n.ID, nil
}

// unsupportedJob refuses what a batch Job does not render, in the words the
// services path uses for the same fields.
func unsupportedJob(job *runpb.Job) error {
	var gaps []string
	add := func(set bool, field, why string) {
		if set {
			gaps = append(gaps, field+": "+why)
		}
	}
	add(job.GetBinaryAuthorization() != nil, "binaryAuthorization", "not enforced locally")
	add(job.GetStartExecutionToken() != "" || job.GetRunExecutionToken() != "", "startExecutionToken/runExecutionToken",
		"an execution is started only by RunJob")
	tt := job.GetTemplate().GetTemplate()
	gaps = append(gaps, podTemplateGaps("template.template", tt)...)
	add(tt.GpuZonalRedundancyDisabled != nil, "template.template.gpuZonalRedundancyDisabled", "there is one kind node and no GPU")
	for _, c := range tt.GetContainers() {
		// A task serves nothing, and Cloud Run probes a job's containers
		// for no purpose this adapter maps.
		add(len(c.GetPorts()) > 0, "container.ports", "a job's task serves no port")
		add(c.GetStartupProbe() != nil, "container.startupProbe", "not mapped for a job")
		add(c.GetLivenessProbe() != nil, "container.livenessProbe", "not mapped for a job")
	}
	if len(gaps) > 0 {
		return apierror.Unimplemented("unsupported Cloud Run configuration: %s", strings.Join(gaps, "; "))
	}
	return nil
}

// checkJob validates a job as given, before defaults.
func checkJob(job *runpb.Job) error {
	if err := unsupportedJob(job); err != nil {
		return err
	}
	et := job.GetTemplate()
	if len(et.GetTemplate().GetContainers()) == 0 {
		return apierror.InvalidArgument("job requires template.template.containers")
	}
	for field, m := range map[string]map[string]string{"labels": job.GetLabels(), "annotations": job.GetAnnotations(),
		"template.labels": et.GetLabels(), "template.annotations": et.GetAnnotations()} {
		if err := validateMetadata(field, m); err != nil {
			return err
		}
	}
	if et.GetTaskCount() < 0 || et.GetParallelism() < 0 {
		return apierror.InvalidArgument("template.taskCount and template.parallelism must not be negative")
	}
	if et.GetTemplate().GetMaxRetries() < 0 {
		return apierror.InvalidArgument("template.template.maxRetries must not be negative")
	}
	if t := et.GetTemplate().GetTimeout(); t != nil && t.AsDuration() <= 0 {
		return apierror.InvalidArgument("template.template.timeout must be positive")
	}
	return nil
}

// jobSpec is what is stored for a job: the fields a caller sets, with Cloud
// Run's defaults filled in, and the generation. Output-only fields are read
// from the ConfigMap and the executions instead.
func jobSpec(job *runpb.Job, generation int64, updated time.Time) *runpb.Job {
	et := proto.Clone(job.GetTemplate()).(*runpb.ExecutionTemplate)
	if et.Template == nil {
		et.Template = &runpb.TaskTemplate{}
	}
	if et.TaskCount == 0 {
		et.TaskCount = 1
	}
	if et.Template.Retries == nil {
		et.Template.Retries = &runpb.TaskTemplate_MaxRetries{MaxRetries: defaultMaxRetries}
	}
	if et.Template.Timeout == nil {
		et.Template.Timeout = durationpb.New(defaultTaskTimeout)
	}
	return &runpb.Job{
		Name: job.GetName(), Labels: job.GetLabels(), Annotations: job.GetAnnotations(),
		Client: job.GetClient(), ClientVersion: job.GetClientVersion(), LaunchStage: job.GetLaunchStage(),
		Template: et, Generation: generation, UpdateTime: timestamppb.New(updated),
	}
}

// jobConfigMap renders the ConfigMap that holds a job. resourceVersion,
// when set, makes a replace fail rather than overwrite a newer write.
func jobConfigMap(spec *runpb.Job, id, namespace, instance, resourceVersion string) (string, error) {
	raw, err := protojson.Marshal(spec)
	if err != nil {
		return "", apierror.Internal(err, "encode job %s", spec.GetName())
	}
	cm := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      jobCMPrefix + id,
			"namespace": namespace,
			"labels": map[string]string{
				labelOwned: "true", labelInstance: instance, labelKind: kindRunJob, labelRunJob: id,
			},
			"annotations": map[string]string{annRunName: spec.GetName()},
		},
		"data": map[string]string{jobDataKey: string(raw)},
	}
	if resourceVersion != "" {
		cm["metadata"].(map[string]any)["resourceVersion"] = resourceVersion
	}
	out, err := json.Marshal(cm)
	return string(out), err
}

// storedJob decodes the job a ConfigMap holds.
func storedJob(cm kconfigMap) (*runpb.Job, error) {
	var job runpb.Job
	if err := protojson.Unmarshal([]byte(cm.Data[jobDataKey]), &job); err != nil {
		return nil, apierror.Internal(err, "decode job %s", cm.Metadata.Name)
	}
	return &job, nil
}

// fromConfigMap renders a stored job with its output-only fields: the
// ConfigMap's uid, resourceVersion as the etag and creation time, and the
// executions that exist now.
func fromConfigMap(cm kconfigMap, execs []kjob) (*runpb.Job, error) {
	job, err := storedJob(cm)
	if err != nil {
		return nil, err
	}
	job.Uid = cm.Metadata.UID
	job.Etag = cm.Metadata.ResourceVersion
	job.CreateTime = timestamppb.New(cm.Metadata.CreationTimestamp)
	if job.UpdateTime == nil {
		job.UpdateTime = job.CreateTime
	}
	job.ObservedGeneration = job.Generation
	// The configuration is stored whole on each write, so the job is ready
	// as soon as it is written.
	job.TerminalCondition = &runpb.Condition{Type: "Ready", State: runpb.Condition_CONDITION_SUCCEEDED,
		LastTransitionTime: job.UpdateTime}
	// executionCount counts the executions that still exist: one deleted
	// is no longer counted, which Cloud Run may count.
	job.ExecutionCount = int32(len(execs))
	if len(execs) > 0 {
		latest := execs[0]
		for _, e := range execs[1:] {
			if e.Metadata.CreationTimestamp.After(latest.Metadata.CreationTimestamp) {
				latest = e
			}
		}
		st := executionStateOf(latest)
		ref := &runpb.ExecutionReference{Name: latest.Metadata.Annotations[annRunName],
			CreateTime: timestamppb.New(latest.Metadata.CreationTimestamp), CompletionStatus: st.completion}
		if !st.finished.IsZero() {
			ref.CompletionTime = timestamppb.New(st.finished)
		}
		job.LatestCreatedExecution = ref
	}
	return job, nil
}

// JobsServer serves google.cloud.run.v2.Jobs. Its IAM methods are not
// implemented and return UNIMPLEMENTED.
type JobsServer struct {
	runpb.UnimplementedJobsServer
	s *Server
}

// Jobs returns the Jobs service over the same adapter, sharing its
// operations.
func (s *Server) Jobs() *JobsServer { return &JobsServer{s: s} }

// Register adds the Jobs service to a gRPC server.
func (j *JobsServer) Register(g *grpc.Server) { runpb.RegisterJobsServer(g, j) }

// loadJob reads a job's ConfigMap and checks it is the one named: owned by
// the adapter, and under the same project and location.
func (s *Server) loadJob(ctx context.Context, name, id string) (kconfigMap, error) {
	var cm kconfigMap
	if err := s.kn.getObject(ctx, &cm, resConfigMap, jobCMPrefix+id, "job "+name); err != nil {
		return kconfigMap{}, err
	}
	if cm.Metadata.Labels[labelOwned] != "true" || cm.Metadata.Labels[labelKind] != kindRunJob ||
		cm.Metadata.Annotations[annRunName] != name {
		return kconfigMap{}, apierror.NotFound("job %s not found", name)
	}
	return cm, nil
}

// jobExecutions lists a job's executions, or with id "" every job's.
func (s *Server) jobExecutions(ctx context.Context, id string) ([]kjob, error) {
	sel := labelOwned + "=true," + labelRunJob
	if id != "" {
		sel += "=" + id
	}
	return listObjects[kjob](ctx, s.kn, resBatchJob, sel)
}

// checkResourceEtag refuses a stale etag, as checkEtag does for a service.
func checkResourceEtag(etag, current, what string) error {
	if etag == "" {
		return nil
	}
	if strings.Trim(etag, `"`) != strings.Trim(current, `"`) {
		return apierror.Aborted("etag %s does not match the %s's current etag %s; read it again and retry", etag, what, current)
	}
	return nil
}

// doneOperation returns an operation that completed with res.
func (s *Server) doneOperation(parent, target string, res proto.Message) (*longrunningpb.Operation, error) {
	op := s.ops.Create(parent, target)
	_ = s.ops.Succeed(op.Name, res)
	return s.toProtoOperation(op.Name)
}

// prepareJob validates a job and renders a trial execution, so an image,
// env or secretKeyRef the execution could not use is refused when the job
// is written, not when it is first run.
func (s *Server) prepareJob(job *runpb.Job, id string, generation int64) (*runpb.Job, error) {
	if err := checkJob(job); err != nil {
		return nil, err
	}
	spec := jobSpec(job, generation, time.Now())
	if _, err := renderExecution(spec, id, executionID(id), spec.GetTemplate().GetTemplate(),
		spec.GetTemplate().GetTaskCount(), s.kn.namespace(), s.instance, s.secrets,
		s.injectedFor(projectOf(spec.GetName()))); err != nil {
		return nil, err
	}
	return spec, nil
}

// CreateJob stores a job. Nothing runs until RunJob. The operation completes
// at once: the job is configuration and is ready when written.
func (j *JobsServer) CreateJob(ctx context.Context, req *runpb.CreateJobRequest) (*longrunningpb.Operation, error) {
	s := j.s
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	id := req.GetJobId()
	if err := validateJobID(id); err != nil {
		return nil, err
	}
	if req.GetJob() == nil {
		return nil, apierror.InvalidArgument("job is required")
	}
	job := proto.Clone(req.GetJob()).(*runpb.Job)
	job.Name = req.GetParent() + "/jobs/" + id
	spec, err := s.prepareJob(job, id, 1)
	if err != nil {
		return nil, err
	}
	var existing kconfigMap
	if err := s.kn.getObject(ctx, &existing, resConfigMap, jobCMPrefix+id, "job "+job.Name); err == nil {
		return nil, apierror.AlreadyExists("job %s already exists", job.Name)
	} else if apierror.From(err).Code != codes.NotFound {
		return nil, err
	}
	if req.GetValidateOnly() {
		return s.doneOperation(req.GetParent(), job.Name, spec)
	}
	manifest, err := jobConfigMap(spec, id, s.kn.namespace(), s.instance, "")
	if err != nil {
		return nil, err
	}
	if err := s.kn.writeObject(ctx, "create", manifest, "job "+job.Name); err != nil {
		return nil, err
	}
	cm, err := s.loadJob(ctx, job.Name, id)
	if err != nil {
		return nil, apierror.Internal(err, "read back job %s", job.Name)
	}
	out, err := fromConfigMap(cm, nil)
	if err != nil {
		return nil, err
	}
	return s.doneOperation(req.GetParent(), job.Name, out)
}

// GetJob reads a job.
func (j *JobsServer) GetJob(ctx context.Context, req *runpb.GetJobRequest) (*runpb.Job, error) {
	_, id, err := parseJobName(req.GetName())
	if err != nil {
		return nil, err
	}
	cm, err := j.s.loadJob(ctx, req.GetName(), id)
	if err != nil {
		return nil, err
	}
	execs, err := j.s.jobExecutions(ctx, id)
	if err != nil {
		return nil, err
	}
	return fromConfigMap(cm, execs)
}

// ListJobs lists the jobs under a parent.
func (j *JobsServer) ListJobs(ctx context.Context, req *runpb.ListJobsRequest) (*runpb.ListJobsResponse, error) {
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	cms, err := listObjects[kconfigMap](ctx, j.s.kn, resConfigMap, labelOwned+"=true,"+labelKind+"="+kindRunJob)
	if err != nil {
		return nil, err
	}
	execs, err := j.s.jobExecutions(ctx, "")
	if err != nil {
		return nil, err
	}
	byJob := map[string][]kjob{}
	for _, e := range execs {
		byJob[e.Metadata.Labels[labelRunJob]] = append(byJob[e.Metadata.Labels[labelRunJob]], e)
	}
	byName := map[string]*runpb.Job{}
	names := []string{}
	prefix := req.GetParent() + "/jobs/"
	for _, cm := range cms {
		name := cm.Metadata.Annotations[annRunName]
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		job, err := fromConfigMap(cm, byJob[cm.Metadata.Labels[labelRunJob]])
		if err != nil {
			return nil, err
		}
		byName[name] = job
		names = append(names, name)
	}
	page, next, err := paging.Page("run-jobs:"+req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	resp := &runpb.ListJobsResponse{NextPageToken: next}
	for _, n := range page {
		resp.Jobs = append(resp.Jobs, byName[n])
	}
	return resp, nil
}

// UpdateJob replaces a job's configuration. Executions already created keep
// the template they were created with; the next RunJob uses the new one.
func (j *JobsServer) UpdateJob(ctx context.Context, req *runpb.UpdateJobRequest) (*longrunningpb.Operation, error) {
	s := j.s
	if req.GetJob() == nil {
		return nil, apierror.InvalidArgument("job is required")
	}
	name := req.GetJob().GetName()
	parent, id, err := parseJobName(name)
	if err != nil {
		return nil, err
	}
	cm, err := s.loadJob(ctx, name, id)
	if err != nil {
		if apierror.From(err).Code == codes.NotFound && req.GetAllowMissing() {
			return j.CreateJob(ctx, &runpb.CreateJobRequest{Parent: parent, JobId: id, Job: req.GetJob(), ValidateOnly: req.GetValidateOnly()})
		}
		return nil, err
	}
	if err := checkResourceEtag(req.GetJob().GetEtag(), cm.Metadata.ResourceVersion, "job"); err != nil {
		return nil, err
	}
	prev, err := storedJob(cm)
	if err != nil {
		return nil, err
	}
	spec, err := s.prepareJob(req.GetJob(), id, prev.GetGeneration()+1)
	if err != nil {
		return nil, err
	}
	if req.GetValidateOnly() {
		return s.doneOperation(parent, name, spec)
	}
	manifest, err := jobConfigMap(spec, id, s.kn.namespace(), s.instance, cm.Metadata.ResourceVersion)
	if err != nil {
		return nil, err
	}
	if err := s.kn.writeObject(ctx, "replace", manifest, "job "+name); err != nil {
		return nil, err
	}
	updated, err := s.loadJob(ctx, name, id)
	if err != nil {
		return nil, apierror.Internal(err, "read back job %s", name)
	}
	execs, err := s.jobExecutions(ctx, id)
	if err != nil {
		return nil, err
	}
	out, err := fromConfigMap(updated, execs)
	if err != nil {
		return nil, err
	}
	return s.doneOperation(parent, name, out)
}

// DeleteJob removes a job and every execution of it, running or not.
func (j *JobsServer) DeleteJob(ctx context.Context, req *runpb.DeleteJobRequest) (*longrunningpb.Operation, error) {
	s := j.s
	parent, id, err := parseJobName(req.GetName())
	if err != nil {
		return nil, err
	}
	cm, err := s.loadJob(ctx, req.GetName(), id)
	if err != nil {
		return nil, err
	}
	if err := checkResourceEtag(req.GetEtag(), cm.Metadata.ResourceVersion, "job"); err != nil {
		return nil, err
	}
	execs, err := s.jobExecutions(ctx, id)
	if err != nil {
		return nil, err
	}
	job, err := fromConfigMap(cm, execs)
	if err != nil {
		return nil, err
	}
	if req.GetValidateOnly() {
		return s.doneOperation(parent, req.GetName(), job)
	}
	if err := s.kn.deleteSelected(ctx, resBatchJob, labelOwned+"=true,"+labelRunJob+"="+id); err != nil {
		return nil, err
	}
	if err := s.kn.deleteObject(ctx, resConfigMap, jobCMPrefix+id); err != nil {
		return nil, err
	}
	return s.doneOperation(parent, req.GetName(), job)
}

// RunJob starts an execution: one batch/v1 Job with a completion index per
// task. The operation carries the execution as its metadata from the start,
// and completes when the execution does, with the execution on success and
// the failing task's own message on failure.
func (j *JobsServer) RunJob(ctx context.Context, req *runpb.RunJobRequest) (*longrunningpb.Operation, error) {
	s := j.s
	parent, id, err := parseJobName(req.GetName())
	if err != nil {
		return nil, err
	}
	cm, err := s.loadJob(ctx, req.GetName(), id)
	if err != nil {
		return nil, err
	}
	if err := checkResourceEtag(req.GetEtag(), cm.Metadata.ResourceVersion, "job"); err != nil {
		return nil, err
	}
	spec, err := storedJob(cm)
	if err != nil {
		return nil, err
	}
	tmpl, taskCount, err := applyOverrides(spec.GetTemplate().GetTemplate(), spec.GetTemplate().GetTaskCount(), req.GetOverrides())
	if err != nil {
		return nil, err
	}
	var manifest, execID string
	var created kjob
	for attempt := 0; ; attempt++ {
		execID = executionID(id)
		manifest, err = renderExecution(spec, id, execID, tmpl, taskCount, s.kn.namespace(), s.instance, s.secrets,
			s.injectedFor(projectOf(spec.GetName())))
		if err != nil {
			return nil, err
		}
		execName := req.GetName() + "/executions/" + execID
		if req.GetValidateOnly() {
			return s.doneOperation(parent, execName, &runpb.Execution{Name: execName, Job: req.GetName(),
				TaskCount: taskCount, Parallelism: parallelismOf(spec.GetTemplate().GetParallelism(), taskCount),
				Template: tmpl, Labels: spec.GetTemplate().GetLabels(), Annotations: spec.GetTemplate().GetAnnotations()})
		}
		err = s.kn.writeObject(ctx, "create", manifest, "execution "+execName)
		// A generated name that collides is drawn again, a few times.
		if err != nil && apierror.From(err).Code == codes.AlreadyExists && attempt < 3 {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := s.kn.getObject(ctx, &created, resBatchJob, execID, "execution "+execName); err != nil {
			return nil, apierror.Internal(err, "read back execution %s", execName)
		}
		break
	}
	exec := fromBatchJob(created, "")
	op := s.ops.Create(parent, exec.GetName())
	_ = s.ops.SetMetadata(op.Name, exec)
	bound := executionBound(tmpl, taskCount, parallelismOf(spec.GetTemplate().GetParallelism(), taskCount), s.readyTimeout)
	go s.awaitExecution(context.WithoutCancel(ctx), op.Name, execID, bound)
	return s.toProtoOperation(op.Name)
}

// applyOverrides applies RunJob's overrides to a copy of the task template.
func applyOverrides(tt *runpb.TaskTemplate, taskCount int32, o *runpb.RunJobRequest_Overrides) (*runpb.TaskTemplate, int32, error) {
	out := proto.Clone(tt).(*runpb.TaskTemplate)
	if o == nil {
		return out, taskCount, nil
	}
	if o.GetTaskCount() < 0 {
		return nil, 0, apierror.InvalidArgument("overrides.taskCount must not be negative")
	}
	if o.GetTaskCount() > 0 {
		taskCount = o.GetTaskCount()
	}
	if t := o.GetTimeout(); t != nil {
		if t.AsDuration() <= 0 {
			return nil, 0, apierror.InvalidArgument("overrides.timeout must be positive")
		}
		out.Timeout = t
	}
	for _, co := range o.GetContainerOverrides() {
		var target *runpb.Container
		for i, c := range out.GetContainers() {
			if c.GetName() == co.GetName() || batchContainerName(c, i) == co.GetName() {
				target = c
				break
			}
		}
		if target == nil && co.GetName() == "" && len(out.GetContainers()) == 1 {
			target = out.GetContainers()[0]
		}
		if target == nil {
			return nil, 0, apierror.InvalidArgument("overrides.containerOverrides: no container is named %q", co.GetName())
		}
		if co.GetClearArgs() {
			target.Args = nil
		} else if len(co.GetArgs()) > 0 {
			target.Args = append([]string(nil), co.GetArgs()...)
		}
		for _, e := range co.GetEnv() {
			replaced := false
			for i, have := range target.Env {
				if have.GetName() == e.GetName() {
					target.Env[i], replaced = e, true
				}
			}
			if !replaced {
				target.Env = append(target.Env, e)
			}
		}
	}
	return out, taskCount, nil
}

// executionID is a new execution's ID: the job's, cut so that with a
// five-character suffix it stays within the 63 characters a Kubernetes Job
// name may have, as Cloud Run names an execution {job}-{suffix}.
func executionID(jobID string) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	base := jobID
	if len(base) > maxJobIDSize-6 {
		base = strings.TrimRight(base[:maxJobIDSize-6], "-")
	}
	suffix := make([]byte, 5)
	for i := range suffix {
		suffix[i] = alphabet[rand.IntN(len(alphabet))]
	}
	return base + "-" + string(suffix)
}

// parallelismOf is the number of tasks run at once: Cloud Run's 0 means as
// many as there are.
func parallelismOf(p, taskCount int32) int32 {
	if p <= 0 || p > taskCount {
		return taskCount
	}
	return p
}

// executionBound is how long a RunJob operation waits before it reports the
// execution as not completing: every wave of tasks using every retry at its
// full timeout, plus the time a revision is given to start.
func executionBound(tt *runpb.TaskTemplate, taskCount, parallelism int32, startup time.Duration) time.Duration {
	timeout := tt.GetTimeout().AsDuration()
	waves := int64(math.Ceil(float64(taskCount) / float64(max(parallelism, 1))))
	return time.Duration(waves*int64(tt.GetMaxRetries()+1))*timeout + startup
}

// sortExecutionsNewestFirst orders executions as Cloud Run lists them.
func sortExecutionsNewestFirst(execs []kjob) {
	sort.SliceStable(execs, func(a, b int) bool {
		ta, tb := execs[a].Metadata.CreationTimestamp, execs[b].Metadata.CreationTimestamp
		if !ta.Equal(tb) {
			return ta.After(tb)
		}
		return execs[a].Metadata.Name > execs[b].Metadata.Name
	})
}

// namespace is where the adapter's objects live.
func (k *Knative) namespace() string {
	if k == nil {
		return WorkloadNamespace
	}
	return k.Namespace
}
