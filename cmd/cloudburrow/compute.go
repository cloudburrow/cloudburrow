package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/authplatform"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/filestoreserver"
	"github.com/cloudburrow/cloudburrow/internal/hostguard"
	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/service/certificatemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
	"github.com/cloudburrow/cloudburrow/internal/service/filestore"
	"github.com/cloudburrow/cloudburrow/internal/service/gke"
	"github.com/cloudburrow/cloudburrow/internal/service/iap"
	"github.com/cloudburrow/cloudburrow/internal/service/networkconnectivity"
	"github.com/cloudburrow/cloudburrow/internal/service/networksecurity"
	"github.com/cloudburrow/cloudburrow/internal/service/networkservices"
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
	// privateca is Certificate Authority Service, when served: Secure Web
	// Proxy's TLS inspection leaves (#2160); nil inspects nothing.
	privateca *privateCAService
	api       *compute.Server
	// vpcAccess is Serverless VPC Access (vpcaccess.googleapis.com,
	// #2140), served on this port beside /compute/v1/, as its connectors
	// live in the networks served here.
	vpcAccess *vpcaccess.Server
	// certManager is Certificate Manager (certificatemanager.googleapis.com)
	// and iap Identity-Aware Proxy (iap.googleapis.com), #2160, served on
	// this port too: the load balancers here serve their certificate maps
	// and enforce IAP on backend services (computecertiap.go).
	certManager *certificatemanager.Server
	iap         *iap.Server
	// netSec is Network Security (networksecurity.googleapis.com, #2160):
	// the server TLS policies, client TLS policies and authorization
	// policies the load balancers here read (computenetsec.go).
	netSec *networksecurity.Server
	// netSvc is Network Services (networkservices.googleapis.com, #2160):
	// Secure Web Proxy's gateways, each a forward proxy here, and Service
	// Extensions' authorization extensions (computenetsvc.go).
	netSvc *networkservices.Server
	// apisGRPC is the gRPC surface of the APIs on this port (computegrpc.go).
	apisGRPC *computeGRPC
	// iapEval and creds are the host's IAM evaluator and credentials, which
	// IAP checks requests with; up sets them (useIAP).
	iapEval *iameval.Evaluator
	creds   *metadata.Credentials
	// authPlatform and metaAddr are IAP's browser sign-in: Google Auth
	// Platform's store and the metadata server's address, where the
	// authorization endpoint is (useIAPSignIn).
	authPlatform *authplatform.Store
	metaAddr     func() string
	// callerOf names a bearer token's caller (the IAM enforcer's), for
	// IAP TCP forwarding's tunnels; tunnel is their relay.
	callerOf func(string) iameval.Caller
	tunnel   *iap.Tunnel
	// vms runs the VM instances (#1963); guestBase is set by up once the
	// cluster host is known, and tokens (the instance's credentials) mint
	// a VM's service account tokens on its metadata server (#2177).
	vms       *computeVMs
	guestBase func() string
	tokens    compute.GuestTokens
	// relayAt is the VM relay's in-cluster address (#2160), set by up.
	relayAt func() string
	// firewallLog writes Firewall Rules Logging records to this
	// instance's Cloud Logging (#2180), set by up when it is served.
	firewallLog func(compute.FirewallLogEntry)
	// opsAgentHosts are the in-cluster Cloud Logging and Cloud Monitoring
	// addresses a VM's Ops Agent writes to (#2139, computeopsagent.go).
	opsAgentHosts func() (logging, monitoring string)
	// guestReach is where a VM's pod reaches a loopback listener of this
	// machine (clusterHost.Reach, #2140).
	guestReach func(loopback string) string
	// natLogs writes Cloud NAT's nat_flows entries to Cloud Logging
	// (computenat.go); set by up while Logging is served.
	natLogs func(context.Context, *loggingpb.WriteLogEntriesRequest) error
	// storage is the Cloud Storage port's host address, for image import
	// (#2139); set by up, "" or nil when Storage is not served.
	storage func() string
	cancel  context.CancelFunc
	// sql is the Cloud SQL Admin API, whose private IPs the VPC networks
	// reach (#2141); nil without Cloud SQL.
	sql    func() *sqladmin.Server
	svcIPs *serviceIPs
	// nc is the Network Connectivity API (#2141), served on this port,
	// and ncGRPC its gRPC server.
	nc     *networkconnectivity.Server
	ncGRPC *grpc.Server
	// registry is the project registry, for Shared VPC's organization
	// rule (#2140); nil allows every project.
	registry func() *resourcemanager.Registry
	srv      *http.Server
	ln       net.Listener
	// handler is the port's own handler, before IAM enforcement and the
	// request log: the console's calls to the APIs served beside
	// /compute/v1/ (Certificate Manager and IAP, #2160) go through it.
	handler http.Handler
}

