package vertexai

// Agent Platform Neural Architecture Search (#2214): JobService's NasJob
// and NasTrialDetail methods over gRPC and REST. A multi-trial search runs
// here: its controller (nassearch.go) samples architectures from the
// search space, each search trial is a real CustomJob on this instance
// (customjobs.go) given the architecture in --nas_params_str, and its
// reward is the metric it reports as hyperparameter tuning trials do
// (cloudml-hypertune's file, hptuning.go). Every train_trial_spec.frequency
// search trials, the best search trials not yet trained are trained by the
// train trial job spec.
//
// Documented behaviour (the NasJob, NasTrial and NasTrialDetail protos,
// cloud.google.com/go/aiplatform v1.127.0; REST projects.locations.nasJobs
// and nasJobs.nasTrialDetails; "Neural Architecture Search overview",
// pre-built-trainer, suggested-workflow and nas-client pages under
// https://docs.cloud.google.com/gemini-enterprise-agent-platform/machine-learning/training/neural-architecture-search/;
// github.com/google/vertex-ai-nas's vertex_nas_cli.py, cloud_nas_utils.py,
// metrics_reporter.py and tutorials 1 and 3, Apache-2.0; read 2026-11-24):
//   - multi_trial_algorithm "Defaults to REINFORCEMENT_LEARNING"; GRID_SEARCH;
//   - max_failed_trial_count is "The number of failed trials that need to
//     be seen before failing the NasJob. If set to 0, Agent Platform
//     decides how many trials must fail before the whole job fails";
//   - train trials: "top N [TrainTrialSpec.max_parallel_trial_count]
//     search trials will be trained for every M [TrainTrialSpec.frequency]
//     trials searched";
//   - "The NAS-service will send some mandatory arguments to your trainer
//     via flags": --nas_params_str ("Nas args serialized in JSON string")
//     and --job-dir; a train trial also gets --retrain_search_job_dir and
//     --retrain_search_job_trials, and maps its CLOUD_ML_TRIAL_ID (1-based)
//     to that list's entry;
//   - the trainer reports its reward to CLOUD_ML_HP_METRIC_FILE (default
//     /tmp/hypertune/output.metrics) under the tag CLOUD_ML_HP_METRIC_TAG,
//     the metric_id;
//   - resume_nas_job_id: "searchSpaceSpec and nas_algorithm_spec are
//     obtained from previous NasJob hence should not provide them again";
//     "you can't resume the search from another search resume job";
//   - "The default NAS job timeout is set to 14 days";
//   - a NasTrialDetail's train_trial is "Only populated if search_trial is
//     used for training".
//
// UNVERIFIED (re-read 2026-11-24): with max_failed_trial_count 0 the job
// fails when the first trial fails or more trials failed than succeeded
// (hyperparameter tuning's rule here); search trial IDs are 1, 2, ...,
// and train trial IDs too, in the order trained; a trial's job directory
// (--job-dir) is BASE/nas/search/ID or BASE/nas/train/ID under its spec's
// base_output_directory; a resumed job's controller learns from the
// previous job's rewards first; a job's NasTrialDetail ID is its search
// trial's.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
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

// nasJobEntry is a NAS job, its trials' parameters and its run.
type nasJobEntry struct {
	Job *aiplatformpb.NasJob
	// Params are the search trials' --nas_params_str, by trial id.
	Params map[string]string
	// TrainOf is the search trial each train trial trains, by train id.
	TrainOf map[string]string
	cancel  context.CancelFunc
	done    chan struct{}
}

type savedNASJob struct {
	Job     []byte            `json:"job"`
	Params  map[string]string `json:"params,omitempty"`
	TrainOf map[string]string `json:"trainOf,omitempty"`
}

const nasTimeout = 14 * 24 * time.Hour

