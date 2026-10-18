package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strconv"

	"google.golang.org/grpc"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/kms"
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
	// kube is the store when it is Kubernetes Secrets, for secretsForget.
	kube *secrets.KubeStore
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
	// stopSweep ends the expiry sweep (#1225); swept closes when it has.
	stopSweep context.CancelFunc
	swept     chan struct{}
	// pubsub is the tunnel to the instance's Pub/Sub, which a secret's
	// event notifications are published to (#1259); nil when Pub/Sub is not
	// enabled, and then a secret with topics is refused.
	pubsub func() *netfwd.Forwarder
	// kms is the instance's Cloud KMS, which encrypts the versions of a
	// secret with customer-managed keys (#1453); nil when KMS is not enabled.
	kms func() *kms.Server
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

// secretsForget makes Secret Manager honour --mode (#483). Its Secrets live
// in the workload namespace, which survives `stop` and an ephemeral `up`, so
// an ephemeral run deletes what earlier runs left, and a persistent run
// deletes what an ephemeral run left. Then it reads the rest, migrating the
// layout before #1596. Registered after the cluster, which the
// service itself starts before, and before `up` reports ready.
type secretsForget struct{ svc *secretsService }

func (f secretsForget) Name() string { return "secretmanager-forget" }

func (f secretsForget) Start(ctx context.Context) error {
	if f.svc.kube == nil {
		return nil
	}
	if err := f.svc.kube.Forget(ctx); err != nil {
		return err
	}
	// Read what is left, and move a secret stored before #1596 to a
	// Kubernetes Secret per version, before `up` reports ready.
	if err := f.svc.kube.Load(); err != nil {
		return fmt.Errorf("read Secret Manager state: %w", err)
	}
	return nil
}

func (f secretsForget) Stop(context.Context) error { return nil }

// registerForget registers secretsForget; call it after the cluster component.
func (s *secretsService) registerForget(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(secretsForget{svc: s})
	}
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
		kube := secrets.NewKubeStore(
			k8s.New(s.cfg.KubeconfigPath(), "", runadapter.WorkloadNamespace), s.cfg.Name)
		if s.cfg.Mode == config.ModeEphemeral {
			// This run's writes carry its epoch, so secretsForget can delete
			// what earlier runs left without touching them (#483).
			epoch := make([]byte, 8)
			_, _ = rand.Read(epoch)
			kube.SetEpoch(hex.EncodeToString(epoch))
		}
		s.kube, db = kube, kube
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
	if s.pubsub != nil && slices.Contains(s.cfg.EnabledServices(), config.ServicePubSub) {
		st.SetPublisher(func(ctx context.Context, topic string, data []byte, attrs map[string]string) error {
			f := s.pubsub()
			if f == nil || f.HostAddr() == "" {
				return errors.New("Pub/Sub has not started on this instance")
			}
			return publishLocal(ctx, f, topic, data, attrs)
		})
	}
	if s.kms != nil {
		st.SetKeyService(func() secrets.KeyService {
			if api := s.kms(); api != nil {
				return api
			}
			return nil
		})
	}
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
	// Secrets are deleted at their expire_time (#1225). Reads already treat
	// an expired secret as gone; the sweep deletes it from storage, the
	// Kubernetes Secret with it. Before the cluster exists a sweep fails
	// and Run retries.
	sweepCtx, cancel := context.WithCancel(context.Background())
	s.stopSweep, s.swept = cancel, make(chan struct{})
	go func() {
		defer close(s.swept)
		st.Run(sweepCtx)
	}()
	return nil
}

// Stop closes the listener and releases the state directory, so a restart can
// claim it again.
func (s *secretsService) Stop(ctx context.Context) error {
	if s.stopSweep != nil {
		s.stopSweep()
		select {
		case <-s.swept:
		case <-ctx.Done():
		}
	}
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
