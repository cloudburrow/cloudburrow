package dataflow

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// ListJobMessages lists a job's messages of at least the minimum
// importance in the time range, oldest first, with its autoscaling
// events.
func (s *Server) ListJobMessages(_ context.Context, req *dataflowpb.ListJobMessagesRequest) (*dataflowpb.ListJobMessagesResponse, error) {
	r, err := s.lookup(req.GetProjectId(), req.GetLocation(), req.GetJobId())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	min := req.GetMinimumImportance()
	in := func(t *timestamppb.Timestamp) bool {
		if req.GetStartTime() != nil && t.AsTime().Before(req.GetStartTime().AsTime()) {
			return false
		}
		return req.GetEndTime() == nil || t.AsTime().Before(req.GetEndTime().AsTime())
	}
	byKey := map[string]*dataflowpb.JobMessage{}
	var keys []string
	for i, m := range r.messages {
		if m.MessageImportance < min || !in(m.Time) {
			continue
		}
		k := fmt.Sprintf("%08d", i)
		byKey[k] = m
		keys = append(keys, k)
	}
	scope := fmt.Sprintf("messages:%s/%d/%v/%v", req.GetJobId(), min, req.GetStartTime().AsTime(), req.GetEndTime().AsTime())
	page, next, err := paging.Page(scope, keys, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token")
	}
	out := &dataflowpb.ListJobMessagesResponse{NextPageToken: next}
	for _, k := range page {
		out.JobMessages = append(out.JobMessages, proto.Clone(byKey[k]).(*dataflowpb.JobMessage))
	}
	if req.GetPageToken() == "" {
		for _, e := range r.scaling {
			if in(e.Time) {
				out.AutoscalingEvents = append(out.AutoscalingEvents, proto.Clone(e).(*dataflowpb.AutoscalingEvent))
			}
		}
	}
	return out, nil
}

// GetJobMetrics answers the job's metrics: each step's ElementCount
// (origin dataflow/v1b3) and the pipeline's own (origin user), each user
// metric committed and tentative, as Google reports them.
func (s *Server) GetJobMetrics(_ context.Context, req *dataflowpb.GetJobMetricsRequest) (*dataflowpb.JobMetrics, error) {
	r, err := s.lookup(req.GetProjectId(), req.GetLocation(), req.GetJobId())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &dataflowpb.JobMetrics{MetricTime: timestamppb.New(s.now())}
	for _, m := range r.metrics {
		if req.GetStartTime() != nil && !m.Updated.IsZero() && m.Updated.Before(req.GetStartTime().AsTime()) {
			continue
		}
		out.Metrics = append(out.Metrics, metricUpdates(m)...)
	}
	return out, nil
}

func metricUpdates(m Metric) []*dataflowpb.MetricUpdate {
	ctx := map[string]string{}
	if m.Origin == "user" {
		ctx["step"], ctx["namespace"] = m.Step, m.Namespace
	} else {
		if m.PCollection != "" {
			ctx["original_name"] = m.PCollection + "-ElementCount"
		}
		if m.Step != "" {
			ctx["output_user_name"] = m.Step + ".out0"
		}
	}
	mk := func(tentative bool) *dataflowpb.MetricUpdate {
		c := map[string]string{}
		for k, v := range ctx {
			c[k] = v
		}
		if tentative {
			c["tentative"] = "true"
		}
		u := &dataflowpb.MetricUpdate{Name: &dataflowpb.MetricStructuredName{Origin: m.Origin, Name: m.Name, Context: c},
			Kind: m.Kind, Cumulative: true}
		if !m.Updated.IsZero() {
			u.UpdateTime = timestamppb.New(m.Updated)
		}
		switch m.Kind {
		case "Distribution":
			u.Distribution = structpb.NewStructValue(&structpb.Struct{Fields: map[string]*structpb.Value{
				"count": structpb.NewNumberValue(float64(m.Count)), "min": structpb.NewNumberValue(float64(m.Min)),
				"max": structpb.NewNumberValue(float64(m.Max)), "sum": structpb.NewNumberValue(float64(m.Total))}})
		default:
			u.Scalar = structpb.NewNumberValue(float64(m.Value))
		}
		return u
	}
	if m.Origin == "user" {
		return []*dataflowpb.MetricUpdate{mk(false), mk(true)}
	}
	return []*dataflowpb.MetricUpdate{mk(false)}
}

