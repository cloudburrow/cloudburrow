package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	pubsub "cloud.google.com/go/pubsub/v2"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/localhost"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/service/scheduler"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// schedulerService serves Cloud Scheduler (#302) in the CLI process, as
// Cloud Tasks is served: there is no upstream emulator to run in the cluster.
// HTTP targets are therefore reached from the host, and Pub/Sub targets are
// published to the local emulator through its tunnel.
type schedulerService struct {
	cfg config.Config
	// tracing, when on, spans gRPC calls (#600), as for Cloud Tasks.
	tracing *telemetry.Tracing
	calls   grpctransport.Observer
	// interpose run inside the call observer, in order: the request log
	// (#314), which is what `cloudburrow logs --service scheduler` reads
	// (#587), then fault injection (#600), so the log sees injected faults.
	interpose []grpc.UnaryServerInterceptor
	// requests reports each JSON request to the admin event log (#591).
	requests func(rest.Request)
	pubsub   func() *netfwd.Forwarder
	server   *grpctransport.Server
	runner   *scheduler.Runner
	// api is the gRPC service, kept so the console pauses, resumes and runs
	// jobs through the same methods an SDK client reaches.
	api   *scheduler.GRPCServer
	db    store.Store
	store *scheduler.Store
}

func newSchedulerService(cfg config.Config, pubsubTunnel func() *netfwd.Forwarder) *schedulerService {
	if !serviceEnabled(cfg, config.ServiceScheduler) {
		return nil
	}
	return &schedulerService{cfg: cfg, pubsub: pubsubTunnel}
}

func (s *schedulerService) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

func (s *schedulerService) Name() string { return "scheduler" }

// Store is the job store, available after Start.
func (s *schedulerService) Store() *scheduler.Store {
	if s == nil {
		return nil
	}
	return s.store
}

// API is the gRPC service, available after Start.
func (s *schedulerService) API() *scheduler.GRPCServer {
	if s == nil {
		return nil
	}
	return s.api
}

// Addr is the API's bound address.
func (s *schedulerService) Addr() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.Addr()
}

// Worker is the runner that fires due jobs, available after Start.
func (s *schedulerService) Worker() lifecycle.Worker {
	if s == nil || s.runner == nil {
		return nil
	}
	return s.runner
}

func (s *schedulerService) Start(ctx context.Context) error {
	var db store.Store = store.NewMemory()
	if s.cfg.Mode == config.ModePersistent {
		durable, err := store.OpenDurable(filepath.Join(s.cfg.StateDir, s.cfg.Name, "scheduler"))
		if err != nil {
			return fmt.Errorf("open Cloud Scheduler state: %w", err)
		}
		db = durable
	}
	s.db = db
	s.store = scheduler.NewStore(db)
	clock := sched.RealClock{}
	// The transport dials `.localhost` targets on loopback whatever the
	// host's resolver does (#714).
	s.runner = scheduler.NewRunner(s.store, &http.Client{Transport: localhost.Transport()}, clock, s.publish, time.Second)

	addr := net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.Scheduler))
	s.server = grpctransport.New(addr)
	s.server.Observe(s.calls)
	if s.tracing != nil {
		s.server.ServerOptions(s.tracing.ServerOptions()...)
	}
	for _, i := range s.interpose {
		s.server.Interpose(i)
	}
	s.api = scheduler.NewGRPCServer(s.store, s.runner, clock)
	// One port for gRPC and JSON, as cloudscheduler.googleapis.com (#591):
	// the same server, transcoded, for REST clients, Terraform and gcloud.
	register := func(g grpc.ServiceRegistrar) { s.api.Register(g) }
	var jsonAPI http.Handler = scheduler.NewRESTHandler(register)
	if s.requests != nil {
		jsonAPI = rest.Observe(jsonAPI, s.requests)
	}
	s.server.ServeHTTP(jsonAPI)
	if err := s.server.Register(func(g *grpc.Server) { register(g) }); err != nil {
		_ = db.Close()
		return err
	}
	if err := s.server.Start(ctx); err != nil {
		_ = db.Close()
		return err
	}
	return nil
}

func (s *schedulerService) Stop(ctx context.Context) error {
	var err error
	if s.server != nil {
		err = s.server.Stop(ctx)
	}
	if s.db != nil {
		if cerr := s.db.Close(); err == nil {
			err = cerr
		}
		s.db = nil
	}
	return err
}

