package tasks

import (
	"context"
	"fmt"
	"strings"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
)

// GRPCServer serves google.cloud.tasks.v2 over gRPC.
//
// Embedding UnimplementedCloudTasksServer means an unimplemented method
// returns Unimplemented rather than failing to compile — and, crucially,
// rather than us writing a stub that returns a plausible empty success.
type GRPCServer struct {
	taskspb.UnimplementedCloudTasksServer
	store *Store
}

// NewGRPCServer returns a Cloud Tasks gRPC service.
func NewGRPCServer(s *Store) *GRPCServer { return &GRPCServer{store: s} }

// Register adds this service to a gRPC server, or to the JSON transcoder.
func (g *GRPCServer) Register(s grpc.ServiceRegistrar) { taskspb.RegisterCloudTasksServer(s, g) }

// --- conversions between the contract types and our model ---

func toProtoQueue(q Queue) *taskspb.Queue {
	state := taskspb.Queue_RUNNING
	switch q.State {
	case StatePaused:
		state = taskspb.Queue_PAUSED
	case StateDisabled:
		state = taskspb.Queue_DISABLED
	}
	return &taskspb.Queue{
		Name:  q.Name,
		State: state,
		RateLimits: &taskspb.RateLimits{
			MaxDispatchesPerSecond:  q.RateLimits.MaxDispatchesPerSecond,
			MaxConcurrentDispatches: int32(q.RateLimits.MaxConcurrentDispatches),
		},
		RetryConfig: &taskspb.RetryConfig{
			MaxAttempts:  int32(q.RetryConfig.MaxAttempts),
			MinBackoff:   durationpb.New(q.RetryConfig.MinBackoff),
			MaxBackoff:   durationpb.New(q.RetryConfig.MaxBackoff),
			MaxDoublings: int32(q.RetryConfig.MaxDoublings),
			MaxRetryDuration: func() *durationpb.Duration {
				if q.RetryConfig.MaxRetryDuration > 0 {
					return durationpb.New(q.RetryConfig.MaxRetryDuration)
				}
				return nil
			}(),
		},
		PurgeTime: func() *timestamppb.Timestamp {
			if q.Purged.IsZero() {
				return nil
			}
			return timestamppb.New(q.Purged)
		}(),
	}
}

// checkQueue refuses queue fields this server would not honour (#578), for
// CreateQueue and, field by field as its mask names them, UpdateQueue.
func checkQueue(p *taskspb.Queue) error {
	if err := checkLogging(p); err != nil {
		return err
	}
	if err := checkAppEngineRouting(p); err != nil {
		return err
	}
	return checkRetry(p)
}

func checkLogging(p *taskspb.Queue) error {
	if p.GetStackdriverLoggingConfig() != nil {
		return apierror.Unimplemented("queue.stackdriver_logging_config is not implemented: no dispatch is written to " +
			"Cloud Logging; attempts are in the console's operations ledger and /admin/events")
	}
	return nil
}

func checkAppEngineRouting(p *taskspb.Queue) error {
	if proto.Size(p.GetAppEngineRoutingOverride()) > 0 {
		return apierror.Unimplemented("queue.app_engine_routing_override is not implemented: App Engine targets are not supported")
	}
	return nil
}

func checkRetry(p *taskspb.Queue) error {
	if d := p.GetRetryConfig().GetMaxRetryDuration(); d != nil && d.AsDuration() < 0 {
		return apierror.InvalidArgument("retry_config.max_retry_duration must not be negative")
	}
	return nil
}

