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
		User: user, Database: user, HostAddr: p.hostAddr(spec.Name), Logical: spec.External != nil}
}

// primaryHost is the Service a replica's server replicates from: for a
// replica of a source representation instance (#1501), its hostPort's
// host, or the Service of the CloudBurrow instance hostPort is the tunnel
// of.
func primaryHost(spec sqladmin.Spec) string {
	if ext := spec.External; ext != nil {
		switch {
		case ext.Instance == "":
			return ext.Host
		case ext.InstanceUp && spec.MySQL():
			return string(config.ServiceCloudSQLMySQL)
		case ext.InstanceUp:
			return string(config.ServiceCloudSQL)
		}
		return components.SQLInstancePrefix + ext.Instance
	}
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
	b := components.CloudSQLInstanceBackend(spec.Name, spec.DatabaseVersion, p.persistent, spec.RootPassword, flags)
	if spec.PITREnabled() && !spec.MySQL() {
		if len(b.Args) == 0 {
			b.Args = []string{"postgres"}
		}
		b.Args = append(append([]string{}, b.Args...), pgPITRArgs()...)
	}
	if spec.MasterInstanceName != "" {
		src := components.SQLReplicaSource{
			Host: primaryHost(spec), Password: spec.ReplicationPassword, ServerID: 2 + crc32.ChecksumIEEE([]byte(spec.Name))%1000000}
		if ext := spec.External; ext != nil {
			src.Password, src.User, src.Logical = ext.Password, ext.User, !spec.MySQL()
			if ext.Instance == "" {
				src.Port = ext.Port
			}
		}
		b = components.CloudSQLReplicaBackend(b, spec.MySQL(), src)
		if ext := spec.External; ext != nil && ext.Physical && ext.PhysicalRestored && spec.MySQL() {
			// The restored copy has the source's accounts (#1570).
			b.Args = append(append([]string{}, b.Args...), "--init-file=/var/lib/mysql/"+physicalInitFile)
		}
	}
	return b
}

// mysqlReplicaSetup gives a MySQL replica its primary's databases, with
// the primary's GTIDs as purged, and starts GTID replication from it, once:
// a replica that has a source already is left as it is.
const mysqlReplicaSetup = `set -e
` + mysqlRootEnv + `if [ -n "$(mysql $MYSQL_LOCAL -N -e 'SHOW REPLICA STATUS')" ]; then exit 0; fi
src() { MYSQL_PWD="$CLOUDSQL_REPLICATION_PASSWORD" "$@" -h "$CLOUDSQL_PRIMARY_HOST" -P "${CLOUDSQL_PRIMARY_PORT:-3306}" -u "${CLOUDSQL_PRIMARY_USER:-` + components.SQLReplicaUser + `}"; }
if [ -z "${CLOUDSQL_SEEDED:-}" ]; then # a replica seeded from dumpFilePath, or demoted by demoteMaster, has the data (#1524)
dbs=$(src mysql -N -e "SELECT schema_name FROM information_schema.schemata WHERE schema_name NOT IN ('mysql','sys','information_schema','performance_schema')")
mysql $MYSQL_LOCAL -e 'RESET BINARY LOGS AND GTIDS' 2>/dev/null || mysql $MYSQL_LOCAL -e 'RESET MASTER' # MySQL 8.0's name
if [ -n "$dbs" ]; then
  src mysqldump --single-transaction --set-gtid-purged=ON --routines --triggers --events --databases $dbs | mysql $MYSQL_LOCAL
else
  mysql $MYSQL_LOCAL -e "SET GLOBAL gtid_purged = '$(src mysql -N -e 'SELECT @@GLOBAL.gtid_executed')'"
fi
fi
mysql $MYSQL_LOCAL -e "CHANGE REPLICATION SOURCE TO SOURCE_HOST = '$CLOUDSQL_PRIMARY_HOST', SOURCE_PORT = ${CLOUDSQL_PRIMARY_PORT:-3306}, SOURCE_USER = '${CLOUDSQL_PRIMARY_USER:-` +
	components.SQLReplicaUser + `}', SOURCE_PASSWORD = '$CLOUDSQL_REPLICATION_PASSWORD', SOURCE_AUTO_POSITION = 1, SOURCE_SSL = 1; START REPLICA; SET PERSIST super_read_only = ON"
`

func (p *sqlProvisioner) rollout(ctx context.Context, name string) error {
	return p.rolloutDeployment(ctx, components.SQLInstancePrefix+name)
}