// v1Handler is the Compute port's handler as the console calls it: the
// APIs served on the port, unwrapped; the Compute API's alone before Start
// has built it (the console's tests). nil without compute.
func (s *computeService) v1Handler() http.Handler {
	switch {
	case s == nil:
		return nil
	case s.handler != nil:
		return s.handler
	case s.api != nil:
		return s.api
	}
	return nil
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
// RelayAddr is the VM relay's bound address, "" when it does not listen.
func (s *computeService) RelayAddr() string {
	if s == nil || s.api == nil {
		return ""
	}
	return s.api.RelayAddr()
}

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
	s.svcIPs = &serviceIPs{kubeconfig: s.cfg.KubeconfigPath(), namespace: s.cfg.Cluster.Namespace}
	dir := func(name string) string {
		if state == "" {
			return ""
		}
		return filepath.Join(s.cfg.InstanceDir(), name)
	}
	cm, err := certificatemanager.New(certificatemanager.Options{State: dir("certificatemanager.json")})
	if err != nil {
		return err
	}
	s.certManager = cm
	ia, err := iap.New(iap.Options{State: dir("iap.json")})
	if err != nil {
		return err
	}
	s.iap = ia
	ns, err := networksecurity.New(networksecurity.Options{State: dir("networksecurity.json")})
	if err != nil {
		return err
	}
	s.netSec = ns
	// The xDS API's TLS bootstrap files (#2160): its CA and each
	// project's workload credentials, beside the state.
	xdsCredDir := ""
	if state != "" {
		xdsCredDir = filepath.Join(filepath.Dir(state), "xds-credentials")
	}
	nsv, err := networkservices.New(networkservices.Options{State: dir("networkservices.json"), Bind: s.cfg.BindAddress})
	if err != nil {
		return err
	}
	s.netSvc = nsv
	s.vms.opsAgentHosts = s.telemetryHosts
	api, err := compute.New(compute.Options{State: state, Extra: s.instanceGroupManagers, ExtraGroups: s.gkeGroups, NetworkUsers: s.networkUsers, VMs: s.vms,
		PrivateServices: s.privateServices, ServiceAttachment: s.serviceAttachment,
		ProjectOrganization: s.projectOrganization, ReadObject: imageObjectReader(func() string {
			if s.storage == nil {
				return ""
			}
			return s.storage()
		}), LB: &computeLB{cfg: s.cfg, vms: s.vms},
		CertMaps: cm, IAP: computeIAPGate{svc: s}, NetSec: computeNetSec{ns: ns, cm: cm, endpoints: xdsEndpointPolicies(nsv)}, XDSCredentialDir: xdsCredDir, SourceTags: s.sourceTags,
		RelayBind: s.cfg.BindAddress, FirewallLog: func(e compute.FirewallLogEntry) {
			if s.firewallLog != nil {
				s.firewallLog(e)
			}
		},
		GuestReach: func(loopback string) string {
			if s.guestReach == nil {
				return ""
			}
			return s.guestReach(loopback)
		}, Tokens: s.tokens, IntegrityEvents: s.integrityEvent})
	if err != nil {
		return err
	}
	s.api = api
	api.UseRelayAddress(func() string {
		if s.relayAt == nil {
			return ""
		}
		return s.relayAt()
	})
	cm.UseIssuer(api)
	cm.UseTargets(func(project string) []certificatemanager.Target { return certMapTargets(api, project) })
	ia.SetBackendServices(func(rel string) bool { return backendServiceExists(api, rel) })
	ia.SetNormalizer(s.iapNormalize)
	s.tunnel = s.newIAPTunnel(api, ia)
	useNetSec(api, ns, cm, nsv)
	s.useNetSvc(api, ns, nsv, cm)
	nsv.Start()
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
	// Cloud Run's pods onto VPC networks through connectors and Direct
	// VPC egress (#2140, computerunegress.go).
	s.startRunEgress(bg)
	s.startNAT(bg)
	// Cloud Router's BGP sessions with router appliances (#2140,
	// computebgp.go).
	s.startRouterBGP(bg)
	ln, err := net.Listen("tcp", net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.Compute)))
	if err != nil {
		return fmt.Errorf("compute: %w", err)
	}
	// A Shielded VM's HTTPS metadata server (#2179): its guest's relay
	// carries port 443 to this port, TLS and all, after a line naming the
	// instance, which the API's own HTTP server would answer 400.
	ln = api.MDSListener(ln)
	s.ln = ln
	// The xDS API over TLS (#2160), as proxyless gRPC reaches Google's:
	// a TLS handshake on this port gets a certificate from the load
	// balancing CA for its bound address, and HTTP/2.
	if cfg, err := api.XDSServerTLS([]string{s.cfg.BindAddress}); err == nil {
		ln = newTLSSniffListener(ln, cfg)
	}
	vrest := vpcaccess.NewRESTHandler(va)
	cmrest := certificatemanager.NewRESTHandler(cm)
	iaprest := iap.NewRESTHandler(ia)
	nsrest := networksecurity.NewRESTHandler(ns)
	nsvrest := networkservices.NewRESTHandler(nsv)
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if iap.Handles(r.URL.Path) {
			iaprest.ServeHTTP(w, r)
			return
		}
		if ns.Handles(r) {
			nsrest.ServeHTTP(w, r)
			return
		}
		if nsv.Handles(r) {
			nsvrest.ServeHTTP(w, r)
			return
		}
		if cm.Handles(r) {
			cmrest.ServeHTTP(w, r)
			return
		}
		if vpcaccess.Handles(r.URL.Path) {
			vrest.ServeHTTP(w, r)
			return
		}
		api.ServeHTTP(w, r)
	})
	s.handler = h
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
	jsonAPI := sysparams.Wrap(h, sysparams.Options{V1Errors: true})
	// gRPC over HTTP/2 without TLS is the xDS API of proxyless gRPC
	// (#2160), served beside the JSON API, ahead of its system
	// parameters.
	var protos http.Protocols
	protos.SetHTTP1(true)
	protos.SetUnencryptedHTTP2(true)
	protos.SetHTTP2(true)
	// Network Security's, Network Services', Certificate Manager's, IAP's
	// and Serverless VPC Access's gRPC APIs beside it (computegrpc.go).
	s.apisGRPC = newComputeGRPC(api.XDSHandler(),
		computeGRPCAPI{pkg: "google.cloud.networksecurity.v1", g: computeGRPCServer(ns.Register), owns: ns.HasOperation, isLoc: true},
		computeGRPCAPI{pkg: "google.cloud.networkservices.v1", g: computeGRPCServer(nsv.Register), owns: nsv.HasOperation},
		computeGRPCAPI{pkg: "google.cloud.certificatemanager.v1", g: computeGRPCServer(cm.Register), owns: cm.HasOperation},
		computeGRPCAPI{pkg: "google.cloud.iap.v1", g: computeGRPCServer(ia.Register)},
		// The Network Connectivity API (#2141), whose operations it owns.
		computeGRPCAPI{pkg: "google.cloud.networkconnectivity.v1", g: s.ncGRPC, owns: s.nc.HasOperation},
		computeGRPCAPI{pkg: "google.cloud.vpcaccess.v1", g: computeGRPCServer(va.Register), owns: func(string) bool { return true }})
	apisGRPC := s.apisGRPC
	guarded := hostguard.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			apisGRPC.ServeHTTP(w, r)
			return
		}
		jsonAPI.ServeHTTP(w, r)
	}))
	// IAP TCP forwarding (#2160): gcloud's
	// --iap-tunnel-url-override=ws://THIS_PORT. gcloud opens the
	// WebSocket with Origin: bot:iap-tunneler, which is no browser
	// origin, so the tunnel takes it beside the usual cross-site rule;
	// the Host check still applies.
	s.srv = &http.Server{Handler: hostguard.WrapHost(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(iap.TunnelPaths, r.URL.Path) {
			guarded.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Origin") != iap.TunnelOrigin && !hostguard.CrossSiteAllowed(r) {
			hostguard.RefuseOrigin(w, r)
			return
		}
		s.tunnel.ServeHTTP(w, r)
	})), ReadHeaderTimeout: 10 * time.Second, Protocols: &protos}
	go func() { _ = s.srv.Serve(ln) }()
	return nil
}

