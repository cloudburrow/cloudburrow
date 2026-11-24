package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/alloydb"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// alloyDBService serves AlloyDB for PostgreSQL (#2191) in the CLI process,
// gRPC and JSON on one port, its instances PostgreSQL servers in the
// cluster (alloyDBEngine), and the connectors' server-side proxy on
// <bind address>:5433.
type alloyDBService struct {
	cfg       config.Config
	logw      io.Writer
	tracing   *telemetry.Tracing
	calls     grpctransport.Observer
	interpose []grpc.UnaryServerInterceptor
	requests  func(rest.Request)
	// creds mints the instance's access tokens, which an AUTO_IAM
	// connection's token must be one of; nil checks none.
	creds *metadata.Credentials
	// storage is the address of the instance's Cloud Storage, for
	// ExportCluster and ImportCluster; nil serves neither.
	storage func() string
	// backupDR resolves the Backup and DR backups and data sources a
	// cluster is restored from (#1524); nil without backupdr.
	backupDR alloydb.BackupDR

	server    *grpctransport.Server
	api       *alloydb.Server
	engine    *alloyDBEngine
	connector *alloydb.Connector
	tlsFront  *alloyDBTLSFront
	stop      chan struct{}
}

func newAlloyDBService(cfg config.Config, logw io.Writer) *alloyDBService {
	if !alloyDBEnabled(cfg) {
		return nil
	}
	return &alloyDBService{cfg: cfg, logw: logw}
}

// alloyDBEnabled is whether `up` binds the AlloyDB port.
func alloyDBEnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceAlloyDB) && cfg.Endpoints.AlloyDB >= 0
}

func (s *alloyDBService) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

func (s *alloyDBService) Name() string { return "alloydb" }

// API is the server, available after Start.
func (s *alloyDBService) API() *alloydb.Server {
	if s == nil {
		return nil
	}
	return s.api
}

// Addr is the bound address.
func (s *alloyDBService) Addr() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.Addr()
}

// ConnectorAddr is the server-side proxy's address, "" when it is not
// serving.
func (s *alloyDBService) ConnectorAddr() string {
	if s == nil {
		return ""
	}
	return s.connector.Addr()
}

func (s *alloyDBService) logf(format string, args ...any) {
	if s.logw != nil {
		fmt.Fprintf(s.logw, "cloudburrow: "+format+"\n", args...)
	}
}

