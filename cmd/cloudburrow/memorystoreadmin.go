package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
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
	server    *grpctransport.Server
	api       *redisadmin.Server
	db        store.Store
	prov      *memorystoreProvisioner
	cancel    context.CancelFunc
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
	// CreateInstance (#1302): a Valkey server per instance, kept across a
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
	s.api.EnableCreate(bg, s.prov, redisadmin.Dial, db, nil)
	s.api.PersistentMode(s.cfg.Mode == config.ModePersistent)
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
	if err := s.api.Restore(); err != nil {
		return fmt.Errorf("Memorystore instances: %w", err)
	}
	// The cluster comes up after this API does: servers are provisioned,
	// and the ones no instance keeps any more removed, once the server
	// `up` runs answers through its tunnel.
	go func() {
		if s.prov.waitCluster(bg, s.tunnel) {
			s.prov.tidy(bg, s.api.Servers())
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
	register := func(g grpc.ServiceRegistrar) { s.api.Register(g); ops.Register(g); locs.Register(g) }
	var jsonAPI http.Handler = redisadmin.NewRESTHandler(register)
	if s.requests != nil {
		jsonAPI = rest.Observe(jsonAPI, s.requests)
	}
	s.server.ServeHTTP(jsonAPI)
	if err := s.server.Register(func(g *grpc.Server) { register(g) }); err != nil {
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
}

func newMemorystoreProvisioner(cfg config.Config, bg context.Context) *memorystoreProvisioner {
	return &memorystoreProvisioner{cfg: cfg, bg: bg, ready: make(chan struct{}), tunnels: map[string]*netfwd.Forwarder{},
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
	b := components.MemorystoreServerOf(components.MemorystoreSettings{Name: spec.Server, Persistent: p.cfg.Mode == config.ModePersistent,
		MaxMemory: spec.MaxMemory, Password: spec.Password, Settings: spec.Configs, RDBSeconds: spec.RDBSeconds})
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
	f := netfwd.New(netfwd.Target{Name: spec.Server, Namespace: p.cfg.Cluster.Namespace, ServicePort: components.MemorystorePort},
		p.cfg.KubeconfigPath(), p.cfg.BindAddress)
	if err := f.Start(p.bg); err != nil {
		return "", fmt.Errorf("tunnel to %s: %w", spec.Server, err)
	}
	p.mu.Lock()
	p.tunnels[spec.Server] = f
	p.mu.Unlock()
	return f.HostAddr(), nil
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
