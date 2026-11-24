package vertexai

// Vertex AI custom training (#2210): google.cloud.aiplatform.v1.JobService's
// CustomJob methods over gRPC and REST. A custom job runs its worker pools'
// containers on this machine's Docker, as pipeline tasks run
// (PipelineRunner), with Vertex AI's training environment.
//
// Documented behaviour (the JobService and CustomJob protos,
// cloud.google.com/go/aiplatform v1.127.0;
// https://docs.cloud.google.com/vertex-ai/docs/training/create-custom-job,
// .../training/code-requirements, .../training/distributed-training,
// .../training/configure-compute; read 2026-11-24):
//   - display_name and job_spec.worker_pool_specs are required; the first
//     worker pool is the primary replica and its replica_count must be 1;
//     an empty later pool is skipped; each pool has a machine_spec with a
//     machine_type and either a container_spec (image_uri) or a
//     python_package_spec (executor_image_uri, package_uris,
//     python_module);
//   - the job is QUEUED, PENDING, RUNNING, then SUCCEEDED, FAILED or
//     CANCELLED; CancelCustomJob is best effort, and a cancelled job's
//     error has code 1 (CANCELLED); a running job cannot be deleted;
//   - with base_output_directory, AIP_MODEL_DIR is
//     BASE_OUTPUT_DIRECTORY/model/, AIP_CHECKPOINT_DIR .../checkpoints/
//     and AIP_TENSORBOARD_LOG_DIR .../logs/; every replica gets
//     CLUSTER_SPEC (and TF_CONFIG) naming its pool and index;
//   - scheduling.timeout bounds the run, 7 days by default;
//   - job_spec.service_account needs iam.serviceAccounts.actAs (round8ai.go).
//
// Local differences: the containers run on this machine's CPU (machine
// types and accelerators are recorded, not provisioned) and with no
// network (--network none), as pipeline tasks do; Cloud Storage is the
// /gcs folder Vertex AI's Cloud Storage FUSE mount gives, holding every
// object the job's command, arguments and environment name (gs://B/P or
// /gcs/B/P) and the base output directory, and every file the job writes
// there is written back to the instance's Cloud Storage when it ends.
// Replicas of a distributed job run side by side but, having no network,
// cannot reach each other.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// jobService serves JobService's CustomJob and BatchPredictionJob methods
// (batchprediction.go); the rest of JobService is UNIMPLEMENTED.
type jobService struct {
	aiplatformpb.UnimplementedJobServiceServer
	s *Server
}

// customJobEntry is a custom job and its run.
type customJobEntry struct {
	Job    *aiplatformpb.CustomJob
	cancel context.CancelFunc
	// done is closed when the run ends.
	done chan struct{}
}

const defaultJobTimeout = 7 * 24 * time.Hour

var trainJobFilter = append([]aipfilter.Option{
	aipfilter.Fields("display_name", "state", "create_time", "update_time", "end_time", "start_time", "labels"),
}, bothCases(nil, nil)...)

var trainJobOrder = aipfilter.Fields("create_time", "update_time", "end_time", "start_time", "display_name")

func jobRunning(s aiplatformpb.JobState) bool {
	switch s {
	case aiplatformpb.JobState_JOB_STATE_QUEUED, aiplatformpb.JobState_JOB_STATE_PENDING, aiplatformpb.JobState_JOB_STATE_RUNNING,
		aiplatformpb.JobState_JOB_STATE_CANCELLING, aiplatformpb.JobState_JOB_STATE_UPDATING:
		return true
	}
	return false
}

