package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strconv"

	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/service/privateca"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// privateCAService serves Certificate Authority Service (#1834) in the CLI
// process, gRPC and JSON on one port as privateca.googleapis.com serves
// both. Its records, the CAs' keys among them, are a file in the
// instance's directory in persistent mode.
type privateCAService struct {
	cfg       config.Config
	tracing   *telemetry.Tracing
	calls     grpctransport.Observer
	interpose []grpc.UnaryServerInterceptor
	requests  func(rest.Request)
	server    *grpctransport.Server
	api       *privateca.Server
}

func newPrivateCAService(cfg config.Config) *privateCAService {
	if !privateCAEnabled(cfg) {
		return nil
	}
	return &privateCAService{cfg: cfg}
}

// privateCAEnabled is whether `up` binds the Certificate Authority Service
// port.
func privateCAEnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServicePrivateCA) && cfg.Endpoints.PrivateCA >= 0
}

func (s *privateCAService) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

func (s *privateCAService) Name() string { return "privateca" }

// API is the server, available after Start.
func (s *privateCAService) API() *privateca.Server {
	if s == nil {
		return nil
	}
	return s.api
}

// Addr is the bound address.
func (s *privateCAService) Addr() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.Addr()
}

func (s *privateCAService) Start(ctx context.Context) error {
	state := ""
	if s.cfg.Mode == config.ModePersistent {
		state = filepath.Join(s.cfg.InstanceDir(), "privateca.json")
	}
	api, err := privateca.NewServer(state)
	if err != nil {
		return err
	}
	s.api = api
	s.server = grpctransport.New(net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.PrivateCA)))
	s.server.Observe(s.calls)
	if s.tracing != nil {
		s.server.ServerOptions(s.tracing.ServerOptions()...)
	}
	for _, i := range s.interpose {
		s.server.Interpose(i)
	}
	var jsonAPI http.Handler = privateca.NewRESTHandler(api)
	if s.requests != nil {
		jsonAPI = rest.Observe(jsonAPI, s.requests)
	}
	s.server.ServeHTTP(jsonAPI)
	if err := s.server.Register(func(g *grpc.Server) { api.Register(g) }); err != nil {
		return err
	}
	return s.server.Start(ctx)
}

func (s *privateCAService) Stop(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Stop(ctx)
}

// privateCAResetter deletes Certificate Authority Service's records.
type privateCAResetter struct{ svc *privateCAService }

func (r *privateCAResetter) Name() string { return "privateca" }

func (r *privateCAResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *privateCAResetter) ResetProject(_ context.Context, project string) error {
	api := r.svc.API()
	if api == nil {
		return errors.New("Certificate Authority Service has not started")
	}
	return api.ResetProject(project)
}
