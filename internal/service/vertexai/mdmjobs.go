package vertexai

// Agent Platform Model Monitoring v1 (#2214):
// google.cloud.aiplatform.v1.JobService's ModelDeploymentMonitoringJob
// methods over gRPC and REST. A job watches an endpoint: the endpoint's
// predictions are logged at the job's sampling rate (modelmonitor.go's
// logPrediction), and each monitor_interval a run compares the window's
// logged instances with the training dataset (skew) and with the window
// before it (drift), per deployed model and feature (monitorstats.go).
//
// Documented behaviour ("Monitor feature skew and drift",
// https://docs.cloud.google.com/vertex-ai/docs/model-monitoring/using-model-monitoring;
// the JobService and ModelDeploymentMonitoringJob protos; read 2026-11-24):
//   - "every categorical and numerical feature is monitored with threshold
//     values of 0.3" by default;
//   - the monitoring frequency's "minimum granularity is 1 hour. The
//     default is 24 hours";
//   - "If not specified, Model Monitoring logs all prediction requests";
//   - logged requests go to PROJECT_ID.model_deployment_monitoring_ENDPOINT_ID.serving_predict;
//   - a new job is JOB_STATE_PENDING with schedule state OFFLINE;
//   - "You must pause a job before you can delete it";
//   - training data is a BigQuery table, CSV or TFRecord in Cloud Storage,
//     or a managed dataset.
//
// UNVERIFIED: a job is JOB_STATE_RUNNING with schedule state PENDING once
// set up, and RUNNING while a run analyses; an endpoint has at most one
// job; a run's window defaults to the monitor interval; JSONL training data
// (data_format "jsonl") is read as the v2 jobs read it.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

const defaultMonitorThreshold = 0.3

var (
	mdmFilter = []aipfilter.Option{aipfilter.Fields("display_name", "state", "endpoint", "schedule_state", "labels", "create_time", "update_time")}
	mdmMasks  = map[string]bool{"display_name": true, "model_deployment_monitoring_objective_configs": true, "model_deployment_monitoring_schedule_config": true,
		"model_monitoring_alert_config": true, "logging_sampling_strategy": true, "labels": true, "log_ttl": true, "enable_monitoring_pipeline_logs": true,
		"predict_instance_schema_uri": true, "sample_predict_instance": true, "analysis_instance_schema_uri": true, "stats_anomalies_base_directory": true}
)

// jobService serves google.cloud.aiplatform.v1.JobService's
// ModelDeploymentMonitoringJob and DataLabelingJob methods (datalabeling.go);
// the others (custom, batch prediction, hyperparameter tuning and Neural
// Architecture Search jobs) are UNIMPLEMENTED.
type jobService struct {
	aiplatformpb.UnimplementedJobServiceServer
	s *Server
}

// mdmStat is one run's statistics of one feature.
type mdmStat struct {
	Job, DeployedModel string
	Objective          aiplatformpb.ModelDeploymentMonitoringObjectiveType
	Feature            string
	Threshold          float64
	Prediction         *aiplatformpb.FeatureStatsAnomaly
	Training           *aiplatformpb.FeatureStatsAnomaly
}

type savedMDMStat struct {
	Job           string          `json:"job"`
	DeployedModel string          `json:"deployedModel"`
	Objective     string          `json:"objective"`
	Feature       string          `json:"feature"`
	Threshold     float64         `json:"threshold"`
	Prediction    json.RawMessage `json:"prediction"`
	Training      json.RawMessage `json:"training,omitempty"`
}

func (s *mdmStat) saved() savedMDMStat {
	out := savedMDMStat{Job: s.Job, DeployedModel: s.DeployedModel, Objective: s.Objective.String(), Feature: s.Feature, Threshold: s.Threshold, Prediction: monJSON(s.Prediction)}
	if s.Training != nil {
		out.Training = monJSON(s.Training)
	}
	return out
}

