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
// (components.CloudSQLInstanceBackend), with a volume, and its own host
// tunnel. The volume is there in either mode (#1740): a restart of the
// server (a flag, tier or edition change, instances.restart, a failover)
// replaces its pod, and Google keeps an instance's data across those. In
// ephemeral mode it goes with the instance's record: the next `up` keeps no
// record and prunes the server with its volume, and `delete` takes the
// cluster, and every volume in it, away.
type sqlProvisioner struct {
	cfg        config.Config
	inst       *components.Installer
	persistent bool
	logw       io.Writer

	mu sync.Mutex
	// fwds are the tunnels, by instance name; up is which have started.
	fwds map[string]*netfwd.Forwarder
	up   map[string]bool
	// pools are the managed connection poolers' tunnels (#2141), by
	// instance name, while their instance pools connections.
	pools map[string]*netfwd.Forwarder
	// poolImage is the storage image MySQL's thread pool runs from,
	// "" until an instance needs it (#2141).
	poolImage string

	// legacyOnce asks the node once whether it runs linux/amd64
	// binaries; legacyWhy is why not, or "" (sqllegacy.go, #1524).
	legacyOnce sync.Once
	legacyWhy  string
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
		pools:      map[string]*netfwd.Forwarder{},
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
	if sqladmin.IsSQLServer(spec.DatabaseVersion) {
		return components.CloudSQLServerPort
	}
	if spec.MySQL() {
		return components.CloudSQLMySQLPort
	}
	return components.CloudSQLPort
}

// forwarder is name's tunnel, made (and its host port reserved) once.
func (p *sqlProvisioner) forwarder(spec sqladmin.Spec) *netfwd.Forwarder {
	p.mu.Lock()
	defer p.mu.Unlock()
	srv := spec.ServerName()
	if f := p.fwds[srv]; f != nil {
		return f
	}
	f := netfwd.New(netfwd.Target{Name: components.SQLInstancePrefix + srv, Namespace: p.inst.Namespace,
		ServicePort: p.port(spec), Label: "cloudsql-instance-" + srv}, p.cfg.KubeconfigPath(), p.cfg.BindAddress)
	f.Logf = p.logf
	p.fwds[srv] = f
	return f
}

// poolerAddr is the host address of name's managed connection pooler,
// "" while it has none (#2141).
func (p *sqlProvisioner) poolerAddr(name string) func() string {
	return func() string {
		p.mu.Lock()
		defer p.mu.Unlock()
		if f := p.pools[name]; f != nil {
			return f.HostAddr()
		}
		return ""
	}
}

// syncPooler starts the tunnel of spec's managed connection pooler when
// it pools connections, and stops it when it no longer does (#2141).
func (p *sqlProvisioner) syncPooler(ctx context.Context, spec sqladmin.Spec) error {
	srv := spec.ServerName()
	p.mu.Lock()
	f := p.pools[srv]
	p.mu.Unlock()
	if !spec.PGPooling() {
		if f != nil {
			p.mu.Lock()
			delete(p.pools, srv)
			p.mu.Unlock()
			_ = f.Stop(ctx)
		}
		return nil
	}
	if f != nil {
		return nil
	}
	f = netfwd.New(netfwd.Target{Name: components.SQLInstancePrefix + srv, Namespace: p.inst.Namespace,
		ServicePort: sqladmin.PoolerPort, Label: "cloudsql-pooler-" + srv}, p.cfg.KubeconfigPath(), p.cfg.BindAddress)
	f.Logf = p.logf
	if err := f.Start(ctx); err != nil {
		return fmt.Errorf("the tunnel of instance %s's connection pooler: %w", spec.Name, err)
	}
	p.mu.Lock()
	p.pools[srv] = f
	p.mu.Unlock()
	p.logf("Cloud SQL instance %s pools connections: its pooler (port %d) is at %s", spec.Name, sqladmin.PoolerPort, f.HostAddr())
	return nil
}

