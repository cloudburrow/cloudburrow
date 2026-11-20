package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strconv"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/cmek"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/service/parametermanager"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// parameterManagerService serves Parameter Manager (#2054) in the CLI
// process, gRPC and JSON on one port. Its versions' secret references are
// read through the instance's Secret Manager, in process.
type parameterManagerService struct {
	cfg       config.Config
	tracing   *telemetry.Tracing
	calls     grpctransport.Observer
	interpose []grpc.UnaryServerInterceptor
	requests  func(rest.Request)
	// wrapJSON wraps the JSON API (IAM enforcement); nil is none.
	wrapJSON func(http.Handler) http.Handler
	// secrets is the instance's Secret Manager; nil without it.
	secrets *secretsService
	// keys are the customer-managed keys (the instance's Cloud KMS).
	keys cmek.Keys
	// enforcer, when IAM is enforced, checks a parameter's principal may
	// access each secret it references.
	enforcer *iameval.Enforcer
	server   *grpctransport.Server
	api      *parametermanager.Server
}

func newParameterManagerService(cfg config.Config) *parameterManagerService {
	if !parameterManagerEnabled(cfg) {
		return nil
	}
	return &parameterManagerService{cfg: cfg}
}

// parameterManagerEnabled is whether `up` binds the Parameter Manager port.
func parameterManagerEnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceParameterManager) && cfg.Endpoints.ParameterManager >= 0
}

func (s *parameterManagerService) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

func (s *parameterManagerService) Name() string { return "parametermanager" }

// API is the server, available after Start.
func (s *parameterManagerService) API() *parametermanager.Server {
	if s == nil {
		return nil
	}
	return s.api
}

// Addr is the bound address.
func (s *parameterManagerService) Addr() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.Addr()
}

func (s *parameterManagerService) Start(ctx context.Context) error {
	state := ""
	if s.cfg.Mode == config.ModePersistent {
		state = filepath.Join(s.cfg.InstanceDir(), "parametermanager.json")
	}
	opts := parametermanager.Options{Keys: s.keys}
	if s.secrets != nil {
		opts.AccessSecret = s.accessSecret
	}
	if e := s.enforcer; e != nil && e.On != nil && e.On() {
		opts.CheckSecretAccess = func(ctx context.Context, principal, name string) error {
			const permission = "secretmanager.versions.access"
			if e.Eval.Check(e.Request(ctx, iameval.SecretManager, principal, permission, name)).Granted {
				return nil
			}
			return iameval.SecretManager.Deny(permission, name)
		}
	}
	api, err := parametermanager.NewServer(state, opts)
	if err != nil {
		return err
	}
	s.api = api
	s.server = grpctransport.New(net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.ParameterManager)))
	s.server.Observe(s.calls)
	if s.tracing != nil {
		s.server.ServerOptions(s.tracing.ServerOptions()...)
	}
	for _, i := range s.interpose {
		s.server.Interpose(i)
	}
	var jsonAPI http.Handler = parametermanager.NewRESTHandler(api)
	if s.wrapJSON != nil {
		jsonAPI = s.wrapJSON(jsonAPI)
	}
	if s.requests != nil {
		jsonAPI = rest.Observe(jsonAPI, s.requests)
	}
	s.server.ServeHTTP(jsonAPI)
	if err := s.server.Register(func(g *grpc.Server) { api.Register(g) }); err != nil {
		return err
	}
	return s.server.Start(ctx)
}

// accessSecret reads a secret version through Secret Manager's own
// AccessSecretVersion, so aliases, latest, disabled versions, regional
// secrets and customer-managed keys answer as they do to a client.
func (s *parameterManagerService) accessSecret(ctx context.Context, name string) ([]byte, error) {
	st := s.secrets.Store()
	if st == nil {
		return nil, errors.New("Secret Manager has not started")
	}
	resp, err := secrets.NewGRPCServer(st).AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: name})
	if err != nil {
		return nil, err
	}
	return resp.GetPayload().GetData(), nil
}

func (s *parameterManagerService) Stop(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Stop(ctx)
}

// parameterManagerResetter deletes the parameters and their versions.
type parameterManagerResetter struct{ svc *parameterManagerService }

func (r *parameterManagerResetter) Name() string { return "parametermanager" }

func (r *parameterManagerResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *parameterManagerResetter) ResetProject(_ context.Context, project string) error {
	api := r.svc.API()
	if api == nil {
		return errors.New("Parameter Manager has not started")
	}
	return api.ResetProject(project)
}