// publish sends a Pub/Sub target's message through the official client to
// the local emulator. A topic that does not exist fails the attempt, as it
// does against the real service.
func (s *schedulerService) publish(ctx context.Context, topic string, data []byte, attrs map[string]string) error {
	f := s.pubsub()
	if f == nil || f.HostAddr() == "" {
		return errors.New("Pub/Sub is not enabled on this instance, so a Pub/Sub target cannot publish")
	}
	project := strings.TrimPrefix(topic, "projects/")
	project, _, _ = strings.Cut(project, "/")
	c, err := pubsub.NewClient(ctx, project, option.WithEndpoint(f.HostAddr()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic,
		Messages: []*pubsubpb.PubsubMessage{{Data: data, Attributes: attrs}}})
	return err
}

// schedulerResetter clears every job.
type schedulerResetter struct{ svc *schedulerService }

func (r *schedulerResetter) Name() string { return "scheduler" }

func (r *schedulerResetter) Reset(context.Context) error {
	st := r.svc.Store()
	if st == nil {
		return errors.New("Cloud Scheduler has not started")
	}
	return st.Reset()
}

// schedulerSeeder creates Cloud Scheduler jobs from a seed document (#600).
//
// Each job becomes the CreateJob request an SDK client would send, is
// validated by the service's own CreateJob validation before anything is
// seeded, and is created through the gRPC service itself, so a seeded job is
// exactly one an application could have made.
type schedulerSeeder struct{ svc *schedulerService }

func (s *schedulerSeeder) Name() string { return "scheduler" }

type schedulerSeed struct {
	IfNotExists bool      `json:"ifNotExists"`
	Jobs        []jobSeed `json:"jobs"`
}

// jobSeed is a Job in the REST API's field names. Output-only fields (state,
// scheduleTime, status and the rest) are not taken: a created job is always
// ENABLED with its next run computed from the schedule.
type jobSeed struct {
	Name            string            `json:"name"`
	Description     string            `json:"description,omitempty"`
	Schedule        string            `json:"schedule"`
	TimeZone        string            `json:"timeZone,omitempty"`
	HTTPTarget      *httpTargetSeed   `json:"httpTarget,omitempty"`
	PubsubTarget    *pubsubTargetSeed `json:"pubsubTarget,omitempty"`
	RetryConfig     *retryConfigSeed  `json:"retryConfig,omitempty"`
	AttemptDeadline string            `json:"attemptDeadline,omitempty"`

	// Refused by name; see refuse.
	AppEngineHTTPTarget json.RawMessage `json:"appEngineHttpTarget,omitempty"`
}