func (s *alloyDBService) Start(ctx context.Context) error {
	state := ""
	persistent := s.cfg.Mode == config.ModePersistent
	if persistent {
		state = filepath.Join(s.cfg.InstanceDir(), "alloydb.json")
	}
	s.engine = newAlloyDBEngine(s.cfg, s.logw)
	opts := alloydb.Options{Engine: s.engine, BackupsDir: filepath.Join(s.cfg.InstanceDir(), "alloydb-backups"),
		ConnectorHost: s.cfg.BindAddress, Logf: s.logf}
	if net.ParseIP(s.cfg.BindAddress) == nil || net.ParseIP(s.cfg.BindAddress).IsUnspecified() {
		opts.ConnectorHost = "127.0.0.1"
	}
	if s.storage != nil {
		opts.Transfers = alloyDBTransfers{e: s.engine}
		opts.Objects = alloyDBObjects{f: storageSourceFetcher{cfg: s.cfg, client: &http.Client{Timeout: 10 * time.Minute}, addr: s.storage}}
	}
	if s.creds != nil {
		creds := s.creds
		opts.IAMToken = func(token string) (string, bool) {
			h := metadata.TokenHash(token)
			for _, t := range creds.LiveTokens(time.Now()) {
				if t.Hash == h {
					return t.Email, true
				}
			}
			return "", false
		}
	}
	opts.InstanceAddresses = alloydb.DefaultInstanceAddresses()
	if v := strings.TrimSpace(os.Getenv("CLOUDBURROW_ALLOYDB_INSTANCE_ADDRESSES")); v != "" {
		// The addresses instances take, in order: on macOS those an
		// administrator aliased onto lo0 (docs/alloydb.md).
		opts.InstanceAddresses = strings.Split(v, ",")
	}
	api, err := alloydb.NewServer(state, filepath.Join(s.cfg.InstanceDir(), "alloydb-ca.json"), opts)
	if err != nil {
		return err
	}
	if s.backupDR != nil {
		// Backup and DR backups and data sources restored (#1524).
		api.SetBackupDR(s.backupDR)
	}
	s.api = api
	s.server = grpctransport.New(net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.AlloyDB)))
	s.server.Observe(s.calls)
	if s.tracing != nil {
		s.server.ServerOptions(s.tracing.ServerOptions()...)
	}
	for _, i := range s.interpose {
		s.server.Interpose(i)
	}
	var jsonAPI http.Handler = alloydb.NewRESTHandler(api)
	if s.requests != nil {
		jsonAPI = rest.Observe(jsonAPI, s.requests)
	}
	s.server.ServeHTTP(jsonAPI)
	if err := s.server.Register(func(g *grpc.Server) { api.Register(g) }); err != nil {
		return err
	}
	if err := s.server.Start(ctx); err != nil {
		return err
	}
	// The connectors dial port 5433, a constant in each. When another
	// instance holds it, the listener takes a free port on the same host,
	// recorded for `env` as CLOUDBURROW_ALLOYDB_CONNECTOR_ENDPOINT: an
	// application beside this instance then redirects the connectors'
	// dial with WithDialFunc, as the Cloud SQL connectors' does (#1364).
	addr := net.JoinHostPort(opts.ConnectorHost, strconv.Itoa(alloydb.ConnectorPort))
	c, err := api.ServeConnector(addr)
	if err != nil {
		moved := net.JoinHostPort(opts.ConnectorHost, "0")
		if c, err = api.ServeConnector(moved); err != nil {
			s.logf("AlloyDB connectors: cannot listen on %s (%v); the API serves without them", addr, err)
		} else {
			s.logf("AlloyDB connectors: port %d is taken; listening on %s", alloydb.ConnectorPort, c.Addr())
		}
	}
	s.connector = c
	recordAlloyDBConnector(s.cfg, s.ConnectorAddr())
	if front, err := serveAlloyDBTLS(s.cfg, api, opts.ConnectorHost, s.server.Addr()); err != nil {
		s.logf("AlloyDB: the admin API's TLS endpoint: %v", err)
	} else {
		s.tlsFront = front
		s.logf("AlloyDB: the admin API over TLS is at %s (its CA: %s)", front.Addr(), alloyDBCAFile(s.cfg))
	}
	// The servers of the instances kept (a persistent instance's), and
	// the removal of those that are not; then the automated backups.
	s.stop = make(chan struct{})
	go func() {
		bg := context.Background()
		if err := s.engine.Prune(bg, api.Servers()); err != nil {
			s.logf("AlloyDB: pruning servers: %v", err)
		}
		api.Resume(bg)
	}()
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case now := <-t.C:
				api.RunAutomatedBackups(now)
			}
		}
	}()
	return nil
}

func (s *alloyDBService) Stop(ctx context.Context) error {
	if s.stop != nil {
		close(s.stop)
		s.stop = nil
	}
	_ = s.connector.Close()
	s.tlsFront.Close()
	if s.engine != nil {
		s.engine.Close()
	}
	if s.server == nil {
		return nil
	}
	return s.server.Stop(ctx)
}

// alloyDBEngine runs AlloyDB's instances (#2191) in the cluster, as
// sqlProvisioner runs Cloud SQL's: each is a Deployment of the Cloud SQL
// for PostgreSQL image of the cluster's major version, with a volume and a
// host tunnel; a read pool is a streaming replica of its primary
// (components.CloudSQLReplicaBackend). Backups and point-in-time recovery
// are Cloud SQL's (sqlDumper).
type alloyDBEngine struct {
	cfg  config.Config
	inst *components.Installer
	logw io.Writer
	dump *sqlDumper

	mu   sync.Mutex
	fwds map[string]*netfwd.Forwarder
	up   map[string]bool
	// pools are the managed connection poolers' tunnels, by server and
	// then by port, while their server pools connections.
	pools map[string]map[int]*netfwd.Forwarder
}