func (s savedMDMStat) load() *mdmStat {
	out := &mdmStat{Job: s.Job, DeployedModel: s.DeployedModel, Feature: s.Feature, Threshold: s.Threshold, Prediction: &aiplatformpb.FeatureStatsAnomaly{},
		Objective: aiplatformpb.ModelDeploymentMonitoringObjectiveType(aiplatformpb.ModelDeploymentMonitoringObjectiveType_value[s.Objective])}
	if lenient.Unmarshal(s.Prediction, out.Prediction) != nil {
		return nil
	}
	if len(s.Training) > 0 {
		out.Training = &aiplatformpb.FeatureStatsAnomaly{}
		_ = lenient.Unmarshal(s.Training, out.Training)
	}
	return out
}

func (s *Server) checkMDMJob(j *aiplatformpb.ModelDeploymentMonitoringJob) error {
	if strings.TrimSpace(j.GetDisplayName()) == "" {
		return apierror.InvalidArgument("model_deployment_monitoring_job.display_name is required")
	}
	if utf8.RuneCountInString(j.GetDisplayName()) > 128 {
		return apierror.InvalidArgument("model_deployment_monitoring_job.display_name can be up to 128 characters long")
	}
	if err := checkLabels(j.GetLabels()); err != nil {
		return err
	}
	if j.GetEndpoint() == "" {
		return apierror.InvalidArgument("model_deployment_monitoring_job.endpoint is required")
	}
	r, err := s.needRegistry()
	if err != nil {
		return err
	}
	ep, err := r.getEndpoint(j.GetEndpoint())
	if err != nil {
		return err
	}
	deployed := map[string]bool{}
	for _, d := range ep.GetDeployedModels() {
		deployed[d.GetId()] = true
	}
	if len(j.GetModelDeploymentMonitoringObjectiveConfigs()) == 0 {
		return apierror.InvalidArgument("model_deployment_monitoring_job.model_deployment_monitoring_objective_configs is required")
	}
	for i, c := range j.GetModelDeploymentMonitoringObjectiveConfigs() {
		f := fmt.Sprintf("model_deployment_monitoring_job.model_deployment_monitoring_objective_configs[%d]", i)
		if !deployed[c.GetDeployedModelId()] {
			return apierror.InvalidArgument("%s.deployed_model_id %q is not deployed on %s", f, c.GetDeployedModelId(), j.GetEndpoint())
		}
		oc := c.GetObjectiveConfig()
		if oc.GetTrainingPredictionSkewDetectionConfig() != nil && oc.GetTrainingDataset() == nil {
			return apierror.InvalidArgument("%s.objective_config.training_dataset is required for training_prediction_skew_detection_config", f)
		}
		if td := oc.GetTrainingDataset(); td != nil {
			switch td.GetDataFormat() {
			case "csv", "tf-record", "jsonl", "":
			default:
				return apierror.InvalidArgument("%s.objective_config.training_dataset.data_format %q is not supported: csv or tf-record", f, td.GetDataFormat())
			}
			if g := td.GetGcsSource(); g != nil {
				if len(g.GetUris()) == 0 {
					return apierror.InvalidArgument("%s.objective_config.training_dataset.gcs_source.uris is required", f)
				}
				for _, u := range g.GetUris() {
					if _, _, err := monGCS(f+".objective_config.training_dataset.gcs_source.uris", u); err != nil {
						return err
					}
				}
				if td.GetDataFormat() == "" {
					return apierror.InvalidArgument("%s.objective_config.training_dataset.data_format is required for a Cloud Storage source", f)
				}
			}
		}
	}
	sc := j.GetModelDeploymentMonitoringScheduleConfig()
	if sc.GetMonitorInterval() == nil {
		return apierror.InvalidArgument("model_deployment_monitoring_job.model_deployment_monitoring_schedule_config.monitor_interval is required")
	}
	if iv := sc.GetMonitorInterval().AsDuration(); iv < time.Hour || iv%time.Hour != 0 {
		return apierror.InvalidArgument("model_deployment_monitoring_schedule_config.monitor_interval must be a whole number of hours, at least 1 hour")
	}
	if r := j.GetLoggingSamplingStrategy().GetRandomSampleConfig().GetSampleRate(); r < 0 || r > 1 {
		return apierror.InvalidArgument("logging_sampling_strategy.random_sample_config.sample_rate must be in [0, 1]")
	}
	if d := j.GetStatsAnomaliesBaseDirectory(); d != nil {
		if _, _, err := monGCS("stats_anomalies_base_directory.output_uri_prefix", d.GetOutputUriPrefix()); err != nil {
			return err
		}
	}
	return nil
}

