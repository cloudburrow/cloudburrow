package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/config"
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
	server    *grpctransport.Server
	api       *workflows.Server
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
// to. An API not served answers "".
func localAPIs(cfg config.Config) func(api string) string {
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
	return func(api string) string { return bases[api] }
}

func (s *workflowsService) Start(ctx context.Context) error {
	state := ""
	if s.cfg.Mode == config.ModePersistent {
		state = filepath.Join(s.cfg.InstanceDir(), "workflows.json")
	}
	local := localAPIs(s.cfg)
	api, err := workflows.NewServer(state, workflows.Options{
		Local:  local,
		Tokens: s.tokens,
		Addr:   s.Addr,
		Log:    workflowsLogger(local),
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
	return s.server.Start(ctx)
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

// workflowsLogger writes an execution's sys.log entries to the instance's
// Cloud Logging when it serves it, as Google writes them to the
// workflows.googleapis.com/Workflow resource; otherwise to the CLI's log.
func workflowsLogger(local func(string) string) func(project, location, workflow, execution, severity string, payload any) {
	client := &http.Client{Timeout: 10 * time.Second}
	return func(project, location, workflow, execution, severity string, payload any) {
		entry := map[string]any{
			"logName":  "projects/" + project + "/logs/Workflows",
			"severity": severity,
			"resource": map[string]any{"type": "workflows.googleapis.com/Workflow",
				"labels": map[string]string{"project_id": project, "location": location, "workflow_id": workflow}},
			"labels": map[string]string{"workflows.googleapis.com/execution_id": execution},
		}
		if s, ok := payload.(string); ok {
			entry["textPayload"] = s
		} else {
			entry["jsonPayload"] = payload
		}
		base := local("logging")
		if base == "" {
			slog.Info("workflows: sys.log", "execution", execution, "severity", severity, "payload", payload)
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