// describe is the pipeline description of ts: the user's transforms (the
// top-level and their parts) and the fused stages.
func describe(ts []Transform) *dataflowpb.PipelineDescription {
	d := &dataflowpb.PipelineDescription{}
	stages := map[string]*dataflowpb.ExecutionStageSummary{}
	var order []string
	for _, t := range ts {
		d.OriginalPipelineTransform = append(d.OriginalPipelineTransform, &dataflowpb.TransformSummary{
			Kind: kindOf(t.Kind), Id: t.ID, Name: t.Name,
			InputCollectionName: t.Inputs, OutputCollectionName: t.Outputs})
		if t.Stage == "" {
			continue
		}
		st := stages[t.Stage]
		if st == nil {
			st = &dataflowpb.ExecutionStageSummary{Name: t.Stage, Id: fmt.Sprintf("S%02d", len(order)+1), Kind: dataflowpb.KindType_PAR_DO_KIND}
			stages[t.Stage] = st
			order = append(order, t.Stage)
		}
		st.ComponentTransform = append(st.ComponentTransform, &dataflowpb.ExecutionStageSummary_ComponentTransform{
			UserName: t.Name, Name: t.ID, OriginalTransform: t.Name})
	}
	for _, n := range order {
		d.ExecutionPipelineStage = append(d.ExecutionPipelineStage, stages[n])
	}
	return d
}

func kindOf(k string) dataflowpb.KindType {
	switch strings.ToLower(k) {
	case "pardo":
		return dataflowpb.KindType_PAR_DO_KIND
	case "groupbykey":
		return dataflowpb.KindType_GROUP_BY_KEY_KIND
	case "flatten":
		return dataflowpb.KindType_FLATTEN_KIND
	case "read", "impulse":
		return dataflowpb.KindType_READ_KIND
	case "write":
		return dataflowpb.KindType_WRITE_KIND
	case "combine":
		return dataflowpb.KindType_GROUP_BY_KEY_KIND
	case "composite":
		return dataflowpb.KindType_UNKNOWN_KIND
	}
	return dataflowpb.KindType_PAR_DO_KIND
}

// GetJobExecutionDetails answers the job's stages and their states.
func (s *Server) GetJobExecutionDetails(_ context.Context, req *dataflowpb.GetJobExecutionDetailsRequest) (*dataflowpb.JobExecutionDetails, error) {
	r, err := s.lookup(req.GetProjectId(), req.GetLocation(), req.GetJobId())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &dataflowpb.JobExecutionDetails{}
	st := stageState(r.job.CurrentState)
	for _, stg := range r.job.GetPipelineDescription().GetExecutionPipelineStage() {
		sum := &dataflowpb.StageSummary{StageId: stg.Id, State: st, StartTime: r.job.StartTime}
		if terminal(r.job.CurrentState) {
			sum.EndTime = r.job.CurrentStateTime
		}
		out.Stages = append(out.Stages, sum)
	}
	sort.Slice(out.Stages, func(i, j int) bool { return out.Stages[i].StageId < out.Stages[j].StageId })
	return out, nil
}

func stageState(st dataflowpb.JobState) dataflowpb.ExecutionState {
	switch st {
	case dataflowpb.JobState_JOB_STATE_DONE, dataflowpb.JobState_JOB_STATE_DRAINED, dataflowpb.JobState_JOB_STATE_UPDATED:
		return dataflowpb.ExecutionState_EXECUTION_STATE_SUCCEEDED
	case dataflowpb.JobState_JOB_STATE_FAILED:
		return dataflowpb.ExecutionState_EXECUTION_STATE_FAILED
	case dataflowpb.JobState_JOB_STATE_CANCELLED:
		return dataflowpb.ExecutionState_EXECUTION_STATE_CANCELLED
	case dataflowpb.JobState_JOB_STATE_PENDING, dataflowpb.JobState_JOB_STATE_QUEUED:
		return dataflowpb.ExecutionState_EXECUTION_STATE_NOT_STARTED
	}
	return dataflowpb.ExecutionState_EXECUTION_STATE_RUNNING
}

// GetStageExecutionDetails answers a stage's worker: each job runs on the
// one worker of its local runner.
func (s *Server) GetStageExecutionDetails(_ context.Context, req *dataflowpb.GetStageExecutionDetailsRequest) (*dataflowpb.StageExecutionDetails, error) {
	r, err := s.lookup(req.GetProjectId(), req.GetLocation(), req.GetJobId())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, stg := range r.job.GetPipelineDescription().GetExecutionPipelineStage() {
		if stg.Id != req.GetStageId() {
			continue
		}
		item := &dataflowpb.WorkItemDetails{TaskId: stg.Id + "-0", AttemptId: "0", StartTime: r.job.StartTime,
			State: stageState(r.job.CurrentState)}
		if terminal(r.job.CurrentState) {
			item.EndTime = r.job.CurrentStateTime
		}
		return &dataflowpb.StageExecutionDetails{Workers: []*dataflowpb.WorkerDetails{{
			WorkerName: r.job.Name + "-harness-0", WorkItems: []*dataflowpb.WorkItemDetails{item}}}}, nil
	}
	return nil, apierror.NotFound("Stage %s of job %s was not found.", req.GetStageId(), req.GetJobId()) // UNVERIFIED (docs re-read 2026-11-24)
}
