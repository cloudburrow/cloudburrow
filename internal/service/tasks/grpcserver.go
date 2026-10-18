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
			// Output only: the burst the dispatcher enforces (#716).
			MaxBurstSize: int32(BurstSize(q.RateLimits.MaxDispatchesPerSecond)),
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
		StackdriverLoggingConfig: func() *taskspb.StackdriverLoggingConfig {
			if q.Logging == nil {
				return nil
			}
			return &taskspb.StackdriverLoggingConfig{SamplingRatio: q.Logging.SamplingRatio}
		}(),
		HttpTarget:               queueHTTPTarget(q),
		AppEngineRoutingOverride: routingToProto(q.AppEngineRouting, projectOf(q.Name)),
	}
}

// checkQueue refuses queue fields this server would not honour (#578), for
// CreateQueue and, field by field as its mask names them, UpdateQueue.
func checkQueue(p *taskspb.Queue) error {
	if err := checkLogging(p); err != nil {
		return err
	}
	if err := checkHTTPTarget(p); err != nil {
		return err
	}
	if err := checkRateLimits(p, ""); err != nil {
		return err
	}
	return checkRetry(p, "")
}

func checkLogging(p *taskspb.Queue) error {
	if c := p.GetStackdriverLoggingConfig(); c != nil {
		if err := checkSamplingRatio(c.GetSamplingRatio()); err != nil {
			return apierror.InvalidArgument("%v", err)
		}
	}
	return nil
}

func checkAppEngineRouting(p *taskspb.Queue) error {
	if proto.Size(p.GetAppEngineRoutingOverride()) > 0 {
		return apierror.Unimplemented("queue.app_engine_routing_override is not implemented: App Engine targets are not supported")
	}
	return nil
}

// checkRateLimits refuses the rate limits the contract documents as out of
// range (#784): "The maximum allowed value is 500" for
// max_dispatches_per_second and "5,000" for max_concurrent_dispatches. sub
// is the update_mask path under rate_limits, empty for the whole message;
// a field the mask does not name is not checked, since it is not applied.
func checkRateLimits(p *taskspb.Queue, sub string) error {
	rl := p.GetRateLimits()
	if (sub == "" || sub == "max_dispatches_per_second") && rl.GetMaxDispatchesPerSecond() > 500 {
		return apierror.InvalidArgument("rate_limits.max_dispatches_per_second %v is above the maximum of 500",
			rl.GetMaxDispatchesPerSecond())
	}
	if (sub == "" || sub == "max_concurrent_dispatches") && rl.GetMaxConcurrentDispatches() > 5000 {
		return apierror.InvalidArgument("rate_limits.max_concurrent_dispatches %d is above the maximum of 5000",
			rl.GetMaxConcurrentDispatches())
	}
	return nil
}

