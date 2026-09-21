package main

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strconv"

	"github.com/identity-wael/cloudburrow/internal/config"
	"github.com/identity-wael/cloudburrow/internal/lifecycle"
	"github.com/identity-wael/cloudburrow/internal/service/secrets"
	"github.com/identity-wael/cloudburrow/internal/store"
)

// secretsService serves Secret Manager.
//
// Like Cloud Tasks, Secret Manager has no upstream backend, so it runs in the
// CLI process rather than as a cluster workload.
type secretsService struct {
	cfg    config.Config
	server *secrets.Server
	db     store.Store
	store  *secrets.Store
}

// Store returns the Secret Manager store, available after Start.
func (s *secretsService) Store() *secrets.Store {
	if s == nil {
		return nil
	}
	return s.store
}

// newSecretsService returns the service, or nil when it is not enabled.
//
// It acquires nothing: opening the state directory and binding the listener
// happen in Start, so a failure is reported through the coordinator and
// unwinds with everything else.
func newSecretsService(cfg config.Config) *secretsService {
	for _, svc := range cfg.EnabledServices() {
		if svc == config.ServiceSecrets {
			return &secretsService{cfg: cfg}
		}
	}
	return nil
}

func (s *secretsService) register(coord *lifecycle.Coordinator) {
	if s == nil {
		return
	}
	coord.Register(s)
}

func (s *secretsService) Name() string { return "secretmanager" }

// Addr returns the host address the Secret Manager API listens on.
func (s *secretsService) Addr() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.Addr()
}

// Start opens state, registers the service and begins serving.
func (s *secretsService) Start(ctx context.Context) error {
	var db store.Store = store.NewMemory()
	if s.cfg.Mode == config.ModePersistent {
		durable, err := store.OpenDurable(filepath.Join(s.cfg.StateDir, s.cfg.Name, "secrets"))
		if err != nil {
			return fmt.Errorf("open Secret Manager state: %w", err)
		}
		db = durable
	}
	s.db = db

	st := secrets.NewStore(db)
	s.store = st

	addr := net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.Secrets))
	s.server = secrets.NewServer(addr, st)
	if err := s.server.Start(ctx); err != nil {
		_ = db.Close()
		return err
	}
	return nil
}

// Stop closes the listener and releases the state directory, so a restart can
// claim it again.
func (s *secretsService) Stop(ctx context.Context) error {
	var err error
	if s.server != nil {
		err = s.server.Stop(ctx)
	}
	if s.db != nil {
		if closeErr := s.db.Close(); err == nil {
			err = closeErr
		}
	}
	return err
}
