package vertexai

// Agent Platform (formerly Vertex AI) Model Monitoring v2 (#2214):
// google.cloud.aiplatform.v1beta1.ModelMonitoringService over gRPC and
// REST: ModelMonitors, their ModelMonitoringJobs, SearchModelMonitoringStats
// and SearchModelMonitoringAlerts. Google serves this service under
// v1beta1 only (the v1 API has no modelMonitors collection), so this does
// too. A job runs here, on this instance: it reads its baseline and target
// datasets (Cloud Storage CSV or JSONL files, or the endpoint predictions
// this instance logged), computes each feature's distance
// (monitorstats.go), and keeps the stats and alerts its thresholds raise.
//
// Documented behaviour (the ModelMonitoringService, ModelMonitor and
// ModelMonitoringJob protos; "Model Monitoring overview" and "Run a
// monitoring job", https://docs.cloud.google.com/vertex-ai/docs/model-monitoring/,
// read 2026-11-24):
//   - IDs are optional, up to 63 characters, /^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$/;
//   - display names are up to 128 characters of any UTF-8;
//   - the target is a Model Registry model version;
//   - a job's spec overrides the monitor's defaults;
//   - a job is JOB_STATE_PENDING while it is created, JOB_STATE_RUNNING once
//     it is, and then JOB_STATE_FAILED, JOB_STATE_SUCCEEDED or
//     JOB_STATE_PARTIALLY_SUCCEEDED;
//   - an alert is "triggered if value above the threshold";
//   - alerts come back sorted by alert time, descending, with their total;
//   - the time offset and window take 'w', 'd' or 'h' units, and the
//     cut-off is the scheduled time or, unscheduled, the job's create time;
//   - DeleteModelMonitor's force deletes "the model monitor with schedules".
//
// UNVERIFIED (re-read 2026-11-24): deleting a monitor deletes its jobs, stats and alerts here;
// an unforced delete of a monitor with schedules is FAILED_PRECONDITION;
// the job ID, when none is given, is numeric, as other Agent Platform IDs.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	betapb "cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/genproto/googleapis/type/interval"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

const (
	objFeatureDrift    = "raw-feature-drift"
	objPredictionDrift = "prediction-output-drift"
	objAttribution     = "feature-attribution"
)

var (
	monitorIDRE   = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	offsetRE      = regexp.MustCompile(`^([0-9]+)([wWdDhH])$`)
	monitorFilter = append([]aipfilter.Option{aipfilter.Fields("display_name", "create_time", "update_time")},
		bothCases(nil, map[string]string{"model": "model_monitoring_target.vertex_model.model", "model_version_id": "model_monitoring_target.vertex_model.model_version_id"})...)
	mmJobFilter = []aipfilter.Option{aipfilter.Fields("display_name", "state", "create_time", "update_time", "schedule", "scheduleTime", "schedule_time")}
)

// monitorState is Model Monitoring's resources, v2 and v1, and the
// predictions endpoints logged for them; guarded by the notebook store's
// mu, saved beside its file.
type monitorState struct {
	Monitors map[string]*betapb.ModelMonitor
	Jobs     map[string]*betapb.ModelMonitoringJob
	Stats    []*mmStat
	Alerts   []*mmAlert
	// SchedReqs are the v1beta1 schedules' create_model_monitoring_job_request,
	// by schedule name: the v1 Schedule the store keeps has no such field.
	SchedReqs map[string]*betapb.CreateModelMonitoringJobRequest
	// MDM are v1's ModelDeploymentMonitoringJobs (mdmjobs.go).
	MDM      map[string]*aiplatformpb.ModelDeploymentMonitoringJob
	MDMStats []*mdmStat
	// Logs are the logged predictions, by endpoint name.
	Logs map[string][]*predLog

	cancels map[string]context.CancelFunc
}

type mmStat struct {
	Monitor, Objective, StatsName string
	Point                         *betapb.ModelMonitoringStatsDataPoint
}

type mmAlert struct {
	Monitor string
	Alert   *betapb.ModelMonitoringAlert
}

// predLog is one logged prediction: its instance and prediction.
type predLog struct {
	Time          time.Time       `json:"time"`
	DeployedModel string          `json:"deployedModel"`
	Model         string          `json:"model,omitempty"`
	Instance      json.RawMessage `json:"instance"`
	Prediction    json.RawMessage `json:"prediction"`
}

// maxLoggedPredictions bounds an endpoint's log; the oldest go first.
const maxLoggedPredictions = 20000

func (st *notebookStore) monitoring() *monitorState {
	if st.mon == nil {
		st.mon = &monitorState{Monitors: map[string]*betapb.ModelMonitor{}, Jobs: map[string]*betapb.ModelMonitoringJob{},
			SchedReqs: map[string]*betapb.CreateModelMonitoringJobRequest{}, MDM: map[string]*aiplatformpb.ModelDeploymentMonitoringJob{},
			Logs:    map[string][]*predLog{},
			cancels: map[string]context.CancelFunc{}}
	}
	return st.mon
}

// ---- persistence

func monitorFile(file string) string {
	return filepath.Join(filepath.Dir(file), "agentplatform-monitoring.json")
}

type savedMonitoring struct {
	Monitors  []json.RawMessage          `json:"modelMonitors,omitempty"`
	Jobs      []json.RawMessage          `json:"modelMonitoringJobs,omitempty"`
	Stats     []savedStat                `json:"stats,omitempty"`
	Alerts    []savedAlert               `json:"alerts,omitempty"`
	SchedReqs map[string]json.RawMessage `json:"scheduleRequests,omitempty"`
	MDM       []json.RawMessage          `json:"modelDeploymentMonitoringJobs,omitempty"`
	MDMStats  []savedMDMStat             `json:"mdmStats,omitempty"`
	Logs      map[string][]*predLog      `json:"predictionLogs,omitempty"`
}

type savedStat struct {
	Monitor   string          `json:"monitor"`
	Objective string          `json:"objective"`
	StatsName string          `json:"statsName"`
	Point     json.RawMessage `json:"point"`
}

type savedAlert struct {
	Monitor string          `json:"monitor"`
	Alert   json.RawMessage `json:"alert"`
}

func monJSON(m proto.Message) json.RawMessage {
	b, _ := protojson.Marshal(m)
	return b
}