// sqlThreadPoolImage is the cloudburrow-storage image's reference, built
// when first asked for, which runs Cloud SQL for MySQL's thread pool
// (#2141); up sets it, nil where it cannot build the image.
var sqlThreadPoolImage func(context.Context) (string, error)

// ensureThreadPoolImage builds the storage image before a server of
// spec's that pools connections starts with its thread pool.
func (p *sqlProvisioner) ensureThreadPoolImage(ctx context.Context, spec sqladmin.Spec) error {
	if !spec.MySQLThreadPool() || p.threadPoolImage() != "" {
		return nil
	}
	if sqlThreadPoolImage == nil {
		return fmt.Errorf("instance %s pools connections, and this instance cannot build the cloudburrow-storage image its thread pool runs from", spec.Name)
	}
	img, err := sqlThreadPoolImage(ctx)
	if err != nil {
		return fmt.Errorf("the thread pool of instance %s: %w", spec.Name, err)
	}
	p.mu.Lock()
	p.poolImage = img
	p.mu.Unlock()
	return nil
}

func (p *sqlProvisioner) threadPoolImage() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.poolImage
}

func hasFlag(flags []components.SQLInstanceFlag, name string) bool {
	for _, f := range flags {
		if f.Name == name {
			return true
		}
	}
	return false
}

// stopPooler stops name's pooler tunnel, if it has one.
func (p *sqlProvisioner) stopPooler(ctx context.Context, name string) {
	p.mu.Lock()
	f := p.pools[name]
	delete(p.pools, name)
	p.mu.Unlock()
	if f != nil {
		_ = f.Stop(ctx)
	}
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
	if sqladmin.IsSQLServer(spec.DatabaseVersion) {
		return &sqladmin.SQLServer{Name: spec.Name, Version: spec.DatabaseVersion, SAPassword: spec.SAPassword,
			HostAddr: p.hostAddr(spec.ServerName())}
	}
	if spec.MySQL() {
		return &sqladmin.MySQL{Name: spec.Name, Version: spec.DatabaseVersion,
			RootPassword: spec.RootPassword, Admin: sqladmin.MySQLAdminUser, HostAddr: p.hostAddr(spec.ServerName())}
	}
	user := "postgres"
	if spec.MasterUp && !spec.AwaitingPrimary {
		// A replica of `up`'s server has its data, and its superuser.
		user = components.CloudSQLUser
	}
	pg := &sqladmin.Postgres{Name: spec.Name, Version: spec.DatabaseVersion,
		User: user, Database: user, HostAddr: p.hostAddr(spec.ServerName()), Logical: spec.External != nil,
		PoolerHostAddr: p.poolerAddr(spec.ServerName())}
	if spec.MasterInstanceName == "" {
		// A primary's failed sign-ins lock a user with a lock after
		// failed attempts (#2014); a replica's users are the primary's.
		pg.SignInLog = pgSignInLog(p.cfg, components.SQLInstancePrefix+spec.ServerName(), true)
		pg.LogKey = spec.ServerName()
	}
	return pg
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
		return components.SQLInstancePrefix + sqlServerOf(ext.Instance)
	}
	switch {
	case !spec.MasterUp:
		return components.SQLInstancePrefix + sqlServerOf(spec.MasterInstanceName)
	case spec.MySQL():
		return string(config.ServiceCloudSQLMySQL)
	}
	return string(config.ServiceCloudSQL)
}