// checkCustomJobSpec validates a CustomJobSpec at field (job_spec,
// training_task_inputs ...).
func checkCustomJobSpec(field string, spec *aiplatformpb.CustomJobSpec) error {
	if spec == nil {
		return apierror.InvalidArgument("%s is required", field)
	}
	pools := spec.GetWorkerPoolSpecs()
	if len(pools) == 0 {
		return apierror.InvalidArgument("%s.worker_pool_specs is required: at least one worker pool", field)
	}
	if len(pools) > 4 {
		return apierror.InvalidArgument("%s.worker_pool_specs: at most 4 worker pools (primary, workers, parameter servers, evaluators), got %d", field, len(pools))
	}
	for i, p := range pools {
		f := fmt.Sprintf("%s.worker_pool_specs[%d]", field, i)
		if i > 0 && emptyPool(p) {
			continue
		}
		if p.GetMachineSpec().GetMachineType() == "" {
			return apierror.InvalidArgument("%s.machine_spec.machine_type is required", f)
		}
		if i == 0 && p.GetReplicaCount() != 1 {
			return apierror.InvalidArgument("%s.replica_count must be 1: the first worker pool is the primary replica", f)
		}
		if p.GetReplicaCount() < 1 {
			return apierror.InvalidArgument("%s.replica_count must be at least 1", f)
		}
		switch t := p.GetTask().(type) {
		case *aiplatformpb.WorkerPoolSpec_ContainerSpec:
			if t.ContainerSpec.GetImageUri() == "" {
				return apierror.InvalidArgument("%s.container_spec.image_uri is required", f)
			}
		case *aiplatformpb.WorkerPoolSpec_PythonPackageSpec:
			pp := t.PythonPackageSpec
			if pp.GetExecutorImageUri() == "" {
				return apierror.InvalidArgument("%s.python_package_spec.executor_image_uri is required", f)
			}
			if len(pp.GetPackageUris()) == 0 {
				return apierror.InvalidArgument("%s.python_package_spec.package_uris is required", f)
			}
			if len(pp.GetPackageUris()) > 100 {
				return apierror.InvalidArgument("%s.python_package_spec.package_uris: at most 100 packages", f)
			}
			for _, u := range pp.GetPackageUris() {
				if !strings.HasPrefix(u, "gs://") {
					return apierror.InvalidArgument("%s.python_package_spec.package_uris %q is not a gs:// URI", f, u)
				}
			}
			if pp.GetPythonModule() == "" {
				return apierror.InvalidArgument("%s.python_package_spec.python_module is required", f)
			}
		default:
			return apierror.InvalidArgument("%s needs a container_spec or a python_package_spec", f)
		}
	}
	if o := spec.GetBaseOutputDirectory().GetOutputUriPrefix(); o != "" && !strings.HasPrefix(o, "gs://") {
		return apierror.InvalidArgument("%s.base_output_directory.output_uri_prefix %q is not a gs:// URI", field, o)
	}
	if sa := spec.GetServiceAccount(); sa != "" && !strings.Contains(sa, "@") {
		return apierror.InvalidArgument("%s.service_account %q is not a service account email", field, sa)
	}
	if t := spec.GetScheduling().GetTimeout(); t != nil && t.AsDuration() <= 0 {
		return apierror.InvalidArgument("%s.scheduling.timeout must be positive", field)
	}
	return nil
}

func emptyPool(p *aiplatformpb.WorkerPoolSpec) bool {
	return p == nil || (p.GetTask() == nil && p.GetReplicaCount() == 0 && p.GetMachineSpec() == nil)
}

func (j *jobService) CreateCustomJob(ctx context.Context, req *aiplatformpb.CreateCustomJobRequest) (*aiplatformpb.CustomJob, error) {
	out, err := j.s.createCustomJob(req.GetParent(), req.GetCustomJob(), nil)
	if err != nil {
		return nil, grpcError(err)
	}
	return out, nil
}

