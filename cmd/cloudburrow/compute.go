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

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/hostguard"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
	"github.com/cloudburrow/cloudburrow/internal/service/gke"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
	"github.com/cloudburrow/cloudburrow/internal/service/vpcaccess"
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
	api      *compute.Server
	// vpcAccess is Serverless VPC Access (vpcaccess.googleapis.com,
	// #2140), served on this port beside /compute/v1/, as its connectors
	// live in the networks served here.
	vpcAccess *vpcaccess.Server
	// vms runs the VM instances (#1963); guestBase and metadataHost are
	// set by up once the cluster host is known.
	vms          *computeVMs
	guestBase    func() string
	metadataHost func() string
	cancel       context.CancelFunc
	// registry is the project registry, for Shared VPC's organization
	// rule (#2140); nil allows every project.
	registry func() *resourcemanager.Registry
	srv      *http.Server
	ln       net.Listener
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

// VPCAccess is Serverless VPC Access, available after Start; nil without
// compute.
func (s *computeService) VPCAccess() *vpcaccess.Server {
	if s == nil {
		return nil
	}
	return s.vpcAccess
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
	api, err := compute.New(compute.Options{State: state, Extra: s.instanceGroupManagers, ExtraGroups: s.gkeGroups, NetworkUsers: s.networkUsers, VMs: s.vms,
		ProjectOrganization: s.projectOrganization})
	if err != nil {
		return err
	}
	s.api = api
	// The schedules' and autoscalers' loops (#2139); with no Addr, Start
	// listens on nothing.
	if err := api.Start(ctx); err != nil {
		return err
	}
	vstate := ""
	if state != "" {
		vstate = filepath.Join(s.cfg.InstanceDir(), "vpcaccess.json")
	}
	va, err := vpcaccess.New(vpcaccess.Options{State: vstate, Networks: api})
	if err != nil {
		return err
	}
	s.vpcAccess = va
	ln, err := net.Listen("tcp", net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.Compute)))
	if err != nil {
		return fmt.Errorf("compute: %w", err)
	}
	s.ln = ln
	vrest := vpcaccess.NewRESTHandler(va)
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if vpcaccess.Handles(r.URL.Path) {
			vrest.ServeHTTP(w, r)
			return
		}
		api.ServeHTTP(w, r)
	})
	if s.wrapJSON != nil {
		h = s.wrapJSON(h)
	}
	if s.requests != nil {
		h = rest.Observe(h, s.requests)
	}
	s.srv = &http.Server{Handler: hostguard.Wrap(sysparams.Wrap(h, sysparams.Options{V1Errors: true})), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return nil
}

func (s *computeService) Stop(ctx context.Context) error {
	if s.api != nil {
		_ = s.api.Stop(ctx)
	}
	if s.vms != nil {
		s.vms.stopAll()
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// instanceGroupManagers answers Kubernetes Engine's instance group
// managers (#1913) on the Compute port, when the compute service runs
// without VMs; with them, the Compute API serves its own managed instance
// groups and lists GKE's beside them (gkeGroups, #2139).
func (s *computeService) instanceGroupManagers(w http.ResponseWriter, r *http.Request) bool {
	if !strings.Contains(r.URL.Path, "/instanceGroupManagers") || s.vms != nil {
		return false
	}
	api := s.gke.API()
	if api == nil {
		return false
	}
	return api.ServeInstanceGroupManagers(w, r)
}

// gkeGroups are a zone's Kubernetes Engine node-pool groups.
func (s *computeService) gkeGroups(project, zone string) map[string]map[string]any {
	if api := s.gke.API(); api != nil {
		return api.InstanceGroupManagers(project, zone)
	}
	return nil
}

func (s *computeService) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

// networkUsers are the Kubernetes Engine clusters on a network or subnet,
// which keep it from being deleted, as on Google.
func (s *computeService) networkUsers(rel string) []string {
	var users []string
	if api := s.gke.API(); api != nil {
		users = api.NetworkUsers(rel)
	}
	// Serverless VPC Access connectors keep their network and subnet
	// (#2140).
	if s.vpcAccess != nil {
		users = append(users, s.vpcAccess.NetworkUsers(rel)...)
	}
	return users
}

// The networks each service checks against: nil interfaces (not a nil
// *compute.Server in one) without Compute, so each checks nothing.

func (s *computeService) memorystoreNetworks() redisadmin.Networks {
	if api := s.API(); api != nil {
		return api
	}
	return nil
}

func (s *computeService) gkeNetworks() gke.Networks {
	if api := s.API(); api != nil {
		return api
	}
	return nil
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
	if err := api.ResetProject(project); err != nil {
		return err
	}
	if va := r.svc.VPCAccess(); va != nil {
		return va.ResetProject(project)
	}
	return nil
}

// projectOrganization is the organization a registered project is in, for
// Shared VPC (#2140): the local organization for a project under it or one
// of its folders, "" for a top-level project, and unknown for a project
// the registry does not keep.
func (s *computeService) projectOrganization(project string) (string, bool) {
	if s.registry == nil {
		return "", false
	}
	reg := s.registry()
	if reg == nil {
		return "", false
	}
	p, err := reg.Get(project)
	if err != nil {
		return "", false
	}
	if p.Parent == "" {
		return "", true
	}
	return resourcemanager.OrganizationName, true
}