func (p *sqlProvisioner) rolloutDeployment(ctx context.Context, deployment string) error {
	_, err := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "rollout", "status",
		"deployment/"+deployment, "--watch=true", "--timeout=280s")
	if err != nil {
		logs, _ := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "logs", "deployment/"+deployment, "--tail=5")
		return fmt.Errorf("the server %s did not become ready: %w %s", deployment, err, strings.TrimSpace(logs))
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
	primary := spec.PrimaryNode()
	b := p.nodeBackend(spec, primary)
	if primary != 0 {
		// The instance's Service, which the tunnel and the replicas reach,
		// selects the primary's server (#1408).
		if err := p.applyService(ctx, spec); err != nil {
			return err
		}
	}
	if err := p.inst.Apply(ctx, b.Manifest(p.inst.Namespace, p.inst.Instance)); err != nil {
		return err
	}
	if err := p.rolloutDeployment(ctx, b.Name); err != nil {
		return err
	}
	if script := replicaSetup(spec); script != "" {
		// A replica of a source representation instance runs idle until
		// startExternalSync (#1501).
		var out strings.Builder
		if err := podExec(p.cfg, components.SQLInstancePrefix+spec.Name)(ctx, nil, &out, "sh", "-c", script); err != nil {
			return fmt.Errorf("start replication of %s from %s: %w %s", spec.Name, spec.MasterInstanceName, err, strings.TrimSpace(out.String()))
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
	if err := p.serves(ctx, spec); err != nil {
		return err
	}
	return p.standby(ctx, spec)
}

func (p *sqlProvisioner) Restart(ctx context.Context, spec sqladmin.Spec) error {
	d := nodeName(spec.Name, spec.PrimaryNode())
	if _, err := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "rollout", "restart", "deployment/"+d); err != nil {
		return err
	}
	if err := p.rolloutDeployment(ctx, d); err != nil {
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
	sb := components.SQLStandbyPrefix + name
	_, err := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "delete",
		"deployment/"+obj, "service/"+obj, "persistentvolumeclaim/"+obj+"-data",
		"deployment/"+sb, "service/"+sb, "persistentvolumeclaim/"+sb+"-data",
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
			if rest, ok = strings.CutPrefix(n, components.SQLStandbyPrefix); !ok {
				continue
			}
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

// replicaSetup is the script that gives a replica its primary's data and
// starts its replication, or "" for none: a PostgreSQL replica of a
// created instance is a standby from the start, and an external replica
// waits for startExternalSync. An OFFLINE sync (#1524) only copies.
func replicaSetup(spec sqladmin.Spec) string {
	ext := spec.External
	switch {
	case spec.MasterInstanceName == "":
		return ""
	case ext != nil && !spec.ExternalSyncStarted:
		return ""
	case ext != nil && !spec.MySQL():
		return "export CLOUDSQL_SYNC_MODE=" + shellQuote(ext.SyncMode) + " CLOUDSQL_DATABASES=" + shellQuote(strings.Join(ext.Databases, " ")) +
			" CLOUDSQL_SYNC_ID=" + fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(spec.Project+":"+spec.Name))) + "\n" + pgExternalSetup
	case !spec.MySQL():
		return ""
	case ext != nil && ext.SyncMode == "OFFLINE":
		return mysqlOfflineSetup
	case ext != nil && ext.Physical:
		// Restored from an XtraBackup backup (#1570, sqlphysical.go).
		return mysqlPhysicalSetup
	case spec.Seeded:
		// A former primary a switchover made a replica (#1614) has its data.
		return "export CLOUDSQL_SEEDED=1\n" + mysqlReplicaSetup
	case ext != nil && (ext.DumpFile != "" || ext.Demoted):
		return "export CLOUDSQL_SEEDED=1\n" + mysqlReplicaSetup
	}
	return mysqlReplicaSetup
}

// mysqlOfflineSetup copies an external source's databases into the
// replica once (#1524): an OFFLINE sync, with no replication after.
const mysqlOfflineSetup = `set -e
` + mysqlRootEnv + `marker=/var/lib/mysql/cloudsql-external-synced
if [ -f "$marker" ]; then exit 0; fi
src() { MYSQL_PWD="$CLOUDSQL_REPLICATION_PASSWORD" "$@" -h "$CLOUDSQL_PRIMARY_HOST" -P "${CLOUDSQL_PRIMARY_PORT:-3306}" -u "$CLOUDSQL_PRIMARY_USER"; }
dbs=$(src mysql -N -e "SELECT schema_name FROM information_schema.schemata WHERE schema_name NOT IN ('mysql','sys','information_schema','performance_schema')")
if [ -n "$dbs" ]; then
  src mysqldump --single-transaction --set-gtid-purged=OFF --routines --triggers --events --databases $dbs | mysql $MYSQL_LOCAL
fi
touch "$marker"
`