// saveMonitoring writes the monitoring state; the caller holds st.mu.
func (st *notebookStore) saveMonitoring() {
	if st.file == "" || st.mon == nil {
		return
	}
	m := st.mon
	out := savedMonitoring{SchedReqs: map[string]json.RawMessage{}, Logs: m.Logs}
	for _, n := range sortedNames(m.Monitors) {
		out.Monitors = append(out.Monitors, monJSON(m.Monitors[n]))
	}
	for _, n := range sortedNames(m.Jobs) {
		out.Jobs = append(out.Jobs, monJSON(m.Jobs[n]))
	}
	for _, s := range m.Stats {
		out.Stats = append(out.Stats, savedStat{s.Monitor, s.Objective, s.StatsName, monJSON(s.Point)})
	}
	for _, a := range m.Alerts {
		out.Alerts = append(out.Alerts, savedAlert{a.Monitor, monJSON(a.Alert)})
	}
	for n, r := range m.SchedReqs {
		out.SchedReqs[n] = monJSON(r)
	}
	for _, n := range sortedNames(m.MDM) {
		out.MDM = append(out.MDM, monJSON(m.MDM[n]))
	}
	for _, s := range m.MDMStats {
		out.MDMStats = append(out.MDMStats, s.saved())
	}
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	f := monitorFile(st.file)
	_ = os.MkdirAll(filepath.Dir(f), 0o755)
	if os.WriteFile(f+".tmp", b, 0o600) == nil {
		_ = os.Rename(f+".tmp", f)
	}
}

// loadMonitoring reads the monitoring state; a job running when the
// instance stopped failed with it.
func (st *notebookStore) loadMonitoring() {
	b, err := os.ReadFile(monitorFile(st.file))
	if err != nil {
		return
	}
	var in savedMonitoring
	if json.Unmarshal(b, &in) != nil {
		return
	}
	m := st.monitoring()
	for _, raw := range in.Monitors {
		x := &betapb.ModelMonitor{}
		if lenient.Unmarshal(raw, x) == nil && x.GetName() != "" {
			m.Monitors[x.GetName()] = x
		}
	}
	for _, raw := range in.Jobs {
		x := &betapb.ModelMonitoringJob{}
		if lenient.Unmarshal(raw, x) == nil && x.GetName() != "" {
			if x.GetState() == betapb.JobState_JOB_STATE_PENDING || x.GetState() == betapb.JobState_JOB_STATE_RUNNING {
				x.State = betapb.JobState_JOB_STATE_FAILED
				if x.JobExecutionDetail == nil {
					x.JobExecutionDetail = &betapb.ModelMonitoringJobExecutionDetail{}
				}
				x.JobExecutionDetail.Error = &status.Status{Code: int32(codes.Aborted), Message: "the instance stopped while the job ran"}
			}
			m.Jobs[x.GetName()] = x
		}
	}
	for _, s := range in.Stats {
		p := &betapb.ModelMonitoringStatsDataPoint{}
		if lenient.Unmarshal(s.Point, p) == nil {
			m.Stats = append(m.Stats, &mmStat{s.Monitor, s.Objective, s.StatsName, p})
		}
	}
	for _, a := range in.Alerts {
		x := &betapb.ModelMonitoringAlert{}
		if lenient.Unmarshal(a.Alert, x) == nil {
			m.Alerts = append(m.Alerts, &mmAlert{a.Monitor, x})
		}
	}
	for n, raw := range in.SchedReqs {
		x := &betapb.CreateModelMonitoringJobRequest{}
		if lenient.Unmarshal(raw, x) == nil {
			m.SchedReqs[n] = x
		}
	}
	for _, raw := range in.MDM {
		x := &aiplatformpb.ModelDeploymentMonitoringJob{}
		if lenient.Unmarshal(raw, x) == nil && x.GetName() != "" {
			m.MDM[x.GetName()] = x
		}
	}
	for _, s := range in.MDMStats {
		if x := s.load(); x != nil {
			m.MDMStats = append(m.MDMStats, x)
		}
	}
	if in.Logs != nil {
		m.Logs = in.Logs
	}
}

// ---- the service

type modelMonitoringService struct {
	betapb.UnimplementedModelMonitoringServiceServer
	s *Server
}

func checkMonitorDisplayName(field, n string) error {
	if utf8.RuneCountInString(n) > 128 {
		return apierror.InvalidArgument("%s can be up to 128 characters long", field)
	}
	return nil
}

// checkSchema validates a monitoring schema's fields.
func checkSchema(sc *betapb.ModelMonitoringSchema) error {
	for _, group := range []struct {
		name   string
		fields []*betapb.ModelMonitoringSchema_FieldSchema
	}{{"feature_fields", sc.GetFeatureFields()}, {"prediction_fields", sc.GetPredictionFields()}, {"ground_truth_fields", sc.GetGroundTruthFields()}} {
		seen := map[string]bool{}
		for i, f := range group.fields {
			if f.GetName() == "" {
				return apierror.InvalidArgument("model_monitoring_schema.%s[%d].name is required", group.name, i)
			}
			if seen[f.GetName()] {
				return apierror.InvalidArgument("model_monitoring_schema.%s has %q twice", group.name, f.GetName())
			}
			seen[f.GetName()] = true
			if _, err := fieldNumeric(f.GetDataType()); err != nil {
				return apierror.InvalidArgument("model_monitoring_schema.%s[%d].%s", group.name, i, apierror.From(err).Message)
			}
		}
	}
	return nil
}

func checkInput(field string, in *betapb.ModelMonitoringInput) error {
	if in == nil {
		return nil
	}
	if ds := in.GetColumnizedDataset(); ds != nil {
		if g := ds.GetGcsSource(); g != nil {
			if _, _, err := monGCS(field+".columnized_dataset.gcs_source.gcs_uri", g.GetGcsUri()); err != nil {
				return err
			}
		}
		if b := ds.GetBigquerySource(); b != nil && b.GetTableUri() != "" && !strings.HasPrefix(b.GetTableUri(), "bq://") {
			return apierror.InvalidArgument("%s.columnized_dataset.bigquery_source.table_uri %q must be bq://project.dataset.table", field, b.GetTableUri())
		}
	}
	if to := in.GetTimeOffset(); to != nil {
		for k, v := range map[string]string{"offset": to.GetOffset(), "window": to.GetWindow()} {
			if v != "" && !offsetRE.MatchString(v) {
				return apierror.InvalidArgument("%s.time_offset.%s %q must be a number of weeks, days or hours: for example 1h, 2d or 1w", field, k, v)
			}
		}
	}
	return nil
}

func checkObjective(field string, o *betapb.ModelMonitoringObjectiveSpec_TabularObjective) error {
	for name, d := range map[string]*betapb.ModelMonitoringObjectiveSpec_DataDriftSpec{"feature_drift_spec": o.GetFeatureDriftSpec(), "prediction_output_drift_spec": o.GetPredictionOutputDriftSpec()} {
		if d == nil {
			continue
		}
		if t := d.GetCategoricalMetricType(); t != "" && t != algoLInf && t != algoJS {
			return apierror.InvalidArgument("%s.%s.categorical_metric_type %q is not supported: l_infinity or jensen_shannon_divergence", field, name, t)
		}
		if t := d.GetNumericMetricType(); t != "" && t != algoJS {
			return apierror.InvalidArgument("%s.%s.numeric_metric_type %q is not supported: jensen_shannon_divergence", field, name, t)
		}
	}
	return nil
}

