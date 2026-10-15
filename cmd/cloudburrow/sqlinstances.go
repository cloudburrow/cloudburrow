package main

import (
	"context"
	"fmt"
	"hash/crc32"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// sqlProvisioner runs the Cloud SQL instances created through the Admin
// API (#1330) in the cluster: each is a Deployment of the image and
// component code `up` runs for --services cloudsql or cloudsql-mysql
// (components.CloudSQLInstanceBackend), with a volume when the instance is
// persistent, and its own host tunnel.
type sqlProvisioner struct {
	cfg        config.Config
	inst       *components.Installer
	persistent bool
	logw       io.Writer

	mu sync.Mutex
	// fwds are the tunnels, by instance name; up is which have started.
	fwds map[string]*netfwd.Forwarder
	up   map[string]bool
}

func newSQLProvisioner(cfg config.Config, logw io.Writer) *sqlProvisioner {
	return &sqlProvisioner{
		cfg: cfg,
		inst: &components.Installer{Kubeconfig: cfg.KubeconfigPath(), Namespace: cfg.Cluster.Namespace,
			Instance: cfg.Name},
		persistent: cfg.Mode == config.ModePersistent,
		logw:       logw,
		fwds:       map[string]*netfwd.Forwarder{},
		up:         map[string]bool{},
	}
}

// sqlInstancesStatePath is where a persistent instance keeps the records
// of the Cloud SQL instances created in it.
func sqlInstancesStatePath(cfg config.Config) string {
	if cfg.Mode != config.ModePersistent {
		return ""
	}
	return filepath.Join(cfg.InstanceDir(), "cloudsql-instances.json")
}

func (p *sqlProvisioner) logf(format string, args ...any) {
	if p.logw != nil {
		fmt.Fprintf(p.logw, "cloudburrow: "+format+"\n", args...)
	}
}

func (p *sqlProvisioner) port(spec sqladmin.Spec) int {
	if spec.MySQL() {
		return components.CloudSQLMySQLPort
	}
	return components.CloudSQLPort
}

// forwarder is name's tunnel, made (and its host port reserved) once.
func (p *sqlProvisioner) forwarder(spec sqladmin.Spec) *netfwd.Forwarder {
	p.mu.Lock()
	defer p.mu.Unlock()
	if f := p.fwds[spec.Name]; f != nil {
		return f
	}
	f := netfwd.New(netfwd.Target{Name: components.SQLInstancePrefix + spec.Name, Namespace: p.inst.Namespace,
		ServicePort: p.port(spec), Label: "cloudsql-instance-" + spec.Name}, p.cfg.KubeconfigPath(), p.cfg.BindAddress)
	f.Logf = p.logf
	p.fwds[spec.Name] = f
	return f
}

func (p *sqlProvisioner) hostAddr(name string) func() string {
	return func() string {
		p.mu.Lock()
		defer p.mu.Unlock()
		if f := p.fwds[name]; f != nil && p.up[name] {
			return f.HostAddr()
		}
		return ""
	}
}

func (p *sqlProvisioner) Engine(spec sqladmin.Spec) sqladmin.Engine {
	if spec.MySQL() {
		return &sqladmin.MySQL{Name: spec.Name, Version: spec.DatabaseVersion,
			RootPassword: spec.RootPassword, HostAddr: p.hostAddr(spec.Name)}
	}
	user := "postgres"
	if spec.MasterUp {
		// A replica of `up`'s server has its data, and its superuser.
		user = components.CloudSQLUser
	}
	return &sqladmin.Postgres{Name: spec.Name, Version: spec.DatabaseVersion,
		User: user, Database: user, HostAddr: p.hostAddr(spec.Name)}
}

// primaryHost is the Service a replica's server replicates from.
func primaryHost(spec sqladmin.Spec) string {
	switch {
	case !spec.MasterUp:
		return components.SQLInstancePrefix + spec.MasterInstanceName
	case spec.MySQL():
		return string(config.ServiceCloudSQLMySQL)
	}
	return string(config.ServiceCloudSQL)
}

func (p *sqlProvisioner) backend(spec sqladmin.Spec) components.Backend {
	var flags []components.SQLInstanceFlag
	for _, f := range spec.Flags {
		flags = append(flags, components.SQLInstanceFlag{Name: f.Name, Value: f.Value})
	}
	b := components.CloudSQLInstanceBackend(spec.Name, spec.MySQL(), p.persistent, spec.RootPassword, flags)
	if spec.MasterInstanceName != "" {
		b = components.CloudSQLReplicaBackend(b, spec.MySQL(), components.SQLReplicaSource{
			Host: primaryHost(spec), Password: spec.ReplicationPassword, ServerID: 2 + crc32.ChecksumIEEE([]byte(spec.Name))%1000000})
	}
	return b
}

// mysqlReplicaSetup gives a MySQL replica its primary's databases, with
// the primary's GTIDs as purged, and starts GTID replication from it, once:
// a replica that has a source already is left as it is.
const mysqlReplicaSetup = `set -e
export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:-}"
if [ -n "$(mysql -uroot -N -e 'SHOW REPLICA STATUS')" ]; then exit 0; fi
src() { MYSQL_PWD="$CLOUDSQL_REPLICATION_PASSWORD" "$@" -h "$CLOUDSQL_PRIMARY_HOST" -u ` + components.SQLReplicaUser + `; }
dbs=$(src mysql -N -e "SELECT schema_name FROM information_schema.schemata WHERE schema_name NOT IN ('mysql','sys','information_schema','performance_schema')")
mysql -uroot -e 'RESET BINARY LOGS AND GTIDS'
if [ -n "$dbs" ]; then
  src mysqldump --single-transaction --set-gtid-purged=ON --routines --triggers --events --databases $dbs | mysql -uroot
else
  mysql -uroot -e "SET GLOBAL gtid_purged = '$(src mysql -N -e 'SELECT @@GLOBAL.gtid_executed')'"
fi
mysql -uroot -e "CHANGE REPLICATION SOURCE TO SOURCE_HOST = '$CLOUDSQL_PRIMARY_HOST', SOURCE_PORT = 3306, SOURCE_USER = '` +
	components.SQLReplicaUser + `', SOURCE_PASSWORD = '$CLOUDSQL_REPLICATION_PASSWORD', SOURCE_AUTO_POSITION = 1, SOURCE_SSL = 1; START REPLICA; SET PERSIST super_read_only = ON"
`

func (p *sqlProvisioner) rollout(ctx context.Context, name string) error {
	_, err := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "rollout", "status",
		"deployment/"+components.SQLInstancePrefix+name, "--watch=true", "--timeout=280s")
	if err != nil {
		logs, _ := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "logs", "deployment/"+components.SQLInstancePrefix+name, "--tail=5")
		return fmt.Errorf("the server of instance %s did not become ready: %w %s", name, err, strings.TrimSpace(logs))
	}
	return nil
}