func (p *sqlProvisioner) backend(spec sqladmin.Spec) components.Backend {
	if sqladmin.IsSQLServer(spec.DatabaseVersion) {
		return sqlServerBackend(spec)
	}
	var flags []components.SQLInstanceFlag
	for _, f := range spec.Flags {
		if sqladmin.IsCloudSQLOnlyFlag(f.Name) {
			// Cloud SQL's own (#1944), not the server's.
			continue
		}
		flags = append(flags, components.SQLInstanceFlag{Name: f.Name, Value: f.Value})
	}
	if spec.MySQLThreadPool() && !hasFlag(flags, "thread_cache_size") {
		// "When Managed Connection Pooling is enabled for your instance,
		// the value of the thread_cache_size metric is defaulted to 0."
		flags = append(flags, components.SQLInstanceFlag{Name: "thread_cache_size", Value: "0"})
	}
	b := components.CloudSQLInstanceBackend(spec.ServerName(), spec.DatabaseVersion, true, spec.RootPassword, flags)
	if spec.PGPooling() {
		// Managed connection pooling (#2141): PgBouncer beside the
		// server, on Google's 6432.
		b.Sidecars = append(b.Sidecars, components.CloudSQLPooler(sqladmin.PoolerPort, spec.PoolerSettings()))
	}
	if spec.MySQLThreadPool() {
		// Cloud SQL for MySQL's managed connection pooling (#2141): the
		// thread pool in front of mysqld, from the storage image, which
		// Start made sure of.
		if img := p.threadPoolImage(); img != "" {
			b = components.CloudSQLMySQLThreadPool(b, img, spec.MySQLPoolSize())
		}
	}
	if spec.PITREnabled() && !spec.MySQL() {
		if len(b.Args) == 0 {
			b.Args = []string{"postgres"}
		}
		b.Args = append(append([]string{}, b.Args...), pgPITRArgs()...)
	}
	// A PostgreSQL replica upgraded on its own (#1524) runs an empty
	// server of its new version until its primary's upgrade rebuilds it.
	if spec.MasterInstanceName != "" && !spec.AwaitingPrimary {
		src := components.SQLReplicaSource{
			Host: primaryHost(spec), Password: spec.ReplicationPassword, ServerID: 2 + crc32.ChecksumIEEE([]byte(spec.ServerName()))%1000000,
			Epoch: spec.RebuildEpoch}
		if ext := spec.External; ext != nil {
			src.Password, src.User, src.Logical = ext.Password, ext.User, !spec.MySQL()
			if ext.Instance == "" {
				src.Port = ext.Port
			}
		}
		b = components.CloudSQLReplicaBackend(b, spec.MySQL(), src)
		if p.scratchPhysical(spec) {
			// An ephemeral instance's physically synced replica (#1614):
			// a restored copy swapped in and given its --init-file at start
			// (sqlphysical.go).
			b.Command = []string{"sh", "-c", physicalStart(spec.DatabaseVersion), "sh"}
		} else if ext := spec.External; ext != nil && ext.Physical && ext.PhysicalRestored && spec.MySQL() {
			// The restored copy has the source's accounts (#1570).
			b.Args = append(append([]string{}, b.Args...), "--init-file=/var/lib/mysql/"+physicalInitFile)
		}
	}
	return b
}

// mysqlReplicaSetup gives a MySQL replica its primary's databases, with
// the primary's GTIDs as purged, and starts GTID replication from it, once:
// a replica that has a source already is left as it is.
//
// A replica of a Cloud SQL instance has its primary's users too, the ones
// made before it as well as after: Cloud SQL's replica is a copy of the
// whole instance. The dump leaves out the mysql database, and the GTIDs it
// marks purged include the CREATE USER and ALTER USER statements made
// before, so replication never replays them: the accounts are copied as a
// backup carries them (mysqlAccountStatements), read through the
// replication user, whose SELECT covers mysql.user. Main run 45126544176:
// the green instance of a blue-green deployment had none of the blue
// instance's users, so after the switchover neither had the blue name,
// nor its clone ("Access denied for user 'appuser'", TestCloudSQLMySQL57).
// They are written with the binary log off, as CloudBurrow's own accounts
// are, so the replica has no transaction of its own, and with --force, so
// a grant the replica's newer version refuses leaves that one out rather
// than the replica unbuilt. An external source's replica
// (CLOUDSQL_PRIMARY_USER) is given its databases only, as before.
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
if [ -z "${CLOUDSQL_PRIMARY_USER:-}" ]; then
  acct() { src mysql "$@"; }
  primary_accounts() {
` + mysqlAccountStatements + `}
  accounts=$(primary_accounts)
  ifx='IF EXISTS '; case "$(mysql $MYSQL_LOCAL -N -B -e 'SELECT VERSION()')" in 5.6*) ifx= ;; esac
  drops=$(mysql $MYSQL_LOCAL -N -B -e "SELECT CONCAT('DROP USER $ifx', QUOTE(user), '@', QUOTE(host), ';') FROM mysql.user WHERE ` + mysqlUserAccounts + `")
  { echo 'SET SESSION sql_log_bin = 0;'; printf '%s\n' "$drops" "$accounts"; echo 'FLUSH PRIVILEGES;'; } | mysql $MYSQL_LOCAL --force ||
    echo "the replica refused some of its primary's account statements; the others were applied" >&2