// checkRetry refuses retry settings no dispatcher could follow: a
// max_attempts below -1, which the contract says it "Must be >= -1" (#784),
// and a negative duration or doubling count, which the store would
// otherwise replace with the default without a word. sub is as for
// checkRateLimits.
func checkRetry(p *taskspb.Queue, sub string) error {
	rc := p.GetRetryConfig()
	names := func(field string) bool { return sub == "" || sub == field }
	if names("max_attempts") && rc.GetMaxAttempts() < -1 {
		return apierror.InvalidArgument("retry_config.max_attempts %d must be -1 (unlimited) or greater",
			rc.GetMaxAttempts())
	}
	if names("max_retry_duration") && rc.GetMaxRetryDuration() != nil && rc.GetMaxRetryDuration().AsDuration() < 0 {
		return apierror.InvalidArgument("retry_config.max_retry_duration must not be negative")
	}
	if names("min_backoff") && rc.GetMinBackoff() != nil && rc.GetMinBackoff().AsDuration() < 0 {
		return apierror.InvalidArgument("retry_config.min_backoff must not be negative")
	}
	if names("max_backoff") && rc.GetMaxBackoff() != nil && rc.GetMaxBackoff().AsDuration() < 0 {
		return apierror.InvalidArgument("retry_config.max_backoff must not be negative")
	}
	if names("max_doublings") && rc.GetMaxDoublings() < 0 {
		return apierror.InvalidArgument("retry_config.max_doublings %d must not be negative", rc.GetMaxDoublings())
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
	if c := p.GetStackdriverLoggingConfig(); c != nil {
		q.Logging = &LoggingConfig{SamplingRatio: c.GetSamplingRatio()}
	}
	q.HTTPTarget = httpTargetJSON(p.GetHttpTarget())
	q.AppEngineRouting = routingFromProto(p.GetAppEngineRoutingOverride())
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

// toProtoTaskIn is a task as the API returns it, for a queue whose
// app_engine_routing_override is override, which sets the host an App
// Engine task reports.
func toProtoTaskIn(t Task, override *AppEngineRouting) *taskspb.Task {
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
		hr := &taskspb.HttpRequest{
			Url:        t.HTTPRequest.URL,
			HttpMethod: method,
			Headers:    t.HTTPRequest.Headers,
			Body:       t.HTTPRequest.Body,
		}
		switch {
		case t.HTTPRequest.OIDCToken != nil:
			hr.AuthorizationHeader = &taskspb.HttpRequest_OidcToken{OidcToken: &taskspb.OidcToken{
				ServiceAccountEmail: t.HTTPRequest.OIDCToken.ServiceAccountEmail, Audience: t.HTTPRequest.OIDCToken.Audience}}
		case t.HTTPRequest.OAuthToken != nil:
			hr.AuthorizationHeader = &taskspb.HttpRequest_OauthToken{OauthToken: &taskspb.OAuthToken{
				ServiceAccountEmail: t.HTTPRequest.OAuthToken.ServiceAccountEmail, Scope: t.HTTPRequest.OAuthToken.Scope}}
		}
		pt.MessageType = &taskspb.Task_HttpRequest{HttpRequest: hr}
	}
	if t.AppEngineHTTPRequest != nil {
		pt.MessageType = &taskspb.Task_AppEngineHttpRequest{
			AppEngineHttpRequest: appEngineToProto(t.AppEngineHTTPRequest, t.Name, override)}
	}
	attemptsToProto(t, pt)
	return pt
}

func fromProtoTask(p *taskspb.Task, parent string) (Task, error) {
	t := Task{Name: p.GetName(), Queue: parent}
	if p.GetScheduleTime() != nil {
		t.ScheduleTime = p.GetScheduleTime().AsTime()
	}
	if dd := p.GetDispatchDeadline(); dd != nil {
		d := dd.AsDuration()
		if d < 15*time.Second || d > 24*time.Hour+15*time.Second {
			return Task{}, apierror.InvalidArgument("dispatch_deadline %s must be between 15s and 24h0m15s", d)
		}
		t.DispatchDeadline = d
	}
	hr := p.GetHttpRequest()
	if hr == nil {
		// An App Engine task is stored; its attempts go to --appengine-url,
		// or fail 404 without it (#1447, #1506, appengine1447.go).
		if ae := p.GetAppEngineHttpRequest(); ae != nil {
			a, err := appEngineFromProto(ae)
			if err != nil {
				return Task{}, err
			}
			t.AppEngineHTTPRequest = a
			return t, nil
		}
		return Task{}, apierror.InvalidArgument("task requires an httpRequest or an appEngineHttpRequest")
	}
	if t.DispatchDeadline > 30*time.Minute {
		return Task{}, apierror.InvalidArgument("dispatch_deadline %s must be between 15s and 30m for an HTTP task", t.DispatchDeadline)
	}
	oidc, oauth, err := authorizationFromProto(hr)
	if err != nil {
		return Task{}, err
	}
	switch hr.GetHttpMethod() {
	case taskspb.HttpMethod_GET, taskspb.HttpMethod_HEAD, taskspb.HttpMethod_DELETE, taskspb.HttpMethod_OPTIONS:
		if len(hr.GetBody()) > 0 {
			return Task{}, apierror.InvalidArgument("httpRequest.body must be empty for an %s task", hr.GetHttpMethod())
		}
	}
	t.HTTPRequest = &HTTPRequest{
		URL:        hr.GetUrl(),
		Method:     hr.GetHttpMethod().String(),
		Headers:    hr.GetHeaders(),
		Body:       hr.GetBody(),
		OIDCToken:  oidc,
		OAuthToken: oauth,
	}
	return t, nil
}

// authorizationFromProto is the token an HTTP task asks Cloud Tasks to mint
// (#1243). The service account is required, as on Google; that it exists
// and that the queue may act as it are not checked, because nothing here
// authenticates or authorises (ADR-0004).
func authorizationFromProto(hr *taskspb.HttpRequest) (*OIDCToken, *OAuthToken, error) {
	switch a := hr.GetAuthorizationHeader().(type) {
	case *taskspb.HttpRequest_OidcToken:
		if a.OidcToken.GetServiceAccountEmail() == "" {
			return nil, nil, apierror.InvalidArgument("httpRequest.oidcToken.serviceAccountEmail is required")
		}
		return &OIDCToken{ServiceAccountEmail: a.OidcToken.GetServiceAccountEmail(), Audience: a.OidcToken.GetAudience()}, nil, nil
	case *taskspb.HttpRequest_OauthToken:
		if a.OauthToken.GetServiceAccountEmail() == "" {
			return nil, nil, apierror.InvalidArgument("httpRequest.oauthToken.serviceAccountEmail is required")
		}
		return nil, &OAuthToken{ServiceAccountEmail: a.OauthToken.GetServiceAccountEmail(), Scope: a.OauthToken.GetScope()}, nil
	}
	return nil, nil, nil
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
var updatableQueuePaths = []string{"app_engine_routing_override", "http_target", "rate_limits", "retry_config", "stackdriver_logging_config"}

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
	var sets []func(*Queue)
	want := fromProtoQueue(in)
	for _, p := range paths {
		field, sub, _ := strings.Cut(p, ".")
		var set func(*Queue)
		var err error
		switch field {
		case "rate_limits":
			if err = checkRateLimits(in, sub); err == nil {
				var f func(*RetryConfig, *RateLimits)
				if f, err = rateLimitsSetter(sub, want.RateLimits); err == nil {
					set = func(q *Queue) { f(&q.RetryConfig, &q.RateLimits) }
				}
			}
		case "retry_config":
			if err = checkRetry(in, sub); err == nil {
				var f func(*RetryConfig, *RateLimits)
				if f, err = retryConfigSetter(sub, want.RetryConfig); err == nil {
					set = func(q *Queue) { f(&q.RetryConfig, &q.RateLimits) }
				}
			}
		case "stackdriver_logging_config", "stackdriver_logging_config.sampling_ratio":
			if err = checkLogging(in); err == nil {
				logging := want.Logging
				set = func(q *Queue) { q.Logging = logging }
			}
		case "http_target":
			// The whole message (#1282); a path under it replaces it too.
			if err = checkHTTPTarget(in); err == nil {
				target := want.HTTPTarget
				set = func(q *Queue) { q.HTTPTarget = target }
			}
		case "app_engine_routing_override":
			// The whole message; its host is output only and ignored.
			routing := routingFromProto(in.GetAppEngineRoutingOverride())
			set = func(q *Queue) { q.AppEngineRouting = routing }
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
	q, _, err := g.store.UpdateQueue(in.GetName(), func(q *Queue) error {
		for _, set := range sets {
			set(q)
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
	keep, err := queueFilter(req.GetFilter())
	if err != nil {
		return nil, err
	}
	queues, err := g.store.ListQueues(req.GetParent())
	if err != nil {
		return nil, err
	}
	byName := map[string]Queue{}
	names := make([]string, 0, len(queues))
	for _, q := range queues {
		if !keep(toProtoQueue(q)) {
			continue
		}
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
		t.NamedByServer = true
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
	return g.protoTask(created), nil
}

func (g *GRPCServer) GetTask(_ context.Context, req *taskspb.GetTaskRequest) (*taskspb.Task, error) {
	t, err := g.store.GetTask(req.GetName())
	if err != nil {
		return nil, err
	}
	return g.protoTask(t), nil
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
		resp.Tasks = append(resp.Tasks, g.protoTask(byName[n]))
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
