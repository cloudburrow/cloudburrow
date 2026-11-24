package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/hostguard"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
	"github.com/cloudburrow/cloudburrow/internal/service/filestore"
	"github.com/cloudburrow/cloudburrow/internal/service/gke"
	"github.com/cloudburrow/cloudburrow/internal/service/networkconnectivity"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
	"github.com/cloudburrow/cloudburrow/internal/sysparams"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// computeService serves the Compute Engine API (#1936, #1963)
// in the CLI process, on a port of its own (endpoints.compute), as
// compute.googleapis.com is an API of its own. With Kubernetes Engine
// enabled too, it also answers the node pools' instance group managers
// (gke.Server.ServeInstanceGroupManagers), which the GKE port answers on
// its own without Compute, so an instance without compute keeps today's
// behaviour and cloudburrow terraform points compute_custom_endpoint here
// when compute is on.
type computeService struct {
	cfg      config.Config
	requests func(rest.Request)
	// wrapJSON wraps the API (IAM enforcement, #1879); nil is none.
	wrapJSON func(http.Handler) http.Handler
	gke      *gkeService
	// kms is this instance's Cloud KMS: disks' customer-managed
	// encryption keys (#2206); nil when kms is not in --services.
	kms *kmsService
	api *compute.Server
	// vms runs the VM instances (#1963); guestBase and metadataHost are
	// set by up once the cluster host is known.
	vms          *computeVMs
	guestBase    func() string
	metadataHost func() string
	cancel       context.CancelFunc
	srv          *http.Server
	ln           net.Listener
	// sql is the Cloud SQL Admin API, whose private IPs the VPC networks
	// reach (#2141); nil without Cloud SQL.
	sql    func() *sqladmin.Server
	svcIPs *serviceIPs
	// nc is the Network Connectivity API (#2141), served on this port,
	// and ncGRPC its gRPC server.
	nc     *networkconnectivity.Server
	ncGRPC *grpc.Server
}

func newComputeService(cfg config.Config) *computeService {
	if !computeEnabled(cfg) {
		return nil
	}
	return &computeService{cfg: cfg}
}

// computeEnabled is whether `up` binds the Compute Engine API's port.
func computeEnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceCompute) && cfg.Endpoints.Compute >= 0
}

func (s *computeService) Name() string { return "compute" }

// API is the server, available after Start; nil without compute.
func (s *computeService) API() *compute.Server {
	if s == nil {
		return nil
	}
	return s.api
}

// Addr is the bound address.
func (s *computeService) Addr() string {
	if s == nil || s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

func (s *computeService) Start(ctx context.Context) error {
	state := ""
	if s.cfg.Mode == config.ModePersistent {
		state = filepath.Join(s.cfg.InstanceDir(), "compute.json")
	}
	bg, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.vms = newComputeVMs(s.cfg, bg)
	s.vms.guestBase = func() string {
		if s.guestBase == nil {
			return ""
		}
		return s.guestBase()
	}
	s.vms.metadataHost = func() string {
		if s.metadataHost == nil {
			return ""
		}
		return s.metadataHost()
	}
	s.svcIPs = &serviceIPs{kubeconfig: s.cfg.KubeconfigPath(), namespace: s.cfg.Cluster.Namespace}
	api, err := compute.New(compute.Options{State: state, Extra: s.instanceGroupManagers, NetworkUsers: s.networkUsers, VMs: s.vms,
		PrivateServices: s.privateServices, ServiceAttachment: s.serviceAttachment})
	if err != nil {
		return err
	}
	s.api = api
	// diskEncryptionKey.kmsKeyName (#2206) wraps a disk's data key with
	// this instance's Cloud KMS, in process.
	if s.kms != nil {
		api.UseKMS(sqlAdminKMS{svc: s.kms})
	}
	// Snapshot schedules (#2206) take their snapshots while up runs.
	go api.RunSnapshotSchedules(bg)
	ln, err := net.Listen("tcp", net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.Compute)))
	if err != nil {
		return fmt.Errorf("compute: %w", err)
	}
	s.ln = ln
	var h http.Handler = api
	if s.wrapJSON != nil {
		h = s.wrapJSON(h)
	}
	// The Network Connectivity API (#2141), gRPC and JSON, on this port.
	h, err = s.startNetworkConnectivity(api, h)
	if err != nil {
		_ = ln.Close()
		return err
	}
	if s.requests != nil {
		h = rest.Observe(h, s.requests)
	}
	s.srv = &http.Server{Handler: h2c.NewHandler(hostguard.Wrap(sysparams.Wrap(h, sysparams.Options{V1Errors: true})), &http2.Server{}),
		ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return nil
}

func (s *computeService) Stop(ctx context.Context) error {
	if s.vms != nil {
		s.vms.stopAll()
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.srv == nil {
		return nil
	}
	defer s.stopNetworkConnectivity()
	return s.srv.Shutdown(ctx)
}

// instanceGroupManagers answers Kubernetes Engine's instance group
// managers (#1913) on the Compute port.
func (s *computeService) instanceGroupManagers(w http.ResponseWriter, r *http.Request) bool {
	if !strings.Contains(r.URL.Path, "/instanceGroupManagers") {
		return false
	}
	api := s.gke.API()
	if api == nil {
		return false
	}
	return api.ServeInstanceGroupManagers(w, r)
}

func (s *computeService) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

// networkUsers are the Kubernetes Engine clusters on a network or subnet,
// which keep it from being deleted, as on Google.
func (s *computeService) networkUsers(rel string) []string {
	out := s.sqlNetworkUsers(rel)
	if api := s.gke.API(); api != nil {
		out = append(out, api.NetworkUsers(rel)...)
	}
	return out
}

// The networks each service checks against: nil interfaces (not a nil
// *compute.Server in one) without Compute, so each checks nothing.

func (s *computeService) memorystoreNetworks() redisadmin.Networks {
	if api := s.API(); api != nil {
		return api
	}
	return nil
}

func (s *computeService) filestoreNetworks() filestore.Networks {
	if api := s.API(); api != nil {
		return api
	}
	return nil
}

func (s *computeService) gkeNetworks() gke.Networks {
	if api := s.API(); api != nil {
		return gkeComputeNetworks{api}
	}
	return nil
}

// gkeComputeNetworks are Compute Engine's networks as Kubernetes Engine
// checks and lists them (ListUsableSubnetworks, #2100).
type gkeComputeNetworks struct{ *compute.Server }

func (n gkeComputeNetworks) UsableSubnetworks(project string) []gke.UsableSubnet {
	var out []gke.UsableSubnet
	for _, sn := range n.ListSubnetworks(project) {
		out = append(out, gke.UsableSubnet{Subnetwork: sn.Name, Network: sn.Network, IPCIDRRange: sn.IPCIDRRange, Secondary: sn.Secondary})
	}
	return out
}

func (s *computeService) sqlNetworks() sqladmin.Networks {
	if api := s.API(); api != nil {
		return api
	}
	return nil
}

// computeResetter forgets the networks, as a reset forgets every
// service's resources.
type computeResetter struct{ svc *computeService }

func (r *computeResetter) Name() string { return "compute" }

func (r *computeResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *computeResetter) ResetProject(_ context.Context, project string) error {
	api := r.svc.API()
	if api == nil {
		return errors.New("the Compute Engine API has not started")
	}
	// The Network Connectivity API's policies and maps (#2141) first:
	// they remove the endpoints they made.
	if nc := r.svc.NetworkConnectivity(); nc != nil {
		if err := nc.ResetProject(project); err != nil {
			return err
		}
	}
	return api.ResetProject(project)
}