// checkNASSpec validates a NasJob's spec.
func checkNASSpec(j *aiplatformpb.NasJob) error {
	spec := j.GetNasJobSpec()
	if spec == nil {
		return apierror.InvalidArgument("nas_job.nas_job_spec is required")
	}
	if spec.GetResumeNasJobId() == "" && spec.GetSearchSpaceSpec() == "" {
		return apierror.InvalidArgument("nas_job.nas_job_spec.search_space_spec is required")
	}
	if spec.GetResumeNasJobId() == "" && spec.GetMultiTrialAlgorithmSpec() == nil {
		return apierror.InvalidArgument("nas_job.nas_job_spec.multi_trial_algorithm_spec is required")
	}
	if spec.GetSearchSpaceSpec() != "" {
		if _, err := parseSearchSpace(spec.GetSearchSpaceSpec()); err != nil {
			return err
		}
	}
	a := spec.GetMultiTrialAlgorithmSpec()
	if a == nil {
		return nil
	}
	f := "nas_job.nas_job_spec.multi_trial_algorithm_spec"
	if a.GetMetric().GetMetricId() == "" {
		return apierror.InvalidArgument("%s.metric.metric_id is required", f)
	}
	if a.GetMetric().GetGoal() == aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_MetricSpec_GOAL_TYPE_UNSPECIFIED {
		return apierror.InvalidArgument("%s.metric.goal is required: MAXIMIZE or MINIMIZE", f)
	}
	st := a.GetSearchTrialSpec()
	if st == nil || st.GetSearchTrialJobSpec() == nil {
		return apierror.InvalidArgument("%s.search_trial_spec.search_trial_job_spec is required", f)
	}
	if err := checkCustomJobSpec(f+".search_trial_spec.search_trial_job_spec", st.GetSearchTrialJobSpec()); err != nil {
		return err
	}
	if st.GetMaxTrialCount() <= 0 {
		return apierror.InvalidArgument("%s.search_trial_spec.max_trial_count must be positive", f)
	}
	if st.GetMaxParallelTrialCount() <= 0 {
		return apierror.InvalidArgument("%s.search_trial_spec.max_parallel_trial_count must be positive", f)
	}
	if st.GetMaxFailedTrialCount() < 0 || st.GetMaxFailedTrialCount() > st.GetMaxTrialCount() {
		return apierror.InvalidArgument("%s.search_trial_spec.max_failed_trial_count must be between 0 and max_trial_count", f)
	}
	if tt := a.GetTrainTrialSpec(); tt != nil {
		if tt.GetTrainTrialJobSpec() == nil {
			return apierror.InvalidArgument("%s.train_trial_spec.train_trial_job_spec is required", f)
		}
		if err := checkCustomJobSpec(f+".train_trial_spec.train_trial_job_spec", tt.GetTrainTrialJobSpec()); err != nil {
			return err
		}
		if tt.GetMaxParallelTrialCount() <= 0 {
			return apierror.InvalidArgument("%s.train_trial_spec.max_parallel_trial_count must be positive", f)
		}
		if tt.GetFrequency() <= 0 {
			return apierror.InvalidArgument("%s.train_trial_spec.frequency must be positive", f)
		}
	}
	return nil
}

func (j *jobService) CreateNasJob(_ context.Context, req *aiplatformpb.CreateNasJobRequest) (*aiplatformpb.NasJob, error) {
	out, err := j.s.createNASJob(req.GetParent(), req.GetNasJob())
	if err != nil {
		return nil, grpcError(err)
	}
	return out, nil
}

