package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"

	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/service/kms"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/store"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
)

// kmsService serves the Cloud KMS resource API (#309) in the CLI process.
// Key material is kept in owned Kubernetes Secrets in the managed namespace
// when there is a cluster, as Secret Manager's payloads are, so it persists
// the same way and /admin/reset deletes it by label.
type kmsService struct {
	cfg    config.Config
	calls  grpctransport.Observer
	server *grpctransport.Server
	db     store.Store
	kube   *kms.KubeStore
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
	if err := s.server.Register(func(g *grpc.Server) { kms.NewServer(s.db).Register(g) }); err != nil {
		return err
	}
	return s.server.Start(ctx)
}

func (s *kmsService) Stop(ctx context.Context) error {
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