func fromProtoQueue(p *taskspb.Queue) Queue {
	q := Queue{Name: p.GetName(), State: StateRunning}
	switch p.GetState() {
	case taskspb.Queue_PAUSED:
		q.State = StatePaused
	case taskspb.Queue_DISABLED:
		q.State = StateDisabled
	}
	if rl := p.GetRateLimits(); rl != nil {
		q.RateLimits = RateLimits{
			MaxDispatchesPerSecond:  rl.GetMaxDispatchesPerSecond(),
			MaxConcurrentDispatches: int(rl.GetMaxConcurrentDispatches()),
		}
	}
	if rc := p.GetRetryConfig(); rc != nil {
		q.RetryConfig = RetryConfig{
			MaxAttempts:      int(rc.GetMaxAttempts()),
			MinBackoff:       rc.GetMinBackoff().AsDuration(),
			MaxBackoff:       rc.GetMaxBackoff().AsDuration(),
			MaxDoublings:     int(rc.GetMaxDoublings()),
			MaxRetryDuration: rc.GetMaxRetryDuration().AsDuration(),
		}
	}
	return q
}

func toProtoTask(t Task) *taskspb.Task {
	pt := &taskspb.Task{
		Name:          t.Name,
		ScheduleTime:  timestamppb.New(t.ScheduleTime),
		CreateTime:    timestamppb.New(t.Created),
		DispatchCount: int32(t.DispatchCount),
		ResponseCount: int32(t.ResponseCount),
	}
	if t.DispatchDeadline > 0 {
		pt.DispatchDeadline = durationpb.New(t.DispatchDeadline)
	}
	if t.HTTPRequest != nil {
		method := taskspb.HttpMethod_POST
		if m, ok := taskspb.HttpMethod_value[strings.ToUpper(t.HTTPRequest.Method)]; ok {
			method = taskspb.HttpMethod(m)
		}
		pt.MessageType = &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{
			Url:        t.HTTPRequest.URL,
			HttpMethod: method,
			Headers:    t.HTTPRequest.Headers,
			Body:       t.HTTPRequest.Body,
		}}
	}
	return pt
}

func fromProtoTask(p *taskspb.Task, parent string) (Task, error) {
	t := Task{Name: p.GetName(), Queue: parent}
	if p.GetScheduleTime() != nil {
		t.ScheduleTime = p.GetScheduleTime().AsTime()
	}
	hr := p.GetHttpRequest()
	if hr == nil {
		// App Engine targets are out of scope. Saying so is better than
		// accepting a task that would never be dispatched.
		if p.GetAppEngineHttpRequest() != nil {
			return Task{}, apierror.Unimplemented("appEngineHttpRequest is not supported; use httpRequest")
		}
		return Task{}, apierror.InvalidArgument("task requires an httpRequest")
	}
	// The Authorization header Cloud Tasks would mint. Refused rather than
	// dispatched without it, which a target checking the token would reject
	// in a way that points nowhere; Cloud Scheduler refuses the same (#578).
	if hr.GetOidcToken() != nil {
		return Task{}, apierror.Unimplemented("httpRequest.oidcToken is not implemented: the task would be dispatched " +
			"without an Authorization header; set the header yourself in httpRequest.headers")
	}
	if hr.GetOauthToken() != nil {
		return Task{}, apierror.Unimplemented("httpRequest.oauthToken is not implemented: the task would be dispatched " +
			"without an Authorization header; set the header yourself in httpRequest.headers")
	}
	switch hr.GetHttpMethod() {
	case taskspb.HttpMethod_GET, taskspb.HttpMethod_HEAD, taskspb.HttpMethod_DELETE, taskspb.HttpMethod_OPTIONS:
		if len(hr.GetBody()) > 0 {
			return Task{}, apierror.InvalidArgument("httpRequest.body must be empty for an %s task", hr.GetHttpMethod())
		}
	}
	if dd := p.GetDispatchDeadline(); dd != nil {
		d := dd.AsDuration()
		if d < 15*time.Second || d > 30*time.Minute {
			return Task{}, apierror.InvalidArgument("dispatch_deadline %s must be between 15s and 30m", d)
		}
		t.DispatchDeadline = d
	}
	t.HTTPRequest = &HTTPRequest{
		URL:     hr.GetUrl(),
		Method:  hr.GetHttpMethod().String(),
		Headers: hr.GetHeaders(),
		Body:    hr.GetBody(),
	}
	return t, nil
}

