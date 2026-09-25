package main

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strconv"

	"google.golang.org/grpc"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// secretsService serves Secret Manager.
//
// Like Cloud Tasks, Secret Manager has no upstream backend, so it runs in the
// CLI process rather than as a cluster workload.
type secretsService struct {
	// tracing, when on, spans gRPC calls (#313). The JSON API is not traced.
	tracing *telemetry.Tracing
	cfg     config.Config
	server  *secrets.Server
	// calls and requests report each completed gRPC call and JSON request to
	// the admin event log.
	calls    grpctransport.Observer
	requests func(rest.Request)
	// interpose run inside the call observer, in order: the request log
	// (#314), then fault injection (#306), so the log sees injected faults.
	interpose []grpc.UnaryServerInterceptor
	db        store.Store
	store     *secrets.Store
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
	// Secrets are stored in the cluster (#84): a Kubernetes Secret is what a
	// Cloud Run revision can actually reference through secretKeyRef, and a
	// payload held only in the CLI's own store could not be mounted at all.
	//
	// They go in the **workload** namespace, not the managed one, because
	// secretKeyRef is namespace-local: a Secret in `cloudburrow` is invisible
	// to a revision in `default`, and the pod fails with a missing-key error
	// that points nowhere near the cause. `reset` removes them by label
	// instead of by namespace.
	//
	// The CLI store remains the fallback for an instance with no cluster,
	// where mounting is impossible anyway.
	var db store.Store
	switch {
	case s.cfg.KubeconfigPath() != "":
		db = secrets.NewKubeStore(
			secrets.KubectlRunner{Kubeconfig: s.cfg.KubeconfigPath()},
			runadapter.WorkloadNamespace, s.cfg.Name)
	case s.cfg.Mode == config.ModePersistent:
		durable, err := store.OpenDurable(filepath.Join(s.cfg.StateDir, s.cfg.Name, "secrets"))
		if err != nil {
			return fmt.Errorf("open Secret Manager state: %w", err)
		}
		db = durable
	default:
		db = store.NewMemory()
	}
	s.db = db

	st := secrets.NewStore(db)
	s.store = st

	addr := net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.Secrets))
	s.server = secrets.NewServer(addr, st)
	s.server.Observe(s.calls, s.requests)
	if s.tracing != nil {
		s.server.ServerOptions(s.tracing.ServerOptions()...)
	}
	for _, i := range s.interpose {
		s.server.Interpose(i)
	}
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

// lazySecretResolver resolves secret references through the Secret Manager
// store once it exists.
//
// The lookup is deferred because the coordinator decides start order: the
// Cloud Run adapter is constructed before Secret Manager has opened its
// store, and capturing a nil store at wiring time would make every reference
// fail with "Secret Manager is not enabled" on an instance where it is.
type lazySecretResolver struct {
	svc *secretsService
}

func (l lazySecretResolver) ResolveSecretRef(project, secret, version string) (string, string, error) {
	st := l.svc.Store()
	if st == nil {
		return "", "", fmt.Errorf("Secret Manager has not started yet")
	}
	return st.ResolveSecretRef(project, secret, version)
}