func (s *Server) createNASJob(parent string, in *aiplatformpb.NasJob) (*aiplatformpb.NasJob, error) {
	if _, _, err := parseParent("parent", parent); err != nil {
		return nil, err
	}
	if in == nil {
		return nil, apierror.InvalidArgument("nas_job is required")
	}
	if in.GetDisplayName() == "" {
		return nil, apierror.InvalidArgument("nas_job.display_name is required")
	}
	if utf8Len(in.GetDisplayName()) > 128 {
		return nil, apierror.InvalidArgument("nas_job.display_name can be up to 128 characters long")
	}
	if err := checkLabels(in.GetLabels()); err != nil {
		return nil, err
	}
	if err := checkNASSpec(in); err != nil {
		return nil, err
	}
	st := s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.pipelineRunner == nil {
		return nil, apierror.FailedPrecondition("Neural Architecture Search runs each trial as a custom job, which needs Docker and this instance's Cloud Storage (start with --services including storage)")
	}
	if st.nasJobs == nil {
		st.nasJobs = map[string]*nasJobEntry{}
	}
	j := proto.Clone(in).(*aiplatformpb.NasJob)
	var prior *nasJobEntry
	if id := j.GetNasJobSpec().GetResumeNasJobId(); id != "" {
		prior = st.nasJobs[parent+"/nasJobs/"+id]
		if prior == nil {
			return nil, apierror.NotFound("NasJob %s/nasJobs/%s not found: resume_nas_job_id names a job in the same project and location", parent, id)
		}
		if prior.Job.GetNasJobSpec().GetResumeNasJobId() != "" {
			return nil, apierror.FailedPrecondition("NasJob %s resumes another job: you can't resume the search from another search resume job", prior.Job.GetName())
		}
		if jobRunning(prior.Job.GetState()) {
			return nil, apierror.FailedPrecondition("NasJob %s is %s: a search is resumed once it has ended", prior.Job.GetName(), prior.Job.GetState())
		}
		for _, e := range st.nasJobs {
			if e.Job.GetNasJobSpec().GetResumeNasJobId() == id {
				return nil, apierror.FailedPrecondition("NasJob %s was already resumed by %s: a search can be resumed only once", prior.Job.GetName(), e.Job.GetName())
			}
		}
		if j.NasJobSpec.SearchSpaceSpec == "" {
			j.NasJobSpec.SearchSpaceSpec = prior.Job.GetNasJobSpec().GetSearchSpaceSpec()
		}
		if j.NasJobSpec.GetMultiTrialAlgorithmSpec() == nil {
			j.NasJobSpec.NasAlgorithmSpec = &aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_{MultiTrialAlgorithmSpec: proto.Clone(prior.Job.GetNasJobSpec().GetMultiTrialAlgorithmSpec()).(*aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec)}
		}
		if err := checkNASSpec(j); err != nil {
			return nil, err
		}
	}
	space, _ := parseSearchSpace(j.GetNasJobSpec().GetSearchSpaceSpec())
	a := j.GetNasJobSpec().GetMultiTrialAlgorithmSpec()
	ctrl, err := newNASController(parent+"/"+j.GetDisplayName(), a.GetMultiTrialAlgorithm(), space, int(a.GetSearchTrialSpec().GetMaxTrialCount()))
	if err != nil {
		return nil, err
	}
	name := parent + "/nasJobs/" + regNumericID()
	for st.nasJobs[name] != nil {
		name = parent + "/nasJobs/" + regNumericID()
	}
	now := timestamppb.New(st.now())
	j.Name, j.CreateTime, j.UpdateTime, j.StartTime, j.EndTime, j.Error, j.NasJobOutput = name, now, now, nil, nil, nil, nil
	j.State = aiplatformpb.JobState_JOB_STATE_QUEUED
	ctx, cancel := context.WithTimeout(context.Background(), nasTimeout)
	e := &nasJobEntry{Job: j, Params: map[string]string{}, TrainOf: map[string]string{}, cancel: cancel, done: make(chan struct{})}
	st.nasJobs[name] = e
	st.save()
	var history []nasObservation
	if prior != nil {
		history = priorRewards(prior, a.GetMetric().GetGoal() == aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_MetricSpec_MINIMIZE, space)
	}
	go s.runNASJob(ctx, e, parent, ctrl, history)
	return proto.Clone(j).(*aiplatformpb.NasJob), nil
}

type nasObservation struct {
	arch   nasArch
	reward float64
}

// priorRewards are a previous job's succeeded search trials, with their
// architectures read back from their parameters.
func priorRewards(prior *nasJobEntry, minimize bool, space []*nasDecision) []nasObservation {
	var out []nasObservation
	for _, t := range prior.Job.GetNasJobOutput().GetMultiTrialJobOutput().GetSearchTrials() {
		if t.GetState() != aiplatformpb.NasTrial_SUCCEEDED || len(t.GetFinalMeasurement().GetMetrics()) == 0 {
			continue
		}
		arch := archFromParams(space, prior.Params[t.GetId()])
		if arch == nil {
			continue
		}
		r := t.GetFinalMeasurement().GetMetrics()[0].GetValue()
		if minimize {
			r = -r
		}
		out = append(out, nasObservation{arch: arch, reward: r})
	}
	return out
}

// archFromParams reads an architecture back from its --nas_params_str.
func archFromParams(space []*nasDecision, params string) nasArch {
	var vals map[string]any
	if params == "" || json.Unmarshal([]byte(params), &vals) != nil {
		return nil
	}
	var arch nasArch
	var rec func(ds []*nasDecision) bool
	idx := func(v any) (int, bool) {
		s, _ := v.(string)
		i, err := strconv.Atoi(s[:max(0, strings.Index(s, "/"))])
		return i, err == nil
	}
	rec = func(ds []*nasDecision) bool {
		for _, d := range ds {
			switch d.kind {
			case "float":
				f, ok := vals[d.id].(float64)
				if !ok {
					return false
				}
				arch = append(arch, nasChoice{d: d, value: f})
			default:
				var picks []int
				for j := 0; j < d.num; j++ {
					key := d.id
					if d.kind == "manyof" {
						key += "[" + strconv.Itoa(j) + "]"
					}
					i, ok := idx(vals[key])
					if !ok || i >= len(d.candidates) {
						return false
					}
					picks = append(picks, i)
				}
				arch = append(arch, nasChoice{d: d, picks: picks})
				for _, p := range picks {
					if !rec(d.children[p]) {
						return false
					}
				}
			}
		}
		return true
	}
	if !rec(space) {
		return nil
	}
	return arch
}