fi
fi
mysql $MYSQL_LOCAL -e "CHANGE REPLICATION SOURCE TO SOURCE_HOST = '$CLOUDSQL_PRIMARY_HOST', SOURCE_PORT = ${CLOUDSQL_PRIMARY_PORT:-3306}, SOURCE_USER = '${CLOUDSQL_PRIMARY_USER:-` +
	components.SQLReplicaUser + `}', SOURCE_PASSWORD = '$CLOUDSQL_REPLICATION_PASSWORD', SOURCE_AUTO_POSITION = 1, SOURCE_SSL = 1; START REPLICA; SET PERSIST super_read_only = ON"
`

// mysqlLegacyFirstStart starts a MySQL 5.7 or 5.6 server's binary log
// afresh at its first start (#1524): it runs with GTIDs from its
// initialisation (legacyMySQLArgs), whose time zone tables the image's
// entrypoint loads with the binary log on, and a new Cloud SQL instance
// has no transaction of its own a replica, or the source it is demoted to
// replicate from, would see. A marker on its volume records it.
const mysqlLegacyFirstStart = `set -e
` + mysqlRootEnv + `m=/var/lib/mysql/cloudsql-binlog-started
[ -f "$m" ] && exit 0
mysql $MYSQL_LOCAL -e 'RESET MASTER'
touch "$m"
`

// mysqlSystemAccountsScript gives a MySQL server CloudBurrow's own
// account (sqladmin.MySQLAdminUser, #1993), reading the statements on its
// standard input so the password never is on a command line. It signs in
// as that account, or as root over TCP or over the socket on a server
// that has none yet: a new one, whose 'root'@'%' no client has had the
// time to delete, or one made before #1993, whose root could not be
// deleted. A read replica's or a standby's super_read_only is lifted
// while it runs; the statements turn the binary log off.
const mysqlSystemAccountsScript = `set -e
sql=$(cat)
` + mysqlRootEnv + `mysql $MYSQL_LOCAL -N -e 'SELECT 1' >/dev/null 2>&1 || MYSQL_LOCAL="-uroot"
sro=$(mysql $MYSQL_LOCAL -N -e 'SELECT @@GLOBAL.super_read_only' 2>/dev/null || echo 0)
{
  if [ "$sro" = 1 ]; then echo 'SET GLOBAL super_read_only = OFF;'; fi
  printf '%s\n' "$sql"
  if [ "$sro" = 1 ]; then echo 'SET GLOBAL super_read_only = ON;'; fi
} | mysql $MYSQL_LOCAL
`

