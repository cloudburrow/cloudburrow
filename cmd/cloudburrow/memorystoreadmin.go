package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/iamcond"
	"github.com/cloudburrow/cloudburrow/internal/iamroles"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// memorystoreAdminService serves the Memorystore for Redis admin API
// (google.cloud.redis.v1.CloudRedis, #1302) in the CLI process, over the
// Valkey server `--services memorystore` runs, reached through its tunnel.
// gRPC and JSON share one port, as redis.googleapis.com serves both.
type memorystoreAdminService struct {
	cfg       config.Config
	tunnel    func() *netfwd.Forwarder
	storage   func() *netfwd.Forwarder
	calls     grpctransport.Observer
	interpose []grpc.UnaryServerInterceptor
	requests  func(rest.Request)
	// events records what the service does on its own (an automatic
	// failover, #1657) in /admin/events; nil records nothing.
	events redisadmin.EventRecorder
	server *grpctransport.Server
	api    *redisadmin.Server
	db     store.Store
	prov   *memorystoreProvisioner
	// kms is Cloud KMS, for customer_managed_key (#1463); nil without it.
	kms *kmsService
	// creds and registry are what IAM auth (#1614) checks a token
	// against: the access tokens the instance minted, and the project's
	// IAM policy; nil refuses every token.
	creds    *metadata.Credentials
	registry func() *resourcemanager.Registry
	cancel   context.CancelFunc
	// storageImage is the storage image `up` built, or builds when asked
	// (#1768).
	storageImage func(context.Context) (string, error)
}

// frontImage is the storage image's reference (#1768), which `up` builds
// when it is first asked for.
func (s *memorystoreAdminService) frontImage(ctx context.Context) (string, error) {
	if s.storageImage == nil {
		return "", errors.New("this instance cannot build the cloudburrow-storage image")
	}
	return s.storageImage(ctx)
}

func newMemorystoreAdminService(cfg config.Config, tunnel func() *netfwd.Forwarder) *memorystoreAdminService {
	if !serviceEnabled(cfg, config.ServiceMemorystore) || cfg.Endpoints.MemorystoreAdmin < 0 {
		return nil
	}
	return &memorystoreAdminService{cfg: cfg, tunnel: tunnel}
}

func (s *memorystoreAdminService) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

func (s *memorystoreAdminService) Name() string { return "memorystore-admin" }

// Addr is the API's bound address.
func (s *memorystoreAdminService) Addr() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.Addr()
}

// memorystoreBackend is the Valkey server behind the tunnel.
func memorystoreBackend(tunnel func() *netfwd.Forwarder) redisadmin.Valkey {
	return redisadmin.Valkey{Addr: func() string {
		if f := tunnel(); f != nil {
			return f.HostAddr()
		}
		return ""
	}}
}

// memorystoreInstance describes the one instance as the host sees it: at
// the tunnel's host address, addr, when it has one.
func memorystoreInstance(cfg config.Config, created time.Time, addr func() string) redisadmin.Instance {
	return redisadmin.Instance{Project: cfg.DefaultProject(), Host: cfg.BindAddress,
		Port: int32(cfg.Endpoints.Memorystore), Created: created, Address: addr}
}