func (st *notebookStore) setNASJob(e *nasJobEntry, f func(j *aiplatformpb.NasJob)) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.nasJobs[e.Job.GetName()] != e {
		return
	}
	f(e.Job)
	e.Job.UpdateTime = timestamppb.New(st.now())
	st.save()
}

// nasTrialJob is a NAS trial's CustomJob: spec, its replicas given args,
// its output under BASE/nas/STAGE/ID.
func nasTrialJob(j *aiplatformpb.NasJob, spec *aiplatformpb.CustomJobSpec, stage, id string, args []string) (*aiplatformpb.CustomJob, string) {
	spec = proto.Clone(spec).(*aiplatformpb.CustomJobSpec)
	dir := ""
	if b := spec.GetBaseOutputDirectory().GetOutputUriPrefix(); b != "" {
		dir = strings.TrimSuffix(b, "/") + "/nas/" + stage + "/" + id
		spec.BaseOutputDirectory.OutputUriPrefix = dir
		args = append(args, "--job-dir="+dir)
	}
	for _, p := range spec.GetWorkerPoolSpecs() {
		switch t := p.GetTask().(type) {
		case *aiplatformpb.WorkerPoolSpec_ContainerSpec:
			t.ContainerSpec.Args = append(t.ContainerSpec.Args, args...)
		case *aiplatformpb.WorkerPoolSpec_PythonPackageSpec:
			t.PythonPackageSpec.Args = append(t.PythonPackageSpec.Args, args...)
		}
	}
	return &aiplatformpb.CustomJob{DisplayName: truncateUTF8(j.GetDisplayName()+"-"+stage+"-trial-"+id, 128), JobSpec: spec,
		Labels: j.GetLabels(), EncryptionSpec: j.GetEncryptionSpec()}, dir
}

type nasTrialDone struct {
	stage, id string
	job       *aiplatformpb.CustomJob
	dir       string
}