// createCustomJob records a custom job and starts it; onEnd, when set, is
// called with the job once it ends.
func (s *Server) createCustomJob(parent string, in *aiplatformpb.CustomJob, onEnd func(*aiplatformpb.CustomJob)) (*aiplatformpb.CustomJob, error) {
	project, _, err := parseParent("parent", parent)
	if err != nil {
		return nil, err
	}
	if in == nil {
		return nil, apierror.InvalidArgument("custom_job is required")
	}
	if in.GetDisplayName() == "" {
		return nil, apierror.InvalidArgument("custom_job.display_name is required")
	}
	if utf8Len(in.GetDisplayName()) > 128 {
		return nil, apierror.InvalidArgument("custom_job.display_name can be up to 128 characters long")
	}
	if err := checkLabels(in.GetLabels()); err != nil {
		return nil, err
	}
	if err := checkCustomJobSpec("custom_job.job_spec", in.GetJobSpec()); err != nil {
		return nil, err
	}
	st := s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.pipelineRunner == nil {
		return nil, apierror.FailedPrecondition("Vertex AI custom training needs Docker and this instance's Cloud Storage to run a job's containers (start with --services including storage)")
	}
	if st.customJobs == nil {
		st.customJobs = map[string]*customJobEntry{}
	}
	name := parent + "/customJobs/" + regNumericID()
	for st.customJobs[name] != nil {
		name = parent + "/customJobs/" + regNumericID()
	}
	j := proto.Clone(in).(*aiplatformpb.CustomJob)
	now := timestamppb.New(st.now())
	j.Name, j.CreateTime, j.UpdateTime, j.StartTime, j.EndTime, j.Error = name, now, now, nil, nil, nil
	j.State = aiplatformpb.JobState_JOB_STATE_QUEUED
	ctx, cancel := context.WithCancel(context.Background())
	e := &customJobEntry{Job: j, cancel: cancel, done: make(chan struct{})}
	st.customJobs[name] = e
	st.save()
	runner := st.pipelineRunner
	go s.runCustomJob(ctx, e, runner, project, onEnd)
	return proto.Clone(j).(*aiplatformpb.CustomJob), nil
}

// setCustomJob updates e under st.mu unless it was deleted.
func (st *notebookStore) setCustomJob(e *customJobEntry, f func(j *aiplatformpb.CustomJob)) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.customJobs[e.Job.GetName()] != e {
		return
	}
	f(e.Job)
	e.Job.UpdateTime = timestamppb.New(st.now())
	st.save()
}

