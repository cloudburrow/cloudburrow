package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	"cloud.google.com/go/workflows/apiv1/workflowspb"
	"cloud.google.com/go/workflows/executions/apiv1/executionspb"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/egress"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/service/workflows"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// workflowsService serves Workflows and Workflow Executions (#1986) in the
// CLI process, gRPC and JSON on one port, as both APIs are answered by the
// same records. Executions call this instance's own endpoints only: the
// connectors' APIs are the ones gcloud-setup points at this instance.
type workflowsService struct {
	cfg       config.Config
	tracing   *telemetry.Tracing
	calls     grpctransport.Observer
	interpose []grpc.UnaryServerInterceptor
	requests  func(rest.Request)
	tokens    workflows.TokenMinter
	// kms is the instance's Cloud KMS, for workflows' customer-managed
	// keys; nil without it.
	kms *kmsService
	// live is `up`'s bound addresses, for the connectors' endpoints
	// (localAPIs); nil uses the configured ports.
	live   func() map[string]string
	server *grpctransport.Server
	api    *workflows.Server
}

func newWorkflowsService(cfg config.Config) *workflowsService {
	if !workflowsEnabled(cfg) {
		return nil
	}
	return &workflowsService{cfg: cfg}
}

// workflowsEnabled is whether `up` binds the Workflows port.
func workflowsEnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceWorkflows) && cfg.Endpoints.Workflows >= 0
}

func (s *workflowsService) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

func (s *workflowsService) Name() string { return "workflows" }

// API is the server, available after Start.
func (s *workflowsService) API() *workflows.Server {
	if s == nil {
		return nil
	}
	return s.api
}

// Addr is the bound address.
func (s *workflowsService) Addr() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.Addr()
}

// localAPIs is the "http://host:port" of each API this instance serves, by
// the first label of its googleapis.com host: the endpoints gcloud-setup
// writes, which a workflow's connectors and googleapis.com URLs are sent
// to. An API not served answers "". live, when not nil, is `up`'s bound
// addresses, read at each call: a port configured as 0 (OS-assigned, as
// CI starts Cloud Storage, Pub/Sub and Cloud Run) is known only once its
// listener or tunnel is up, so the configured ports alone left those APIs
// unserved, and a workflow's storage connector answered 403
// SERVICE_DISABLED on an instance serving storage.
func localAPIs(cfg config.Config, live func() map[string]string) func(api string) string {
	if live == nil {
		bases := localAPIBases(cfg)
		return func(api string) string { return bases[api] }
	}
	return func(api string) string {
		return localAPIBases(withLivePorts(cfg, live()))[api]
	}
}

// localAPIBases is localAPIs' map for cfg's ports.
func localAPIBases(cfg config.Config) map[string]string {
	host := cfg.BindAddress
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	ports := gcloudPorts(cfg)
	bases := map[string]string{}
	for _, v := range gcloudVerified {
		if !gcloudOverrideOn(cfg, v) || ports[v.service] <= 0 {
			continue
		}
		if _, seen := bases[v.property]; !seen {
			bases[v.property] = "http://" + net.JoinHostPort(host, strconv.Itoa(ports[v.service]))
		}
	}
	return bases
}

func (s *workflowsService) Start(ctx context.Context) error {
	state := ""
	if s.cfg.Mode == config.ModePersistent {
		state = filepath.Join(s.cfg.InstanceDir(), "workflows.json")
	}
	local := localAPIs(s.cfg, s.live)
	api, err := workflows.NewServer(state, workflows.Options{
		Local:   local,
		Tokens:  s.tokens,
		Addr:    s.Addr,
		Log:     workflowsLogger(local),
		Ingress: workflowsIngress(s.cfg),
		KMS:     s.kmsAPI,
	})
	if err != nil {
		return err
	}
	s.api = api
	s.server = grpctransport.New(net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.Workflows)))
	s.server.Observe(s.calls)
	if s.tracing != nil {
		s.server.ServerOptions(s.tracing.ServerOptions()...)
	}
	for _, i := range s.interpose {
		s.server.Interpose(i)
	}
	var jsonAPI http.Handler = workflows.NewRESTHandler(api)
	if s.requests != nil {
		jsonAPI = rest.Observe(jsonAPI, s.requests)
	}
	s.server.ServeHTTP(jsonAPI)
	if err := s.server.Register(func(g *grpc.Server) { api.Register(g) }); err != nil {
		return err
	}
	if err := s.server.Start(ctx); err != nil {
		return err
	}
	// Executions that were running when the instance stopped carry on.
	api.Resume()
	return nil
}

