package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	pubsub "cloud.google.com/go/pubsub/v2"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/service/scheduler"
	"github.com/cloudburrow/cloudburrow/internal/store"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
)

// schedulerService serves Cloud Scheduler (#302) in the CLI process, as
// Cloud Tasks is served: there is no upstream emulator to run in the cluster.
// HTTP targets are therefore reached from the host, and Pub/Sub targets are
// published to the local emulator through its tunnel.
type schedulerService struct {
	cfg    config.Config
	calls  grpctransport.Observer
	pubsub func() *netfwd.Forwarder
	server *grpctransport.Server
	runner *scheduler.Runner
	db     store.Store
	store  *scheduler.Store
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
	s.runner = scheduler.NewRunner(s.store, nil, clock, s.publish, time.Second)

	addr := net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.Scheduler))
	s.server = grpctransport.New(addr)
	s.server.Observe(s.calls)
	if err := s.server.Register(func(g *grpc.Server) { scheduler.NewGRPCServer(s.store, s.runner, clock).Register(g) }); err != nil {
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