func (s *Server) checkMonitor(m *betapb.ModelMonitor) error {
	if err := checkMonitorDisplayName("model_monitor.display_name", m.GetDisplayName()); err != nil {
		return err
	}
	vm := m.GetModelMonitoringTarget().GetVertexModel()
	if vm.GetModel() == "" {
		return apierror.InvalidArgument("model_monitor.model_monitoring_target.vertex_model.model is required")
	}
	r, err := s.needRegistry()
	if err != nil {
		return err
	}
	ref := vm.GetModel()
	if vm.GetModelVersionId() != "" {
		ref += "@" + vm.GetModelVersionId()
	}
	if _, err := r.get(ref); err != nil {
		return err
	}
	if err := checkSchema(m.GetModelMonitoringSchema()); err != nil {
		return err
	}
	if err := checkInput("model_monitor.training_dataset", m.GetTrainingDataset()); err != nil {
		return err
	}
	if err := checkNotificationSpec("model_monitor.notification_spec", m.GetNotificationSpec()); err != nil {
		return err
	}
	if g := m.GetOutputSpec().GetGcsBaseDirectory(); g != nil {
		if _, _, err := monGCS("model_monitor.output_spec.gcs_base_directory.output_uri_prefix", g.GetOutputUriPrefix()); err != nil {
			return err
		}
	}
	return checkObjective("model_monitor.tabular_objective", m.GetTabularObjective())
}

func (x *modelMonitoringService) CreateModelMonitor(_ context.Context, req *betapb.CreateModelMonitorRequest) (*longrunningpb.Operation, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	if req.GetModelMonitor() == nil {
		return nil, grpcError(apierror.InvalidArgument("model_monitor is required"))
	}
	id := req.GetModelMonitorId()
	if id != "" && !monitorIDRE.MatchString(id) {
		return nil, grpcError(apierror.InvalidArgument("model_monitor_id %q must be up to 63 characters and match /^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$/", id))
	}
	m := proto.Clone(req.GetModelMonitor()).(*betapb.ModelMonitor)
	if err := x.s.checkMonitor(m); err != nil {
		return nil, grpcError(err)
	}
	st := x.s.notebooks()
	st.mu.Lock()
	mon := st.monitoring()
	if id == "" {
		id = numericID()
	}
	m.Name = req.GetParent() + "/modelMonitors/" + id
	if mon.Monitors[m.Name] != nil {
		st.mu.Unlock()
		return nil, grpcError(apierror.AlreadyExists("ModelMonitor %s already exists", m.Name))
	}
	now := timestamppb.New(st.now())
	m.CreateTime, m.UpdateTime, m.SatisfiesPzs, m.SatisfiesPzi = now, now, false, false
	mon.Monitors[m.Name] = m
	st.saveMonitoring()
	out := proto.Clone(m)
	st.mu.Unlock()
	return x.s.finished(m.Name, &betapb.CreateModelMonitorOperationMetadata{GenericMetadata: betaGeneric()}, out, nil)
}

func betaGeneric() *betapb.GenericOperationMetadata {
	now := timestamppb.Now()
	return &betapb.GenericOperationMetadata{CreateTime: now, UpdateTime: now}
}

func (st *notebookStore) monitor(name string) (*betapb.ModelMonitor, error) {
	if _, _, _, err := parseChild("name", "modelMonitors", name); err != nil {
		return nil, err
	}
	m := st.monitoring().Monitors[name]
	if m == nil {
		return nil, apierror.NotFound("ModelMonitor %s not found", name)
	}
	return m, nil
}

func (x *modelMonitoringService) GetModelMonitor(_ context.Context, req *betapb.GetModelMonitorRequest) (*betapb.ModelMonitor, error) {
	st := x.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	m, err := st.monitor(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	return proto.Clone(m).(*betapb.ModelMonitor), nil
}

// readMasked clears the fields keep does not name (name always stays).
func readMasked(m proto.Message, paths []string) {
	if len(paths) == 0 {
		return
	}
	keep := map[string]bool{}
	for _, p := range paths {
		top, _, _ := strings.Cut(camelToSnake(p), ".")
		keep[top] = true
	}
	r := m.ProtoReflect()
	fields := r.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		if fd := fields.Get(i); !keep[string(fd.Name())] && fd.Name() != "name" {
			r.Clear(fd)
		}
	}
}

func (x *modelMonitoringService) ListModelMonitors(_ context.Context, req *betapb.ListModelMonitorsRequest) (*betapb.ListModelMonitorsResponse, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	st := x.s.notebooks()
	st.mu.Lock()
	var all []*betapb.ModelMonitor
	mon := st.monitoring()
	for _, k := range sortedNames(mon.Monitors) {
		if strings.HasPrefix(k, req.GetParent()+"/") {
			all = append(all, proto.Clone(mon.Monitors[k]).(*betapb.ModelMonitor))
		}
	}
	st.mu.Unlock()
	all, err := aipfilter.Apply(req.GetFilter(), all, monitorFilter...)
	if err != nil {
		return nil, grpcError(err)
	}
	for _, m := range all {
		readMasked(m, req.GetReadMask().GetPaths())
	}
	items, next, err := aipfilter.Page("modelMonitors:"+req.GetParent()+"|"+req.GetFilter(), all, req.GetPageToken(), pageSize(req.GetPageSize()))
	if err != nil {
		return nil, grpcError(err)
	}
	return &betapb.ListModelMonitorsResponse{ModelMonitors: items, NextPageToken: next}, nil
}

// monitorMasks are the ModelMonitor fields an update may set.
var monitorMasks = map[string]bool{"display_name": true, "model_monitoring_target": true, "training_dataset": true, "notification_spec": true,
	"output_spec": true, "explanation_spec": true, "model_monitoring_schema": true, "tabular_objective": true}