func (x *jobService) CreateModelDeploymentMonitoringJob(_ context.Context, req *aiplatformpb.CreateModelDeploymentMonitoringJobRequest) (*aiplatformpb.ModelDeploymentMonitoringJob, error) {
	project, _, err := parseParent("parent", req.GetParent())
	if err != nil {
		return nil, grpcError(err)
	}
	if req.GetModelDeploymentMonitoringJob() == nil {
		return nil, grpcError(apierror.InvalidArgument("model_deployment_monitoring_job is required"))
	}
	j := proto.Clone(req.GetModelDeploymentMonitoringJob()).(*aiplatformpb.ModelDeploymentMonitoringJob)
	if !strings.HasPrefix(j.GetEndpoint(), req.GetParent()+"/endpoints/") {
		return nil, grpcError(apierror.InvalidArgument("model_deployment_monitoring_job.endpoint %q must be an endpoint in %s", j.GetEndpoint(), req.GetParent()))
	}
	if err := x.s.checkMDMJob(j); err != nil {
		return nil, grpcError(err)
	}
	st := x.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	mon := st.monitoring()
	for _, o := range mon.MDM {
		if o.GetEndpoint() == j.GetEndpoint() {
			return nil, grpcError(apierror.AlreadyExists("Endpoint %s already has the ModelDeploymentMonitoringJob %s", j.GetEndpoint(), o.GetName()))
		}
	}
	now := st.now()
	j.Name = req.GetParent() + "/modelDeploymentMonitoringJobs/" + numericID()
	j.CreateTime, j.UpdateTime = timestamppb.New(now), timestamppb.New(now)
	j.State, j.ScheduleState = aiplatformpb.JobState_JOB_STATE_PENDING, aiplatformpb.ModelDeploymentMonitoringJob_OFFLINE
	j.LatestMonitoringPipelineMetadata, j.Error, j.SatisfiesPzs, j.SatisfiesPzi = nil, nil, false, false
	j.BigqueryTables = []*aiplatformpb.ModelDeploymentMonitoringBigQueryTable{{
		LogSource: aiplatformpb.ModelDeploymentMonitoringBigQueryTable_SERVING, LogType: aiplatformpb.ModelDeploymentMonitoringBigQueryTable_PREDICT,
		BigqueryTablePath: "bq://" + project + ".model_deployment_monitoring_" + lastSegment(j.GetEndpoint()) + ".serving_predict", RequestResponseLoggingSchemaVersion: "v1",
	}}
	j.NextScheduleTime = timestamppb.New(now.Add(j.GetModelDeploymentMonitoringScheduleConfig().GetMonitorInterval().AsDuration()))
	mon.MDM[j.Name] = j
	st.saveMonitoring()
	out := proto.Clone(j).(*aiplatformpb.ModelDeploymentMonitoringJob)
	// Set up: the job runs, and waits for its first run.
	go func() {
		time.Sleep(time.Second)
		st.mu.Lock()
		defer st.mu.Unlock()
		if cur := mon.MDM[out.Name]; cur != nil && cur.GetState() == aiplatformpb.JobState_JOB_STATE_PENDING {
			cur.State, cur.ScheduleState, cur.UpdateTime = aiplatformpb.JobState_JOB_STATE_RUNNING, aiplatformpb.ModelDeploymentMonitoringJob_PENDING, timestamppb.New(st.now())
			st.saveMonitoring()
		}
	}()
	x.s.startScheduler()
	return out, nil
}