// pgExternalSetup gives a PostgreSQL replica of an external primary
// (#1524) the source's databases: every one but the templates, or
// CLOUDSQL_DATABASES. Each database's schema is copied with pg_dump; an
// ONLINE sync then publishes its tables on the source and subscribes the
// replica's database to them, which copies their rows and goes on
// replicating; an OFFLINE sync copies the rows with pg_dump instead. It
// runs once: a marker beside the data directory records it.
const pgExternalSetup = `set -e
marker="$(dirname "$PGDATA")/cloudsql-external-synced"
if [ -f "$marker" ]; then exit 0; fi
i=0; until pg_isready -q -h 127.0.0.1 -U postgres; do i=$((i+1)); if [ "$i" -ge 120 ]; then echo "postgres did not answer" >&2; exit 1; fi; sleep 1; done
export PGPASSWORD="$CLOUDSQL_REPLICATION_PASSWORD"
conn="host=$CLOUDSQL_PRIMARY_HOST port=${CLOUDSQL_PRIMARY_PORT:-5432} user=$CLOUDSQL_PRIMARY_USER"
# An empty "password=" would take the next keyword as its value.
if [ -n "$CLOUDSQL_REPLICATION_PASSWORD" ]; then conn="$conn password=$CLOUDSQL_REPLICATION_PASSWORD"; fi
dst() { PGPASSWORD= psql -h 127.0.0.1 -U postgres -v ON_ERROR_STOP=1 -qAt "$@"; }
dbs="$CLOUDSQL_DATABASES"
if [ -z "$dbs" ]; then
  dbs=$(psql "$conn dbname=postgres" -qAt -c "SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate AND datname NOT IN ('template0','template1','cloudsqladmin','rdsadmin') ORDER BY 1")
fi
n=0
for db in $dbs; do
  n=$((n+1))
  if [ -z "$(dst -d postgres -c "SELECT 1 FROM pg_database WHERE datname = '$db'")" ]; then
    dst -d postgres -c "CREATE DATABASE \"$db\""
  fi
  pg_dump "$conn dbname=$db" --schema-only --no-owner --no-privileges --no-publications --no-subscriptions | PGPASSWORD= psql -h 127.0.0.1 -U postgres -q -d "$db" >/dev/null
  if [ "$CLOUDSQL_SYNC_MODE" = OFFLINE ]; then
    pg_dump "$conn dbname=$db" --data-only --no-owner --no-privileges --no-publications --no-subscriptions | dst -d "$db" >/dev/null
    continue
  fi
  pub="cloudsql_external_$CLOUDSQL_SYNC_ID"
  if [ -z "$(psql "$conn dbname=$db" -qAt -c "SELECT 1 FROM pg_publication WHERE pubname = '$pub'")" ]; then
    if [ "$(psql "$conn dbname=$db" -qAt -c "SELECT rolsuper FROM pg_roles WHERE rolname = current_user")" = t ]; then
      psql "$conn dbname=$db" -v ON_ERROR_STOP=1 -q -c "CREATE PUBLICATION $pub FOR ALL TABLES"
    else
      tables=$(psql "$conn dbname=$db" -qAt -c "SELECT string_agg(format('%I.%I', n.nspname, c.relname), ', ') FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind IN ('r','p') AND NOT c.relispartition AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg\_%'")
      if [ -n "$tables" ]; then
        psql "$conn dbname=$db" -v ON_ERROR_STOP=1 -q -c "CREATE PUBLICATION $pub FOR TABLE $tables"
      else
        psql "$conn dbname=$db" -v ON_ERROR_STOP=1 -q -c "CREATE PUBLICATION $pub"
      fi
    fi
  fi
  dst -d "$db" -c "CREATE SUBSCRIPTION cloudsql_external_$n CONNECTION '$conn dbname=$db' PUBLICATION $pub WITH (slot_name = '${pub}_$n')"
done
touch "$marker"
`