func (s *Server) runNASJob(ctx context.Context, e *nasJobEntry, parent string, ctrl nasController, history []nasObservation) {
	st := s.notebooks()
	defer close(e.done)
	defer e.cancel()
	for _, h := range history {
		ctrl.observe(h.arch, h.reward)
	}
	st.setNASJob(e, func(j *aiplatformpb.NasJob) {
		j.State, j.StartTime = aiplatformpb.JobState_JOB_STATE_RUNNING, timestamppb.New(st.now())
		j.NasJobOutput = &aiplatformpb.NasJobOutput{Output: &aiplatformpb.NasJobOutput_MultiTrialJobOutput_{MultiTrialJobOutput: &aiplatformpb.NasJobOutput_MultiTrialJobOutput{}}}
	})
	st.mu.Lock()
	spec := proto.Clone(e.Job).(*aiplatformpb.NasJob)
	st.mu.Unlock()
	a := spec.GetNasJobSpec().GetMultiTrialAlgorithmSpec()
	sts, tts := a.GetSearchTrialSpec(), a.GetTrainTrialSpec()
	metric := []*aiplatformpb.StudySpec_MetricSpec{{MetricId: a.GetMetric().GetMetricId()}}
	minimize := a.GetMetric().GetGoal() == aiplatformpb.NasJobSpec_MultiTrialAlgorithmSpec_MetricSpec_MINIMIZE
	maxTrials, parallel, maxFailed := int(sts.GetMaxTrialCount()), int(sts.GetMaxParallelTrialCount()), int(sts.GetMaxFailedTrialCount())
	results := make(chan nasTrialDone, parallel+int(tts.GetMaxParallelTrialCount())+1)
	running := map[string]string{} // stage/id -> custom job
	archs := map[string]nasArch{}
	rewards := map[string]float64{} // succeeded search trials
	var trainQueue []string         // search trials to train
	trained := map[string]bool{}
	var trainOrder []string
	searchBase := strings.TrimSuffix(sts.GetSearchTrialJobSpec().GetBaseOutputDirectory().GetOutputUriPrefix(), "/")
	if searchBase != "" {
		searchBase += "/nas/search"
	}
	started, failed, succeeded, completedSearch, trainsRunning := 0, 0, 0, 0, 0
	var firstEnded *bool
	var lastErr *status.Status
	stop, exhausted := false, false
	trials := func(j *aiplatformpb.NasJob, stage string) *[]*aiplatformpb.NasTrial {
		o := j.GetNasJobOutput().GetMultiTrialJobOutput()
		if stage == "train" {
			return &o.TrainTrials
		}
		return &o.SearchTrials
	}
	setTrial := func(stage, id string, f func(t *aiplatformpb.NasTrial)) {
		st.setNASJob(e, func(j *aiplatformpb.NasJob) {
			for _, t := range *trials(j, stage) {
				if t.GetId() == id {
					f(t)
				}
			}
		})
	}
	tooManyFailed := func() bool {
		if maxFailed > 0 {
			return failed >= maxFailed
		}
		return (firstEnded != nil && !*firstEnded) || (failed > 0 && failed > succeeded)
	}
	launch := func(stage, id string, job *aiplatformpb.CustomJob) error {
		dir, err := os.MkdirTemp("", "cb-nastrial-")
		if err != nil {
			return err
		}
		_ = os.Chmod(dir, 0o777)
		tctx := context.WithValue(context.Background(), trialRunKey{}, &trialRun{id: id, metricsDir: dir,
			env: map[string]string{"CLOUD_ML_HP_METRIC_TAG": a.GetMetric().GetMetricId()}})
		cj, err := s.startCustomJob(tctx, parent, job, func(done *aiplatformpb.CustomJob) {
			results <- nasTrialDone{stage: stage, id: id, job: done, dir: dir}
		})
		if err != nil {
			_ = os.RemoveAll(dir)
			return err
		}
		running[stage+"/"+id] = cj.GetName()
		return nil
	}
	startTrain := func() {
		for !stop && ctx.Err() == nil && tts != nil && len(trainQueue) > 0 && trainsRunning < int(tts.GetMaxParallelTrialCount()) {
			sid := trainQueue[0]
			trainQueue = trainQueue[1:]
			trainOrder = append(trainOrder, sid)
			tid := strconv.Itoa(len(trainOrder))
			params := e.Params[sid]
			args := []string{"--nas_params_str=" + params, "--retrain_search_job_trials=" + strings.Join(trainOrder, ",")}
			if searchBase != "" {
				args = append(args, "--retrain_search_job_dir="+searchBase)
			}
			job, _ := nasTrialJob(spec, tts.GetTrainTrialJobSpec(), "train", tid, args)
			t := &aiplatformpb.NasTrial{Id: tid, State: aiplatformpb.NasTrial_ACTIVE, StartTime: timestamppb.New(st.now())}
			st.setNASJob(e, func(j *aiplatformpb.NasJob) {
				o := j.GetNasJobOutput().GetMultiTrialJobOutput()
				o.TrainTrials = append(o.TrainTrials, t)
				e.TrainOf[tid] = sid
			})
			if err := launch("train", tid, job); err != nil {
				setTrial("train", tid, func(t *aiplatformpb.NasTrial) {
					t.State, t.EndTime = aiplatformpb.NasTrial_INFEASIBLE, timestamppb.New(st.now())
				})
				continue
			}
			trainsRunning++
		}
	}
	queueTrains := func() {
		if tts == nil || completedSearch%int(tts.GetFrequency()) != 0 {
			return
		}
		var ids []string
		for id := range rewards {
			if !trained[id] {
				ids = append(ids, id)
			}
		}
		sort.Slice(ids, func(i, j int) bool {
			if rewards[ids[i]] != rewards[ids[j]] {
				return rewards[ids[i]] > rewards[ids[j]]
			}
			a, _ := strconv.Atoi(ids[i])
			b, _ := strconv.Atoi(ids[j])
			return a < b
		})
		for _, id := range ids[:min(len(ids), int(tts.GetMaxParallelTrialCount()))] {
			trained[id] = true
			trainQueue = append(trainQueue, id)
		}
	}
	searchRunning := func() int {
		n := 0
		for k := range running {
			if strings.HasPrefix(k, "search/") {
				n++
			}
		}
		return n
	}
	for {
		for !stop && !exhausted && ctx.Err() == nil && searchRunning() < parallel && started < maxTrials {
			arch, ok := ctrl.suggest()
			if !ok {
				exhausted = true
				break
			}
			started++
			id := strconv.Itoa(started)
			params := arch.params()
			archs[id] = arch
			t := &aiplatformpb.NasTrial{Id: id, State: aiplatformpb.NasTrial_ACTIVE, StartTime: timestamppb.New(st.now())}
			st.setNASJob(e, func(j *aiplatformpb.NasJob) {
				o := j.GetNasJobOutput().GetMultiTrialJobOutput()
				o.SearchTrials = append(o.SearchTrials, t)
				e.Params[id] = params
			})
			job, _ := nasTrialJob(spec, sts.GetSearchTrialJobSpec(), "search", id, []string{"--nas_params_str=" + params})
			if err := launch("search", id, job); err != nil {
				failed++
				ae := apierror.From(err)
				lastErr = &status.Status{Code: int32(ae.Code), Message: ae.Message}
				setTrial("search", id, func(t *aiplatformpb.NasTrial) {
					t.State, t.EndTime = aiplatformpb.NasTrial_INFEASIBLE, timestamppb.New(st.now())
				})
				if firstEnded == nil {
					f := false
					firstEnded = &f
				}
				if tooManyFailed() {
					stop = true
				}
			}
		}
		startTrain()
		if len(running) == 0 {
			break
		}
		var r nasTrialDone
		select {
		case r = <-results:
		case <-ctx.Done():
			for _, cj := range running {
				_ = s.cancelCustomJob(cj)
			}
			r = <-results
		}
		delete(running, r.stage+"/"+r.id)
		var start time.Time
		if r.job.GetStartTime() != nil {
			start = r.job.GetStartTime().AsTime()
		}
		ms := readHypertune(r.dir, metric, start)
		sortMeasurements(ms)
		_ = os.RemoveAll(r.dir)
		var final *aiplatformpb.Measurement
		if len(ms) > 0 {
			final = ms[len(ms)-1]
		}
		ok := r.job.GetState() == aiplatformpb.JobState_JOB_STATE_SUCCEEDED && final != nil
		switch {
		case r.job.GetState() == aiplatformpb.JobState_JOB_STATE_CANCELLED:
		case r.job.GetState() != aiplatformpb.JobState_JOB_STATE_SUCCEEDED:
			lastErr = r.job.GetError()
		case final == nil:
			lastErr = &status.Status{Code: int32(codes.InvalidArgument), Message: fmt.Sprintf(
				"The trial ended without reporting the metric %s (to CLOUD_ML_HP_METRIC_FILE, tag CLOUD_ML_HP_METRIC_TAG), so it has no reward.", a.GetMetric().GetMetricId())}
		}
		setTrial(r.stage, r.id, func(t *aiplatformpb.NasTrial) {
			t.EndTime = timestamppb.New(st.now())
			if ok {
				t.State, t.FinalMeasurement = aiplatformpb.NasTrial_SUCCEEDED, final
			} else {
				t.State = aiplatformpb.NasTrial_INFEASIBLE
			}
		})
		if r.stage == "train" {
			trainsRunning--
			continue
		}
		if ctx.Err() != nil {
			continue
		}
		completedSearch++
		if firstEnded == nil {
			firstEnded = &ok
		}
		if ok {
			succeeded++
			y := final.GetMetrics()[0].GetValue()
			if minimize {
				y = -y
			}
			rewards[r.id] = y
			ctrl.observe(archs[r.id], y)
		} else {
			failed++
			if tooManyFailed() && !stop {
				stop = true
				for k, cj := range running {
					if strings.HasPrefix(k, "search/") {
						_ = s.cancelCustomJob(cj)
					}
				}
			}
		}
		queueTrains()
	}
	st.setNASJob(e, func(j *aiplatformpb.NasJob) {
		j.EndTime = timestamppb.New(st.now())
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			j.State, j.Error = aiplatformpb.JobState_JOB_STATE_CANCELLED, &status.Status{Code: int32(codes.DeadlineExceeded), Message: "The NAS job reached its 14-day timeout and was cancelled."}
		case ctx.Err() != nil:
			j.State, j.Error = aiplatformpb.JobState_JOB_STATE_CANCELLED, &status.Status{Code: int32(codes.Canceled), Message: "CANCELED"}
		case stop || succeeded == 0:
			code, msg := int32(codes.Internal), ""
			if lastErr != nil {
				code, msg = lastErr.GetCode(), lastErr.GetMessage()
			}
			j.State, j.Error = aiplatformpb.JobState_JOB_STATE_FAILED, &status.Status{Code: code,
				Message: fmt.Sprintf("%d of the %d search trials run failed. The last failure: %s", failed, started, msg)}
		default:
			j.State = aiplatformpb.JobState_JOB_STATE_SUCCEEDED
		}
	})
}