func (st *notebookStore) mdmJob(name string) (*aiplatformpb.ModelDeploymentMonitoringJob, error) {
	if _, _, _, err := parseChild("name", "modelDeploymentMonitoringJobs", name); err != nil {
		return nil, err
	}
	j := st.monitoring().MDM[name]
	if j == nil {
		return nil, apierror.NotFound("ModelDeploymentMonitoringJob %s not found", name)
	}
	return j, nil
}

func (x *jobService) GetModelDeploymentMonitoringJob(_ context.Context, req *aiplatformpb.GetModelDeploymentMonitoringJobRequest) (*aiplatformpb.ModelDeploymentMonitoringJob, error) {
	st := x.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	j, err := st.mdmJob(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	return proto.Clone(j).(*aiplatformpb.ModelDeploymentMonitoringJob), nil
}

func (x *jobService) ListModelDeploymentMonitoringJobs(_ context.Context, req *aiplatformpb.ListModelDeploymentMonitoringJobsRequest) (*aiplatformpb.ListModelDeploymentMonitoringJobsResponse, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	st := x.s.notebooks()
	st.mu.Lock()
	var all []*aiplatformpb.ModelDeploymentMonitoringJob
	mon := st.monitoring()
	for _, k := range sortedNames(mon.MDM) {
		if strings.HasPrefix(k, req.GetParent()+"/") {
			all = append(all, proto.Clone(mon.MDM[k]).(*aiplatformpb.ModelDeploymentMonitoringJob))
		}
	}
	st.mu.Unlock()
	all, err := aipfilter.Apply(req.GetFilter(), all, mdmFilter...)
	if err != nil {
		return nil, grpcError(err)
	}
	sort.SliceStable(all, func(i, k int) bool { return all[i].GetCreateTime().AsTime().After(all[k].GetCreateTime().AsTime()) })
	for _, j := range all {
		readMasked(j, req.GetReadMask().GetPaths())
	}
	items, next, err := aipfilter.Page("mdmJobs:"+req.GetParent()+"|"+req.GetFilter(), all, req.GetPageToken(), pageSize(req.GetPageSize()))
	if err != nil {
		return nil, grpcError(err)
	}
	return &aiplatformpb.ListModelDeploymentMonitoringJobsResponse{ModelDeploymentMonitoringJobs: items, NextPageToken: next}, nil
}

func (x *jobService) UpdateModelDeploymentMonitoringJob(_ context.Context, req *aiplatformpb.UpdateModelDeploymentMonitoringJobRequest) (*longrunningpb.Operation, error) {
	in := req.GetModelDeploymentMonitoringJob()
	if in == nil {
		return nil, grpcError(apierror.InvalidArgument("model_deployment_monitoring_job is required"))
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, grpcError(apierror.InvalidArgument("update_mask is required"))
	}
	st := x.s.notebooks()
	st.mu.Lock()
	cur, err := st.mdmJob(in.GetName())
	if err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	next := proto.Clone(cur).(*aiplatformpb.ModelDeploymentMonitoringJob)
	st.mu.Unlock()
	for _, p := range paths {
		top, _, _ := strings.Cut(camelToSnake(p), ".")
		if !mdmMasks[top] {
			return nil, grpcError(apierror.InvalidArgument("update_mask path %q cannot be updated", p))
		}
		fd := next.ProtoReflect().Descriptor().Fields().ByName(protoName(top))
		if in.ProtoReflect().Has(fd) {
			next.ProtoReflect().Set(fd, in.ProtoReflect().Get(fd))
		} else {
			next.ProtoReflect().Clear(fd)
		}
	}
	if err := x.s.checkMDMJob(next); err != nil {
		return nil, grpcError(err)
	}
	st.mu.Lock()
	if _, err := st.mdmJob(in.GetName()); err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	now := st.now()
	next.UpdateTime = timestamppb.New(now)
	if next.GetState() == aiplatformpb.JobState_JOB_STATE_RUNNING || next.GetState() == aiplatformpb.JobState_JOB_STATE_PENDING {
		next.NextScheduleTime = timestamppb.New(now.Add(next.GetModelDeploymentMonitoringScheduleConfig().GetMonitorInterval().AsDuration()))
	}
	st.monitoring().MDM[next.Name] = next
	st.saveMonitoring()
	out := proto.Clone(next)
	st.mu.Unlock()
	return x.s.finished(next.Name, &aiplatformpb.UpdateModelDeploymentMonitoringJobOperationMetadata{GenericMetadata: generic()}, out, nil)
}

func (x *jobService) DeleteModelDeploymentMonitoringJob(_ context.Context, req *aiplatformpb.DeleteModelDeploymentMonitoringJobRequest) (*longrunningpb.Operation, error) {
	st := x.s.notebooks()
	st.mu.Lock()
	j, err := st.mdmJob(req.GetName())
	if err == nil && (j.GetState() == aiplatformpb.JobState_JOB_STATE_RUNNING || j.GetState() == aiplatformpb.JobState_JOB_STATE_PENDING) {
		err = apierror.FailedPrecondition("ModelDeploymentMonitoringJob %s is %s: pause it before deleting it", req.GetName(), j.GetState())
	}
	if err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	mon := st.monitoring()
	delete(mon.MDM, req.GetName())
	kept := mon.MDMStats[:0]
	for _, s := range mon.MDMStats {
		if s.Job != req.GetName() {
			kept = append(kept, s)
		}
	}
	mon.MDMStats = kept
	st.saveMonitoring()
	st.mu.Unlock()
	return x.s.finished(req.GetName(), &aiplatformpb.DeleteOperationMetadata{GenericMetadata: generic()}, &emptypb.Empty{}, nil)
}

func (x *jobService) PauseModelDeploymentMonitoringJob(_ context.Context, req *aiplatformpb.PauseModelDeploymentMonitoringJobRequest) (*emptypb.Empty, error) {
	st := x.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	j, err := st.mdmJob(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	switch j.GetState() {
	case aiplatformpb.JobState_JOB_STATE_PAUSED:
		return &emptypb.Empty{}, nil
	case aiplatformpb.JobState_JOB_STATE_RUNNING, aiplatformpb.JobState_JOB_STATE_PENDING:
	default:
		return nil, grpcError(apierror.FailedPrecondition("ModelDeploymentMonitoringJob %s is %s and cannot be paused", req.GetName(), j.GetState()))
	}
	j.State, j.ScheduleState, j.NextScheduleTime, j.UpdateTime = aiplatformpb.JobState_JOB_STATE_PAUSED, aiplatformpb.ModelDeploymentMonitoringJob_OFFLINE, nil, timestamppb.New(st.now())
	st.saveMonitoring()
	return &emptypb.Empty{}, nil
}

func (x *jobService) ResumeModelDeploymentMonitoringJob(_ context.Context, req *aiplatformpb.ResumeModelDeploymentMonitoringJobRequest) (*emptypb.Empty, error) {
	st := x.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	j, err := st.mdmJob(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	if j.GetState() != aiplatformpb.JobState_JOB_STATE_PAUSED {
		return nil, grpcError(apierror.FailedPrecondition("ModelDeploymentMonitoringJob %s is %s: only a paused job can be resumed", req.GetName(), j.GetState()))
	}
	now := st.now()
	j.State, j.ScheduleState, j.UpdateTime = aiplatformpb.JobState_JOB_STATE_RUNNING, aiplatformpb.ModelDeploymentMonitoringJob_PENDING, timestamppb.New(now)
	j.NextScheduleTime = timestamppb.New(now.Add(j.GetModelDeploymentMonitoringScheduleConfig().GetMonitorInterval().AsDuration()))
	st.saveMonitoring()
	return &emptypb.Empty{}, nil
}

// ---- runs

// runDueMDM starts the monitoring runs that are due.
func (s *Server) runDueMDM() {
	st := s.notebooks()
	st.mu.Lock()
	now := st.now()
	var due []string
	for _, k := range sortedNames(st.monitoring().MDM) {
		j := st.mon.MDM[k]
		if j.GetState() == aiplatformpb.JobState_JOB_STATE_RUNNING && j.GetScheduleState() == aiplatformpb.ModelDeploymentMonitoringJob_PENDING &&
			j.NextScheduleTime != nil && !j.NextScheduleTime.AsTime().After(now) {
			j.ScheduleState = aiplatformpb.ModelDeploymentMonitoringJob_RUNNING
			due = append(due, k)
		}
	}
	st.mu.Unlock()
	for _, k := range due {
		s.RunMDMJob(context.Background(), k, now)
	}
}

// inferFields are the features of logged instances: their keys, numeric
// where every value is a number; the training target field excluded.
func inferFields(rows []map[string]any, exclude string) []monitorField {
	kinds := map[string]bool{}
	for _, r := range rows {
		for k, v := range r {
			if k == exclude {
				continue
			}
			num, seen := kinds[k]
			if !seen {
				num = true
			}
			if _, ok := v.(float64); !ok {
				if s, isStr := v.(string); !isStr || func() bool { _, err := strconv.ParseFloat(s, 64); return err != nil }() {
					num = false
				}
			}
			kinds[k] = num
		}
	}
	var out []monitorField
	for _, k := range sortedNames(kinds) {
		out = append(out, monitorField{Name: k, Numeric: kinds[k]})
	}
	return out
}

func thresholdOf(m map[string]*aiplatformpb.ThresholdConfig, def *aiplatformpb.ThresholdConfig, feature string) float64 {
	if t := m[feature]; t != nil {
		return t.GetValue()
	}
	if def != nil {
		return def.GetValue()
	}
	return defaultMonitorThreshold
}

// RunMDMJob runs one monitoring analysis of job name at time at.
func (s *Server) RunMDMJob(ctx context.Context, name string, at time.Time) {
	st := s.notebooks()
	st.mu.Lock()
	mon := st.monitoring()
	j := mon.MDM[name]
	if j == nil {
		st.mu.Unlock()
		return
	}
	j = proto.Clone(j).(*aiplatformpb.ModelDeploymentMonitoringJob)
	logs := append([]*predLog{}, mon.Logs[j.GetEndpoint()]...)
	var store objectStore
	if st.pipelineRunner != nil {
		store = st.pipelineRunner
	}
	st.mu.Unlock()

	interval := j.GetModelDeploymentMonitoringScheduleConfig().GetMonitorInterval().AsDuration()
	window := j.GetModelDeploymentMonitoringScheduleConfig().GetMonitorWindow().AsDuration()
	if window <= 0 {
		window = interval
	}
	start, prevStart := at.Add(-window), at.Add(-2*window)
	var stats []*mdmStat
	var runErr error
	for _, c := range j.GetModelDeploymentMonitoringObjectiveConfigs() {
		oc := c.GetObjectiveConfig()
		var cur, prev []map[string]any
		for _, l := range logs {
			if l.DeployedModel != c.GetDeployedModelId() {
				continue
			}
			row := logRow(l, nil, nil)
			switch {
			case !l.Time.Before(start) && l.Time.Before(at):
				cur = append(cur, row)
			case !l.Time.Before(prevStart) && l.Time.Before(start):
				prev = append(prev, row)
			}
		}
		target := oc.GetTrainingDataset().GetTargetField()
		fields := inferFields(cur, target)
		if sk := oc.GetTrainingPredictionSkewDetectionConfig(); sk != nil {
			train, err := s.readTraining(ctx, store, oc.GetTrainingDataset())
			if err != nil {
				runErr = err
			} else {
				for _, f := range fields {
					d, err := compareFeature(f, "", train, cur)
					if err != nil {
						runErr = err
						break
					}
					stats = append(stats, mdmStatOf(name, c.GetDeployedModelId(), aiplatformpb.ModelDeploymentMonitoringObjectiveType_RAW_FEATURE_SKEW, d,
						thresholdOf(sk.GetSkewThresholds(), sk.GetDefaultSkewThreshold(), f.Name), start, at))
				}
			}
		}
		if dr := oc.GetPredictionDriftDetectionConfig(); dr != nil && len(prev) > 0 {
			for _, f := range fields {
				d, err := compareFeature(f, "", prev, cur)
				if err != nil {
					runErr = err
					break
				}
				stats = append(stats, mdmStatOf(name, c.GetDeployedModelId(), aiplatformpb.ModelDeploymentMonitoringObjectiveType_RAW_FEATURE_DRIFT, d,
					thresholdOf(dr.GetDriftThresholds(), dr.GetDefaultDriftThreshold(), f.Name), start, at))
			}
		}
	}
	if dir := j.GetStatsAnomaliesBaseDirectory().GetOutputUriPrefix(); dir != "" && store != nil && len(stats) > 0 {
		uri := strings.TrimSuffix(dir, "/") + "/" + at.UTC().Format("20060102T150405Z")
		if b, o, err := monGCS("output_uri_prefix", uri); err == nil {
			var out []json.RawMessage
			for _, x := range stats {
				out = append(out, monJSON(x.Prediction))
				x.Prediction.StatsUri = uri + "/stats.json"
				if x.Prediction.GetScore() > x.Threshold {
					x.Prediction.AnomalyUri = uri + "/stats.json"
				}
			}
			data, _ := json.Marshal(map[string]any{"featureStats": out})
			if err := store.WriteObject(ctx, b, o+"/stats.json", "application/json", data); err != nil && runErr == nil {
				runErr = err
			}
		}
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	cur := mon.MDM[name]
	if cur == nil {
		return
	}
	mon.MDMStats = append(mon.MDMStats, stats...)
	meta := &aiplatformpb.ModelDeploymentMonitoringJob_LatestMonitoringPipelineMetadata{RunTime: timestamppb.New(at), Status: &status.Status{}}
	if runErr != nil {
		e := apierror.From(runErr)
		meta.Status = &status.Status{Code: int32(e.Code), Message: e.Message}
	}
	cur.LatestMonitoringPipelineMetadata = meta
	if cur.GetState() == aiplatformpb.JobState_JOB_STATE_RUNNING {
		cur.ScheduleState = aiplatformpb.ModelDeploymentMonitoringJob_PENDING
		cur.NextScheduleTime = timestamppb.New(at.Add(interval))
	}
	cur.UpdateTime = timestamppb.New(st.now())
	st.saveMonitoring()
}

func mdmStatOf(job, dm string, obj aiplatformpb.ModelDeploymentMonitoringObjectiveType, d *featureDistance, threshold float64, start, end time.Time) *mdmStat {
	return &mdmStat{Job: job, DeployedModel: dm, Objective: obj, Feature: d.Field.Name, Threshold: threshold,
		Prediction: &aiplatformpb.FeatureStatsAnomaly{Score: d.Distance, DistributionDeviation: d.Distance, AnomalyDetectionThreshold: threshold,
			StartTime: timestamppb.New(start), EndTime: timestamppb.New(end)}}
}

func (s *Server) readTraining(ctx context.Context, store objectStore, td *aiplatformpb.ModelMonitoringObjectiveConfig_TrainingDataset) ([]map[string]any, error) {
	switch src := td.GetDataSource().(type) {
	case *aiplatformpb.ModelMonitoringObjectiveConfig_TrainingDataset_GcsSource:
		if td.GetDataFormat() == "tf-record" {
			return nil, apierror.Unimplemented("tf-record training data is not read here: give it as csv")
		}
		var rows []map[string]any
		for _, u := range src.GcsSource.GetUris() {
			r, err := readGCSRows(ctx, store, u, td.GetDataFormat())
			if err != nil {
				return nil, err
			}
			rows = append(rows, r...)
		}
		return rows, nil
	case *aiplatformpb.ModelMonitoringObjectiveConfig_TrainingDataset_BigquerySource:
		return nil, apierror.Unimplemented("BigQuery training data is not read by model monitoring on this instance: export the table to Cloud Storage as CSV")
	case *aiplatformpb.ModelMonitoringObjectiveConfig_TrainingDataset_Dataset:
		return nil, apierror.NotFound("Dataset %s not found: Agent Platform managed datasets are not served", src.Dataset)
	}
	return nil, apierror.InvalidArgument("the training dataset has no source")
}

func (x *jobService) SearchModelDeploymentMonitoringStatsAnomalies(_ context.Context, req *aiplatformpb.SearchModelDeploymentMonitoringStatsAnomaliesRequest) (*aiplatformpb.SearchModelDeploymentMonitoringStatsAnomaliesResponse, error) {
	if req.GetDeployedModelId() == "" {
		return nil, grpcError(apierror.InvalidArgument("deployed_model_id is required"))
	}
	if len(req.GetObjectives()) == 0 {
		return nil, grpcError(apierror.InvalidArgument("objectives is required"))
	}
	st := x.s.notebooks()
	st.mu.Lock()
	if _, err := st.mdmJob(req.GetModelDeploymentMonitoringJob()); err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	var all []*aiplatformpb.ModelMonitoringStatsAnomalies
	for _, o := range req.GetObjectives() {
		byFeature := map[string]*aiplatformpb.ModelMonitoringStatsAnomalies_FeatureHistoricStatsAnomalies{}
		out := &aiplatformpb.ModelMonitoringStatsAnomalies{Objective: o.GetType(), DeployedModelId: req.GetDeployedModelId()}
		for _, s := range st.monitoring().MDMStats {
			if s.Job != req.GetModelDeploymentMonitoringJob() || s.DeployedModel != req.GetDeployedModelId() || s.Objective != o.GetType() ||
				req.GetFeatureDisplayName() != "" && s.Feature != req.GetFeatureDisplayName() {
				continue
			}
			if req.StartTime != nil && s.Prediction.GetEndTime().AsTime().Before(req.StartTime.AsTime()) ||
				req.EndTime != nil && !s.Prediction.GetEndTime().AsTime().Before(req.EndTime.AsTime()) {
				continue
			}
			f := byFeature[s.Feature]
			if f == nil {
				f = &aiplatformpb.ModelMonitoringStatsAnomalies_FeatureHistoricStatsAnomalies{FeatureDisplayName: s.Feature,
					Threshold: &aiplatformpb.ThresholdConfig{Threshold: &aiplatformpb.ThresholdConfig_Value{Value: s.Threshold}}}
				byFeature[s.Feature] = f
			}
			f.PredictionStats = append(f.PredictionStats, proto.Clone(s.Prediction).(*aiplatformpb.FeatureStatsAnomaly))
			if s.Prediction.GetScore() > s.Threshold {
				out.AnomalyCount++
			}
		}
		for _, k := range sortedNames(byFeature) {
			f := byFeature[k]
			sort.SliceStable(f.PredictionStats, func(i, j int) bool {
				return f.PredictionStats[i].GetEndTime().AsTime().After(f.PredictionStats[j].GetEndTime().AsTime())
			})
			out.FeatureStats = append(out.FeatureStats, f)
		}
		if n := int(o.GetTopFeatureCount()); n > 0 && len(out.FeatureStats) > n {
			sort.SliceStable(out.FeatureStats, func(i, j int) bool {
				return maxScore(out.FeatureStats[i]) > maxScore(out.FeatureStats[j])
			})
			out.FeatureStats = out.FeatureStats[:n]
		}
		all = append(all, out)
	}
	st.mu.Unlock()
	items, next, err := aipfilter.Page("mdmAnomalies:"+req.GetModelDeploymentMonitoringJob()+"|"+req.GetDeployedModelId()+"|"+req.GetFeatureDisplayName(),
		all, req.GetPageToken(), pageSize(req.GetPageSize()))
	if err != nil {
		return nil, grpcError(err)
	}
	return &aiplatformpb.SearchModelDeploymentMonitoringStatsAnomaliesResponse{MonitoringStats: items, NextPageToken: next}, nil
}

func maxScore(f *aiplatformpb.ModelMonitoringStatsAnomalies_FeatureHistoricStatsAnomalies) float64 {
	m := 0.0
	for _, p := range f.GetPredictionStats() {
		if p.GetScore() > m {
			m = p.GetScore()
		}
	}
	return m
}

var _ = codes.OK