func (x *modelMonitoringService) UpdateModelMonitor(_ context.Context, req *betapb.UpdateModelMonitorRequest) (*longrunningpb.Operation, error) {
	in := req.GetModelMonitor()
	if in == nil {
		return nil, grpcError(apierror.InvalidArgument("model_monitor is required"))
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, grpcError(apierror.InvalidArgument("update_mask is required"))
	}
	st := x.s.notebooks()
	st.mu.Lock()
	cur, err := st.monitor(in.GetName())
	if err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	next := proto.Clone(cur).(*betapb.ModelMonitor)
	st.mu.Unlock()
	for _, p := range paths {
		top, _, _ := strings.Cut(camelToSnake(p), ".")
		if !monitorMasks[top] {
			return nil, grpcError(apierror.InvalidArgument("update_mask path %q cannot be updated", p))
		}
		if top == "tabular_objective" {
			next.DefaultObjective = in.DefaultObjective
			continue
		}
		fd := next.ProtoReflect().Descriptor().Fields().ByName(protoName(top))
		if in.ProtoReflect().Has(fd) {
			next.ProtoReflect().Set(fd, in.ProtoReflect().Get(fd))
		} else {
			next.ProtoReflect().Clear(fd)
		}
	}
	if err := x.s.checkMonitor(next); err != nil {
		return nil, grpcError(err)
	}
	st.mu.Lock()
	if _, err := st.monitor(in.GetName()); err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	next.UpdateTime = timestamppb.New(st.now())
	st.monitoring().Monitors[next.Name] = next
	st.saveMonitoring()
	out := proto.Clone(next)
	st.mu.Unlock()
	return x.s.finished(next.Name, &betapb.UpdateModelMonitorOperationMetadata{GenericMetadata: betaGeneric()}, out, nil)
}

func (x *modelMonitoringService) DeleteModelMonitor(_ context.Context, req *betapb.DeleteModelMonitorRequest) (*longrunningpb.Operation, error) {
	st := x.s.notebooks()
	st.mu.Lock()
	if _, err := st.monitor(req.GetName()); err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	mon := st.monitoring()
	var scheds []string
	for n, r := range mon.SchedReqs {
		if r.GetParent() == req.GetName() {
			scheds = append(scheds, n)
		}
	}
	if len(scheds) > 0 && !req.GetForce() {
		st.mu.Unlock()
		sort.Strings(scheds)
		return nil, grpcError(apierror.FailedPrecondition("ModelMonitor %s has schedules (%s): delete them first, or delete with force", req.GetName(), strings.Join(scheds, ", ")))
	}
	for _, n := range scheds {
		delete(mon.SchedReqs, n)
		delete(st.schedules, n)
	}
	for n := range mon.Jobs {
		if strings.HasPrefix(n, req.GetName()+"/") {
			if c := mon.cancels[n]; c != nil {
				c()
			}
			delete(mon.Jobs, n)
		}
	}
	mon.Stats = keepStats(mon.Stats, func(s *mmStat) bool { return s.Monitor != req.GetName() })
	alerts := mon.Alerts[:0]
	for _, a := range mon.Alerts {
		if a.Monitor != req.GetName() {
			alerts = append(alerts, a)
		}
	}
	mon.Alerts = alerts
	delete(mon.Monitors, req.GetName())
	st.save()
	st.saveMonitoring()
	st.mu.Unlock()
	return x.s.finished(req.GetName(), &betapb.DeleteOperationMetadata{GenericMetadata: betaGeneric()}, &emptypb.Empty{}, nil)
}

func keepStats(in []*mmStat, keep func(*mmStat) bool) []*mmStat {
	out := in[:0]
	for _, s := range in {
		if keep(s) {
			out = append(out, s)
		}
	}
	return out
}

// ---- jobs

func (x *modelMonitoringService) CreateModelMonitoringJob(_ context.Context, req *betapb.CreateModelMonitoringJobRequest) (*betapb.ModelMonitoringJob, error) {
	j, err := x.s.createMonitoringJob(req, "", time.Time{})
	return j, grpcError(err)
}

// checkJobRequest validates a CreateModelMonitoringJobRequest, as a call
// or a schedule's template (prefix names its fields).
func (st *notebookStore) checkJobRequest(prefix string, req *betapb.CreateModelMonitoringJobRequest) error {
	if _, err := st.monitor(req.GetParent()); err != nil {
		if apierror.From(err).Code == codes.InvalidArgument {
			return apierror.InvalidArgument("%sparent %q is not projects/{project}/locations/{location}/modelMonitors/{model_monitor}", prefix, req.GetParent())
		}
		return err
	}
	if id := req.GetModelMonitoringJobId(); id != "" && !monitorIDRE.MatchString(id) {
		return apierror.InvalidArgument("%smodel_monitoring_job_id %q must be up to 63 characters and match /^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$/", prefix, id)
	}
	j := req.GetModelMonitoringJob()
	if j == nil {
		return apierror.InvalidArgument("%smodel_monitoring_job is required", prefix)
	}
	if err := checkMonitorDisplayName(prefix+"model_monitoring_job.display_name", j.GetDisplayName()); err != nil {
		return err
	}
	sp := j.GetModelMonitoringSpec()
	if err := checkInput(prefix+"model_monitoring_job.model_monitoring_spec.objective_spec.baseline_dataset", sp.GetObjectiveSpec().GetBaselineDataset()); err != nil {
		return err
	}
	if err := checkInput(prefix+"model_monitoring_job.model_monitoring_spec.objective_spec.target_dataset", sp.GetObjectiveSpec().GetTargetDataset()); err != nil {
		return err
	}
	return checkObjective(prefix+"model_monitoring_job.model_monitoring_spec.objective_spec.tabular_objective", sp.GetObjectiveSpec().GetTabularObjective())
}

// createMonitoringJob creates a job and starts it; schedule and at name
// the schedule run that made it.
func (s *Server) createMonitoringJob(req *betapb.CreateModelMonitoringJobRequest, schedule string, at time.Time) (*betapb.ModelMonitoringJob, error) {
	st := s.notebooks()
	st.mu.Lock()
	if err := st.checkJobRequest("", req); err != nil {
		st.mu.Unlock()
		return nil, err
	}
	mon := st.monitoring()
	j := proto.Clone(req.GetModelMonitoringJob()).(*betapb.ModelMonitoringJob)
	id := req.GetModelMonitoringJobId()
	if id == "" {
		id = numericID()
	}
	j.Name = req.GetParent() + "/modelMonitoringJobs/" + id
	if mon.Jobs[j.Name] != nil {
		st.mu.Unlock()
		return nil, apierror.AlreadyExists("ModelMonitoringJob %s already exists", j.Name)
	}
	now := st.now()
	j.CreateTime, j.UpdateTime = timestamppb.New(now), timestamppb.New(now)
	j.State, j.JobExecutionDetail, j.Schedule, j.ScheduleTime = betapb.JobState_JOB_STATE_PENDING, nil, schedule, nil
	if schedule != "" {
		j.ScheduleTime = timestamppb.New(at)
	}
	mon.Jobs[j.Name] = j
	ctx, cancel := context.WithCancel(context.Background())
	mon.cancels[j.Name] = cancel
	st.saveMonitoring()
	out := proto.Clone(j).(*betapb.ModelMonitoringJob)
	st.mu.Unlock()
	go s.runMonitoringJob(ctx, j.Name)
	return out, nil
}

