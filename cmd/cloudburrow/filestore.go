package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/filestoreserver"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/service/filestore"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// filestoreService serves Filestore (#2192) in the CLI process, gRPC and
// JSON on one port. Each instance is an NFS server in the host's Docker,
// on the kind network (internal/filestoreserver).
type filestoreService struct {
	cfg       config.Config
	tracing   *telemetry.Tracing
	calls     grpctransport.Observer
	interpose []grpc.UnaryServerInterceptor
	requests  func(rest.Request)
	// networks are Compute Engine's networks; nil checks none.
	networks func() filestore.Networks
	logf     func(format string, args ...any)
	server   *grpctransport.Server
	api      *filestore.Server
	backend  *filestoreserver.Docker
}

func newFilestoreService(cfg config.Config) *filestoreService {
	if !filestoreEnabled(cfg) {
		return nil
	}
	return &filestoreService{cfg: cfg}
}

// filestoreEnabled is whether `up` binds the Filestore port.
func filestoreEnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceFilestore) && cfg.Endpoints.Filestore >= 0
}

func (s *filestoreService) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

func (s *filestoreService) Name() string { return "filestore" }

// API is the server, available after Start.
func (s *filestoreService) API() *filestore.Server {
	if s == nil {
		return nil
	}
	return s.api
}

// Addr is the bound address.
func (s *filestoreService) Addr() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.Addr()
}

func (s *filestoreService) Start(ctx context.Context) error {
	state := ""
	if s.cfg.Mode == config.ModePersistent {
		state = filepath.Join(s.cfg.InstanceDir(), "filestore.json")
	}
	s.backend = &filestoreserver.Docker{Instance: s.cfg.Name, Cluster: s.cfg.ClusterName(), Dir: filepath.Join(s.cfg.InstanceDir(), "filestore", "backups")}
	s.backend.Logf = s.logf
	opts := filestore.Options{Backend: s.backend, Networks: s.networks}
	api, err := filestore.NewServer(state, opts)
	if err != nil {
		return err
	}
	s.api = api
	s.server = grpctransport.New(net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.Filestore)))
	s.server.Observe(s.calls)
	if s.tracing != nil {
		s.server.ServerOptions(s.tracing.ServerOptions()...)
	}
	for _, i := range s.interpose {
		s.server.Interpose(i)
	}
	var jsonAPI http.Handler = filestore.NewRESTHandler(api)
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
	// The instances' NFS servers start again in the background, as `stop`
	// left them (persistent mode).
	go api.Resume(context.Background())
	return nil
}

func (s *filestoreService) Stop(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	err := s.server.Stop(ctx)
	s.api.Close()
	// Persistent mode keeps the servers' volumes for the next `up`;
	// ephemeral mode removes servers, volumes and backups.
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return errors.Join(err, s.backend.Stop(sctx, s.cfg.Mode != config.ModePersistent))
}

// filestoreResetter deletes the instances, their servers and data,
// snapshots and backups.
type filestoreResetter struct{ svc *filestoreService }

func (r *filestoreResetter) Name() string { return "filestore" }

func (r *filestoreResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *filestoreResetter) ResetProject(_ context.Context, project string) error {
	api := r.svc.API()
	if api == nil {
		return errors.New("Filestore has not started")
	}
	return api.ResetProject(project)
}

// deleteFilestoreServers removes the instance's NFS servers and their
// volumes, for `cloudburrow delete`, as stop keeps them.
func deleteFilestoreServers(ctx context.Context, cfg config.Config, stdout io.Writer) {
	if err := filestoreserver.RemoveAll(ctx, filestoreserver.ExecRunner{}, cfg.Name); err != nil {
		fmt.Fprintf(stdout, "warning: Filestore's NFS servers were not all removed: %v\n", err)
	}
}