func (s *memorystoreAdminService) Start(ctx context.Context) error {
	be := memorystoreBackend(s.tunnel)
	s.api = redisadmin.NewServer(memorystoreInstance(s.cfg, time.Now(), be.Addr), be)
	s.api.RecordEvents(s.events)
	// CreateInstance (#1302): a server per instance, of its redis_version's
	// image (memorystoreImages, #1386), kept across a
	// restart in persistent mode, as the instance `up` runs keeps its data.
	var db store.Store = store.NewMemory()
	if s.cfg.Mode == config.ModePersistent {
		durable, err := store.OpenDurable(filepath.Join(s.cfg.StateDir, s.cfg.Name, "memorystore"))
		if err != nil {
			return fmt.Errorf("open Memorystore state: %w", err)
		}
		db = durable
	}
	s.db = db
	bg, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.prov = newMemorystoreProvisioner(s.cfg, bg)
	s.prov.frontImage = s.frontImage
	s.api.EnableCreate(bg, s.prov, redisadmin.Dial, db, nil)
	s.api.PersistentMode(s.cfg.Mode == config.ModePersistent)
	// Memorystore for Redis Cluster (#1519): a real Redis Cluster per
	// cluster, its nodes Valkey servers in cluster mode.
	s.api.EnableClusters(bg, s.prov, redisadmin.DialCluster, db)
	// Memorystore for Valkey (#1519): each instance's nodes as a Redis
	// cluster's are, cluster mode or a primary and its replicas.
	s.api.EnableValkey(bg, s.prov, redisadmin.DialCluster, db)
	// Import and export (#1384) read and write this instance's Cloud
	// Storage, so without it they say so.
	if serviceEnabled(s.cfg, config.ServiceStorage) {
		sf := storageSourceFetcher{cfg: s.cfg, client: &http.Client{Timeout: 10 * time.Minute}}
		if s.storage != nil {
			sf.addr = func() string {
				if f := s.storage(); f != nil {
					return f.HostAddr()
				}
				return ""
			}
		}
		s.api.UseStorage(memorystoreObjects{f: sf})
	}
	// customer_managed_key (#1463) wraps each instance's data key with
	// this instance's Cloud KMS, in process.
	if s.kms != nil {
		s.api.UseKMS(memorystoreKMS{svc: s.kms})
	}
	if s.creds != nil {
		s.api.UseIAM(memorystoreIAM(s.creds, s.registry))
		s.creds.OnIssue(func(t metadata.IssuedToken) {
			s.api.TokenIssued(redisadmin.IssuedToken{Hash: t.Hash, Email: t.Email, Expires: t.Expires})
		})
	}
	if err := s.api.Restore(); err != nil {
		return fmt.Errorf("Memorystore instances: %w", err)
	}
	if err := s.api.RestoreClusters(); err != nil {
		return fmt.Errorf("Memorystore clusters: %w", err)
	}
	// The cluster comes up after this API does: servers are provisioned,
	// and the ones no instance keeps any more removed, once the server
	// `up` runs answers through its tunnel.
	go func() {
		if s.prov.waitCluster(bg, s.tunnel) {
			s.prov.tidy(bg, append(s.api.Servers(), s.api.ClusterServers()...))
		}
	}()
	addr := net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.MemorystoreAdmin))
	s.server = grpctransport.New(addr)
	s.server.Observe(s.calls)
	for _, i := range s.interpose {
		s.server.Interpose(i)
	}
	ops := redisadmin.NewOperationsServer(s.api)
	locs := redisadmin.NewLocationsServer(s.api)
	clusters := redisadmin.NewClusterServer(s.api)
	register := func(g grpc.ServiceRegistrar) {
		s.api.Register(g)
		clusters.Register(g)
		ops.Register(g)
		locs.Register(g)
	}
	// Memorystore for Valkey (#1519): gRPC on this port beside the rest,
	// its JSON API under /memorystore, as its paths are CloudRedis's.
	valkey := redisadmin.NewMemorystoreServer(s.api)
	registerValkey := func(g grpc.ServiceRegistrar) {
		valkey.Register(g)
		ops.Register(g)
		locs.Register(g)
	}
	redisJSON := s.api.ClusterLabels(redisadmin.NewRESTHandler(register))
	valkeyJSON := redisadmin.NewMemorystoreRESTHandler(registerValkey)
	var jsonAPI http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, redisadmin.MemorystoreRESTPrefix+"/") {
			valkeyJSON.ServeHTTP(w, r)
			return
		}
		redisJSON.ServeHTTP(w, r)
	})
	if s.requests != nil {
		jsonAPI = rest.Observe(jsonAPI, s.requests)
	}
	s.server.ServeHTTP(jsonAPI)
	if err := s.server.Register(func(g *grpc.Server) { register(g); valkey.Register(g) }); err != nil {
		return err
	}
	return s.server.Start(ctx)
}