func (s *computeService) Stop(ctx context.Context) error {
	if s.api != nil {
		_ = s.api.Stop(ctx)
	}
	if s.netSvc != nil {
		s.netSvc.Stop()
	}
	if s.apisGRPC != nil {
		s.apisGRPC.Stop()
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
	defer s.stopNetworkConnectivity()
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
	out := s.sqlNetworkUsers(rel)
	if api := s.gke.API(); api != nil {
		out = append(out, api.NetworkUsers(rel)...)
	}
	// Serverless VPC Access connectors keep their network and subnet
	// (#2140).
	if s.vpcAccess != nil {
		out = append(out, s.vpcAccess.NetworkUsers(rel)...)
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

// filestoreClients are the VMs whose NFS traffic Filestore's servers see
// from their internal IPs (#2194).
func (s *computeService) filestoreClients() []filestoreserver.Client {
	api := s.API()
	if api == nil {
		return nil
	}
	var out []filestoreserver.Client
	for _, v := range api.VMAddresses() {
		out = append(out, filestoreserver.Client{PodIP: v.PodIP, InternalIP: v.InternalIP})
	}
	return out
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
	if err := api.ResetProject(project); err != nil {
		return err
	}
	if va := r.svc.VPCAccess(); va != nil {
		if err := va.ResetProject(project); err != nil {
			return err
		}
	}
	if cm := r.svc.certManager; cm != nil {
		if err := cm.ResetProject(project); err != nil {
			return err
		}
	}
	if ns := r.svc.netSec; ns != nil {
		if err := ns.ResetProject(project); err != nil {
			return err
		}
	}
	if nsv := r.svc.netSvc; nsv != nil {
		if err := nsv.ResetProject(project); err != nil {
			return err
		}
	}
	if ia := r.svc.iap; ia != nil {
		return ia.ResetProject(project)
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