// generateTaskName builds a name when the caller does not supply one, which
// the contract permits.
func generateTaskName(parent string) string {
	return fmt.Sprintf("%s/tasks/%d", parent, time.Now().UnixNano())
}

// --- queue methods ---

func (g *GRPCServer) CreateQueue(_ context.Context, req *taskspb.CreateQueueRequest) (*taskspb.Queue, error) {
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	if err := checkQueue(req.GetQueue()); err != nil {
		return nil, err
	}
	q := fromProtoQueue(req.GetQueue())
	if q.Name == "" {
		return nil, apierror.InvalidArgument("queue.name is required")
	}
	if !strings.HasPrefix(q.Name, req.GetParent()+"/queues/") {
		return nil, apierror.InvalidArgument("queue.name %q is not under parent %q", q.Name, req.GetParent())
	}
	created, err := g.store.CreateQueue(q)
	if err != nil {
		return nil, err
	}
	return toProtoQueue(created), nil
}

// updatableQueuePaths are the fields an empty update_mask replaces: every
// Queue field that is neither the name nor output only. A field left out of
// the request is cleared, then defaulted as on create.
var updatableQueuePaths = []string{"app_engine_routing_override", "rate_limits", "retry_config", "stackdriver_logging_config"}

// UpdateQueue replaces the fields its mask names, or with an empty mask
// every settable field, as the API documents; it creates a queue that does
// not exist (#692). A path naming the name or an output-only field (state,
// purge_time, rate_limits.max_burst_size) or no field at all is
// INVALID_ARGUMENT naming it. Fields CreateQueue refuses are refused here
// when the mask names them. The dispatcher reads the stored queue on every
// pass and retry, so the new configuration applies to tasks already queued.
func (g *GRPCServer) UpdateQueue(_ context.Context, req *taskspb.UpdateQueueRequest) (*taskspb.Queue, error) {
	in := req.GetQueue()
	if in.GetName() == "" {
		return nil, apierror.InvalidArgument("queue.name is required")
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = updatableQueuePaths
	}
	var sets []func(*RetryConfig, *RateLimits)
	want := fromProtoQueue(in)
	for _, p := range paths {
		field, sub, _ := strings.Cut(p, ".")
		var set func(*RetryConfig, *RateLimits)
		var err error
		switch field {
		case "rate_limits":
			set, err = rateLimitsSetter(sub, want.RateLimits)
		case "retry_config":
			if err = checkRetry(in); err == nil {
				set, err = retryConfigSetter(sub, want.RetryConfig)
			}
		case "stackdriver_logging_config":
			err = checkLogging(in)
		case "http_target":
			// Newer than the contract this server is built from, which has
			// no such field, so the value never reaches it: refused rather
			// than dropped. It would rewrite every task's URL, method and
			// headers and mint their tokens.
			err = apierror.Unimplemented("update_mask path %q is not implemented: each task is dispatched with its own "+
				"httpRequest; set the URL, method and headers there", p)
		case "app_engine_routing_override":
			err = checkAppEngineRouting(in)
		case "name":
			err = apierror.InvalidArgument("update_mask path %q cannot be updated: a queue's name cannot be changed", p)
		case "state":
			err = apierror.InvalidArgument("update_mask path %q names an output-only field: use PauseQueue or ResumeQueue", p)
		case "purge_time":
			err = apierror.InvalidArgument("update_mask path %q names an output-only field", p)
		default:
			err = apierror.InvalidArgument("update_mask path %q is not a field of Queue", p)
		}
		if err != nil {
			return nil, err
		}
		if set != nil {
			sets = append(sets, set)
		}
	}
	q, _, err := g.store.UpdateQueue(in.GetName(), func(rc *RetryConfig, rl *RateLimits) error {
		for _, set := range sets {
			set(rc, rl)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return toProtoQueue(q), nil
}

// rateLimitsSetter returns what an update_mask path under rate_limits sets:
// the whole message, or one field of it.
func rateLimitsSetter(sub string, want RateLimits) (func(*RetryConfig, *RateLimits), error) {
	switch sub {
	case "":
		return func(_ *RetryConfig, rl *RateLimits) { *rl = want }, nil
	case "max_dispatches_per_second":
		return func(_ *RetryConfig, rl *RateLimits) { rl.MaxDispatchesPerSecond = want.MaxDispatchesPerSecond }, nil
	case "max_concurrent_dispatches":
		return func(_ *RetryConfig, rl *RateLimits) { rl.MaxConcurrentDispatches = want.MaxConcurrentDispatches }, nil
	case "max_burst_size":
		return nil, apierror.InvalidArgument("update_mask path %q names an output-only field", "rate_limits."+sub)
	}
	return nil, apierror.InvalidArgument("update_mask path %q is not a field of Queue", "rate_limits."+sub)
}

// retryConfigSetter is rateLimitsSetter for retry_config.
func retryConfigSetter(sub string, want RetryConfig) (func(*RetryConfig, *RateLimits), error) {
	switch sub {
	case "":
		return func(rc *RetryConfig, _ *RateLimits) { *rc = want }, nil
	case "max_attempts":
		return func(rc *RetryConfig, _ *RateLimits) { rc.MaxAttempts = want.MaxAttempts }, nil
	case "max_retry_duration":
		return func(rc *RetryConfig, _ *RateLimits) { rc.MaxRetryDuration = want.MaxRetryDuration }, nil
	case "min_backoff":
		return func(rc *RetryConfig, _ *RateLimits) { rc.MinBackoff = want.MinBackoff }, nil
	case "max_backoff":
		return func(rc *RetryConfig, _ *RateLimits) { rc.MaxBackoff = want.MaxBackoff }, nil
	case "max_doublings":
		return func(rc *RetryConfig, _ *RateLimits) { rc.MaxDoublings = want.MaxDoublings }, nil
	}
	return nil, apierror.InvalidArgument("update_mask path %q is not a field of Queue", "retry_config."+sub)
}

func (g *GRPCServer) GetQueue(_ context.Context, req *taskspb.GetQueueRequest) (*taskspb.Queue, error) {
	q, err := g.store.GetQueue(req.GetName())
	if err != nil {
		return nil, err
	}
	return toProtoQueue(q), nil
}

func (g *GRPCServer) ListQueues(_ context.Context, req *taskspb.ListQueuesRequest) (*taskspb.ListQueuesResponse, error) {
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	// Ignoring it would return every queue and read as a filter that
	// matched them all (#578).
	if f := strings.TrimSpace(req.GetFilter()); f != "" {
		return nil, apierror.Unimplemented("ListQueues filter %q is not implemented; list without one and select client-side", f)
	}
	queues, err := g.store.ListQueues(req.GetParent())
	if err != nil {
		return nil, err
	}
	byName := map[string]Queue{}
	names := make([]string, 0, len(queues))
	for _, q := range queues {
		byName[q.Name] = q
		names = append(names, q.Name)
	}
	page, next, err := paging.Page("queues:"+req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	resp := &taskspb.ListQueuesResponse{NextPageToken: next}
	for _, n := range page {
		resp.Queues = append(resp.Queues, toProtoQueue(byName[n]))
	}
	return resp, nil
}

func (g *GRPCServer) DeleteQueue(_ context.Context, req *taskspb.DeleteQueueRequest) (*emptypb.Empty, error) {
	if err := g.store.DeleteQueue(req.GetName()); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (g *GRPCServer) PauseQueue(_ context.Context, req *taskspb.PauseQueueRequest) (*taskspb.Queue, error) {
	q, err := g.store.SetQueueState(req.GetName(), StatePaused)
	if err != nil {
		return nil, err
	}
	return toProtoQueue(q), nil
}

func (g *GRPCServer) ResumeQueue(_ context.Context, req *taskspb.ResumeQueueRequest) (*taskspb.Queue, error) {
	q, err := g.store.SetQueueState(req.GetName(), StateRunning)
	if err != nil {
		return nil, err
	}
	return toProtoQueue(q), nil
}

func (g *GRPCServer) PurgeQueue(_ context.Context, req *taskspb.PurgeQueueRequest) (*taskspb.Queue, error) {
	if err := g.store.PurgeQueue(req.GetName()); err != nil {
		return nil, err
	}
	q, err := g.store.GetQueue(req.GetName())
	if err != nil {
		return nil, err
	}
	return toProtoQueue(q), nil
}

// --- task methods ---

func (g *GRPCServer) CreateTask(ctx context.Context, req *taskspb.CreateTaskRequest) (*taskspb.Task, error) {
	parent := req.GetParent()
	if _, err := resource.Parse(parent); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	t, err := fromProtoTask(req.GetTask(), parent)
	if err != nil {
		return nil, err
	}
	if t.Name == "" {
		t.Name = generateTaskName(parent)
	}
	// A span is in the context only when tracing is on: its stats handler
	// put it there. Recording it lets the dispatch, later and elsewhere,
	// continue the caller's trace.
	if trace.SpanContextFromContext(ctx).IsValid() {
		carrier := propagation.MapCarrier{}
		telemetry.Propagator.Inject(ctx, carrier)
		t.Traceparent = carrier.Get("traceparent")
	}
	created, err := g.store.CreateTask(t)
	if err != nil {
		return nil, err
	}
	return toProtoTask(created), nil
}

func (g *GRPCServer) GetTask(_ context.Context, req *taskspb.GetTaskRequest) (*taskspb.Task, error) {
	t, err := g.store.GetTask(req.GetName())
	if err != nil {
		return nil, err
	}
	return toProtoTask(t), nil
}

func (g *GRPCServer) ListTasks(_ context.Context, req *taskspb.ListTasksRequest) (*taskspb.ListTasksResponse, error) {
	tasks, err := g.store.ListTasks(req.GetParent())
	if err != nil {
		return nil, err
	}
	byName := map[string]Task{}
	names := make([]string, 0, len(tasks))
	for _, t := range tasks {
		byName[t.Name] = t
		names = append(names, t.Name)
	}
	page, next, err := paging.Page("tasks:"+req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	resp := &taskspb.ListTasksResponse{NextPageToken: next}
	for _, n := range page {
		resp.Tasks = append(resp.Tasks, toProtoTask(byName[n]))
	}
	return resp, nil
}

func (g *GRPCServer) DeleteTask(_ context.Context, req *taskspb.DeleteTaskRequest) (*emptypb.Empty, error) {
	if err := g.store.DeleteTask(req.GetName()); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// GetIamPolicy returns a queue's stored policy (ADR-0006, #366). Stored,
// never enforced.
func (g *GRPCServer) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	q, err := g.store.GetQueue(req.GetResource())
	if err != nil {
		return nil, err
	}
	return iampolicy.Get(q.IAMPolicy, req.GetOptions())
}

// SetIamPolicy stores a policy. Stored, never enforced.
func (g *GRPCServer) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	p, err := g.store.SetIamPolicy(req.GetResource(), req)
	if err != nil {
		return nil, err
	}
	return p.Proto(), nil
}

// TestIamPermissions returns every requested permission of an existing
// queue: nothing is enforced, so nothing is denied.
func (g *GRPCServer) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	if _, err := g.store.GetQueue(req.GetResource()); err != nil {
		return nil, err
	}
	return iampolicy.Permissions(req), nil
}