func (s *memorystoreAdminService) Stop(ctx context.Context) error {
	var err error
	if s.server != nil {
		err = s.server.Stop(ctx)
	}
	if s.prov != nil {
		s.prov.stopTunnels(ctx)
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.db != nil {
		if cerr := s.db.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

// memorystoreProvisioner runs each created instance's Valkey server
// (#1302): a Deployment and Service of the image and settings of the one
// `up` runs (components.MemorystoreServer), on a volume of its own in
// persistent mode, reached from the host through its own tunnel on an
// OS-assigned port.
type memorystoreProvisioner struct {
	cfg       config.Config
	installer *components.Installer
	bg        context.Context
	ready     chan struct{}
	readyOnce sync.Once

	mu      sync.Mutex
	tunnels map[string]*netfwd.Forwarder
	// roleServices are the role Services applied, by base, and their port
	// (#1614).
	roleServices map[string]int
	// frontImage is the storage image's reference, the TLS front of a
	// Redis before 6.0 with in-transit encryption (#1768), built when it
	// is first asked for.
	frontImage func(context.Context) (string, error)
	// clusterIPs are the cluster nodes' Services' cluster IPs, by server,
	// the address a node announces to in-cluster clients (#1614).
	clusterIPs map[string]string
}

func newMemorystoreProvisioner(cfg config.Config, bg context.Context) *memorystoreProvisioner {
	return &memorystoreProvisioner{cfg: cfg, bg: bg, ready: make(chan struct{}), tunnels: map[string]*netfwd.Forwarder{},
		roleServices: map[string]int{}, clusterIPs: map[string]string{},
		installer: &components.Installer{Kubeconfig: cfg.KubeconfigPath(), Namespace: cfg.Cluster.Namespace, Instance: cfg.Name}}
}

// waitCluster returns once the server `up` runs is reachable through its
// tunnel, so the cluster and its namespace are there, and opens Provision.
func (p *memorystoreProvisioner) waitCluster(ctx context.Context, tunnel func() *netfwd.Forwarder) bool {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		if f := tunnel(); f != nil && f.Running() {
			p.readyOnce.Do(func() { close(p.ready) })
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
	}
}

// memorystoreReadyTimeout bounds a created server's rollout.
const memorystoreReadyTimeout = 3 * time.Minute

// Provision applies the server's Deployment and Service, waits for its
// rollout, and opens its tunnel.
func (p *memorystoreProvisioner) Provision(ctx context.Context, spec redisadmin.Spec) (string, error) {
	select {
	case <-p.ready:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	var tlsFiles *redisadmin.TLSFiles
	if spec.TransitCA != "" {
		// In-transit encryption (#1413): a server certificate of the
		// instance's CA for its Services, the instance's own among them
		// (a Standard tier node's server is the instance's and "-nN").
		ns := p.cfg.Cluster.Namespace
		var names []string
		for _, n := range []string{spec.Server, memorystoreNodeSuffix.ReplaceAllString(spec.Server, "")} {
			names = append(names, n, n+"."+ns, n+"."+ns+".svc", n+"."+ns+".svc.cluster.local")
		}
		var err error
		if tlsFiles, err = redisadmin.ServerTLS(spec, time.Now(), names...); err != nil {
			return "", fmt.Errorf("issue %s a server certificate: %w", spec.Server, err)
		}
	}
	settings := components.MemorystoreSettings{Name: spec.Server, Persistent: p.cfg.Mode == config.ModePersistent,
		MaxMemory: spec.MaxMemory, Password: spec.Password, Settings: spec.Configs, RDBSeconds: spec.RDBSeconds,
		ReplicaOf: spec.ReplicaOf, Image: memorystoreImages[spec.Version]}
	port := components.MemorystorePort
	if tlsFiles != nil {
		settings.TLSCert, settings.TLSKey, settings.TLSCA = tlsFiles.Cert, tlsFiles.Key, tlsFiles.CA
		port = redisadmin.TLSPort
		if redisadmin.PreTLS(spec.Version) {
			// Redis before 6.0 has no TLS: a front serves it (#1768),
			// from the storage image.
			if p.frontImage == nil {
				return "", fmt.Errorf("in-transit encryption on %s needs the cloudburrow-storage image, which this instance cannot build", spec.Version)
			}
			img, err := p.frontImage(ctx)
			if err != nil {
				return "", fmt.Errorf("in-transit encryption on %s: the cloudburrow-storage image its TLS front runs from: %w", spec.Version, err)
			}
			settings.TLSFront, settings.FrontImage = true, img
			settings.Instance, settings.Namespace = memorystoreNodeSuffix.ReplaceAllString(spec.Server, ""), p.cfg.Cluster.Namespace
		}
	}
	b := components.MemorystoreServerOf(settings)
	if err := p.installer.InstallBackends(ctx, []components.Backend{b}, memorystoreReadyTimeout); err != nil {
		return "", err
	}
	p.mu.Lock()
	old := p.tunnels[spec.Server]
	delete(p.tunnels, spec.Server)
	p.mu.Unlock()
	if old != nil {
		_ = old.Stop(ctx)
	}
	f := netfwd.New(netfwd.Target{Name: spec.Server, Namespace: p.cfg.Cluster.Namespace, ServicePort: port},
		p.cfg.KubeconfigPath(), p.cfg.BindAddress)
	if err := f.Start(p.bg); err != nil {
		return "", fmt.Errorf("tunnel to %s: %w", spec.Server, err)
	}
	p.mu.Lock()
	p.tunnels[spec.Server] = f
	p.mu.Unlock()
	return f.HostAddr(), nil
}

// StartClusterNode runs a Redis Cluster node (#1519): the server of spec
// in cluster mode, serving on one port that is the same in the cluster and
// on the host, so the port the node announces to cluster clients is the
// one its tunnel listens on, and returns its host address, pod IP and port.
func (p *memorystoreProvisioner) StartClusterNode(ctx context.Context, spec redisadmin.Spec) (string, string, int, error) {
	select {
	case <-p.ready:
	case <-ctx.Done():
		return "", "", 0, ctx.Err()
	}
	f := netfwd.New(netfwd.Target{Name: spec.Server, Namespace: p.cfg.Cluster.Namespace}, p.cfg.KubeconfigPath(), p.cfg.BindAddress)
	_, ps, err := net.SplitHostPort(f.HostAddr())
	if err != nil {
		return "", "", 0, fmt.Errorf("reserve a port for %s: %w", spec.Server, err)
	}
	port, _ := strconv.Atoi(ps)
	cfg := map[string]string{"port": ps}
	for k, v := range spec.Configs {
		cfg[k] = v
	}
	settings := components.MemorystoreSettings{Name: spec.Server, Persistent: p.cfg.Mode == config.ModePersistent,
		MaxMemory: spec.MaxMemory, Password: spec.Password, Settings: cfg, Image: memorystoreImages[spec.Version], Dir: spec.Dir,
		PodAndLoopback6: p.byFamily()}
	clusterIP := ""
	if settings.PodAndLoopback6 {
		// The node's Service first, for its cluster IP, which the node
		// announces to in-cluster clients, and which its certificate names.
		b := components.MemorystoreServerOf(settings)
		b.Port, b.PortName = port, spec.PortName
		ip, err := p.applyNodeService(ctx, b)
		if err != nil {
			return "", "", 0, err
		}
		clusterIP = ip
	}
	if spec.TransitCA != "" {
		// In-transit encryption (#1549): TLS only, on the node's one port,
		// with a certificate of the cluster's CA for the host's address,
		// where clients reach the node, and the node's Service.
		ns := p.cfg.Cluster.Namespace
		services := []string{spec.Server}
		if spec.PortName == redisadmin.ValkeyPortName {
			// A CLUSTER_DISABLED node is also reached at its instance's
			// role Services (#1614): a secondary of cross-instance
			// replication replicates the primary Service.
			base := memorystoreNodeNumber.ReplaceAllString(spec.Server, "")
			services = append(services, base+"-primary", base+"-reader")
		}
		names := []string{p.cfg.BindAddress}
		if clusterIP != "" {
			names = append(names, "::1", clusterIP)
		}
		for _, n := range services {
			names = append(names, n+"."+ns, n+"."+ns+".svc", n+"."+ns+".svc.cluster.local")
		}
		files, err := redisadmin.ServerTLS(spec, time.Now(), names...)
		if err != nil {
			return "", "", 0, fmt.Errorf("issue %s a server certificate: %w", spec.Server, err)
		}
		delete(cfg, "port")
		settings.TLSCert, settings.TLSKey, settings.TLSCA, settings.TLSPort = files.Cert, files.Key, files.CA, port
	}
	b := components.MemorystoreServerOf(settings)
	b.Port, b.PortName = port, spec.PortName
	if err := p.installer.InstallBackends(ctx, []components.Backend{b}, memorystoreReadyTimeout); err != nil {
		return "", "", 0, err
	}
	ip, err := p.installer.Kubectl(ctx, "-n", p.cfg.Cluster.Namespace, "get", "pods", "-l", "app="+spec.Server,
		"--field-selector", "status.phase=Running", "-o", "jsonpath={.items[0].status.podIP}")
	if err != nil || strings.TrimSpace(ip) == "" {
		return "", "", 0, fmt.Errorf("pod IP of %s: %v", spec.Server, err)
	}
	f = netfwd.New(netfwd.Target{Name: spec.Server, Namespace: p.cfg.Cluster.Namespace, ServicePort: port, HostPort: port,
		Loopback6: settings.PodAndLoopback6}, p.cfg.KubeconfigPath(), p.cfg.BindAddress)
	if err := f.Start(p.bg); err != nil {
		return "", "", 0, fmt.Errorf("tunnel to %s: %w", spec.Server, err)
	}
	p.mu.Lock()
	old := p.tunnels[spec.Server]
	p.tunnels[spec.Server] = f
	if clusterIP != "" {
		p.clusterIPs[spec.Server] = clusterIP
	}
	p.mu.Unlock()
	if old != nil {
		_ = old.Stop(ctx)
	}
	return f.HostAddr(), strings.TrimSpace(ip), port, nil
}

// byFamily is whether cluster nodes serve host clients and in-cluster
// clients each an address of their own (#1614): when the instance binds
// 127.0.0.1, its tunnels also listen on ::1, which the nodes announce to
// the clients the tunnels bring in, over IPv6, and the nodes announce their
// Services' cluster IPs to the pods' clients, over IPv4. Neither address
// is on any network. An instance bound elsewhere (--allow-remote) announces
// that address to every client, as before.
func (p *memorystoreProvisioner) byFamily() bool {
	return p.cfg.BindAddress == "127.0.0.1"
}

// ClientAddrs is redisadmin.ClientAddrsByFamily.
func (p *memorystoreProvisioner) ClientAddrs(server string) (string, string) {
	if !p.byFamily() {
		return "", ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if ip := p.clusterIPs[server]; ip != "" {
		return ip, "::1"
	}
	return "", ""
}

// applyNodeService applies b's Service alone and returns its cluster IP.
func (p *memorystoreProvisioner) applyNodeService(ctx context.Context, b components.Backend) (string, error) {
	m := b.Manifest(p.cfg.Cluster.Namespace, p.cfg.Name)
	const sep = "---\napiVersion: v1\nkind: Service\n"
	i := strings.Index(m, sep)
	if i < 0 {
		return "", fmt.Errorf("node %s: no Service in its manifest", b.Name)
	}
	if err := p.installer.Apply(ctx, m[i+4:]); err != nil {
		return "", err
	}
	ip, err := p.installer.Kubectl(ctx, "-n", p.cfg.Cluster.Namespace, "get", "service", b.Name, "-o", "jsonpath={.spec.clusterIP}")
	if err != nil || net.ParseIP(strings.TrimSpace(ip)) == nil {
		return "", fmt.Errorf("the cluster IP of node %s: %v %q", b.Name, err, ip)
	}
	return strings.TrimSpace(ip), nil
}

// memorystoreNodeNumber ends a Memorystore for Valkey node's server name
// (redisadmin.ValkeyServerBase).
var memorystoreNodeNumber = regexp.MustCompile(`-[0-9]+$`)

// memorystoreNodeSuffix ends a Standard tier node's server name.
var memorystoreNodeSuffix = regexp.MustCompile(`-n[0-9]+$`)

// ClusterAddr is where the other servers reach server inside the cluster:
// its Service (#1385).
func (p *memorystoreProvisioner) ClusterAddr(server string) (string, int) {
	return server + "." + p.cfg.Cluster.Namespace + ".svc.cluster.local", components.MemorystorePort
}

// Route applies the Standard tier instance's Service, service, selecting
// the pod of node's server: the primary, which in-cluster clients of the
// instance reach (#1385).
func (p *memorystoreProvisioner) Route(ctx context.Context, service, node string, port int) error {
	select {
	case <-p.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	return p.installer.Apply(ctx, components.MemorystorePrimaryService(service, node, p.cfg.Cluster.Namespace, p.cfg.Name, port))
}

var _ redisadmin.RoleRouter = (*memorystoreProvisioner)(nil)

// NodeHost is where the other nodes of a Memorystore for Valkey instance
// reach server in the cluster: its Service, which follows its pod
// (#1614).
func (p *memorystoreProvisioner) NodeHost(server string) string {
	return server + "." + p.cfg.Cluster.Namespace + ".svc.cluster.local"
}

// Roles labels each node's pod of a CLUSTER_DISABLED Memorystore for
// Valkey instance with its role, and applies the instance's primary and
// reader Services, which select by it (#1614). A pod started again has
// no role until it is labelled again.
func (p *memorystoreProvisioner) Roles(ctx context.Context, base string, roles map[string]string, port int) error {
	select {
	case <-p.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	ns := p.cfg.Cluster.Namespace
	for server, role := range roles {
		if _, err := p.installer.Kubectl(ctx, "-n", ns, "label", "pods", "-l", "app="+server, "--overwrite",
			components.MemorystoreOfLabel+"="+base, components.MemorystoreRoleLabel+"="+role); err != nil {
			return fmt.Errorf("label %s %s: %w", server, role, err)
		}
	}
	p.mu.Lock()
	applied := p.roleServices[base] == port
	p.mu.Unlock()
	if applied {
		return nil
	}
	if err := p.installer.Apply(ctx, components.MemorystoreRoleServices(base, ns, p.cfg.Name, redisadmin.ValkeyPortName, port)); err != nil {
		return err
	}
	p.mu.Lock()
	p.roleServices[base] = port
	p.mu.Unlock()
	return nil
}

// RemoveRoles deletes a Memorystore for Valkey instance's primary and
// reader Services (#1614).
func (p *memorystoreProvisioner) RemoveRoles(ctx context.Context, base string) error {
	p.mu.Lock()
	delete(p.roleServices, base)
	p.mu.Unlock()
	select {
	case <-p.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	_, err := p.installer.Kubectl(ctx, "-n", p.cfg.Cluster.Namespace, "delete", "service/"+base+"-primary", "service/"+base+"-reader",
		"--ignore-not-found", "--wait=false")
	return err
}

// Exec runs argv in the server's container, with stdin: the file copies
// and server processes import and export run (#1384).
func (p *memorystoreProvisioner) Exec(ctx context.Context, server string, stdin []byte, argv ...string) ([]byte, error) {
	select {
	case <-p.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	args := append([]string{"-n", p.cfg.Cluster.Namespace, "exec"}, "deployment/"+server)
	if stdin != nil {
		args = append(args, "-i")
	}
	out, err := p.installer.KubectlIn(ctx, string(stdin), append(append(args, "--"), argv...)...)
	return []byte(out), err
}

// memorystoreObjects reads and writes import and export files in this
// instance's Cloud Storage, through its JSON API (#1384).
type memorystoreObjects struct{ f storageSourceFetcher }

func (o memorystoreObjects) Read(ctx context.Context, bucket, object string) ([]byte, error) {
	rc, err := o.f.Fetch(ctx, bucket, object, 0)
	if err != nil {
		if e := apierror.From(err); e.Code == codes.NotFound {
			return nil, apierror.NotFound("gs://%s/%s not found", bucket, object)
		}
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func (o memorystoreObjects) Write(ctx context.Context, bucket, object string, data []byte) error {
	u := o.f.base() + "/upload/storage/v1/b/" + url.PathEscape(bucket) + "/o?uploadType=media&name=" + url.QueryEscape(object)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := o.f.client.Do(req)
	if err != nil {
		return fmt.Errorf("write gs://%s/%s: %w", bucket, object, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return apierror.NotFound("bucket %s not found", bucket)
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return apierror.FailedPrecondition("write gs://%s/%s: HTTP %d %s", bucket, object, resp.StatusCode, strings.TrimSpace(string(b)))
	}
}

// memorystoreImages are the server images of redis_version (#1386); the
// Valkey image, REDIS_7_2, is the default.
var memorystoreImages = map[string]string{
	"REDIS_3_2": components.MemorystoreRedis32Image,
	"REDIS_4_0": components.MemorystoreRedis40Image,
	"REDIS_5_0": components.MemorystoreRedis50Image,
	"REDIS_6_X": components.MemorystoreRedis62Image,
	"REDIS_7_0": components.MemorystoreRedis70Image,
	// Memorystore for Valkey's engine_version (#1557); VALKEY_8_0 is the
	// default image.
	"VALKEY_7_2": components.MemorystoreValkey72Image,
	"VALKEY_9_0": components.MemorystoreValkey90Image,
}

// Suspend stops a suspended instance's server and keeps its volume
// (#1463): it runs again, with its data, once the instance resumes.
func (p *memorystoreProvisioner) Suspend(ctx context.Context, server string) error {
	return p.remove(ctx, server, false)
}

// memorystoreKMS encrypts and decrypts with the Cloud KMS server.
type memorystoreKMS struct{ svc *kmsService }

func (k memorystoreKMS) Encrypt(ctx context.Context, key string, plaintext []byte) ([]byte, error) {
	api := k.svc.API()
	if api == nil {
		return nil, errors.New("Cloud KMS has not started yet")
	}
	r, err := api.Encrypt(ctx, &kmspb.EncryptRequest{Name: key, Plaintext: plaintext})
	if err != nil {
		return nil, err
	}
	return r.GetCiphertext(), nil
}

// sqlAdminKMS is memorystoreKMS for the Cloud SQL Admin API (#1538),
// whose Encrypt also answers the CryptoKeyVersion that wrapped the key, for
// diskEncryptionStatus.
type sqlAdminKMS struct{ svc *kmsService }

func (k sqlAdminKMS) Encrypt(ctx context.Context, key string, plaintext []byte) ([]byte, string, error) {
	api := k.svc.API()
	if api == nil {
		return nil, "", errors.New("Cloud KMS has not started yet")
	}
	r, err := api.Encrypt(ctx, &kmspb.EncryptRequest{Name: key, Plaintext: plaintext})
	if err != nil {
		return nil, "", err
	}
	return r.GetCiphertext(), r.GetName(), nil
}

func (k sqlAdminKMS) Decrypt(ctx context.Context, key string, ciphertext []byte) ([]byte, error) {
	return buildKMSDecrypter(k).Decrypt(ctx, key, ciphertext)
}

func (k memorystoreKMS) Decrypt(ctx context.Context, key string, ciphertext []byte) ([]byte, error) {
	return buildKMSDecrypter(k).Decrypt(ctx, key, ciphertext)
}

// Remove closes the server's tunnel and deletes its Deployment, Service
// and volume.
func (p *memorystoreProvisioner) Remove(ctx context.Context, server string) error {
	return p.remove(ctx, server, true)
}

func (p *memorystoreProvisioner) remove(ctx context.Context, server string, volume bool) error {
	p.mu.Lock()
	f := p.tunnels[server]
	delete(p.tunnels, server)
	p.mu.Unlock()
	if f != nil {
		_ = f.Stop(ctx)
	}
	select {
	case <-p.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	objects := []string{"deployment/" + server, "service/" + server}
	if volume {
		objects = append(objects, "persistentvolumeclaim/"+server+"-data")
	}
	_, err := p.installer.Kubectl(ctx, append(append([]string{"-n", p.cfg.Cluster.Namespace, "delete"}, objects...),
		"--ignore-not-found", "--wait=false")...)
	return err
}

// tidy removes the created servers no instance keeps: every one, in
// ephemeral mode, whose instances went with the last run. An ephemeral
// run keeps their volumes, as it keeps the one of the server `up` runs:
// the persistent run after it has its instances back, with their data.
func (p *memorystoreProvisioner) tidy(ctx context.Context, keep []string) {
	out, err := p.installer.Kubectl(ctx, "-n", p.cfg.Cluster.Namespace, "get", "deployments",
		"-l", "cloudburrow.dev/instance="+p.cfg.Name, "-o", "name")
	if err != nil {
		return
	}
	kept := map[string]bool{}
	for _, k := range keep {
		kept[k] = true
	}
	for _, line := range strings.Fields(out) {
		name := strings.TrimPrefix(line, "deployment.apps/")
		if strings.HasPrefix(name, redisadmin.ServerPrefix) && !kept[name] {
			_ = p.remove(ctx, name, p.cfg.Mode == config.ModePersistent)
		}
	}
}

func (p *memorystoreProvisioner) stopTunnels(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for name, f := range p.tunnels {
		_ = f.Stop(ctx)
		delete(p.tunnels, name)
	}
}

// connectPermission is the permission each connection role grants, from
// Google's IAM roles reference (redis.clusters.connect, memorystore.instances.connect).
var connectPermission = map[string]string{
	"roles/redis.dbConnectionUser":       "redis.clusters.connect",
	"roles/memorystore.dbConnectionUser": "memorystore.instances.connect",
}

// memorystoreIAM is IAM auth's token source (#1614): the access tokens
// creds minted, and the members of a project's bindings of a role in the
// registry's policies (none while the registry is not open).
func memorystoreIAM(creds *metadata.Credentials, registry func() *resourcemanager.Registry) redisadmin.IAMSource {
	return redisadmin.IAMSource{
		Tokens: func(now time.Time) []redisadmin.IssuedToken {
			live := creds.LiveTokens(now)
			out := make([]redisadmin.IssuedToken, len(live))
			for i, t := range live {
				out[i] = redisadmin.IssuedToken{Hash: t.Hash, Email: t.Email, Expires: t.Expires}
			}
			return out
		},
		Members: func(project, resource, role string) []string {
			var reg *resourcemanager.Registry
			if registry != nil {
				reg = registry()
			}
			if reg == nil {
				return nil
			}
			p, err := reg.IAMPolicy("projects/" + project)
			if err != nil || p == nil {
				return nil
			}
			// A custom role (#1743) grants the connection when it holds the
			// permission the predefined role does.
			var out []string
			// A conditional binding (#1742, #1745) counts where its
			// condition holds now for this cluster or instance: request.time
			// and its effective tags are evaluated; resource.* is not
			// (Google: Memorystore does not recognize resource
			// attributes), and grants nothing.
			attrs := iamcond.Attributes{RequestTime: time.Now(), TagsKnown: true}
			service := "//redis.googleapis.com/"
			if strings.Contains(resource, "/instances/") {
				service = "//memorystore.googleapis.com/"
			}
			if tags, err := reg.EffectiveTags(service + resource); err == nil {
				attrs.Tags = condTags(tags)
			} else {
				attrs.TagsKnown = false
			}
			for _, b := range p.Bindings {
				if !b.Grants(attrs) {
					continue
				}
				if b.Role == role {
					out = append(out, b.Members...)
					continue
				}
				if perms, ok := reg.RolePermissions(b.Role); ok && iamroles.Grants(perms, connectPermission[role]) {
					out = append(out, b.Members...)
				}
			}
			return out
		},
	}
}

// condTags are effective tags as internal/iamcond reads them.
func condTags(tags []*rmpb.EffectiveTag) []iamcond.Tag {
	out := make([]iamcond.Tag, 0, len(tags))
	for _, t := range tags {
		short := t.GetNamespacedTagValue()
		if i := strings.LastIndex(short, "/"); i >= 0 {
			short = short[i+1:]
		}
		out = append(out, iamcond.Tag{NamespacedKey: t.GetNamespacedTagKey(), ShortValue: short, KeyID: t.GetTagKey(), ValueID: t.GetTagValue()})
	}
	return out
}