// mysqlSystemAccounts runs mysqlSystemAccountsScript in deployment's
// server, each time it starts: an instance made before #1993 gets the
// account at its first start after it.
func (p *sqlProvisioner) mysqlSystemAccounts(ctx context.Context, deployment string, spec sqladmin.Spec) error {
	var out strings.Builder
	sql := sqladmin.MySQLSystemAccountsSQL(spec.DatabaseVersion, spec.RootPassword)
	if err := nodeExec(p.cfg, deployment)(ctx, strings.NewReader(sql), &out, "sh", "-c", mysqlSystemAccountsScript); err != nil {
		return fmt.Errorf("create CloudBurrow's account on %s: %w %s", deployment, err, strings.TrimSpace(out.String()))
	}
	return nil
}

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
	if err := p.ensureThreadPoolImage(ctx, spec); err != nil {
		return err
	}
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
	if spec.MySQL() {
		if err := p.mysqlSystemAccounts(ctx, b.Name, spec); err != nil {
			return err
		}
	}
	if sqladmin.IsLegacyMySQL(spec.DatabaseVersion) {
		var out strings.Builder
		if err := nodeExec(p.cfg, b.Name)(ctx, nil, &out, "sh", "-c", mysqlLegacyFirstStart); err != nil {
			return fmt.Errorf("start the binary log of %s: %w %s", spec.Name, err, strings.TrimSpace(out.String()))
		}
	}
	if script := replicaSetup(spec); script != "" {
		// A replica of a source representation instance runs idle until
		// startExternalSync (#1501).
		script = sqladmin.MySQLDialect(spec.DatabaseVersion, script)
		var out strings.Builder
		if err := podExec(p.cfg, components.SQLInstancePrefix+spec.ServerName())(ctx, nil, &out, "sh", "-c", script); err != nil {
			return fmt.Errorf("start replication of %s from %s: %w %s", spec.Name, spec.MasterInstanceName, err, strings.TrimSpace(out.String()))
		}
	}
	f := p.forwarder(spec)
	p.mu.Lock()
	started := p.up[spec.ServerName()]
	p.mu.Unlock()
	if !started {
		if err := f.Start(ctx); err != nil {
			return fmt.Errorf("the tunnel of instance %s: %w", spec.Name, err)
		}
		p.mu.Lock()
		p.up[spec.ServerName()] = true
		p.mu.Unlock()
	}
	if err := p.serves(ctx, spec); err != nil {
		return err
	}
	if sqladmin.IsSQLServer(spec.DatabaseVersion) {
		return p.sqlServerSetup(ctx, spec)
	}
	if err := p.syncPooler(ctx, spec); err != nil {
		return err
	}
	if err := p.standby(ctx, spec); err != nil {
		return err
	}
	p.watchSignIns(ctx, spec)
	return nil
}

// watchSignIns counts the failed sign-ins of spec's server, when one of
// its roles is locked by them (#2014).
func (p *sqlProvisioner) watchSignIns(ctx context.Context, spec sqladmin.Spec) {
	if pg, ok := p.Engine(spec).(*sqladmin.Postgres); ok {
		if err := pg.WatchSignIns(ctx); err != nil {
			p.logf("counting the failed sign-ins of Cloud SQL instance %s: %v", spec.Name, err)
		}
	}
}

func (p *sqlProvisioner) Restart(ctx context.Context, spec sqladmin.Spec) error {
	d := nodeName(spec.ServerName(), spec.PrimaryNode())
	if _, err := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "rollout", "restart", "deployment/"+d); err != nil {
		return err
	}
	if err := p.rolloutDeployment(ctx, d); err != nil {
		return err
	}
	if err := p.serves(ctx, spec); err != nil {
		return err
	}
	if sqladmin.IsSQLServer(spec.DatabaseVersion) {
		// Trace flags last until the server stops.
		return p.sqlServerSetup(ctx, spec)
	}
	if sqladmin.IsLegacyMySQL(spec.DatabaseVersion) && spec.Regional() {
		// MySQL 5.7 and 5.6 have no SET PERSIST (#1524): the primary's
		// semi-synchronous replication is set again, as at its start.
		return p.mysqlStandby(ctx, spec)
	}
	return nil
}