type httpTargetSeed struct {
	URI        string            `json:"uri"`
	HTTPMethod string            `json:"httpMethod,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	// At most one of Body and BodyBase64.
	Body       *string `json:"body,omitempty"`
	BodyBase64 *string `json:"bodyBase64,omitempty"`

	// Refused by name; see refuse.
	OAuthToken json.RawMessage `json:"oauthToken,omitempty"`
	OIDCToken  json.RawMessage `json:"oidcToken,omitempty"`
}

type pubsubTargetSeed struct {
	TopicName string `json:"topicName"`
	// At most one of Data and DataBase64.
	Data       *string           `json:"data,omitempty"`
	DataBase64 *string           `json:"dataBase64,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

type retryConfigSeed struct {
	RetryCount         int32  `json:"retryCount,omitempty"`
	MaxRetryDuration   string `json:"maxRetryDuration,omitempty"`
	MinBackoffDuration string `json:"minBackoffDuration,omitempty"`
	MaxBackoffDuration string `json:"maxBackoffDuration,omitempty"`
	MaxDoublings       int32  `json:"maxDoublings,omitempty"`
}

// seedBytes is the plain or base64 form of a byte field, exclusive.
func seedBytes(field string, plain, b64 *string) ([]byte, error) {
	switch {
	case plain != nil && b64 != nil:
		return nil, fmt.Errorf("%s and %sBase64 are exclusive", field, field)
	case b64 != nil:
		b, err := base64.StdEncoding.DecodeString(*b64)
		if err != nil {
			return nil, fmt.Errorf("%sBase64: %w", field, err)
		}
		return b, nil
	case plain != nil:
		return []byte(*plain), nil
	default:
		return nil, nil
	}
}

// job converts one seed job to the proto an SDK client would send.
func (j jobSeed) job() (*schedulerpb.Job, error) {
	if err := refuse("job", map[string]bool{"appEngineHttpTarget": len(j.AppEngineHTTPTarget) > 0}); err != nil {
		return nil, err
	}
	out := &schedulerpb.Job{Name: j.Name, Description: j.Description, Schedule: j.Schedule, TimeZone: j.TimeZone}
	if j.HTTPTarget != nil && j.PubsubTarget != nil {
		return nil, errors.New("httpTarget and pubsubTarget are exclusive")
	}
	if h := j.HTTPTarget; h != nil {
		if err := refuse("httpTarget", map[string]bool{
			"oauthToken": len(h.OAuthToken) > 0, "oidcToken": len(h.OIDCToken) > 0,
		}); err != nil {
			return nil, err
		}
		method := schedulerpb.HttpMethod_HTTP_METHOD_UNSPECIFIED
		if h.HTTPMethod != "" {
			v, ok := schedulerpb.HttpMethod_value[h.HTTPMethod]
			if !ok || v == 0 {
				return nil, fmt.Errorf("httpTarget.httpMethod %q is not an HTTP method such as POST", h.HTTPMethod)
			}
			method = schedulerpb.HttpMethod(v)
		}
		body, err := seedBytes("body", h.Body, h.BodyBase64)
		if err != nil {
			return nil, fmt.Errorf("httpTarget.%w", err)
		}
		out.Target = &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{
			Uri: h.URI, HttpMethod: method, Headers: h.Headers, Body: body}}
	}
	if p := j.PubsubTarget; p != nil {
		data, err := seedBytes("data", p.Data, p.DataBase64)
		if err != nil {
			return nil, fmt.Errorf("pubsubTarget.%w", err)
		}
		out.Target = &schedulerpb.Job_PubsubTarget{PubsubTarget: &schedulerpb.PubsubTarget{
			TopicName: p.TopicName, Data: data, Attributes: p.Attributes}}
	}
	if r := j.RetryConfig; r != nil {
		rc := &schedulerpb.RetryConfig{RetryCount: r.RetryCount, MaxDoublings: r.MaxDoublings}
		for _, d := range []struct {
			field string
			value string
			into  **durationpb.Duration
		}{
			{"maxRetryDuration", r.MaxRetryDuration, &rc.MaxRetryDuration},
			{"minBackoffDuration", r.MinBackoffDuration, &rc.MinBackoffDuration},
			{"maxBackoffDuration", r.MaxBackoffDuration, &rc.MaxBackoffDuration},
		} {
			v, err := protoDuration(d.value)
			if err != nil {
				return nil, fmt.Errorf("retryConfig.%s: %w", d.field, err)
			}
			*d.into = v
		}
		out.RetryConfig = rc
	}
	deadline, err := protoDuration(j.AttemptDeadline)
	if err != nil {
		return nil, fmt.Errorf("attemptDeadline: %w", err)
	}
	out.AttemptDeadline = deadline
	return out, nil
}

// parse decodes and validates the whole document, returning a CreateJob
// request per job.
func (s *schedulerSeeder) parse(spec json.RawMessage) (schedulerSeed, []*schedulerpb.CreateJobRequest, error) {
	var doc schedulerSeed
	if err := strictDecode(spec, &doc); err != nil {
		return doc, nil, err
	}
	var reqs []*schedulerpb.CreateJobRequest
	seen := map[string]bool{}
	for i, j := range doc.Jobs {
		where := fmt.Sprintf("jobs[%d]", i)
		if seen[j.Name] {
			return doc, nil, fmt.Errorf("%s.name %q appears twice", where, j.Name)
		}
		seen[j.Name] = true
		job, err := j.job()
		if err != nil {
			return doc, nil, fmt.Errorf("%s: %w", where, err)
		}
		// CreateJob's own validation, so a seed cannot disagree with it.
		req, err := scheduler.CreateRequest(job)
		if err != nil {
			return doc, nil, fmt.Errorf("%s: %w", where, err)
		}
		reqs = append(reqs, req)
	}
	return doc, reqs, nil
}

func (s *schedulerSeeder) Validate(spec json.RawMessage) error {
	_, _, err := s.parse(spec)
	return err
}

func (s *schedulerSeeder) Seed(ctx context.Context, spec json.RawMessage) error {
	doc, reqs, err := s.parse(spec)
	if err != nil {
		return apierror.InvalidArgument("%v", err)
	}
	api := s.svc.API()
	if api == nil {
		return errors.New("Cloud Scheduler is not running")
	}
	for _, req := range reqs {
		_, err := api.CreateJob(ctx, req)
		if status.Code(err) == codes.AlreadyExists {
			if doc.IfNotExists {
				continue
			}
			return apierror.AlreadyExists("job %s already exists; set ifNotExists to skip it", req.GetJob().GetName())
		}
		if err != nil {
			return fmt.Errorf("create job %s: %w", req.GetJob().GetName(), err)
		}
	}
	return nil
}
