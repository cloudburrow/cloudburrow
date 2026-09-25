package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"

	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/service/kms"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/store"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// kmsService serves the Cloud KMS resource API (#309) in the CLI process.
// Key material is kept in owned Kubernetes Secrets in the managed namespace
// when there is a cluster, as Secret Manager's payloads are, so it persists
// the same way and /admin/reset deletes it by label.
type kmsService struct {
	cfg   config.Config
	calls grpctransport.Observer
	// interpose run inside the call observer, in order: the request log
	// (#314), then fault injection (#306), so the log sees injected faults
	// (#392).
	interpose []grpc.UnaryServerInterceptor
	// requests reports each JSON request to the admin event log (#414).
	requests func(rest.Request)
	server   *grpctransport.Server
	db       store.Store
	kube     *kms.KubeStore
	// api is the KMS server; its sweep moves versions to DESTROYED at their
	// destroy_time (#403), and runs from Start until Stop.
	api       *kms.Server
	stopSweep context.CancelFunc
	swept     chan struct{}
	// clock is the sweep's clock; nil means the real one. Tests set it.
	clock sched.Clock
}

func newKMSService(cfg config.Config) *kmsService {
	if !serviceEnabled(cfg, config.ServiceKMS) {
		return nil
	}
	return &kmsService{cfg: cfg}
}

func (s *kmsService) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

func (s *kmsService) Name() string { return "kms" }

// Addr is the API's bound address.
func (s *kmsService) Addr() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.Addr()
}

func (s *kmsService) Start(ctx context.Context) error {
	switch {
	case s.db != nil:
		// Already chosen: a test hands the service its store.
	case s.cfg.KubeconfigPath() != "":
		s.kube = kms.NewKubeStore(secrets.KubectlRunner{Kubeconfig: s.cfg.KubeconfigPath()}, s.cfg.Cluster.Namespace, s.cfg.Name)
		s.db = s.kube
	case s.cfg.Mode == config.ModePersistent:
		d, err := store.OpenDurable(filepath.Join(s.cfg.StateDir, s.cfg.Name, "kms"))
		if err != nil {
			return fmt.Errorf("open Cloud KMS state: %w", err)
		}
		s.db = d
	default:
		s.db = store.NewMemory()
	}
	addr := net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.KMS))
	s.server = grpctransport.New(addr)
	s.server.Observe(s.calls)
	for _, i := range s.interpose {
		s.server.Interpose(i)
	}
	clock := s.clock
	if clock == nil {
		clock = sched.RealClock{}
	}
	s.api = kms.NewServerWithClock(s.db, clock)
	// Sweep once before serving, so a version that fell due while nothing
	// was running is stored DESTROYED at once; Run keeps it so. The store may
	// not be reachable yet: this service starts before the cluster its
	// Secrets live in, so a failed sweep is left to Run, which retries.
	// Nothing depends on it meanwhile, since every read computes the
	// effective state itself.
	_, _ = s.api.Sweep()
	// One port for gRPC and JSON, as cloudkms.googleapis.com (#414).
	var jsonAPI http.Handler = kms.NewRESTHandler(s.api)
	if s.requests != nil {
		jsonAPI = rest.Observe(jsonAPI, s.requests)
	}
	s.server.ServeHTTP(jsonAPI)
	if err := s.server.Register(func(g *grpc.Server) { s.api.Register(g) }); err != nil {
		return err
	}
	if err := s.server.Start(ctx); err != nil {
		return err
	}
	sweepCtx, cancel := context.WithCancel(context.Background())
	s.stopSweep, s.swept = cancel, make(chan struct{})
	go func() {
		defer close(s.swept)
		s.api.Run(sweepCtx)
	}()
	return nil
}

func (s *kmsService) Stop(ctx context.Context) error {
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
		if cerr := s.db.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

// kmsResetter deletes every key ring, key and version.
type kmsResetter struct{ svc *kmsService }

func (r *kmsResetter) Name() string { return "kms" }

func (r *kmsResetter) Reset(context.Context) error {
	s := r.svc
	if s == nil || s.db == nil {
		return errors.New("Cloud KMS has not started")
	}
	if s.kube != nil {
		return s.kube.DeleteAll()
	}
	keys, err := s.db.List("kms/")
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := s.db.Delete(k); err != nil {
			return err
		}
	}
	return nil
}