func newAlloyDBEngine(cfg config.Config, logw io.Writer) *alloyDBEngine {
	e := &alloyDBEngine{cfg: cfg, logw: logw,
		inst: &components.Installer{Kubeconfig: cfg.KubeconfigPath(), Namespace: cfg.Cluster.Namespace, Instance: cfg.Name},
		fwds: map[string]*netfwd.Forwarder{}, up: map[string]bool{}, pools: map[string]map[int]*netfwd.Forwarder{}}
	// sqlDumper names a created instance's server sqlinst-<name>; an
	// AlloyDB server's Deployment is its name alone.
	e.dump = &sqlDumper{cfg: cfg, exec: func(d string) func(context.Context, io.Reader, io.Writer, ...string) error {
		return nodeExec(cfg, strings.TrimPrefix(d, components.SQLInstancePrefix))
	}}
	return e
}

var _ alloydb.Engine = (*alloyDBEngine)(nil)

func (e *alloyDBEngine) logf(format string, args ...any) {
	if e.logw != nil {
		fmt.Fprintf(e.logw, "cloudburrow: "+format+"\n", args...)
	}
}

// target is server's dump target; sqlServerOf leaves a name it does not
// know as it is.
func (e *alloyDBEngine) target(server string) sqladmin.DumpTarget {
	return sqladmin.DumpTarget{Instance: server, Created: true}
}

// backend is spec's Deployment.
func (e *alloyDBEngine) backend(spec alloydb.ServerSpec) components.Backend {
	var flags []components.SQLInstanceFlag
	for _, k := range sortedFlagNames(spec.Flags) {
		flags = append(flags, components.SQLInstanceFlag{Name: k, Value: spec.Flags[k]})
	}
	b := components.CloudSQLInstanceBackend(spec.Server, spec.Version, true, "", flags)
	b.Name = spec.Server
	if spec.PITR {
		if len(b.Args) == 0 {
			b.Args = []string{"postgres"}
		}
		b.Args = append(append([]string{}, b.Args...), pgPITRArgs()...)
	}
	if spec.ReplicaOf != "" {
		b = components.CloudSQLReplicaBackend(b, false, components.SQLReplicaSource{Host: spec.ReplicaOf, Epoch: int(spec.Epoch)})
	}
	if spec.Pooler != nil {
		// Managed connection pooling: PgBouncer beside the server, as
		// Cloud SQL's (#2141), with the connectors' pool beside it.
		b.Sidecars = append(b.Sidecars, components.AlloyDBPoolers(spec.Pooler)...)
	}
	return b
}

func sortedFlagNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (e *alloyDBEngine) forwarder(server string) *netfwd.Forwarder {
	e.mu.Lock()
	defer e.mu.Unlock()
	if f := e.fwds[server]; f != nil {
		return f
	}
	f := netfwd.New(netfwd.Target{Name: server, Namespace: e.inst.Namespace, ServicePort: components.CloudSQLPort,
		Label: server}, e.cfg.KubeconfigPath(), e.cfg.BindAddress)
	f.Logf = e.logf
	e.fwds[server] = f
	return f
}

func (e *alloyDBEngine) Addr(server string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if f := e.fwds[server]; f != nil && e.up[server] {
		return f.HostAddr()
	}
	return ""
}

func (e *alloyDBEngine) rollout(ctx context.Context, server string) error {
	_, err := e.inst.Kubectl(ctx, "-n", e.inst.Namespace, "rollout", "status", "deployment/"+server, "--watch=true", "--timeout=280s")
	if err != nil {
		logs, _ := e.inst.Kubectl(ctx, "-n", e.inst.Namespace, "logs", "deployment/"+server, "--tail=5")
		return fmt.Errorf("the server %s did not become ready: %w %s", server, err, strings.TrimSpace(logs))
	}
	return nil
}

