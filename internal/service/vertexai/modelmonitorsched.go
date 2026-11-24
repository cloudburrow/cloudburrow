package vertexai

// google.cloud.aiplatform.v1beta1.ScheduleService (#2214): v1beta1's
// schedules are v1's (schedules.go) plus create_model_monitoring_job_request,
// Model Monitoring v2's scheduled runs ("Schedule continuous monitoring
// runs", https://docs.cloud.google.com/vertex-ai/docs/model-monitoring/run-monitoring-job,
// read 2026-11-24). Each method converts to v1 strictly, so another
// v1beta1-only field is refused by name, and keeps a monitoring
// schedule's request beside the v1 schedule; each due run creates a
// ModelMonitoringJob under the monitor, with schedule and schedule_time
// set.

import (
	"context"
	"strings"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	betapb "cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

type betaScheduleService struct {
	betapb.UnimplementedScheduleServiceServer
	s *Server
}

func (b *betaScheduleService) v1() *scheduleService { return &scheduleService{s: b.s} }

// splitSchedule is a v1beta1 schedule as the v1 schedule and its model
// monitoring request.
func splitSchedule(in *betapb.Schedule) (*aiplatformpb.Schedule, *betapb.CreateModelMonitoringJobRequest, error) {
	c := proto.Clone(in).(*betapb.Schedule)
	mreq := c.GetCreateModelMonitoringJobRequest()
	if mreq != nil {
		c.Request = nil
	}
	var v aiplatformpb.Schedule
	if err := toV1(c, &v); err != nil {
		return nil, nil, err
	}
	return &v, mreq, nil
}

// betaView is a v1 schedule as v1beta1's; the caller holds st.mu.
func (st *notebookStore) betaView(sc *aiplatformpb.Schedule) (*betapb.Schedule, error) {
	out := &betapb.Schedule{}
	if err := fromV1(sc, out); err != nil {
		return nil, err
	}
	if r := st.monitoring().SchedReqs[sc.GetName()]; r != nil {
		out.Request = &betapb.Schedule_CreateModelMonitoringJobRequest{CreateModelMonitoringJobRequest: proto.Clone(r).(*betapb.CreateModelMonitoringJobRequest)}
	}
	return out, nil
}

func (b *betaScheduleService) view(sc *aiplatformpb.Schedule, err error) (*betapb.Schedule, error) {
	if err != nil {
		return nil, err
	}
	st := b.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	out, e := st.betaView(sc)
	return out, grpcError(e)
}

func (b *betaScheduleService) CreateSchedule(ctx context.Context, req *betapb.CreateScheduleRequest) (*betapb.Schedule, error) {
	if req.GetSchedule() == nil {
		return nil, grpcError(apierror.InvalidArgument("schedule is required"))
	}
	v, mreq, err := splitSchedule(req.GetSchedule())
	if err != nil {
		return nil, grpcError(err)
	}
	if mreq == nil {
		return b.view(b.v1().CreateSchedule(ctx, &aiplatformpb.CreateScheduleRequest{Parent: req.GetParent(), Schedule: v}))
	}
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	st := b.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	if !strings.HasPrefix(mreq.GetParent(), req.GetParent()+"/modelMonitors/") {
		return nil, grpcError(apierror.InvalidArgument("schedule.create_model_monitoring_job_request.parent %q must be a ModelMonitor in %s", mreq.GetParent(), req.GetParent()))
	}
	if mreq.GetModelMonitoringJobId() != "" {
		return nil, grpcError(apierror.InvalidArgument("schedule.create_model_monitoring_job_request.model_monitoring_job_id: User-specified ID is not supported in the request template"))
	}
	if err := st.checkJobRequest("schedule.create_model_monitoring_job_request.", mreq); err != nil {
		return nil, grpcError(err)
	}
	now := st.now()
	v.Name = req.GetParent() + "/schedules/" + numericID()
	mon := st.monitoring()
	mon.SchedReqs[v.Name] = proto.Clone(mreq).(*betapb.CreateModelMonitoringJobRequest)
	if err := st.checkSchedule(req.GetParent(), v); err != nil {
		delete(mon.SchedReqs, v.Name)
		return nil, grpcError(err)
	}
	v.CreateTime, v.UpdateTime = timestamppb.New(now), timestamppb.New(now)
	if v.StartTime == nil {
		v.StartTime = v.CreateTime
	}
	v.State, v.StartedRunCount, v.LastPauseTime, v.LastResumeTime, v.LastScheduledRunResponse, v.CatchUp =
		aiplatformpb.Schedule_ACTIVE, 0, nil, nil, nil, false
	v.NextRunTime = nextRun(v, now)
	if v.NextRunTime == nil {
		v.State = aiplatformpb.Schedule_COMPLETED
	}
	st.schedules[v.Name] = v
	st.owners[v.Name] = b.s.callerOf(ctx)
	st.save()
	st.saveMonitoring()
	out, err := st.betaView(v)
	return out, grpcError(err)
}

func (b *betaScheduleService) GetSchedule(ctx context.Context, req *betapb.GetScheduleRequest) (*betapb.Schedule, error) {
	return b.view(b.v1().GetSchedule(ctx, &aiplatformpb.GetScheduleRequest{Name: req.GetName()}))
}

func (b *betaScheduleService) ListSchedules(ctx context.Context, req *betapb.ListSchedulesRequest) (*betapb.ListSchedulesResponse, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	st := b.s.notebooks()
	st.mu.Lock()
	var all []*betapb.Schedule
	for _, k := range sortedNames(st.schedules) {
		if strings.HasPrefix(k, req.GetParent()+"/") {
			v, err := st.betaView(st.schedules[k])
			if err != nil {
				st.mu.Unlock()
				return nil, grpcError(err)
			}
			all = append(all, v)
		}
	}
	st.mu.Unlock()
	opts := []aipfilter.Option{aipfilter.Fields("display_name", "state", "create_notebook_execution_job_request", "create_pipeline_job_request",
		"create_model_monitoring_job_request", "create_time", "start_time", "end_time", "next_run_time")}
	all, err := aipfilter.Apply(req.GetFilter(), all, opts...)
	if err != nil {
		return nil, grpcError(err)
	}
	order := req.GetOrderBy()
	if order == "" {
		order = "create_time desc"
	}
	if err := aipfilter.SortBy(order, all, scheduleOrder); err != nil {
		return nil, grpcError(err)
	}
	items, next, err := aipfilter.Page("schedules:"+req.GetParent()+"|"+req.GetFilter()+"|"+order, all, req.GetPageToken(), pageSize(req.GetPageSize()))
	if err != nil {
		return nil, grpcError(err)
	}
	return &betapb.ListSchedulesResponse{Schedules: items, NextPageToken: next}, nil
}

func (b *betaScheduleService) DeleteSchedule(ctx context.Context, req *betapb.DeleteScheduleRequest) (*longrunningpb.Operation, error) {
	if _, err := b.v1().DeleteSchedule(ctx, &aiplatformpb.DeleteScheduleRequest{Name: req.GetName()}); err != nil {
		return nil, err
	}
	return b.s.finished(req.GetName(), &betapb.DeleteOperationMetadata{GenericMetadata: betaGeneric()}, &emptypb.Empty{}, nil)
}

func (b *betaScheduleService) PauseSchedule(ctx context.Context, req *betapb.PauseScheduleRequest) (*emptypb.Empty, error) {
	return b.v1().PauseSchedule(ctx, &aiplatformpb.PauseScheduleRequest{Name: req.GetName()})
}

func (b *betaScheduleService) ResumeSchedule(ctx context.Context, req *betapb.ResumeScheduleRequest) (*emptypb.Empty, error) {
	return b.v1().ResumeSchedule(ctx, &aiplatformpb.ResumeScheduleRequest{Name: req.GetName(), CatchUp: req.GetCatchUp()})
}

func (b *betaScheduleService) UpdateSchedule(ctx context.Context, req *betapb.UpdateScheduleRequest) (*betapb.Schedule, error) {
	if req.GetSchedule() == nil {
		return nil, grpcError(apierror.InvalidArgument("schedule is required"))
	}
	v, mreq, err := splitSchedule(req.GetSchedule())
	if err != nil {
		return nil, grpcError(err)
	}
	var rest []string
	monitorPath := false
	for _, p := range req.GetUpdateMask().GetPaths() {
		if camelToSnake(p) == "create_model_monitoring_job_request" {
			monitorPath = true
			continue
		}
		rest = append(rest, p)
	}
	if monitorPath {
		st := b.s.notebooks()
		st.mu.Lock()
		cur := st.monitoring().SchedReqs[req.GetSchedule().GetName()]
		var err error
		switch {
		case st.schedules[req.GetSchedule().GetName()] == nil:
			err = apierror.NotFound("Schedule %s not found", req.GetSchedule().GetName())
		case cur == nil || mreq == nil:
			err = apierror.InvalidArgument("the scheduled request type cannot be changed")
		case mreq.GetParent() != cur.GetParent():
			err = apierror.InvalidArgument("schedule.create_model_monitoring_job_request.parent cannot be changed")
		default:
			err = st.checkJobRequest("schedule.create_model_monitoring_job_request.", mreq)
		}
		if err != nil {
			st.mu.Unlock()
			return nil, grpcError(err)
		}
		st.monitoring().SchedReqs[req.GetSchedule().GetName()] = proto.Clone(mreq).(*betapb.CreateModelMonitoringJobRequest)
		if sc := st.schedules[req.GetSchedule().GetName()]; sc != nil {
			sc.UpdateTime = timestamppb.New(st.now())
		}
		st.save()
		st.saveMonitoring()
		st.mu.Unlock()
		if len(rest) == 0 {
			return b.GetSchedule(ctx, &betapb.GetScheduleRequest{Name: req.GetSchedule().GetName()})
		}
	}
	return b.view(b.v1().UpdateSchedule(ctx, &aiplatformpb.UpdateScheduleRequest{Schedule: v, UpdateMask: &fieldmaskpb.FieldMask{Paths: rest}}))
}