func (st *notebookStore) monitoringJob(name string) (*betapb.ModelMonitoringJob, error) {
	p := strings.Split(name, "/")
	if len(p) != 8 || p[0] != "projects" || p[2] != "locations" || p[4] != "modelMonitors" || p[6] != "modelMonitoringJobs" {
		return nil, apierror.InvalidArgument("name %q is not projects/{project}/locations/{location}/modelMonitors/{model_monitor}/modelMonitoringJobs/{model_monitoring_job}", name)
	}
	j := st.monitoring().Jobs[name]
	if j == nil {
		return nil, apierror.NotFound("ModelMonitoringJob %s not found", name)
	}
	return j, nil
}

func (x *modelMonitoringService) GetModelMonitoringJob(_ context.Context, req *betapb.GetModelMonitoringJobRequest) (*betapb.ModelMonitoringJob, error) {
	st := x.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	j, err := st.monitoringJob(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	return proto.Clone(j).(*betapb.ModelMonitoringJob), nil
}

func (x *modelMonitoringService) ListModelMonitoringJobs(_ context.Context, req *betapb.ListModelMonitoringJobsRequest) (*betapb.ListModelMonitoringJobsResponse, error) {
	st := x.s.notebooks()
	st.mu.Lock()
	if _, err := st.monitor(req.GetParent()); err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	mon := st.monitoring()
	var all []*betapb.ModelMonitoringJob
	for _, k := range sortedNames(mon.Jobs) {
		if strings.HasPrefix(k, req.GetParent()+"/") {
			all = append(all, proto.Clone(mon.Jobs[k]).(*betapb.ModelMonitoringJob))
		}
	}
	st.mu.Unlock()
	all, err := aipfilter.Apply(req.GetFilter(), all, mmJobFilter...)
	if err != nil {
		return nil, grpcError(err)
	}
	sort.SliceStable(all, func(i, k int) bool { return all[i].GetCreateTime().AsTime().After(all[k].GetCreateTime().AsTime()) })
	for _, j := range all {
		readMasked(j, req.GetReadMask().GetPaths())
	}
	items, next, err := aipfilter.Page("modelMonitoringJobs:"+req.GetParent()+"|"+req.GetFilter(), all, req.GetPageToken(), pageSize(req.GetPageSize()))
	if err != nil {
		return nil, grpcError(err)
	}
	return &betapb.ListModelMonitoringJobsResponse{ModelMonitoringJobs: items, NextPageToken: next}, nil
}

func (x *modelMonitoringService) DeleteModelMonitoringJob(_ context.Context, req *betapb.DeleteModelMonitoringJobRequest) (*longrunningpb.Operation, error) {
	st := x.s.notebooks()
	st.mu.Lock()
	j, err := st.monitoringJob(req.GetName())
	if err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	mon := st.monitoring()
	if c := mon.cancels[j.Name]; c != nil {
		c()
	}
	delete(mon.Jobs, j.Name)
	st.saveMonitoring()
	st.mu.Unlock()
	return x.s.finished(req.GetName(), &betapb.DeleteOperationMetadata{GenericMetadata: betaGeneric()}, &emptypb.Empty{}, nil)
}

// ---- running a job

// effectiveSpec is a job's spec with the monitor's defaults filled in.
func effectiveSpec(m *betapb.ModelMonitor, j *betapb.ModelMonitoringJob) (obj *betapb.ModelMonitoringObjectiveSpec_TabularObjective,
	baseline, target *betapb.ModelMonitoringInput, out *betapb.ModelMonitoringOutputSpec) {
	sp := j.GetModelMonitoringSpec()
	obj = sp.GetObjectiveSpec().GetTabularObjective()
	if obj == nil {
		obj = m.GetTabularObjective()
	}
	baseline = sp.GetObjectiveSpec().GetBaselineDataset()
	if baseline == nil {
		baseline = m.GetTrainingDataset()
	}
	target = sp.GetObjectiveSpec().GetTargetDataset()
	out = sp.GetOutputSpec()
	if out == nil {
		out = m.GetOutputSpec()
	}
	return obj, baseline, target, out
}

// parseSpan reads "2d" as a duration.
func parseSpan(s string) time.Duration {
	m := offsetRE.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	unit := map[string]time.Duration{"h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[strings.ToLower(m[2])]
	return time.Duration(n) * unit
}

// inputRange is the time range in selects, cut off at cutoff; nil when
// it selects all.
func inputRange(in *betapb.ModelMonitoringInput, cutoff time.Time) *interval.Interval {
	if ti := in.GetTimeInterval(); ti != nil {
		return ti
	}
	if to := in.GetTimeOffset(); to != nil {
		end := cutoff.Add(-parseSpan(to.GetOffset()))
		iv := &interval.Interval{EndTime: timestamppb.New(end)}
		if w := parseSpan(to.GetWindow()); w > 0 {
			iv.StartTime = timestamppb.New(end.Add(-w))
		}
		return iv
	}
	return nil
}

func inRange(iv *interval.Interval, t time.Time) bool {
	if iv == nil {
		return true
	}
	if iv.StartTime != nil && t.Before(iv.StartTime.AsTime()) {
		return false
	}
	if iv.EndTime != nil && !t.Before(iv.EndTime.AsTime()) {
		return false
	}
	return true
}

// parseRowTime reads a timestamp field: RFC 3339, or seconds since the
// epoch.
func parseRowTime(v any) (time.Time, bool) {
	s := scalarText(v)
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t, true
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Unix(0, int64(f*1e9)), true
	}
	return time.Time{}, false
}

// timeFiltered keeps the rows whose timestamp field is in iv (all of
// them with no field or range).
func timeFiltered(rows []map[string]any, field string, iv *interval.Interval) []map[string]any {
	if field == "" || iv == nil {
		return rows
	}
	kept := rows[:0]
	for _, r := range rows {
		if t, ok := parseRowTime(r[field]); ok && inRange(iv, t) {
			kept = append(kept, r)
		}
	}
	return kept
}

// modelProject is the project of a resource name projects/{p}/....
func modelProject(name string) string {
	parts := strings.Split(name, "/")
	if len(parts) > 1 && parts[0] == "projects" {
		return parts[1]
	}
	return ""
}

// readInput reads an input's rows and where they came from.
func (s *Server) readInput(ctx context.Context, store objectStore, in *betapb.ModelMonitoringInput, schema *betapb.ModelMonitoringSchema,
	model string, cutoff time.Time) ([]map[string]any, *betapb.ModelMonitoringJobExecutionDetail_ProcessedDataset, error) {
	iv := inputRange(in, cutoff)
	done := &betapb.ModelMonitoringJobExecutionDetail_ProcessedDataset{TimeRange: iv}
	switch d := in.GetDataset().(type) {
	case *betapb.ModelMonitoringInput_ColumnizedDataset:
		ds := d.ColumnizedDataset
		switch src := ds.GetDataLocation().(type) {
		case *betapb.ModelMonitoringInput_ModelMonitoringDataset_GcsSource:
			format := "csv"
			switch src.GcsSource.GetFormat() {
			case betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringGcsSource_JSONL:
				format = "jsonl"
			case betapb.ModelMonitoringInput_ModelMonitoringDataset_ModelMonitoringGcsSource_TF_RECORD:
				format = "tf-record"
			}
			done.Location = src.GcsSource.GetGcsUri()
			rows, err := readGCSRows(ctx, store, src.GcsSource.GetGcsUri(), format)
			return timeFiltered(rows, ds.GetTimestampField(), iv), done, err
		case *betapb.ModelMonitoringInput_ModelMonitoringDataset_BigquerySource:
			var rows []map[string]any
			var err error
			if q := src.BigquerySource.GetQuery(); q != "" {
				done.Location = q
				rows, err = bigQueryQueryRows(ctx, store, modelProject(model), q)
			} else {
				done.Location = src.BigquerySource.GetTableUri()
				rows, err = bigQueryURIRows(ctx, store, "columnized_dataset.bigquery_source.table_uri", src.BigquerySource.GetTableUri())
			}
			return timeFiltered(rows, ds.GetTimestampField(), iv), done, err
		case *betapb.ModelMonitoringInput_ModelMonitoringDataset_VertexDataset:
			done.Location = src.VertexDataset
			rows, err := s.datasetRows(ctx, store, src.VertexDataset)
			return timeFiltered(rows, ds.GetTimestampField(), iv), done, err
		}
		return nil, done, apierror.InvalidArgument("the columnized dataset has no data location")
	case *betapb.ModelMonitoringInput_BatchPredictionOutput_:
		done.Location = d.BatchPredictionOutput.GetBatchPredictionJob()
		return nil, done, apierror.NotFound("BatchPredictionJob %s not found: batch prediction jobs are not served", d.BatchPredictionOutput.GetBatchPredictionJob())
	case *betapb.ModelMonitoringInput_VertexEndpointLogs_:
		done.Location = strings.Join(d.VertexEndpointLogs.GetEndpoints(), ",")
		var rows []map[string]any
		st := s.notebooks()
		st.mu.Lock()
		defer st.mu.Unlock()
		for _, ep := range d.VertexEndpointLogs.GetEndpoints() {
			for _, l := range st.monitoring().Logs[ep] {
				if model != "" && l.Model != "" && !strings.HasPrefix(l.Model, model) {
					continue
				}
				if inRange(iv, l.Time) {
					rows = append(rows, logRow(l, schema.GetFeatureFields(), schema.GetPredictionFields()))
				}
			}
		}
		return rows, done, nil
	}
	return nil, done, apierror.InvalidArgument("the dataset is required")
}

// logRow is a logged prediction as a row: an object instance's keys, or
// an array instance's values in the schema's feature order; an object
// prediction's keys, or a scalar prediction as the first prediction field.
func logRow(l *predLog, features, predictions []*betapb.ModelMonitoringSchema_FieldSchema) map[string]any {
	row := map[string]any{}
	var inst any
	_ = json.Unmarshal(l.Instance, &inst)
	switch v := inst.(type) {
	case map[string]any:
		for k, x := range v {
			row[k] = x
		}
	case []any:
		for i, f := range features {
			if i < len(v) {
				row[f.GetName()] = v[i]
			}
		}
	}
	if predictions == nil {
		return row // the instance's features only (v1's skew and drift)
	}
	var pred any
	_ = json.Unmarshal(l.Prediction, &pred)
	switch v := pred.(type) {
	case map[string]any:
		for k, x := range v {
			row[k] = x
		}
	case nil:
	default:
		if len(predictions) > 0 {
			if arr, ok := v.([]any); ok && len(arr) == 1 {
				v = arr[0]
			}
			row[predictions[0].GetName()] = v
		}
	}
	return row
}

func fieldsOf(fs []*betapb.ModelMonitoringSchema_FieldSchema, only []string) []monitorField {
	want := map[string]bool{}
	for _, n := range only {
		want[n] = true
	}
	var out []monitorField
	for _, f := range fs {
		if len(want) > 0 && !want[f.GetName()] {
			continue
		}
		num, _ := fieldNumeric(f.GetDataType())
		out = append(out, monitorField{Name: f.GetName(), Numeric: num})
	}
	return out
}

func (s *Server) runMonitoringJob(ctx context.Context, name string) {
	st := s.notebooks()
	st.mu.Lock()
	mon := st.monitoring()
	j := mon.Jobs[name]
	if j == nil {
		st.mu.Unlock()
		return
	}
	monitorName := name[:strings.Index(name, "/modelMonitoringJobs/")]
	m := proto.Clone(mon.Monitors[monitorName]).(*betapb.ModelMonitor)
	j.State, j.UpdateTime = betapb.JobState_JOB_STATE_RUNNING, timestamppb.New(st.now())
	spec := proto.Clone(j).(*betapb.ModelMonitoringJob)
	var store objectStore
	if st.pipelineRunner != nil {
		store = st.pipelineRunner
	}
	st.saveMonitoring()
	st.mu.Unlock()

	cutoff := spec.GetCreateTime().AsTime()
	if spec.ScheduleTime != nil {
		cutoff = spec.ScheduleTime.AsTime()
	}
	obj, baseIn, targetIn, outSpec := effectiveSpec(m, spec)
	detail := &betapb.ModelMonitoringJobExecutionDetail{ObjectiveStatus: map[string]*status.Status{}}
	model := m.GetModelMonitoringTarget().GetVertexModel().GetModel()
	var stats []*mmStat
	var alerts []*mmAlert
	fail := func(err error) {
		e := apierror.From(err)
		detail.Error = &status.Status{Code: int32(e.Code), Message: e.Message}
	}
	switch {
	case obj == nil:
		fail(apierror.InvalidArgument("the job has no tabular objective, and its ModelMonitor has no default one"))
	case targetIn == nil:
		fail(apierror.InvalidArgument("model_monitoring_spec.objective_spec.target_dataset is required"))
	case baseIn == nil:
		fail(apierror.InvalidArgument("the job has no baseline dataset, and its ModelMonitor has no training dataset"))
	default:
		base, bdone, berr := s.readInput(ctx, store, baseIn, m.GetModelMonitoringSchema(), model, cutoff)
		detail.BaselineDatasets = []*betapb.ModelMonitoringJobExecutionDetail_ProcessedDataset{bdone}
		target, tdone, terr := s.readInput(ctx, store, targetIn, m.GetModelMonitoringSchema(), model, cutoff)
		detail.TargetDatasets = []*betapb.ModelMonitoringJobExecutionDetail_ProcessedDataset{tdone}
		if berr != nil {
			fail(berr)
			break
		}
		if terr != nil {
			fail(terr)
			break
		}
		now := time.Now()
		for _, o := range []struct {
			key    string
			spec   *betapb.ModelMonitoringObjectiveSpec_DataDriftSpec
			fields []*betapb.ModelMonitoringSchema_FieldSchema
		}{{objFeatureDrift, obj.GetFeatureDriftSpec(), m.GetModelMonitoringSchema().GetFeatureFields()},
			{objPredictionDrift, obj.GetPredictionOutputDriftSpec(), m.GetModelMonitoringSchema().GetPredictionFields()}} {
			if o.spec == nil {
				continue
			}
			var oerr error
			for _, f := range fieldsOf(o.fields, o.spec.GetFeatures()) {
				algo := o.spec.GetNumericMetricType()
				cond := o.spec.GetDefaultNumericAlertCondition()
				if !f.Numeric {
					algo, cond = o.spec.GetCategoricalMetricType(), o.spec.GetDefaultCategoricalAlertCondition()
				}
				if c := o.spec.GetFeatureAlertConditions()[f.Name]; c != nil {
					cond = c
				}
				d, err := compareFeature(f, algo, base, target)
				if err != nil {
					oerr = err
					break
				}
				p := &betapb.ModelMonitoringStatsDataPoint{
					CurrentStats: &betapb.ModelMonitoringStatsDataPoint_TypedValue{Value: &betapb.ModelMonitoringStatsDataPoint_TypedValue_DistributionValue{
						DistributionValue: &betapb.ModelMonitoringStatsDataPoint_TypedValue_DistributionDataValue{Distribution: d.Current, DistributionDeviation: d.Distance}}},
					BaselineStats: &betapb.ModelMonitoringStatsDataPoint_TypedValue{Value: &betapb.ModelMonitoringStatsDataPoint_TypedValue_DistributionValue{
						DistributionValue: &betapb.ModelMonitoringStatsDataPoint_TypedValue_DistributionDataValue{Distribution: d.Baseline}}},
					ModelMonitoringJob: name, Schedule: spec.GetSchedule(), CreateTime: timestamppb.New(now), Algorithm: d.Algorithm,
				}
				if c := cond.GetThreshold(); cond != nil {
					p.ThresholdValue = c
					p.HasAnomaly = d.Distance > c
				}
				stats = append(stats, &mmStat{Monitor: monitorName, Objective: o.key, StatsName: f.Name, Point: p})
				if p.HasAnomaly {
					body, _ := structpb.NewValue(map[string]any{"featureName": f.Name, "distance": d.Distance, "threshold": p.ThresholdValue, "algorithm": d.Algorithm,
						"baselineCount": float64(d.BaseN), "targetCount": float64(d.CurrentN)})
					a := &betapb.ModelMonitoringAlert{StatsName: f.Name, ObjectiveType: o.key, AlertTime: timestamppb.New(now),
						Anomaly: &betapb.ModelMonitoringAnomaly{ModelMonitoringJob: name, Algorithm: d.Algorithm,
							Anomaly: &betapb.ModelMonitoringAnomaly_TabularAnomaly_{TabularAnomaly: &betapb.ModelMonitoringAnomaly_TabularAnomaly{
								Summary:     fmt.Sprintf("%s of %s is %g, above the threshold %g", d.Algorithm, f.Name, d.Distance, p.ThresholdValue),
								Anomaly:     body,
								TriggerTime: timestamppb.New(now),
								Condition:   cond,
							}}}}
					alerts = append(alerts, &mmAlert{Monitor: monitorName, Alert: a})
				}
			}
			if oerr != nil {
				e := apierror.From(oerr)
				detail.ObjectiveStatus[o.key] = &status.Status{Code: int32(e.Code), Message: e.Message}
			} else {
				detail.ObjectiveStatus[o.key] = &status.Status{}
			}
		}
		if fa := obj.GetFeatureAttributionSpec(); fa != nil {
			as, aa, err := s.monitorAttribution(ctx, m, monitorName, name, spec.GetSchedule(), fa, base, target, now)
			stats, alerts = append(stats, as...), append(alerts, aa...)
			detail.ObjectiveStatus[objAttribution] = &status.Status{}
			if err != nil {
				e := apierror.From(err)
				detail.ObjectiveStatus[objAttribution] = &status.Status{Code: int32(e.Code), Message: e.Message}
			}
		}
	}
	if dir := outSpec.GetGcsBaseDirectory().GetOutputUriPrefix(); dir != "" && store != nil && detail.Error == nil {
		uri := strings.TrimSuffix(dir, "/") + "/" + lastSegment(monitorName) + "/" + lastSegment(name)
		if b, o, err := monGCS("output_uri_prefix", uri); err == nil {
			var res []json.RawMessage
			for _, x := range stats {
				res = append(res, monJSON(&betapb.ModelMonitoringTabularStats{StatsName: x.StatsName, ObjectiveType: x.Objective, DataPoints: []*betapb.ModelMonitoringStatsDataPoint{x.Point}}))
			}
			data, _ := json.Marshal(map[string]any{"stats": res})
			if err := store.WriteObject(ctx, b, o+"/stats.json", "application/json", data); err != nil {
				fail(apierror.FailedPrecondition("writing %s/stats.json: %v", uri, err))
			}
			for _, a := range alerts {
				a.Alert.GetAnomaly().GetTabularAnomaly().AnomalyUri = uri + "/stats.json"
			}
		}
	}

	if ctx.Err() == nil {
		ns := spec.GetModelMonitoringSpec().GetNotificationSpec()
		if ns == nil {
			ns = m.GetNotificationSpec()
		}
		s.notifyMonitorAlerts(ctx, name, ns, alerts)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	j = mon.Jobs[name]
	delete(mon.cancels, name)
	if j == nil || ctx.Err() != nil {
		return
	}
	j.JobExecutionDetail = detail
	j.State = betapb.JobState_JOB_STATE_SUCCEEDED
	failed, ok := 0, 0
	for _, s := range detail.ObjectiveStatus {
		if s.GetCode() != 0 {
			failed++
		} else {
			ok++
		}
	}
	switch {
	case detail.Error != nil || (failed > 0 && ok == 0):
		j.State = betapb.JobState_JOB_STATE_FAILED
	case failed > 0:
		j.State = betapb.JobState_JOB_STATE_PARTIALLY_SUCCEEDED
	}
	j.UpdateTime = timestamppb.New(st.now())
	mon.Stats = append(mon.Stats, stats...)
	mon.Alerts = append(mon.Alerts, alerts...)
	st.saveMonitoring()
}

// ---- search

func (x *modelMonitoringService) SearchModelMonitoringStats(_ context.Context, req *betapb.SearchModelMonitoringStatsRequest) (*betapb.SearchModelMonitoringStatsResponse, error) {
	st := x.s.notebooks()
	st.mu.Lock()
	if _, err := st.monitor(req.GetModelMonitor()); err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	f := req.GetStatsFilter().GetTabularStatsFilter()
	groups := map[string]*betapb.ModelMonitoringTabularStats{}
	var keys []string
	for _, s := range st.monitoring().Stats {
		if s.Monitor != req.GetModelMonitor() {
			continue
		}
		if f.GetStatsName() != "" && f.GetStatsName() != s.StatsName || f.GetObjectiveType() != "" && f.GetObjectiveType() != s.Objective ||
			f.GetModelMonitoringJob() != "" && f.GetModelMonitoringJob() != s.Point.GetModelMonitoringJob() ||
			f.GetModelMonitoringSchedule() != "" && f.GetModelMonitoringSchedule() != s.Point.GetSchedule() ||
			f.GetAlgorithm() != "" && f.GetAlgorithm() != s.Point.GetAlgorithm() {
			continue
		}
		if ti := req.GetTimeInterval(); ti != nil && !inRange(ti, s.Point.GetCreateTime().AsTime()) {
			continue
		}
		k := s.Objective + "\x00" + s.StatsName
		g := groups[k]
		if g == nil {
			g = &betapb.ModelMonitoringTabularStats{StatsName: s.StatsName, ObjectiveType: s.Objective}
			groups[k] = g
			keys = append(keys, k)
		}
		g.DataPoints = append(g.DataPoints, proto.Clone(s.Point).(*betapb.ModelMonitoringStatsDataPoint))
	}
	st.mu.Unlock()
	sort.Strings(keys)
	var all []*betapb.ModelMonitoringStats
	for _, k := range keys {
		g := groups[k]
		sort.SliceStable(g.DataPoints, func(i, j int) bool {
			return g.DataPoints[i].GetCreateTime().AsTime().After(g.DataPoints[j].GetCreateTime().AsTime())
		})
		all = append(all, &betapb.ModelMonitoringStats{Stats: &betapb.ModelMonitoringStats_TabularStats{TabularStats: g}})
	}
	items, next, err := aipfilter.Page("modelMonitoringStats:"+req.GetModelMonitor()+"|"+monString(req.GetStatsFilter()), all, req.GetPageToken(), pageSize(req.GetPageSize()))
	if err != nil {
		return nil, grpcError(err)
	}
	return &betapb.SearchModelMonitoringStatsResponse{MonitoringStats: items, NextPageToken: next}, nil
}

func monString(m proto.Message) string { return string(monJSON(m)) }

func (x *modelMonitoringService) SearchModelMonitoringAlerts(_ context.Context, req *betapb.SearchModelMonitoringAlertsRequest) (*betapb.SearchModelMonitoringAlertsResponse, error) {
	st := x.s.notebooks()
	st.mu.Lock()
	if _, err := st.monitor(req.GetModelMonitor()); err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	var all []*betapb.ModelMonitoringAlert
	for _, a := range st.monitoring().Alerts {
		al := a.Alert
		if a.Monitor != req.GetModelMonitor() || req.GetModelMonitoringJob() != "" && req.GetModelMonitoringJob() != al.GetAnomaly().GetModelMonitoringJob() ||
			req.GetStatsName() != "" && req.GetStatsName() != al.GetStatsName() || req.GetObjectiveType() != "" && req.GetObjectiveType() != al.GetObjectiveType() {
			continue
		}
		if ti := req.GetAlertTimeInterval(); ti != nil && !inRange(ti, al.GetAlertTime().AsTime()) {
			continue
		}
		all = append(all, proto.Clone(al).(*betapb.ModelMonitoringAlert))
	}
	st.mu.Unlock()
	sort.SliceStable(all, func(i, j int) bool { return all[i].GetAlertTime().AsTime().After(all[j].GetAlertTime().AsTime()) })
	items, next, err := aipfilter.Page("modelMonitoringAlerts:"+req.GetModelMonitor()+"|"+req.GetModelMonitoringJob()+"|"+req.GetStatsName()+"|"+req.GetObjectiveType(),
		all, req.GetPageToken(), pageSize(req.GetPageSize()))
	if err != nil {
		return nil, grpcError(err)
	}
	return &betapb.SearchModelMonitoringAlertsResponse{ModelMonitoringAlerts: items, TotalNumberAlerts: int64(len(all)), NextPageToken: next}, nil
}

// ---- logging predictions

// logPrediction keeps an endpoint's prediction for Model Monitoring when
// the endpoint logs requests (predict_request_response_logging_config) or
// a ModelDeploymentMonitoringJob monitors it, at their sampling rate.
func (s *Server) logPrediction(project, location, endpoint, deployedModel string, model *aiplatformpb.Model, instances, predictions []json.RawMessage) {
	name := "projects/" + project + "/locations/" + location + "/endpoints/" + endpoint
	rate := 0.0
	if r := s.registry(); r != nil {
		if e, err := r.getEndpoint(name); err == nil {
			if c := e.GetPredictRequestResponseLoggingConfig(); c.GetEnabled() {
				rate = c.GetSamplingRate()
			}
		}
	}
	st := s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	mon := st.monitoring()
	for _, j := range mon.MDM {
		if lastSegment(j.GetEndpoint()) == endpoint && strings.HasPrefix(j.GetEndpoint(), "projects/"+project+"/locations/"+location+"/") &&
			j.GetState() != aiplatformpb.JobState_JOB_STATE_PAUSED {
			r := j.GetLoggingSamplingStrategy().GetRandomSampleConfig().GetSampleRate()
			if r == 0 {
				r = 1
			}
			if r > rate {
				rate = r
			}
		}
	}
	if rate <= 0 {
		return
	}
	now := time.Now()
	modelName := ""
	if model != nil {
		modelName = model.GetName()
	}
	logs := mon.Logs[name]
	for i, in := range instances {
		if rate < 1 && sampleRand() >= rate {
			continue
		}
		var p json.RawMessage
		if i < len(predictions) {
			p = predictions[i]
		}
		logs = append(logs, &predLog{Time: now, DeployedModel: deployedModel, Model: modelName, Instance: in, Prediction: p})
	}
	if len(logs) > maxLoggedPredictions {
		logs = logs[len(logs)-maxLoggedPredictions:]
	}
	mon.Logs[name] = logs
	st.saveMonitoring()
}

// protoName is a snake_case field name as a protoreflect name.
func protoName(s string) protoreflect.Name { return protoreflect.Name(s) }

// sampleRand draws a prediction's sample.
var sampleRand = rand.Float64
