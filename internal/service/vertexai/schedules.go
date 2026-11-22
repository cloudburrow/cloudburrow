package vertexai

// Colab Enterprise schedules (#2105): google.cloud.aiplatform.v1.ScheduleService
// over gRPC and REST, for schedules whose request is a notebook execution
// (create_notebook_execution_job_request). Each due run creates a
// NotebookExecutionJob (notebookjobs.go) named after the schedule.
//
// Documented behaviour (the ScheduleService protos; the v1 discovery
// document, revision 20261114; https://docs.cloud.google.com/colab/docs/schedule-notebook-run;
// .../reference/rest/v1/projects.locations.schedules/pause; read 2026-11-22):
//   - display_name is required, up to 128 characters; cron is required,
//     unix-cron with an optional "CRON_TZ=" or "TZ=" IANA time zone
//     prefix; max_concurrent_run_count is required;
//   - start_time defaults to the create time; the schedule is COMPLETED
//     when end_time is reached or started_run_count reaches max_run_count;
//   - a paused schedule creates no runs and "Already created runs will NOT
//     be paused or canceled"; a COMPLETED schedule "cannot be paused or
//     resumed"; resume's catch_up backfills the runs missed while paused;
//   - allow_queueing queues a run the max_concurrent_run_count limit holds
//     back instead of skipping it;
//   - "User-specified ID is not supported in the request template";
//   - an update cannot change the request type, and ignores output-only
//     fields; its mask is required;
//   - the list filters display_name, state, request presence
//     (create_notebook_execution_job_request:*), create_time, start_time,
//     end_time and next_run_time, orders by create_time, start_time,
//     end_time and next_run_time, by default create_time desc, and pages
//     100 by default;
//   - "Deleting a schedule doesn't delete the runs it generated."
//
// UNVERIFIED (re-read 2026-11-22: https://docs.cloud.google.com/colab/docs/schedule-notebook-run):
// pausing a paused schedule and resuming an active one succeed without a
// change; a schedule's ID is generated, numeric; last_scheduled_run_response's
// run_response is the created job's name; a run counts toward
// max_concurrent_run_count until its job ends.
//
// Not served: a schedule of a Vertex AI pipeline (create_pipeline_job_request)
// is UNIMPLEMENTED, as Vertex AI Pipelines is not run on this instance.

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/robfig/cron/v3"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// scheduleTick is how often due schedules are run.
var scheduleTick = 5 * time.Second

var (
	scheduleFilter = []aipfilter.Option{
		aipfilter.Fields("display_name", "state", "create_notebook_execution_job_request", "create_pipeline_job_request", "create_time", "start_time", "end_time", "next_run_time"),
	}
	scheduleOrder  = aipfilter.Fields("create_time", "start_time", "end_time", "next_run_time")
	scheduleMasks  = map[string]bool{"display_name": true, "cron": true, "start_time": true, "end_time": true, "max_run_count": true, "max_concurrent_run_count": true, "allow_queueing": true, "create_notebook_execution_job_request": true}
	scheduleParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
)

type scheduleService struct {
	aiplatformpb.UnimplementedScheduleServiceServer
	s *Server
}

// parseCron is a schedule's cron with its CRON_TZ= or TZ= prefix.
func parseCron(spec string) (cron.Schedule, error) {
	spec = strings.TrimSpace(spec)
	if strings.HasPrefix(spec, "TZ=") {
		spec = "CRON_TZ=" + strings.TrimPrefix(spec, "TZ=")
	}
	return scheduleParser.Parse(spec)
}

