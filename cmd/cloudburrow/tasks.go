package main

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/store"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"google.golang.org/grpc"
)

// tasksService serves Cloud Tasks.
//
// Unlike Pub/Sub and Cloud Storage, Cloud Tasks has no upstream backend, so it
// runs in the CLI process rather than as a cluster workload. Its dispatch
// targets are therefore addresses reachable from the host.
type tasksService struct {
	// calls reports each completed API call to the admin event log.
	calls grpctransport.Observer
	// faults injects the admin API's fault rules into every call (#306).
	faults grpc.UnaryServerInterceptor
	cfg    config.Config
	// observer receives each dispatch attempt, so the console can show the
	// attempt history a queue count does not.
	observer tasks.AttemptObserver
	server   *grpctransport.Server
	worker   *tasks.Worker
	db       store.Store
	store    *tasks.Store
}

// Store returns the Cloud Tasks store, available after Start.
func (t *tasksService) Store() *tasks.Store {
	if t == nil {
		return nil
	}
	return t.store
}

// newTasksService returns the Cloud Tasks service, or nil when not enabled.
//
// It acquires nothing: opening the state directory and binding the listener
// happen in Start, so a failure is reported through the coordinator and
// unwinds with everything else. Acquiring resources in a constructor also made
// a locked data directory mask an unrelated bind error.
func newTasksService(cfg config.Config) *tasksService {
	for _, s := range cfg.EnabledServices() {
		if s == config.ServiceTasks {
			return &tasksService{cfg: cfg}
		}
	}
	return nil
}

// observeAttempts attaches an observer before Start.
func (t *tasksService) observeAttempts(fn tasks.AttemptObserver) {
	if t == nil {
		return
	}
	t.observer = fn
}

// register adds the service and its dispatch worker to the coordinator.
func (t *tasksService) register(coord *lifecycle.Coordinator) {
	if t == nil {
		return
	}
	coord.Register(t)
}

func (t *tasksService) Name() string { return "tasks" }

// Addr returns the host address the Cloud Tasks API listens on.
func (t *tasksService) Addr() string {
	if t == nil || t.server == nil {
		return ""
	}
	return t.server.Addr()
}

// Worker returns the dispatch worker, available after Start.
func (t *tasksService) Worker() lifecycle.Worker {
	if t == nil {
		return nil
	}
	return t.worker
}

// Start opens state, registers the service and begins serving.
func (t *tasksService) Start(ctx context.Context) error {
	// Cloud Tasks state lives with the CLI. Memory mode is honest about not
	// surviving; persistent mode claims a directory under the state dir.
	var db store.Store = store.NewMemory()
	if t.cfg.Mode == config.ModePersistent {
		durable, err := store.OpenDurable(filepath.Join(t.cfg.StateDir, t.cfg.Name, "tasks"))
		if err != nil {
			return fmt.Errorf("open Cloud Tasks state: %w", err)
		}
		db = durable
	}
	t.db = db

	st := tasks.NewStore(db)
	t.store = st
	addr := net.JoinHostPort(t.cfg.BindAddress, strconv.Itoa(t.cfg.Endpoints.Tasks))
	t.server = grpctransport.New(addr)
	t.server.Observe(t.calls)
	if t.faults != nil {
		t.server.Interpose(t.faults)
	}
	if err := t.server.Register(func(g *grpc.Server) { tasks.NewGRPCServer(st).Register(g) }); err != nil {
		_ = db.Close()
		return err
	}
	if err := t.server.Start(ctx); err != nil {
		_ = db.Close()
		return err
	}

	clock := sched.RealClock{}
	dispatcher := tasks.NewDispatcher(st, nil, clock)
	if t.observer != nil {
		dispatcher = dispatcher.Observe(t.observer)
	}
	t.worker = tasks.NewWorker(st, dispatcher, clock, 200*time.Millisecond)
	return nil
}

// Stop closes the listener and releases the state directory, so a restart can
// claim it again.
func (t *tasksService) Stop(ctx context.Context) error {
	var err error
	if t.server != nil {
		err = t.server.Stop(ctx)
	}
	if t.db != nil {
		if closeErr := t.db.Close(); err == nil {
			err = closeErr
		}
		t.db = nil
	}
	return err
}