// serves waits for the server to answer through its tunnel.
func (p *sqlProvisioner) serves(ctx context.Context, spec sqladmin.Spec) error {
	e := p.Engine(spec)
	for {
		_, err := e.InstalledVersion(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the server of instance %s does not answer through its tunnel: %w", spec.Name, err)
		case <-time.After(time.Second):
		}
	}
}

func (p *sqlProvisioner) Start(ctx context.Context, spec sqladmin.Spec) error {
	b := p.backend(spec)
	if err := p.inst.Apply(ctx, b.Manifest(p.inst.Namespace, p.inst.Instance)); err != nil {
		return err
	}
	if err := p.rollout(ctx, spec.Name); err != nil {
		return err
	}
	if spec.MasterInstanceName != "" && spec.MySQL() {
		var out strings.Builder
		if err := podExec(p.cfg, components.SQLInstancePrefix+spec.Name)(ctx, nil, &out, "sh", "-c", mysqlReplicaSetup); err != nil {
			return fmt.Errorf("start replication of %s from %s: %w", spec.Name, spec.MasterInstanceName, err)
		}
	}
	f := p.forwarder(spec)
	p.mu.Lock()
	started := p.up[spec.Name]
	p.mu.Unlock()
	if !started {
		if err := f.Start(ctx); err != nil {
			return fmt.Errorf("the tunnel of instance %s: %w", spec.Name, err)
		}
		p.mu.Lock()
		p.up[spec.Name] = true
		p.mu.Unlock()
	}
	return p.serves(ctx, spec)
}

func (p *sqlProvisioner) Restart(ctx context.Context, spec sqladmin.Spec) error {
	if _, err := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "rollout", "restart",
		"deployment/"+components.SQLInstancePrefix+spec.Name); err != nil {
		return err
	}
	if err := p.rollout(ctx, spec.Name); err != nil {
		return err
	}
	return p.serves(ctx, spec)
}

func (p *sqlProvisioner) Remove(ctx context.Context, name string) error {
	p.mu.Lock()
	f := p.fwds[name]
	delete(p.fwds, name)
	delete(p.up, name)
	p.mu.Unlock()
	if f != nil {
		_ = f.Stop(ctx)
	}
	obj := components.SQLInstancePrefix + name
	// The volume goes with the server, as a deleted Cloud SQL instance's
	// data does.
	_, err := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "delete",
		"deployment/"+obj, "service/"+obj, "persistentvolumeclaim/"+obj+"-data",
		"--ignore-not-found", "--wait=true")
	return err
}

func (p *sqlProvisioner) Prune(ctx context.Context, keep []string) error {
	out, err := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "get", "deployments,persistentvolumeclaims",
		"-l", "cloudburrow.dev/owned=true", "-o", "name")
	if err != nil {
		return err
	}
	kept := map[string]bool{}
	for _, k := range keep {
		kept[k] = true
	}
	for _, line := range strings.Fields(out) {
		_, n, _ := strings.Cut(line, "/")
		rest, ok := strings.CutPrefix(n, components.SQLInstancePrefix)
		if !ok {
			continue
		}
		if strings.HasPrefix(line, "persistentvolumeclaim") {
			rest = strings.TrimSuffix(rest, "-data")
		}
		if !kept[rest] {
			p.logf("removing Cloud SQL instance %s, which this run does not keep", rest)
			_ = p.Remove(ctx, rest)
		}
	}
	return nil
}

// Close stops every tunnel; the servers stay, with the cluster.
func (p *sqlProvisioner) Close() {
	p.mu.Lock()
	fs := p.fwds
	p.fwds, p.up = map[string]*netfwd.Forwarder{}, map[string]bool{}
	p.mu.Unlock()
	for _, f := range fs {
		_ = f.Stop(context.Background())
	}
}