// checkSchedule validates a schedule's fields; the caller holds st.mu.
func (st *notebookStore) checkSchedule(parent string, sc *aiplatformpb.Schedule) error {
	if strings.TrimSpace(sc.GetDisplayName()) == "" {
		return apierror.InvalidArgument("schedule.display_name is required")
	}
	if utf8.RuneCountInString(sc.GetDisplayName()) > 128 {
		return apierror.InvalidArgument("schedule.display_name can be up to 128 characters long")
	}
	if sc.GetCron() == "" {
		return apierror.InvalidArgument("schedule.cron is required")
	}
	if _, err := parseCron(sc.GetCron()); err != nil {
		return apierror.InvalidArgument("schedule.cron %q is not a valid cron schedule: %v", sc.GetCron(), err)
	}
	if sc.GetMaxConcurrentRunCount() <= 0 {
		return apierror.InvalidArgument("schedule.max_concurrent_run_count is required and must be positive")
	}
	if sc.GetMaxRunCount() < 0 {
		return apierror.InvalidArgument("schedule.max_run_count must not be negative")
	}
	if sc.EndTime != nil && sc.StartTime != nil && !sc.EndTime.AsTime().After(sc.StartTime.AsTime()) {
		return apierror.InvalidArgument("schedule.end_time must be after start_time")
	}
	switch r := sc.GetRequest().(type) {
	case nil:
		return apierror.InvalidArgument("schedule.request is required: create_notebook_execution_job_request")
	case *aiplatformpb.Schedule_CreatePipelineJobRequest:
		return apierror.Unimplemented("schedule.create_pipeline_job_request: Vertex AI Pipelines is not served on this instance; schedule a notebook execution (create_notebook_execution_job_request)")
	case *aiplatformpb.Schedule_CreateNotebookExecutionJobRequest:
		req := r.CreateNotebookExecutionJobRequest
		if req.GetParent() != parent {
			return apierror.InvalidArgument("schedule.create_notebook_execution_job_request.parent %q must be %s", req.GetParent(), parent)
		}
		if req.GetNotebookExecutionJobId() != "" {
			return apierror.InvalidArgument("schedule.create_notebook_execution_job_request.notebook_execution_job_id: User-specified ID is not supported in the request template")
		}
		if req.GetNotebookExecutionJob() == nil {
			return apierror.InvalidArgument("schedule.create_notebook_execution_job_request.notebook_execution_job is required")
		}
		j := proto.Clone(req.GetNotebookExecutionJob()).(*aiplatformpb.NotebookExecutionJob)
		if err := st.checkJob("schedule.create_notebook_execution_job_request.notebook_execution_job.", parent, j); err != nil {
			return err
		}
	}
	return nil
}

// nextRun is sc's next run time after t, nil when it has none.
func nextRun(sc *aiplatformpb.Schedule, after time.Time) *timestamppb.Timestamp {
	c, err := parseCron(sc.GetCron())
	if err != nil {
		return nil
	}
	if s := sc.GetStartTime(); s != nil && s.AsTime().After(after) {
		after = s.AsTime().Add(-time.Second)
	}
	n := c.Next(after)
	if n.IsZero() || (sc.EndTime != nil && n.After(sc.EndTime.AsTime())) {
		return nil
	}
	return timestamppb.New(n)
}

func (x *scheduleService) CreateSchedule(_ context.Context, req *aiplatformpb.CreateScheduleRequest) (*aiplatformpb.Schedule, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	if req.GetSchedule() == nil {
		return nil, grpcError(apierror.InvalidArgument("schedule is required"))
	}
	sc := proto.Clone(req.GetSchedule()).(*aiplatformpb.Schedule)
	st := x.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.checkSchedule(req.GetParent(), sc); err != nil {
		return nil, grpcError(err)
	}
	now := st.now()
	sc.Name = req.GetParent() + "/schedules/" + numericID()
	sc.CreateTime, sc.UpdateTime = timestamppb.New(now), timestamppb.New(now)
	if sc.StartTime == nil {
		sc.StartTime = sc.CreateTime
	}
	sc.State, sc.StartedRunCount, sc.LastPauseTime, sc.LastResumeTime, sc.LastScheduledRunResponse, sc.CatchUp =
		aiplatformpb.Schedule_ACTIVE, 0, nil, nil, nil, false
	sc.NextRunTime = nextRun(sc, now)
	if sc.NextRunTime == nil {
		sc.State = aiplatformpb.Schedule_COMPLETED
	}
	st.schedules[sc.Name] = sc
	st.save()
	return proto.Clone(sc).(*aiplatformpb.Schedule), nil
}