func (st *notebookStore) nasJob(name string) (*nasJobEntry, error) {
	if _, _, _, err := parseChild("name", "nasJobs", name); err != nil {
		return nil, err
	}
	e, ok := st.nasJobs[name]
	if !ok {
		return nil, apierror.NotFound("NasJob %s not found", name)
	}
	return e, nil
}

func (j *jobService) GetNasJob(_ context.Context, req *aiplatformpb.GetNasJobRequest) (*aiplatformpb.NasJob, error) {
	st := j.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	e, err := st.nasJob(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	return proto.Clone(e.Job).(*aiplatformpb.NasJob), nil
}

func (j *jobService) ListNasJobs(_ context.Context, req *aiplatformpb.ListNasJobsRequest) (*aiplatformpb.ListNasJobsResponse, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	st := j.s.notebooks()
	st.mu.Lock()
	var all []*aiplatformpb.NasJob
	for _, k := range sortedNames(st.nasJobs) {
		if strings.HasPrefix(k, req.GetParent()+"/nasJobs/") {
			all = append(all, proto.Clone(st.nasJobs[k].Job).(*aiplatformpb.NasJob))
		}
	}
	st.mu.Unlock()
	items, next, err := listJobs("nasJobs", req.GetParent(), req.GetFilter(), "", req.GetPageToken(), req.GetPageSize(), all)
	if err != nil {
		return nil, grpcError(err)
	}
	if req.GetReadMask() != nil {
		keep := map[string]bool{}
		for _, p := range req.GetReadMask().GetPaths() {
			keep[camelToSnake(p)] = true
		}
		for _, it := range items {
			m := it.ProtoReflect()
			fields := m.Descriptor().Fields()
			for i := 0; i < fields.Len(); i++ {
				if fd := fields.Get(i); !keep[string(fd.Name())] && fd.Name() != "name" {
					m.Clear(fd)
				}
			}
		}
	}
	return &aiplatformpb.ListNasJobsResponse{NasJobs: items, NextPageToken: next}, nil
}

func (j *jobService) DeleteNasJob(_ context.Context, req *aiplatformpb.DeleteNasJobRequest) (*longrunningpb.Operation, error) {
	st := j.s.notebooks()
	st.mu.Lock()
	e, err := st.nasJob(req.GetName())
	if err == nil && jobRunning(e.Job.GetState()) {
		err = apierror.FailedPrecondition("NasJob %s is %s: cancel it before deleting it", req.GetName(), e.Job.GetState())
	}
	if err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	delete(st.nasJobs, req.GetName())
	st.save()
	st.mu.Unlock()
	return j.s.finished(req.GetName(), &aiplatformpb.DeleteOperationMetadata{GenericMetadata: generic()}, &emptypb.Empty{}, nil)
}

func (j *jobService) CancelNasJob(_ context.Context, req *aiplatformpb.CancelNasJobRequest) (*emptypb.Empty, error) {
	st := j.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	e, err := st.nasJob(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	if !jobRunning(e.Job.GetState()) {
		return nil, grpcError(apierror.FailedPrecondition("NasJob %s is %s and cannot be cancelled", req.GetName(), e.Job.GetState()))
	}
	e.Job.State, e.Job.UpdateTime = aiplatformpb.JobState_JOB_STATE_CANCELLING, timestamppb.New(st.now())
	if e.cancel != nil {
		e.cancel()
	}
	st.save()
	return &emptypb.Empty{}, nil
}

// trialDetails are a job's NasTrialDetails: one per search trial, with
// the train trial that trained it.
func trialDetails(e *nasJobEntry) []*aiplatformpb.NasTrialDetail {
	o := e.Job.GetNasJobOutput().GetMultiTrialJobOutput()
	trainOf := map[string]*aiplatformpb.NasTrial{}
	for _, t := range o.GetTrainTrials() {
		if sid := e.TrainOf[t.GetId()]; sid != "" {
			trainOf[sid] = t
		}
	}
	var out []*aiplatformpb.NasTrialDetail
	for _, t := range o.GetSearchTrials() {
		d := &aiplatformpb.NasTrialDetail{Name: e.Job.GetName() + "/nasTrialDetails/" + t.GetId(), Parameters: e.Params[t.GetId()],
			SearchTrial: proto.Clone(t).(*aiplatformpb.NasTrial)}
		if tt := trainOf[t.GetId()]; tt != nil {
			d.TrainTrial = proto.Clone(tt).(*aiplatformpb.NasTrial)
		}
		out = append(out, d)
	}
	return out
}

func (j *jobService) GetNasTrialDetail(_ context.Context, req *aiplatformpb.GetNasTrialDetailRequest) (*aiplatformpb.NasTrialDetail, error) {
	job, _, ok := strings.Cut(req.GetName(), "/nasTrialDetails/")
	if !ok {
		return nil, grpcError(apierror.InvalidArgument("name %q is not projects/{project}/locations/{location}/nasJobs/{nas_job}/nasTrialDetails/{nas_trial_detail}", req.GetName()))
	}
	st := j.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	e, err := st.nasJob(job)
	if err != nil {
		return nil, grpcError(err)
	}
	for _, d := range trialDetails(e) {
		if d.GetName() == req.GetName() {
			return d, nil
		}
	}
	return nil, grpcError(apierror.NotFound("NasTrialDetail %s not found", req.GetName()))
}

func (j *jobService) ListNasTrialDetails(_ context.Context, req *aiplatformpb.ListNasTrialDetailsRequest) (*aiplatformpb.ListNasTrialDetailsResponse, error) {
	st := j.s.notebooks()
	st.mu.Lock()
	e, err := st.nasJob(req.GetParent())
	var all []*aiplatformpb.NasTrialDetail
	if err == nil {
		all = trialDetails(e)
	}
	st.mu.Unlock()
	if err != nil {
		return nil, grpcError(err)
	}
	items, next, err := aipfilter.Page("nasTrialDetails:"+req.GetParent(), all, req.GetPageToken(), pageSize(req.GetPageSize()))
	if err != nil {
		return nil, grpcError(err)
	}
	return &aiplatformpb.ListNasTrialDetailsResponse{NasTrialDetails: items, NextPageToken: next}, nil
}

// waitNASJob waits for a NAS job's run to end and returns it.
func (s *Server) waitNASJob(ctx context.Context, name string) (*aiplatformpb.NasJob, error) {
	st := s.notebooks()
	st.mu.Lock()
	e, err := st.nasJob(name)
	st.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case <-e.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return proto.Clone(e.Job).(*aiplatformpb.NasJob), nil
}

// ---- persistence

func (st *notebookStore) savedNASJobs() []savedNASJob {
	var out []savedNASJob
	for _, n := range sortedNames(st.nasJobs) {
		e := st.nasJobs[n]
		if b, err := protojson.Marshal(e.Job); err == nil {
			out = append(out, savedNASJob{Job: b, Params: e.Params, TrainOf: e.TrainOf})
		}
	}
	return out
}

// loadNASJobs restores saved NAS jobs: one running when the instance
// stopped failed with it, its active trials infeasible.
func (st *notebookStore) loadNASJobs(raw []savedNASJob) {
	st.nasJobs = map[string]*nasJobEntry{}
	closed := make(chan struct{})
	close(closed)
	for _, r := range raw {
		j := &aiplatformpb.NasJob{}
		if lenient.Unmarshal(r.Job, j) != nil || j.GetName() == "" {
			continue
		}
		if jobRunning(j.GetState()) {
			j.State, j.Error, j.EndTime = aiplatformpb.JobState_JOB_STATE_FAILED, stoppedWithInstance, timestamppb.New(time.Now())
			o := j.GetNasJobOutput().GetMultiTrialJobOutput()
			for _, t := range append(append([]*aiplatformpb.NasTrial{}, o.GetSearchTrials()...), o.GetTrainTrials()...) {
				if t.GetState() == aiplatformpb.NasTrial_ACTIVE || t.GetState() == aiplatformpb.NasTrial_STOPPING || t.GetState() == aiplatformpb.NasTrial_REQUESTED {
					t.State, t.EndTime = aiplatformpb.NasTrial_INFEASIBLE, j.EndTime
				}
			}
		}
		e := &nasJobEntry{Job: j, Params: r.Params, TrainOf: r.TrainOf, done: closed}
		if e.Params == nil {
			e.Params = map[string]string{}
		}
		if e.TrainOf == nil {
			e.TrainOf = map[string]string{}
		}
		st.nasJobs[j.GetName()] = e
	}
}