// kmsAPI is Cloud KMS for a workflow's key, or nil.
func (s *workflowsService) kmsAPI() workflows.KMS {
	if s.kms == nil {
		return nil
	}
	if api := s.kms.API(); api != nil {
		return api
	}
	return nil
}

func (s *workflowsService) Stop(ctx context.Context) error {
	if s.api != nil {
		s.api.Close()
	}
	if s.server == nil {
		return nil
	}
	return s.server.Stop(ctx)
}

// workflowsIngress is the Knative gateway's host port, which a workflow's
// calls to a Cloud Run service's URL go through, or "" without Cloud Run.
func workflowsIngress(cfg config.Config) string {
	if !serviceEnabled(cfg, config.ServiceRun) || cfg.Endpoints.Ingress <= 0 {
		return ""
	}
	host := cfg.BindAddress
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(cfg.Endpoints.Ingress))
}

// workflowsLogger writes an execution's log entries (sys.log's, its start
// and end, call logs) to the instance's Cloud Logging when it serves it, as
// Google writes them to the workflows.googleapis.com/Workflow resource;
// otherwise to the CLI's log.
func workflowsLogger(local func(string) string) func(project, location, workflow, execution, logID, severity string, payload any) {
	// Behind the local-only guard (#2015), as every outbound client is.
	client := egress.Client(&http.Client{Timeout: 10 * time.Second})
	return func(project, location, workflow, execution, logID, severity string, payload any) {
		entry := map[string]any{
			"logName":  "projects/" + project + "/logs/" + url.PathEscape(logID),
			"severity": severity,
			"resource": map[string]any{"type": "workflows.googleapis.com/Workflow",
				"labels": map[string]string{"project_id": project, "location": location, "workflow_id": workflow}},
			"labels": map[string]string{"workflows.googleapis.com/execution_id": execution},
		}
		// The entry's time is when it happened, so entries written from
		// different goroutines still list in order.
		entry["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
		if s, ok := payload.(string); ok {
			entry["textPayload"] = s
		} else {
			entry["jsonPayload"] = payload
			if m, ok := payload.(map[string]any); ok {
				if at, ok := m["activityTime"].(string); ok {
					entry["timestamp"] = at
				}
			}
		}
		base := local("logging")
		if base == "" {
			slog.Info("workflows: log entry", "log", logID, "execution", execution, "severity", severity, "payload", payload)
			return
		}
		b, err := json.Marshal(map[string]any{"entries": []any{entry}})
		if err != nil {
			return
		}
		resp, err := client.Post(base+"/v2/entries:write", "application/json", bytes.NewReader(b))
		if err != nil {
			slog.Warn("workflows: sys.log to Cloud Logging failed", "err", err)
			return
		}
		resp.Body.Close()
	}
}

// WorkflowExists is Eventarc's check of a workflow destination (#1986).
func (s *workflowsService) WorkflowExists(name string) bool {
	api := s.API()
	if api == nil {
		return false
	}
	_, err := api.Workflows().GetWorkflow(context.Background(), &workflowspb.GetWorkflowRequest{Name: name})
	return err == nil
}

// StartWorkflow starts an execution of an Eventarc trigger's workflow with
// the event as its argument.
func (s *workflowsService) StartWorkflow(_ context.Context, name, argument string) error {
	api := s.API()
	if api == nil {
		return errors.New("Workflows has not started")
	}
	_, err := api.Start(name, &executionspb.Execution{Argument: argument})
	return err
}

// workflowsResetter deletes the workflows and their executions.
type workflowsResetter struct{ svc *workflowsService }

func (r *workflowsResetter) Name() string { return "workflows" }

func (r *workflowsResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *workflowsResetter) ResetProject(_ context.Context, project string) error {
	api := r.svc.API()
	if api == nil {
		return errors.New("Workflows has not started")
	}
	return api.ResetProject(project)
}
