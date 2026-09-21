package tasks

import (
	"context"
	"fmt"
	"strings"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"github.com/identity-wael/cloudburrow/internal/apierror"
	"github.com/identity-wael/cloudburrow/internal/paging"
	"github.com/identity-wael/cloudburrow/internal/resource"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
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

// Register adds this service to a gRPC server.
func (g *GRPCServer) Register(s *grpc.Server) { taskspb.RegisterCloudTasksServer(s, g) }

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
		},
	}
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
			MaxAttempts:  int(rc.GetMaxAttempts()),
			MinBackoff:   rc.GetMinBackoff().AsDuration(),
			MaxBackoff:   rc.GetMaxBackoff().AsDuration(),
			MaxDoublings: int(rc.GetMaxDoublings()),
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

func (g *GRPCServer) CreateTask(_ context.Context, req *taskspb.CreateTaskRequest) (*taskspb.Task, error) {
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
