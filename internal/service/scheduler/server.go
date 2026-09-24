package scheduler

import (
	"context"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // IANA zones on any host, including ones without a zoneinfo database

	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"github.com/robfig/cron/v3"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/sched"
)

// GRPCServer serves google.cloud.scheduler.v1.CloudScheduler.
type GRPCServer struct {
	schedulerpb.UnimplementedCloudSchedulerServer
	store  *Store
	runner *Runner
	clock  sched.Clock
}

// NewGRPCServer returns the API over a store; RunJob hands jobs to runner.
func NewGRPCServer(st *Store, runner *Runner, clock sched.Clock) *GRPCServer {
	if clock == nil {
		clock = sched.RealClock{}
	}
	return &GRPCServer{store: st, runner: runner, clock: clock}
}

// Register adds the service to a gRPC server.
func (g *GRPCServer) Register(s *grpc.Server) { schedulerpb.RegisterCloudSchedulerServer(s, g) }

var (
	parentRE = regexp.MustCompile(`^projects/[a-z][a-z0-9-]{4,28}[a-z0-9]/locations/[a-z0-9-]+$`)
	jobIDRE  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,500}$`)
)

// cronParser accepts the unix-cron format Cloud Scheduler documents: five
// fields, with ranges, steps and names.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// nextRun is the first time after `after` that the schedule fires, in the
// job's zone.
func nextRun(schedule, zone string, after time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return time.Time{}, apierror.InvalidArgument("time_zone %q is not an IANA time zone", zone)
	}
	s, err := cronParser.Parse(schedule)
	if err != nil {
		return time.Time{}, apierror.InvalidArgument("schedule %q is not a unix-cron expression: %v", schedule, err)
	}
	return s.Next(after.In(loc)).UTC(), nil
}

func parseJobName(name string) error {
	i := strings.LastIndex(name, "/jobs/")
	if i < 0 || !parentRE.MatchString(name[:i]) || !jobIDRE.MatchString(name[i+len("/jobs/"):]) {
		return apierror.InvalidArgument("job name %q must be projects/{project}/locations/{location}/jobs/{job}", name)
	}
	return nil
}

// fromProto validates and converts a job, refusing what is not mapped.
func fromProto(p *schedulerpb.Job) (Job, error) {
	j := Job{
		Name: p.GetName(), Description: p.GetDescription(), Schedule: p.GetSchedule(),
		TimeZone: p.GetTimeZone(), State: StateEnabled, Retry: DefaultRetryConfig(),
		AttemptDeadline: 3 * time.Minute,
	}
	if j.TimeZone == "" {
		j.TimeZone = "Etc/UTC"
	}
	if j.Schedule == "" {
		return Job{}, apierror.InvalidArgument("schedule is required")
	}
	switch t := p.GetTarget().(type) {
	case *schedulerpb.Job_HttpTarget:
		h := t.HttpTarget
		if h.GetOauthToken() != nil || h.GetOidcToken() != nil {
			return Job{}, apierror.Unimplemented(
				"http_target OAuth and OIDC tokens are not implemented: CloudBurrow mints no tokens, " +
					"and a target that checks one would receive nothing it could verify")
		}
		if !strings.HasPrefix(h.GetUri(), "http://") && !strings.HasPrefix(h.GetUri(), "https://") {
			return Job{}, apierror.InvalidArgument("http_target.uri %q must be an http or https URL", h.GetUri())
		}
		method := h.GetHttpMethod().String()
		if h.GetHttpMethod() == schedulerpb.HttpMethod_HTTP_METHOD_UNSPECIFIED {
			method = "POST"
		}
		j.HTTP = &HTTPTarget{URI: h.GetUri(), Method: method, Headers: h.GetHeaders(), Body: h.GetBody()}
	case *schedulerpb.Job_PubsubTarget:
		topic := t.PubsubTarget.GetTopicName()
		if !strings.HasPrefix(topic, "projects/") || !strings.Contains(topic, "/topics/") {
			return Job{}, apierror.InvalidArgument("pubsub_target.topic_name %q must be projects/{project}/topics/{topic}", topic)
		}
		if len(t.PubsubTarget.GetData()) == 0 && len(t.PubsubTarget.GetAttributes()) == 0 {
			return Job{}, apierror.InvalidArgument("a Pub/Sub target needs data or at least one attribute")
		}
		j.PubSub = &PubSubTarget{Topic: topic, Data: t.PubsubTarget.GetData(), Attributes: t.PubsubTarget.GetAttributes()}
	case *schedulerpb.Job_AppEngineHttpTarget:
		return Job{}, apierror.Unimplemented("app_engine_http_target is not implemented: there is no App Engine locally")
	default:
		return Job{}, apierror.InvalidArgument("a job needs a target: http_target or pubsub_target")
	}
	if rc := p.GetRetryConfig(); rc != nil {
		if rc.GetRetryCount() < 0 || rc.GetRetryCount() > 5 {
			return Job{}, apierror.InvalidArgument("retry_config.retry_count must be 0 to 5")
		}
		j.Retry.RetryCount = int(rc.GetRetryCount())
		if d := rc.GetMaxRetryDuration(); d != nil {
			j.Retry.MaxRetryDuration = d.AsDuration()
		}
		if d := rc.GetMinBackoffDuration(); d != nil && d.AsDuration() > 0 {
			j.Retry.MinBackoff = d.AsDuration()
		}
		if d := rc.GetMaxBackoffDuration(); d != nil && d.AsDuration() > 0 {
			j.Retry.MaxBackoff = d.AsDuration()
		}
		if rc.GetMaxDoublings() > 0 {
			j.Retry.MaxDoublings = int(rc.GetMaxDoublings())
		}
	}
	if d := p.GetAttemptDeadline(); d != nil && d.AsDuration() > 0 {
		if d.AsDuration() < 15*time.Second || d.AsDuration() > 30*time.Minute {
			return Job{}, apierror.InvalidArgument("attempt_deadline must be between 15 seconds and 30 minutes")
		}
		j.AttemptDeadline = d.AsDuration()
	}
	return j, nil
}

func toProto(j Job) *schedulerpb.Job {
	out := &schedulerpb.Job{
		Name: j.Name, Description: j.Description, Schedule: j.Schedule, TimeZone: j.TimeZone,
		UserUpdateTime:  timestamppb.New(j.UserUpdateTime),
		ScheduleTime:    timestamppb.New(j.ScheduleTime),
		AttemptDeadline: durationpb.New(j.AttemptDeadline),
		RetryConfig: &schedulerpb.RetryConfig{
			RetryCount:         int32(j.Retry.RetryCount),
			MaxRetryDuration:   durationpb.New(j.Retry.MaxRetryDuration),
			MinBackoffDuration: durationpb.New(j.Retry.MinBackoff),
			MaxBackoffDuration: durationpb.New(j.Retry.MaxBackoff),
			MaxDoublings:       int32(j.Retry.MaxDoublings),
		},
		State: schedulerpb.Job_ENABLED,
	}
	if j.State == StatePaused {
		out.State = schedulerpb.Job_PAUSED
	}
	if !j.LastAttemptTime.IsZero() {
		out.LastAttemptTime = timestamppb.New(j.LastAttemptTime)
		out.Status = &rpcstatus.Status{Code: j.LastCode, Message: j.LastMessage}
	}
	switch {
	case j.HTTP != nil:
		m := schedulerpb.HttpMethod(schedulerpb.HttpMethod_value[j.HTTP.Method])
		out.Target = &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{
			Uri: j.HTTP.URI, HttpMethod: m, Headers: j.HTTP.Headers, Body: j.HTTP.Body}}
	case j.PubSub != nil:
		out.Target = &schedulerpb.Job_PubsubTarget{PubsubTarget: &schedulerpb.PubsubTarget{
			TopicName: j.PubSub.Topic, Data: j.PubSub.Data, Attributes: j.PubSub.Attributes}}
	}
	return out
}

func (g *GRPCServer) CreateJob(_ context.Context, req *schedulerpb.CreateJobRequest) (*schedulerpb.Job, error) {
	if !parentRE.MatchString(req.GetParent()) {
		return nil, apierror.Wrap(apierror.InvalidArgument("parent %q must be projects/{project}/locations/{location}", req.GetParent()))
	}
	in := req.GetJob()
	if in == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("job is required"))
	}
	if in.GetName() == "" {
		return nil, apierror.Wrap(apierror.InvalidArgument("job.name is required"))
	}
	if err := parseJobName(in.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	if !strings.HasPrefix(in.GetName(), req.GetParent()+"/jobs/") {
		return nil, apierror.Wrap(apierror.InvalidArgument("job %s is not under %s", in.GetName(), req.GetParent()))
	}
	j, err := fromProto(in)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	now := g.clock.Now().UTC()
	j.UserUpdateTime = now
	if j.ScheduleTime, err = nextRun(j.Schedule, j.TimeZone, now); err != nil {
		return nil, apierror.Wrap(err)
	}
	created, err := g.store.Create(j)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return toProto(created), nil
}

func (g *GRPCServer) GetJob(_ context.Context, req *schedulerpb.GetJobRequest) (*schedulerpb.Job, error) {
	j, err := g.store.Get(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return toProto(j), nil
}

func (g *GRPCServer) ListJobs(_ context.Context, req *schedulerpb.ListJobsRequest) (*schedulerpb.ListJobsResponse, error) {
	if !parentRE.MatchString(req.GetParent()) {
		return nil, apierror.Wrap(apierror.InvalidArgument("parent %q must be projects/{project}/locations/{location}", req.GetParent()))
	}
	jobs, err := g.store.List(req.GetParent() + "/jobs/")
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	byName := map[string]Job{}
	names := make([]string, 0, len(jobs))
	for _, j := range jobs {
		byName[j.Name] = j
		names = append(names, j.Name)
	}
	page, next, err := paging.Page("scheduler:"+req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("%v", err))
	}
	resp := &schedulerpb.ListJobsResponse{NextPageToken: next}
	for _, n := range page {
		resp.Jobs = append(resp.Jobs, toProto(byName[n]))
	}
	return resp, nil
}

// UpdateJob replaces the fields its mask names; an empty mask replaces every
// settable field, as the API does.
func (g *GRPCServer) UpdateJob(_ context.Context, req *schedulerpb.UpdateJobRequest) (*schedulerpb.Job, error) {
	in := req.GetJob()
	if in == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("job is required"))
	}
	now := g.clock.Now().UTC()
	updated, err := g.store.Update(in.GetName(), func(cur *Job) error {
		// The incoming job over the current one, field by field per mask.
		merged := toProto(*cur)
		paths := req.GetUpdateMask().GetPaths()
		if len(paths) == 0 {
			paths = []string{"description", "schedule", "time_zone", "target", "retry_config", "attempt_deadline"}
		}
		for _, p := range paths {
			switch {
			case p == "description":
				merged.Description = in.GetDescription()
			case p == "schedule":
				merged.Schedule = in.GetSchedule()
			case p == "time_zone":
				merged.TimeZone = in.GetTimeZone()
			case p == "target" || strings.HasPrefix(p, "http_target") || strings.HasPrefix(p, "pubsub_target") ||
				strings.HasPrefix(p, "app_engine_http_target"):
				merged.Target = in.GetTarget()
			case p == "retry_config" || strings.HasPrefix(p, "retry_config."):
				merged.RetryConfig = in.GetRetryConfig()
			case p == "attempt_deadline":
				merged.AttemptDeadline = in.GetAttemptDeadline()
			default:
				return apierror.InvalidArgument("update_mask path %q cannot be updated", p)
			}
		}
		next, err := fromProto(merged)
		if err != nil {
			return err
		}
		next.State, next.LastAttemptTime, next.LastCode, next.LastMessage = cur.State, cur.LastAttemptTime, cur.LastCode, cur.LastMessage
		next.UserUpdateTime = now
		if next.ScheduleTime, err = nextRun(next.Schedule, next.TimeZone, now); err != nil {
			return err
		}
		*cur = next
		return nil
	})
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return toProto(updated), nil
}

func (g *GRPCServer) DeleteJob(_ context.Context, req *schedulerpb.DeleteJobRequest) (*emptypb.Empty, error) {
	if err := g.store.Delete(req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

func (g *GRPCServer) PauseJob(_ context.Context, req *schedulerpb.PauseJobRequest) (*schedulerpb.Job, error) {
	j, err := g.store.Update(req.GetName(), func(j *Job) error {
		j.State = StatePaused
		return nil
	})
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return toProto(j), nil
}

// ResumeJob re-enables a job from now: runs it missed while paused are not
// made up, as the service does not make them up.
func (g *GRPCServer) ResumeJob(_ context.Context, req *schedulerpb.ResumeJobRequest) (*schedulerpb.Job, error) {
	now := g.clock.Now().UTC()
	j, err := g.store.Update(req.GetName(), func(j *Job) error {
		if j.State == StateEnabled {
			return apierror.FailedPrecondition("job %s is not paused", j.Name)
		}
		next, err := nextRun(j.Schedule, j.TimeZone, now)
		if err != nil {
			return err
		}
		j.State, j.ScheduleTime = StateEnabled, next
		return nil
	})
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return toProto(j), nil
}

// RunJob runs a job now, whatever its schedule and whether it is paused,
// as the API does. The run is started before this returns.
func (g *GRPCServer) RunJob(_ context.Context, req *schedulerpb.RunJobRequest) (*schedulerpb.Job, error) {
	j, err := g.store.Get(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if g.runner == nil {
		return nil, apierror.Wrap(apierror.FailedPrecondition("the scheduler is not running"))
	}
	g.runner.fire(j, g.clock.Now().UTC())
	return toProto(j), nil
}