// serves waits until server's PostgreSQL answers through its tunnel.
func (e *alloyDBEngine) serves(ctx context.Context, server string) error {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		addr := e.Addr(server)
		if addr != "" {
			c, err := pgx.Connect(ctx, "postgres://postgres@"+addr+"/postgres?sslmode=disable&connect_timeout=5")
			if err == nil {
				err = c.Ping(ctx)
				_ = c.Close(ctx)
				if err == nil {
					return nil
				}
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("the server %s does not answer through its tunnel: %w", server, err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (e *alloyDBEngine) psql(ctx context.Context, server, script string) (string, error) {
	var out strings.Builder
	err := nodeExec(e.cfg, server)(ctx, strings.NewReader(script), &out,
		"psql", "-v", "ON_ERROR_STOP=1", "-X", "-qtA", "-U", "postgres", "-d", "postgres")
	if err != nil {
		return "", fmt.Errorf("%s: %w %s", server, err, strings.TrimSpace(out.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func (e *alloyDBEngine) Start(ctx context.Context, spec alloydb.ServerSpec) error {
	b := e.backend(spec)
	if err := e.inst.Apply(ctx, b.Manifest(e.inst.Namespace, e.inst.Instance)); err != nil {
		return err
	}
	if _, err := e.inst.Kubectl(ctx, "-n", e.inst.Namespace, "scale", "deployment/"+spec.Server, "--replicas=1"); err != nil {
		return err
	}
	if err := e.rollout(ctx, spec.Server); err != nil {
		return err
	}
	if spec.ReplicaOf == "" {
		// A read pool's server takes a base backup of the primary and
		// streams from it, as a Cloud SQL replica's does: the replication
		// role, its pg_hba.conf line, and the WAL kept for it.
		if _, err := e.psql(ctx, spec.Server, pgStandbySource+pgKeepWAL(spec.Version)); err != nil {
			return fmt.Errorf("prepare %s for its read pools: %w", spec.Server, err)
		}
		const line = "host replication " + components.SQLReplicaUser + " all trust"
		const add = `hba=$(psql -U postgres -XtAc 'SHOW hba_file') && { grep -qxF '` + line + `' "$hba" || ` +
			`{ echo '` + line + `' >> "$hba" && psql -U postgres -XtAc 'SELECT pg_reload_conf()'; }; }`
		var o strings.Builder
		if err := nodeExec(e.cfg, spec.Server)(ctx, nil, &o, "sh", "-c", add); err != nil {
			return fmt.Errorf("let read pools connect to %s: %w %s", spec.Server, err, o.String())
		}
	}
	f := e.forwarder(spec.Server)
	e.mu.Lock()
	started := e.up[spec.Server]
	e.mu.Unlock()
	if !started {
		if err := f.Start(ctx); err != nil {
			return fmt.Errorf("the tunnel of %s: %w", spec.Server, err)
		}
		e.mu.Lock()
		e.up[spec.Server] = true
		e.mu.Unlock()
		e.logf("AlloyDB server %s (port %d) is at %s", spec.Server, components.CloudSQLPort, f.HostAddr())
	}
	if err := e.serves(ctx, spec.Server); err != nil {
		return err
	}
	return e.syncPoolers(ctx, spec)
}

// syncPoolers starts the tunnels of spec's poolers when it pools
// connections, and stops them when it does not.
func (e *alloyDBEngine) syncPoolers(ctx context.Context, spec alloydb.ServerSpec) error {
	if spec.Pooler == nil {
		e.stopPoolers(ctx, spec.Server)
		return nil
	}
	for _, port := range []int{components.AlloyDBPoolerPort, components.AlloyDBConnectorPoolerPort} {
		e.mu.Lock()
		f := e.pools[spec.Server][port]
		e.mu.Unlock()
		if f != nil {
			continue
		}
		f = netfwd.New(netfwd.Target{Name: spec.Server, Namespace: e.inst.Namespace, ServicePort: port,
			Label: fmt.Sprintf("alloydb-pooler-%s-%d", spec.Server, port)}, e.cfg.KubeconfigPath(), e.cfg.BindAddress)
		f.Logf = e.logf
		if err := f.Start(ctx); err != nil {
			return fmt.Errorf("the tunnel of %s's connection pooler: %w", spec.Server, err)
		}
		e.mu.Lock()
		if e.pools[spec.Server] == nil {
			e.pools[spec.Server] = map[int]*netfwd.Forwarder{}
		}
		e.pools[spec.Server][port] = f
		e.mu.Unlock()
		e.logf("AlloyDB server %s pools connections: its pooler (port %d) is at %s", spec.Server, port, f.HostAddr())
	}
	return nil
}

func (e *alloyDBEngine) stopPoolers(ctx context.Context, server string) {
	e.mu.Lock()
	fs := e.pools[server]
	delete(e.pools, server)
	e.mu.Unlock()
	for _, f := range fs {
		_ = f.Stop(ctx)
	}
}

func (e *alloyDBEngine) PoolerAddr(server string, connectors bool) string {
	port := components.AlloyDBPoolerPort
	if connectors {
		port = components.AlloyDBConnectorPoolerPort
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if f := e.pools[server][port]; f != nil {
		return f.HostAddr()
	}
	return ""
}

// Promote ends server's recovery, and waits until it takes writes.
func (e *alloyDBEngine) Promote(ctx context.Context, server string) error {
	// The block promotes only a server still in recovery, so it is run
	// again when kubectl exec fails to start it: a pod's exec can fail
	// once ("OCI runtime exec failed ... broken pipe") while the node is
	// busy.
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("promote %s: %w", server, err)
			case <-time.After(3 * time.Second):
			}
		}
		if _, err = e.psql(ctx, server, `DO $$BEGIN
  IF pg_is_in_recovery() THEN
    PERFORM pg_promote(true, 120);
  END IF;
END$$;
SELECT pg_is_in_recovery();`); err == nil {
			return nil
		}
	}
	return fmt.Errorf("promote %s: %w", server, err)
}

func (e *alloyDBEngine) stopTunnel(ctx context.Context, server string) {
	e.mu.Lock()
	f := e.fwds[server]
	delete(e.fwds, server)
	delete(e.up, server)
	e.mu.Unlock()
	if f != nil {
		_ = f.Stop(ctx)
	}
	e.stopPoolers(ctx, server)
}

func (e *alloyDBEngine) Stop(ctx context.Context, server string) error {
	e.stopTunnel(ctx, server)
	if _, err := e.inst.Kubectl(ctx, "-n", e.inst.Namespace, "scale", "deployment/"+server, "--replicas=0"); err != nil {
		return err
	}
	if _, err := e.inst.Kubectl(ctx, "-n", e.inst.Namespace, "wait", "--for=delete", "pod", "-l", "app="+server,
		"--timeout=120s"); err != nil && !strings.Contains(err.Error(), "no matching resources") {
		return err
	}
	return nil
}

func (e *alloyDBEngine) Restart(ctx context.Context, server string) error {
	if _, err := e.inst.Kubectl(ctx, "-n", e.inst.Namespace, "rollout", "restart", "deployment/"+server); err != nil {
		return err
	}
	if err := e.rollout(ctx, server); err != nil {
		return err
	}
	return e.serves(ctx, server)
}

func (e *alloyDBEngine) Remove(ctx context.Context, server string) error {
	e.stopTunnel(ctx, server)
	_, err := e.inst.Kubectl(ctx, "-n", e.inst.Namespace, "delete", "deployment/"+server, "service/"+server,
		"persistentvolumeclaim/"+server+"-data", "--ignore-not-found", "--wait=true")
	return err
}

// Prune removes the AlloyDB servers that keep names none of.
func (e *alloyDBEngine) Prune(ctx context.Context, keep []string) error {
	out, err := e.inst.Kubectl(ctx, "-n", e.inst.Namespace, "get", "deployments", "-l", "cloudburrow.dev/owned=true", "-o", "name")
	if err != nil {
		return err
	}
	kept := map[string]bool{}
	for _, k := range keep {
		kept[k] = true
	}
	for _, line := range strings.Fields(out) {
		_, n, _ := strings.Cut(line, "/")
		if strings.HasPrefix(n, "alloydb-") && !kept[n] {
			e.logf("removing AlloyDB server %s, which this run does not keep", n)
			_ = e.Remove(ctx, n)
		}
	}
	return nil
}

func (e *alloyDBEngine) Dump(ctx context.Context, server, dir string) error {
	return e.dump.Dump(ctx, e.target(server), dir)
}

func (e *alloyDBEngine) Load(ctx context.Context, server, dir string) error {
	return e.dump.Load(ctx, e.target(server), dir)
}

func (e *alloyDBEngine) PITRBase(ctx context.Context, server string) error {
	return e.dump.PITRBase(ctx, e.target(server))
}

func (e *alloyDBEngine) RecoverTo(ctx context.Context, server string, at time.Time, dir string) error {
	return e.dump.pgRecoverToDir(ctx, e.target(server), at, dir)
}

// Close stops every tunnel; the servers stay, with the cluster.
func (e *alloyDBEngine) Close() {
	e.mu.Lock()
	fs := e.fwds
	pools := e.pools
	e.fwds, e.up, e.pools = map[string]*netfwd.Forwarder{}, map[string]bool{}, map[string]map[int]*netfwd.Forwarder{}
	e.mu.Unlock()
	for _, f := range fs {
		_ = f.Stop(context.Background())
	}
	for _, m := range pools {
		for _, f := range m {
			_ = f.Stop(context.Background())
		}
	}
}