func (st *notebookStore) schedule(name string) (*aiplatformpb.Schedule, error) {
	if _, _, _, err := parseChild("name", "schedules", name); err != nil {
		return nil, err
	}
	sc, ok := st.schedules[name]
	if !ok {
		return nil, apierror.NotFound("Schedule %s not found", name)
	}
	return sc, nil
}

func (x *scheduleService) GetSchedule(_ context.Context, req *aiplatformpb.GetScheduleRequest) (*aiplatformpb.Schedule, error) {
	st := x.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	sc, err := st.schedule(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	return proto.Clone(sc).(*aiplatformpb.Schedule), nil
}

func (x *scheduleService) ListSchedules(_ context.Context, req *aiplatformpb.ListSchedulesRequest) (*aiplatformpb.ListSchedulesResponse, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	st := x.s.notebooks()
	st.mu.Lock()
	var all []*aiplatformpb.Schedule
	for _, k := range sortedNames(st.schedules) {
		if strings.HasPrefix(k, req.GetParent()+"/") {
			all = append(all, proto.Clone(st.schedules[k]).(*aiplatformpb.Schedule))
		}
	}
	st.mu.Unlock()
	all, err := aipfilter.Apply(req.GetFilter(), all, scheduleFilter...)
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
	size := int(req.GetPageSize())
	if size <= 0 {
		size = 100
	}
	items, next, err := aipfilter.Page("schedules:"+req.GetParent()+"|"+req.GetFilter()+"|"+order, all, req.GetPageToken(), size)
	if err != nil {
		return nil, grpcError(err)
	}
	return &aiplatformpb.ListSchedulesResponse{Schedules: items, NextPageToken: next}, nil
}

func (x *scheduleService) DeleteSchedule(_ context.Context, req *aiplatformpb.DeleteScheduleRequest) (*longrunningpb.Operation, error) {
	st := x.s.notebooks()
	st.mu.Lock()
	if _, err := st.schedule(req.GetName()); err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	delete(st.schedules, req.GetName())
	st.save()
	st.mu.Unlock()
	return x.s.finished(req.GetName(), &aiplatformpb.DeleteOperationMetadata{GenericMetadata: generic()}, &emptypb.Empty{}, nil)
}

func (x *scheduleService) PauseSchedule(_ context.Context, req *aiplatformpb.PauseScheduleRequest) (*emptypb.Empty, error) {
	st := x.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	sc, err := st.schedule(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	switch sc.GetState() {
	case aiplatformpb.Schedule_COMPLETED:
		return nil, grpcError(apierror.FailedPrecondition("Schedule %s is COMPLETED: a completed schedule cannot be paused or resumed", req.GetName()))
	case aiplatformpb.Schedule_ACTIVE:
		now := timestamppb.New(st.now())
		sc.State, sc.LastPauseTime, sc.UpdateTime = aiplatformpb.Schedule_PAUSED, now, now
		st.save()
	}
	return &emptypb.Empty{}, nil
}

func (x *scheduleService) ResumeSchedule(_ context.Context, req *aiplatformpb.ResumeScheduleRequest) (*emptypb.Empty, error) {
	st := x.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	sc, err := st.schedule(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	switch sc.GetState() {
	case aiplatformpb.Schedule_COMPLETED:
		return nil, grpcError(apierror.FailedPrecondition("Schedule %s is COMPLETED: a completed schedule cannot be paused or resumed", req.GetName()))
	case aiplatformpb.Schedule_PAUSED:
		now := st.now()
		sc.State, sc.LastResumeTime, sc.UpdateTime, sc.CatchUp = aiplatformpb.Schedule_ACTIVE, timestamppb.New(now), timestamppb.New(now), req.GetCatchUp()
		from := now
		if req.GetCatchUp() && sc.GetLastPauseTime() != nil && sc.NextRunTime != nil && sc.NextRunTime.AsTime().Before(now) {
			// Backfill: the runs missed while paused are started, from
			// the first one due.
			from = sc.NextRunTime.AsTime().Add(-time.Second)
		}
		sc.NextRunTime = nextRun(sc, from)
		st.complete(sc)
		st.save()
	}
	return &emptypb.Empty{}, nil
}

func (x *scheduleService) UpdateSchedule(_ context.Context, req *aiplatformpb.UpdateScheduleRequest) (*aiplatformpb.Schedule, error) {
	in := req.GetSchedule()
	if in == nil {
		return nil, grpcError(apierror.InvalidArgument("schedule is required"))
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, grpcError(apierror.InvalidArgument("update_mask is required"))
	}
	st := x.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	cur, err := st.schedule(in.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	next := proto.Clone(cur).(*aiplatformpb.Schedule)
	for _, p := range paths {
		p = camelToSnake(p)
		if !scheduleMasks[p] {
			return nil, grpcError(apierror.InvalidArgument("update_mask path %q cannot be updated: the updatable fields are display_name, cron, start_time, end_time, max_run_count, max_concurrent_run_count, allow_queueing and create_notebook_execution_job_request", p))
		}
		switch p {
		case "display_name":
			next.DisplayName = in.GetDisplayName()
		case "cron":
			next.TimeSpecification = in.GetTimeSpecification()
		case "start_time":
			next.StartTime = in.GetStartTime()
		case "end_time":
			next.EndTime = in.GetEndTime()
		case "max_run_count":
			next.MaxRunCount = in.GetMaxRunCount()
		case "max_concurrent_run_count":
			next.MaxConcurrentRunCount = in.GetMaxConcurrentRunCount()
		case "allow_queueing":
			next.AllowQueueing = in.GetAllowQueueing()
		case "create_notebook_execution_job_request":
			if _, ok := in.GetRequest().(*aiplatformpb.Schedule_CreateNotebookExecutionJobRequest); !ok {
				return nil, grpcError(apierror.InvalidArgument("the scheduled request type cannot be changed"))
			}
			next.Request = in.GetRequest()
		}
	}
	if next.StartTime == nil {
		next.StartTime = cur.StartTime
	}
	parent := cur.GetName()[:strings.Index(cur.GetName(), "/schedules/")]
	if err := st.checkSchedule(parent, next); err != nil {
		return nil, grpcError(err)
	}
	now := st.now()
	next.UpdateTime = timestamppb.New(now)
	if next.GetState() != aiplatformpb.Schedule_COMPLETED {
		next.NextRunTime = nextRun(next, now)
	}
	st.complete(next)
	st.schedules[next.Name] = next
	st.save()
	return proto.Clone(next).(*aiplatformpb.Schedule), nil
}

// complete marks sc COMPLETED when it has no runs left; the caller holds
// st.mu.
func (st *notebookStore) complete(sc *aiplatformpb.Schedule) {
	if sc.GetState() == aiplatformpb.Schedule_COMPLETED {
		return
	}
	done := sc.GetMaxRunCount() > 0 && sc.GetStartedRunCount() >= sc.GetMaxRunCount()
	if sc.EndTime != nil && !st.now().Before(sc.EndTime.AsTime()) {
		done = true
	}
	if sc.GetState() == aiplatformpb.Schedule_ACTIVE && sc.NextRunTime == nil {
		done = true
	}
	if done {
		sc.State, sc.NextRunTime = aiplatformpb.Schedule_COMPLETED, nil
	}
}

// activeRuns are sc's jobs not yet ended; the caller holds st.mu.
func (st *notebookStore) activeRuns(name string) int64 {
	var n int64
	for _, e := range st.jobs {
		if e.Job.GetScheduleResourceName() != name {
			continue
		}
		switch e.Job.GetJobState() {
		case aiplatformpb.JobState_JOB_STATE_PENDING, aiplatformpb.JobState_JOB_STATE_QUEUED, aiplatformpb.JobState_JOB_STATE_RUNNING:
			n++
		}
	}
	return n
}

// startScheduler runs due schedules every scheduleTick until the server
// stops.
func (s *Server) startScheduler() {
	s.mu.Lock()
	if s.schedStop != nil {
		s.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	s.schedStop = stop
	s.mu.Unlock()
	go func() {
		t := time.NewTicker(scheduleTick)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.runDueSchedules()
			}
		}
	}()
}

// stopScheduler ends startScheduler's loop.
func (s *Server) stopScheduler() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schedStop != nil {
		close(s.schedStop)
		s.schedStop = nil
	}
}

// runDueSchedules starts the runs that are due.
func (s *Server) runDueSchedules() {
	st := s.notebooks()
	var runs []dueRun
	st.mu.Lock()
	now := st.now()
	for _, k := range sortedNames(st.schedules) {
		sc := st.schedules[k]
		st.complete(sc)
		if sc.GetState() != aiplatformpb.Schedule_ACTIVE || sc.NextRunTime == nil || sc.NextRunTime.AsTime().After(now) {
			continue
		}
		req := sc.GetCreateNotebookExecutionJobRequest()
		at := sc.NextRunTime.AsTime()
		if st.activeRuns(k)+int64(countDue(runs, k)) >= sc.GetMaxConcurrentRunCount() {
			if sc.GetAllowQueueing() {
				continue // queued: it starts once a run ends
			}
			sc.NextRunTime = nextRun(sc, now)
			st.complete(sc)
			st.save()
			continue
		}
		sc.StartedRunCount++
		sc.NextRunTime = nextRun(sc, at)
		if !sc.GetCatchUp() && sc.NextRunTime != nil && sc.NextRunTime.AsTime().Before(now) {
			sc.NextRunTime = nextRun(sc, now)
		}
		st.complete(sc)
		st.save()
		runs = append(runs, dueRun{name: k, parent: req.GetParent(), job: proto.Clone(req.GetNotebookExecutionJob()).(*aiplatformpb.NotebookExecutionJob), at: at})
	}
	st.mu.Unlock()
	for _, r := range runs {
		if r.job.GetDisplayName() == "" {
			st.mu.Lock()
			if sc := st.schedules[r.name]; sc != nil {
				r.job.DisplayName = sc.GetDisplayName()
			}
			st.mu.Unlock()
		}
		j, err := s.createJob(r.parent, "", r.job, r.name)
		resp := ""
		if err != nil {
			resp = err.Error()
		} else {
			resp = j.GetName()
		}
		st.mu.Lock()
		if sc := st.schedules[r.name]; sc != nil {
			sc.LastScheduledRunResponse = &aiplatformpb.Schedule_RunResponse{ScheduledRunTime: timestamppb.New(r.at), RunResponse: resp}
			st.save()
		}
		st.mu.Unlock()
	}
}

// dueRun is a schedule's run to start.
type dueRun struct {
	name, parent string
	job          *aiplatformpb.NotebookExecutionJob
	at           time.Time
}

func countDue(runs []dueRun, name string) int {
	n := 0
	for _, r := range runs {
		if r.name == name {
			n++
		}
	}
	return n
}

// loadJobs restores the saved executions, schedules and policies: an
// execution running when the instance stopped failed with it.
func (st *notebookStore) loadJobs(jobs, schedules []json.RawMessage, policies map[string]*iampolicy.Stored) {
	for _, raw := range jobs {
		j := &aiplatformpb.NotebookExecutionJob{}
		if lenient.Unmarshal(raw, j) != nil || j.GetName() == "" {
			continue
		}
		switch j.GetJobState() {
		case aiplatformpb.JobState_JOB_STATE_PENDING, aiplatformpb.JobState_JOB_STATE_QUEUED, aiplatformpb.JobState_JOB_STATE_RUNNING:
			j.JobState = aiplatformpb.JobState_JOB_STATE_FAILED
			j.Status = &status.Status{Code: int32(codes.Aborted), Message: "the instance stopped while the execution ran"}
		}
		st.jobs[j.GetName()] = &jobEntry{Job: j}
	}
	now := st.now()
	for _, raw := range schedules {
		sc := &aiplatformpb.Schedule{}
		if lenient.Unmarshal(raw, sc) != nil || sc.GetName() == "" {
			continue
		}
		if sc.GetState() == aiplatformpb.Schedule_ACTIVE {
			// The runs missed while the instance was stopped are not
			// started.
			sc.NextRunTime = nextRun(sc, now)
		}
		st.schedules[sc.GetName()] = sc
	}
	for k, v := range policies {
		st.policies[k] = v
	}
}