// sqlServerSetup gives a SQL Server instance's server its sqlserver user
// and its database flags (#1916), each time it starts.
func (p *sqlProvisioner) sqlServerSetup(ctx context.Context, spec sqladmin.Spec) error {
	ss, ok := p.Engine(spec).(*sqladmin.SQLServer)
	if !ok {
		return nil
	}
	if err := ss.Setup(ctx, spec.RootPassword, spec.Flags); err != nil {
		return fmt.Errorf("set up the sqlserver user and flags of %s: %w", spec.Name, err)
	}
	return nil
}

// Stop shuts a stopped instance's server down (#1875): its tunnel goes,
// and its Deployments (a REGIONAL instance's standby too) are scaled to no
// pods, their volumes kept. Start applies them again, with one pod each.
func (p *sqlProvisioner) Stop(ctx context.Context, spec sqladmin.Spec) error {
	srv := spec.ServerName()
	sqladmin.StopSignInWatch(srv)
	p.stopPooler(ctx, srv)
	p.mu.Lock()
	f := p.fwds[srv]
	delete(p.fwds, srv)
	delete(p.up, srv)
	p.mu.Unlock()
	if f != nil {
		_ = f.Stop(ctx)
	}
	for node := 0; node < 2; node++ {
		d := nodeName(srv, node)
		if node == 1 && spec.HAEpochs == [2]int{} {
			break // never had a standby
		}
		if _, err := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "scale", "deployment/"+d, "--replicas=0"); err != nil {
			if node == 1 {
				continue // a standby removed when high availability was turned off
			}
			return fmt.Errorf("stop the server of instance %s: %w", spec.Name, err)
		}
		// kubectl wait answers "no matching resources" once none is left.
		if _, err := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "wait", "--for=delete", "pod", "-l", "app="+d,
			"--timeout=120s"); err != nil && !strings.Contains(err.Error(), "no matching resources") {
			return fmt.Errorf("stop the server of instance %s: %w", spec.Name, err)
		}
	}
	return nil
}

// sqlServerOf is the name of a created instance's server in the cluster
// (sqladmin.Server.ServerOf): its own, unless a blue-green switchover
// moved it (#1614).
var sqlServerOf = func(name string) string { return name }

// Remove deletes server name (a Spec's ServerName) and its volume.
func (p *sqlProvisioner) Remove(ctx context.Context, name string) error {
	sqladmin.StopSignInWatch(name)
	p.stopPooler(ctx, name)
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
	sqladmin.StopSignInWatches()
	p.mu.Lock()
	fs := p.fwds
	p.fwds, p.up = map[string]*netfwd.Forwarder{}, map[string]bool{}
	for k, f := range p.pools {
		fs["pooler:"+k] = f
	}
	p.pools = map[string]*netfwd.Forwarder{}
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
	case ext != nil && (!spec.ExternalSyncStarted || ext.ManualSetup):
		// demoteMaster with skipReplicationSetup (#1614): set up by hand
		// with the mysql.* stored procedures.
		return ""
	case ext != nil && !spec.MySQL():
		demoted := ""
		if ext.Demoted {
			// demote of an existing instance (#1614).
			demoted = " CLOUDSQL_DEMOTED=1"
		}
		return "export CLOUDSQL_SYNC_MODE=" + shellQuote(ext.SyncMode) + " CLOUDSQL_DATABASES=" + shellQuote(strings.Join(ext.Databases, " ")) + demoted +
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
  # A demoted instance's database named as a migrated one is dropped first,
  # as Database Migration Service does for an existing destination (#1614).
  if [ -n "${CLOUDSQL_DEMOTED:-}" ] && [ -n "$(dst -d template1 -c "SELECT 1 FROM pg_database WHERE datname = '$db'")" ]; then
    dst -d template1 -c "DROP DATABASE \"$db\" WITH (FORCE)" 2>/dev/null || dst -d template1 -c "DROP DATABASE \"$db\""
  fi
  if [ -z "$(dst -d template1 -c "SELECT 1 FROM pg_database WHERE datname = '$db'")" ]; then
    dst -d template1 -c "CREATE DATABASE \"$db\""
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