func (s *Server) runCustomJob(ctx context.Context, e *customJobEntry, runner PipelineRunner, project string, onEnd func(*aiplatformpb.CustomJob)) {
	st := s.notebooks()
	defer close(e.done)
	st.setCustomJob(e, func(j *aiplatformpb.CustomJob) {
		j.State = aiplatformpb.JobState_JOB_STATE_PENDING
	})
	st.setCustomJob(e, func(j *aiplatformpb.CustomJob) {
		j.State, j.StartTime = aiplatformpb.JobState_JOB_STATE_RUNNING, timestamppb.New(st.now())
	})
	st.mu.Lock()
	spec := proto.Clone(e.Job.GetJobSpec()).(*aiplatformpb.CustomJobSpec)
	name := e.Job.GetName()
	st.mu.Unlock()
	timeout := defaultJobTimeout
	if t := spec.GetScheduling().GetTimeout(); t != nil {
		timeout = t.AsDuration()
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	err := s.runWorkerPools(runCtx, runner, project, name, spec)
	timedOut := runCtx.Err() == context.DeadlineExceeded
	cancel()
	st.setCustomJob(e, func(j *aiplatformpb.CustomJob) {
		j.EndTime = timestamppb.New(st.now())
		switch {
		case ctx.Err() != nil:
			j.State, j.Error = aiplatformpb.JobState_JOB_STATE_CANCELLED, &status.Status{Code: int32(codes.Canceled), Message: "CANCELED"}
		case timedOut:
			j.State, j.Error = aiplatformpb.JobState_JOB_STATE_FAILED, &status.Status{Code: int32(codes.DeadlineExceeded),
				Message: fmt.Sprintf("The job ran longer than its scheduling.timeout (%s) and was cancelled.", timeout)}
		case err != nil:
			j.State, j.Error = aiplatformpb.JobState_JOB_STATE_FAILED, jobFailure(err)
		default:
			j.State = aiplatformpb.JobState_JOB_STATE_SUCCEEDED
		}
	})
	if onEnd != nil {
		st.mu.Lock()
		out := proto.Clone(e.Job).(*aiplatformpb.CustomJob)
		st.mu.Unlock()
		onEnd(out)
	}
}

// replicaError is a replica's container failure.
type replicaError struct {
	replica string
	err     error
}

func (r *replicaError) Error() string { return r.replica + ": " + r.err.Error() }

var exitStatusRE = regexp.MustCompile(`exit status (\d+)`)

// jobFailure is a failed job's error, as Vertex AI words a replica's
// non-zero exit.
func jobFailure(err error) *status.Status {
	var re *replicaError
	if errors.As(err, &re) {
		code := "1"
		if m := exitStatusRE.FindStringSubmatch(re.err.Error()); m != nil {
			code = m[1]
		}
		return &status.Status{Code: int32(codes.InvalidArgument), Message: fmt.Sprintf("The replica %s exited with a non-zero status of %s. Termination reason: Error. %s",
			re.replica, code, tailLines(re.err.Error(), 20))}
	}
	e := apierror.From(err)
	return &status.Status{Code: int32(e.Code), Message: e.Message}
}

// poolName is a worker pool's name in CLUSTER_SPEC.
func poolName(i int) string { return "workerpool" + strconv.Itoa(i) }

// tfTaskType is a pool's TF_CONFIG task type.
func tfTaskType(i int) string {
	return [...]string{"chief", "worker", "ps", "evaluator"}[i]
}

// runWorkerPools runs every replica of spec's pools side by side and
// waits for them; the first failure is the job's.
func (s *Server) runWorkerPools(ctx context.Context, runner PipelineRunner, project, jobName string, spec *aiplatformpb.CustomJobSpec) error {
	jobID := path.Base(jobName)
	base := strings.TrimSuffix(spec.GetBaseOutputDirectory().GetOutputUriPrefix(), "/")
	cluster := map[string][]string{}
	tfCluster := map[string][]string{}
	for i, p := range spec.GetWorkerPoolSpecs() {
		if i > 0 && emptyPool(p) {
			continue
		}
		for r := 0; r < int(p.GetReplicaCount()); r++ {
			addr := fmt.Sprintf("%s-%d:2222", poolName(i), r)
			cluster[poolName(i)] = append(cluster[poolName(i)], addr)
			tfCluster[tfTaskType(i)] = append(tfCluster[tfTaskType(i)], addr)
		}
	}
	dir, err := os.MkdirTemp("", "cb-customjob-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	gcsDir := filepath.Join(dir, "gcs")
	_ = os.MkdirAll(gcsDir, 0o777)
	_ = os.Chmod(gcsDir, 0o777)
	// The objects the job names, as Cloud Storage FUSE would show them.
	var refs []string
	for _, p := range spec.GetWorkerPoolSpecs() {
		refs = append(refs, p.GetContainerSpec().GetCommand()...)
		refs = append(refs, p.GetContainerSpec().GetArgs()...)
		refs = append(refs, p.GetPythonPackageSpec().GetArgs()...)
		refs = append(refs, p.GetPythonPackageSpec().GetPackageUris()...)
		for _, ev := range append(p.GetContainerSpec().GetEnv(), p.GetPythonPackageSpec().GetEnv()...) {
			refs = append(refs, ev.GetValue())
		}
	}
	if base != "" {
		refs = append(refs, base)
	}
	started := time.Now().Add(-time.Second)
	if err := stageGCS(ctx, runner, gcsDir, refs); err != nil {
		return err
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	for i, p := range spec.GetWorkerPoolSpecs() {
		if i > 0 && emptyPool(p) {
			continue
		}
		for r := 0; r < int(p.GetReplicaCount()); r++ {
			c, err := s.replicaContainer(project, jobID, base, i, r, p, cluster, tfCluster)
			if err != nil {
				return err
			}
			c.Mounts = map[string]string{"/gcs": gcsDir}
			wg.Add(1)
			go func(c PipelineContainer, replica string) {
				defer wg.Done()
				if err := runner.RunContainer(ctx, c); err != nil {
					mu.Lock()
					if first == nil && ctx.Err() == nil {
						first = &replicaError{replica: replica, err: err}
					}
					mu.Unlock()
				}
			}(c, fmt.Sprintf("%s-%d", poolName(i), r))
		}
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// What the job wrote under /gcs goes back to Cloud Storage, failed or
	// not, as the FUSE mount writes it through.
	if err := unstageGCS(context.Background(), runner, gcsDir, started); err != nil && first == nil {
		return fmt.Errorf("writing the job's Cloud Storage output: %v", err)
	}
	return first
}

// replicaContainer is one replica's container, with Vertex AI's training
// environment.
func (s *Server) replicaContainer(project, jobID, base string, pool, replica int, p *aiplatformpb.WorkerPoolSpec,
	cluster, tfCluster map[string][]string) (PipelineContainer, error) {
	env := map[string]string{"CLOUD_ML_PROJECT_ID": project, "GOOGLE_CLOUD_PROJECT": project, "CLOUD_ML_JOB_ID": jobID, "NO_GCE_CHECK": "True"}
	if base != "" {
		env["AIP_MODEL_DIR"] = base + "/model/"
		env["AIP_CHECKPOINT_DIR"] = base + "/checkpoints/"
		env["AIP_TENSORBOARD_LOG_DIR"] = base + "/logs/"
	}
	cs, _ := json.Marshal(map[string]any{"cluster": cluster, "environment": "cloud",
		"task": map[string]any{"type": poolName(pool), "index": replica}, "job": map[string]any{"worker_pool_specs": []any{}}})
	env["CLUSTER_SPEC"] = string(cs)
	tf, _ := json.Marshal(map[string]any{"cluster": tfCluster, "environment": "cloud", "task": map[string]any{"type": tfTaskType(pool), "index": replica}})
	env["TF_CONFIG"] = string(tf)
	c := PipelineContainer{Key: jobID + "-" + poolName(pool) + "-" + strconv.Itoa(replica),
		Labels: map[string]string{"cloudburrow.custom-job": jobID}}
	switch t := p.GetTask().(type) {
	case *aiplatformpb.WorkerPoolSpec_ContainerSpec:
		c.Image, c.Command, c.Args = t.ContainerSpec.GetImageUri(), t.ContainerSpec.GetCommand(), t.ContainerSpec.GetArgs()
		for _, ev := range t.ContainerSpec.GetEnv() {
			env[ev.GetName()] = ev.GetValue()
		}
	case *aiplatformpb.WorkerPoolSpec_PythonPackageSpec:
		pp := t.PythonPackageSpec
		c.Image = pp.GetExecutorImageUri()
		// The package is installed, then its module run, as the prebuilt
		// training containers do; with no network, only what the image
		// already has can satisfy its dependencies.
		var pkgs []string
		for _, u := range pp.GetPackageUris() {
			pkgs = append(pkgs, shellQuote(gcsPath(u)))
		}
		c.Command = []string{"sh", "-c", "python3 -m pip install --no-index --quiet --user " + strings.Join(pkgs, " ") +
			" && exec python3 -m " + shellQuote(pp.GetPythonModule()) + ` "$@"`, "sh"}
		c.Args = pp.GetArgs()
		for _, ev := range pp.GetEnv() {
			env[ev.GetName()] = ev.GetValue()
		}
	default:
		return c, fmt.Errorf("worker pool %d has no container_spec or python_package_spec", pool)
	}
	c.Env = env
	return c, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var gcsRefRE = regexp.MustCompile(`(?:gs://|/gcs/)([a-z0-9][a-z0-9._-]{1,220})(/[^\s"',;]*)?`)

// maxStagedBytes bounds what a job's /gcs folder is given.
const maxStagedBytes = 2 << 30

// stageGCS downloads into dir every object under each gs:// (or /gcs/)
// prefix refs name.
func stageGCS(ctx context.Context, runner PipelineRunner, dir string, refs []string) error {
	seen := map[string]bool{}
	total := 0
	for _, ref := range refs {
		for _, m := range gcsRefRE.FindAllStringSubmatch(ref, -1) {
			bucket, object := m[1], strings.Trim(m[2], "/")
			key := bucket + "/" + object
			if seen[key] {
				continue
			}
			seen[key] = true
			var names []string
			if object != "" {
				if _, err := runner.ReadObject(ctx, bucket, object, 0); err == nil {
					names = append(names, object)
				}
			}
			prefix := object
			if prefix != "" {
				prefix += "/"
			}
			more, err := runner.ListObjects(ctx, bucket, prefix)
			if err != nil {
				// A bucket the job names but that does not exist is simply
				// absent from /gcs.
				continue
			}
			names = append(names, more...)
			for _, n := range names {
				if seen["obj:"+bucket+"/"+n] {
					continue
				}
				seen["obj:"+bucket+"/"+n] = true
				b, err := runner.ReadObject(ctx, bucket, n, 0)
				if err != nil {
					continue
				}
				if total += len(b); total > maxStagedBytes {
					return fmt.Errorf("the Cloud Storage objects the job names are more than %d bytes", maxStagedBytes)
				}
				p := filepath.Join(dir, bucket, filepath.FromSlash(n))
				_ = os.MkdirAll(filepath.Dir(p), 0o777)
				if err := os.WriteFile(p, b, 0o666); err != nil {
					return err
				}
			}
		}
	}
	_ = filepath.Walk(dir, func(p string, _ os.FileInfo, _ error) error { _ = os.Chmod(p, 0o777); return nil })
	return nil
}

// unstageGCS writes every file of dir changed since since back to its
// object (dir/BUCKET/OBJECT).
func unstageGCS(ctx context.Context, runner PipelineRunner, dir string, since time.Time) error {
	var files []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() && !info.ModTime().Before(since) {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	for _, p := range files {
		rel, _ := filepath.Rel(dir, p)
		bucket, object, ok := strings.Cut(filepath.ToSlash(rel), "/")
		if !ok || object == "" {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := runner.WriteObject(ctx, bucket, object, "application/octet-stream", b); err != nil {
			return err
		}
	}
	return nil
}

func (st *notebookStore) customJob(name string) (*customJobEntry, error) {
	if _, _, _, err := parseChild("name", "customJobs", name); err != nil {
		return nil, err
	}
	e, ok := st.customJobs[name]
	if !ok {
		return nil, apierror.NotFound("CustomJob %s not found", name)
	}
	return e, nil
}

func (j *jobService) GetCustomJob(_ context.Context, req *aiplatformpb.GetCustomJobRequest) (*aiplatformpb.CustomJob, error) {
	st := j.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	e, err := st.customJob(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	return proto.Clone(e.Job).(*aiplatformpb.CustomJob), nil
}

// listJobs filters, orders and pages a job list, as every JobService list
// does: the filter's fields, create_time desc by default.
func listJobs[T proto.Message](kind, parent, filter, orderBy, token string, size int32, all []T) ([]T, string, error) {
	all, err := aipfilter.Apply(filter, all, trainJobFilter...)
	if err != nil {
		return nil, "", err
	}
	order := orderBy
	if order == "" {
		order = "create_time desc"
	}
	if err := aipfilter.SortBy(order, all, trainJobOrder); err != nil {
		return nil, "", err
	}
	n := int(size)
	if n <= 0 {
		n = 100
	}
	return aipfilter.Page(kind+":"+parent+"|"+filter+"|"+order, all, token, n)
}

func (j *jobService) ListCustomJobs(_ context.Context, req *aiplatformpb.ListCustomJobsRequest) (*aiplatformpb.ListCustomJobsResponse, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	st := j.s.notebooks()
	st.mu.Lock()
	var all []*aiplatformpb.CustomJob
	for _, k := range sortedNames(st.customJobs) {
		if strings.HasPrefix(k, req.GetParent()+"/customJobs/") {
			all = append(all, proto.Clone(st.customJobs[k].Job).(*aiplatformpb.CustomJob))
		}
	}
	st.mu.Unlock()
	items, next, err := listJobs("customJobs", req.GetParent(), req.GetFilter(), "", req.GetPageToken(), req.GetPageSize(), all)
	if err != nil {
		return nil, grpcError(err)
	}
	return &aiplatformpb.ListCustomJobsResponse{CustomJobs: items, NextPageToken: next}, nil
}

func (j *jobService) DeleteCustomJob(_ context.Context, req *aiplatformpb.DeleteCustomJobRequest) (*longrunningpb.Operation, error) {
	st := j.s.notebooks()
	st.mu.Lock()
	e, err := st.customJob(req.GetName())
	if err == nil && jobRunning(e.Job.GetState()) {
		err = apierror.FailedPrecondition("CustomJob %s is %s: cancel it before deleting it", req.GetName(), e.Job.GetState())
	}
	if err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	delete(st.customJobs, req.GetName())
	st.save()
	st.mu.Unlock()
	return j.s.finished(req.GetName(), &aiplatformpb.DeleteOperationMetadata{GenericMetadata: generic()}, &emptypb.Empty{}, nil)
}

func (j *jobService) CancelCustomJob(_ context.Context, req *aiplatformpb.CancelCustomJobRequest) (*emptypb.Empty, error) {
	st := j.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	e, err := st.customJob(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	if !jobRunning(e.Job.GetState()) {
		return nil, grpcError(apierror.FailedPrecondition("CustomJob %s is %s and cannot be cancelled", req.GetName(), e.Job.GetState()))
	}
	e.Job.State, e.Job.UpdateTime = aiplatformpb.JobState_JOB_STATE_CANCELLING, timestamppb.New(st.now())
	if e.cancel != nil {
		e.cancel()
	}
	st.save()
	return &emptypb.Empty{}, nil
}

// waitCustomJob waits for a custom job's run to end and returns it.
func (s *Server) waitCustomJob(ctx context.Context, name string) (*aiplatformpb.CustomJob, error) {
	st := s.notebooks()
	st.mu.Lock()
	e, err := st.customJob(name)
	st.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case <-e.done:
	case <-ctx.Done():
		st.mu.Lock()
		if jobRunning(e.Job.GetState()) && e.cancel != nil {
			e.cancel()
		}
		st.mu.Unlock()
		<-e.done
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return proto.Clone(e.Job).(*aiplatformpb.CustomJob), nil
}

// ---- persistence

// savedJobs are the custom jobs, batch prediction jobs and training
// pipelines as the store's file keeps them; the caller holds st.mu.
func (st *notebookStore) savedJobs() map[string][]json.RawMessage {
	out := map[string][]json.RawMessage{}
	for _, n := range sortedNames(st.customJobs) {
		if b, err := protojson.Marshal(st.customJobs[n].Job); err == nil {
			out["customJobs"] = append(out["customJobs"], b)
		}
	}
	for _, n := range sortedNames(st.batchJobs) {
		if b, err := protojson.Marshal(st.batchJobs[n].Job); err == nil {
			out["batchPredictionJobs"] = append(out["batchPredictionJobs"], b)
		}
	}
	for _, n := range sortedNames(st.trainingPipelines) {
		if b, err := protojson.Marshal(st.trainingPipelines[n].TP); err == nil {
			out["trainingPipelines"] = append(out["trainingPipelines"], b)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

var stoppedWithInstance = &status.Status{Code: int32(codes.Aborted), Message: "the instance stopped while the job ran"}

// loadJobs restores saved jobs: one running when the instance stopped
// failed with it.
func (st *notebookStore) loadTrainingJobs(raw map[string][]json.RawMessage) {
	st.customJobs, st.batchJobs, st.trainingPipelines = map[string]*customJobEntry{}, map[string]*batchJobEntry{}, map[string]*trainingEntry{}
	closed := make(chan struct{})
	close(closed)
	for _, b := range raw["customJobs"] {
		j := &aiplatformpb.CustomJob{}
		if lenient.Unmarshal(b, j) != nil || j.GetName() == "" {
			continue
		}
		if jobRunning(j.GetState()) {
			j.State, j.Error, j.EndTime = aiplatformpb.JobState_JOB_STATE_FAILED, stoppedWithInstance, timestamppb.New(time.Now())
		}
		st.customJobs[j.GetName()] = &customJobEntry{Job: j, done: closed}
	}
	for _, b := range raw["batchPredictionJobs"] {
		j := &aiplatformpb.BatchPredictionJob{}
		if lenient.Unmarshal(b, j) != nil || j.GetName() == "" {
			continue
		}
		if jobRunning(j.GetState()) {
			j.State, j.Error, j.EndTime = aiplatformpb.JobState_JOB_STATE_FAILED, stoppedWithInstance, timestamppb.New(time.Now())
		}
		st.batchJobs[j.GetName()] = &batchJobEntry{Job: j, done: closed}
	}
	for _, b := range raw["trainingPipelines"] {
		t := &aiplatformpb.TrainingPipeline{}
		if lenient.Unmarshal(b, t) != nil || t.GetName() == "" {
			continue
		}
		if trainingRunning(t.GetState()) {
			t.State, t.Error, t.EndTime = aiplatformpb.PipelineState_PIPELINE_STATE_FAILED, stoppedWithInstance, timestamppb.New(time.Now())
		}
		st.trainingPipelines[t.GetName()] = &trainingEntry{TP: t}
	}
}

func tailLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}
