package main

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"time"

	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// memorystoreAdminService serves the Memorystore for Redis admin API
// (google.cloud.redis.v1.CloudRedis, #1302) in the CLI process, over the
// Valkey server `--services memorystore` runs, reached through its tunnel.
// gRPC and JSON share one port, as redis.googleapis.com serves both.
type memorystoreAdminService struct {
	cfg       config.Config
	tunnel    func() *netfwd.Forwarder
	calls     grpctransport.Observer
	interpose []grpc.UnaryServerInterceptor
	requests  func(rest.Request)
	server    *grpctransport.Server
	api       *redisadmin.Server
}

func newMemorystoreAdminService(cfg config.Config, tunnel func() *netfwd.Forwarder) *memorystoreAdminService {
	if !serviceEnabled(cfg, config.ServiceMemorystore) || cfg.Endpoints.MemorystoreAdmin < 0 {
		return nil
	}
	return &memorystoreAdminService{cfg: cfg, tunnel: tunnel}
}

func (s *memorystoreAdminService) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

func (s *memorystoreAdminService) Name() string { return "memorystore-admin" }

// Addr is the API's bound address.
func (s *memorystoreAdminService) Addr() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.Addr()
}

// memorystoreBackend is the Valkey server behind the tunnel.
func memorystoreBackend(tunnel func() *netfwd.Forwarder) redisadmin.Valkey {
	return redisadmin.Valkey{Addr: func() string {
		if f := tunnel(); f != nil {
			return f.HostAddr()
		}
		return ""
	}}
}

// memorystoreInstance describes the one instance as the host sees it: at
// the tunnel's host address, addr, when it has one.
func memorystoreInstance(cfg config.Config, created time.Time, addr func() string) redisadmin.Instance {
	return redisadmin.Instance{Project: cfg.DefaultProject(), Host: cfg.BindAddress,
		Port: int32(cfg.Endpoints.Memorystore), Created: created, Address: addr}
}

func (s *memorystoreAdminService) Start(ctx context.Context) error {
	be := memorystoreBackend(s.tunnel)
	s.api = redisadmin.NewServer(memorystoreInstance(s.cfg, time.Now(), be.Addr), be)
	addr := net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.MemorystoreAdmin))
	s.server = grpctransport.New(addr)
	s.server.Observe(s.calls)
	for _, i := range s.interpose {
		s.server.Interpose(i)
	}
	var jsonAPI http.Handler = redisadmin.NewRESTHandler(s.api.Register)
	if s.requests != nil {
		jsonAPI = rest.Observe(jsonAPI, s.requests)
	}
	s.server.ServeHTTP(jsonAPI)
	if err := s.server.Register(func(g *grpc.Server) { s.api.Register(g) }); err != nil {
		return err
	}
	return s.server.Start(ctx)
}

func (s *memorystoreAdminService) Stop(ctx context.Context) error {
	if s.server != nil {
		return s.server.Stop(ctx)
	}
	return nil
}
