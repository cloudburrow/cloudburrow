package main

// The servers of a Cloud SQL high-availability instance (#1408). A
// REGIONAL instance is two Deployments: sqlinst-NAME, the server it was
// created with, and sqlha-NAME, its second. One is the primary, which the
// instance's Service, sqlinst-NAME, selects, so the host tunnel, the
// connector and the instance's read replicas reach it; the other is a
// standby streaming from that Service as cloudsqlstandby, which the
// primary's synchronous_standby_names names with synchronous_commit on: a
// commit returns once the standby has flushed it, Cloud SQL's guarantee
// that a failover loses no committed transaction. Read replicas stream as
// walreceiver, so they never stand in for the standby.
//
// A failover selects the standby in the Service, stops the primary's
// server, promotes the standby, and rebuilds the old primary from it as the
// new standby. The promoted server's Deployment is not changed, so it does
// not restart: its epoch is the one its volume records, and it keeps its
// data across later restarts.

import (
	"context"
	"fmt"
	"hash/crc32"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// nodeName is the Deployment of server node (0 or 1) of instance name.
func nodeName(name string, node int) string {
	if node == 1 {
		return components.SQLStandbyPrefix + name
	}
	return components.SQLInstancePrefix + name
}

// nodeBackend is server node of spec: its own name and, for node 0, the
// instance's Service, which selects the primary's server; a server that
// has been a standby keeps the standby's start, at its epoch.
func (p *sqlProvisioner) nodeBackend(spec sqladmin.Spec, node int) components.Backend {
	b := p.backend(spec)
	b.Name = nodeName(spec.Name, node)
	if node == 0 {
		b.Selects = nodeName(spec.Name, spec.PrimaryNode())
	}
	if e := spec.HAEpochs[node]; e > 0 {
		if spec.MySQL() {
			b = components.CloudSQLMySQLStandbyBackend(b, components.SQLInstancePrefix+spec.Name, e,
				2+crc32.ChecksumIEEE([]byte(b.Name))%1000000)
			b.Env["CLOUDSQL_STANDBY_PASSWORD"] = spec.HAPassword
		} else {
			b = components.CloudSQLStandbyBackend(b, components.SQLInstancePrefix+spec.Name, e)
		}
	}
	return b
}

// applyService applies the instance's Service alone, selecting the
// primary's server.
func (p *sqlProvisioner) applyService(ctx context.Context, spec sqladmin.Spec) error {
	m := p.nodeBackend(spec, 0).Manifest(p.inst.Namespace, p.inst.Instance)
	const sep = "---\napiVersion: v1\nkind: Service\n"
	i := strings.Index(m, sep)
	if i < 0 {
		return fmt.Errorf("instance %s: no Service in its manifest", spec.Name)
	}
	return p.inst.Apply(ctx, m[i+4:])
}

// psql runs a script in deployment's server as postgres.
func (p *sqlProvisioner) psql(ctx context.Context, deployment, script string) (string, error) {
	var out strings.Builder
	err := nodeExec(p.cfg, deployment)(ctx, strings.NewReader(script), &out,
		"psql", "-v", "ON_ERROR_STOP=1", "-X", "-qtA", "-U", "postgres", "-d", "postgres")
	if err != nil {
		return "", fmt.Errorf("%s: %w %s", deployment, err, strings.TrimSpace(out.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// pgStandbySource lets the standby connect to the primary for
// replication: the replication role and its pg_hba.conf line, as a read
// replica's primary has. The role's commit is local: no standby may be
// streaming yet to acknowledge it.
const pgStandbySource = `SET synchronous_commit TO local;
DO $$BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '` + components.SQLReplicaUser + `') THEN
    CREATE ROLE ` + components.SQLReplicaUser + ` WITH REPLICATION LOGIN;
  END IF;
END$$;
`

// pgKeepWAL keeps the last WAL on the primary, so a standby or a read
// replica that reconnects after a failover, or while the standby is
// rebuilt, finds what it has not received (no replication slot is used,
// so a replica that is gone holds none back).
const pgKeepWAL = "ALTER SYSTEM SET wal_keep_size = '256MB';\nSELECT pg_reload_conf();\n"

// standby makes spec's standby, for a REGIONAL instance, or removes it,
// for a ZONAL one, and sets the primary's synchronous replication to
// match.
func (p *sqlProvisioner) standby(ctx context.Context, spec sqladmin.Spec) error {
	if spec.MySQL() {
		return p.mysqlStandby(ctx, spec)
	}
	primary := nodeName(spec.Name, spec.PrimaryNode())
	sb := nodeName(spec.Name, 1-spec.PrimaryNode())
	if !spec.Regional() {
		if spec.HAEpochs == [2]int{} {
			return nil // never had a standby
		}
		if _, err := p.psql(ctx, primary, "ALTER SYSTEM RESET synchronous_standby_names;\nSELECT pg_reload_conf();\n"); err != nil {
			return err
		}
		obj := []string{"deployment/" + sb, "persistentvolumeclaim/" + sb + "-data"}
		if spec.PrimaryNode() == 0 {
			obj = append(obj, "service/"+sb)
		}
		_, err := p.inst.Kubectl(ctx, append(append([]string{"-n", p.inst.Namespace, "delete"}, obj...), "--ignore-not-found", "--wait=true")...)
		return err
	}
	if _, err := p.psql(ctx, primary, pgKeepWAL); err != nil {
		return err
	}
	if _, err := p.psql(ctx, primary, pgStandbySource); err != nil {
		return fmt.Errorf("prepare %s as the standby's source: %w", primary, err)
	}
	{
		const line = "host replication " + components.SQLReplicaUser + " all trust"
		const add = `hba=$(psql -U postgres -XtAc 'SHOW hba_file') && { grep -qxF '` + line + `' "$hba" || ` +
			`{ echo '` + line + `' >> "$hba" && psql -U postgres -XtAc 'SELECT pg_reload_conf()'; }; }`
		var o strings.Builder
		if err := nodeExec(p.cfg, primary)(ctx, nil, &o, "sh", "-c", add); err != nil {
			return fmt.Errorf("let the standby connect to %s: %w %s", primary, err, o.String())
		}
	}
	b := p.nodeBackend(spec, 1-spec.PrimaryNode())
	if err := p.inst.Apply(ctx, b.Manifest(p.inst.Namespace, p.inst.Instance)); err != nil {
		return err
	}
	if err := p.rolloutDeployment(ctx, sb); err != nil {
		return err
	}
	if _, err := p.psql(ctx, primary, "ALTER SYSTEM SET synchronous_standby_names = '"+components.SQLStandbyApplicationName+
		"';\nALTER SYSTEM SET synchronous_commit = 'on';\nSELECT pg_reload_conf();\n"); err != nil {
		return err
	}
	// Done once the standby streams synchronously.
	for {
		st, err := p.psql(ctx, primary, "SELECT sync_state FROM pg_stat_replication WHERE application_name = '"+
			components.SQLStandbyApplicationName+"' AND state = 'streaming';\n")
		if err == nil && strings.Contains(st, "sync") && !strings.Contains(st, "async") {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the standby of %s does not stream synchronously (%q): %w", spec.Name, st, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// Failover makes the standby the primary and rebuilds the old primary as
// the new standby (#1408).
func (p *sqlProvisioner) Failover(ctx context.Context, from, to sqladmin.Spec) error {
	oldPrimary := nodeName(from.Name, from.PrimaryNode())
	newPrimary := nodeName(to.Name, to.PrimaryNode())
	// The Service first: a client reconnecting once the old primary has
	// stopped reaches the new one.
	if err := p.applyService(ctx, to); err != nil {
		return err
	}
	pods, _ := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "get", "pods", "-l", "app="+oldPrimary, "-o", "name")
	if _, err := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "scale", "deployment/"+oldPrimary, "--replicas=0"); err != nil {
		return err
	}
	if !from.MySQL() {
		// A fast shutdown, whatever clients are connected: PostgreSQL then
		// sends every standby and replica the WAL up to its shutdown
		// checkpoint before it exits, so the replicas are never ahead of
		// the standby and follow its new timeline. The pod is being
		// deleted, so its server is not started again.
		for _, pod := range strings.Fields(pods) {
			_, _ = p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "exec", pod, "-c", oldPrimary, "--", "sh", "-c", "kill -INT 1")
		}
	}
	if _, err := p.inst.Kubectl(ctx, "-n", p.inst.Namespace, "wait", "--for=delete", "pod", "-l", "app="+oldPrimary,
		"--timeout=120s"); err != nil {
		return fmt.Errorf("stop the primary %s: %w", oldPrimary, err)
	}
	// Every transaction the old primary committed, the standby had
	// flushed; promotion replays what it has and ends recovery.
	if to.MySQL() {
		if out, err := p.mysqlScript(ctx, newPrimary, mysqlPromoteStandby); err != nil {
			return fmt.Errorf("promote the standby %s: %v %s", newPrimary, err, out)
		}
	} else {
		if out, err := p.psql(ctx, newPrimary, "SELECT pg_promote(true, 60);\n"); err != nil || out != "t" {
			return fmt.Errorf("promote the standby %s: %v %s", newPrimary, err, out)
		}
		// Before the old primary is rebuilt, whose base backup checkpoints:
		// the read replicas still need the WAL from the promotion on.
		if _, err := p.psql(ctx, newPrimary, pgKeepWAL); err != nil {
			return err
		}
	}
	// Start applies the old primary's server again, at its new epoch: it
	// rebuilds from the new primary and streams as its standby.
	return p.Start(ctx, to)
}

// mysqlScript runs a shell script, after mysqlRootEnv, in deployment's
// server.
func (p *sqlProvisioner) mysqlScript(ctx context.Context, deployment, script string) (string, error) {
	var out strings.Builder
	err := nodeExec(p.cfg, deployment)(ctx, nil, &out, "sh", "-c", "set -e\n"+mysqlRootEnv+script)
	return strings.TrimSpace(out.String()), err
}

// mysqlStandbySource makes the primary a source its standby can replicate
// from: GTIDs on, by MySQL's online procedure, the semi-synchronous source
// plugin, and the standby's user, created only when missing, so a primary
// with semi-synchronous replication on and no standby yet commits nothing
// it would wait on.
const mysqlStandbySource = `q() { mysql $MYSQL_LOCAL -N -e "$1"; }
mode=$(q 'SELECT @@GLOBAL.gtid_mode')
if [ "$mode" != ON ]; then
  q 'SET PERSIST enforce_gtid_consistency = ON'
  for m in OFF_PERMISSIVE ON_PERMISSIVE ON; do
    case "$mode:$m" in OFF_PERMISSIVE:OFF_PERMISSIVE|ON_PERMISSIVE:OFF_PERMISSIVE|ON_PERMISSIVE:ON_PERMISSIVE) continue ;; esac
    if [ "$m" = ON ]; then
      i=0; while [ "$(q "SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'ONGOING_ANONYMOUS_TRANSACTION_COUNT'")" != 0 ]; do
        i=$((i+1)); [ $i -lt 120 ] || { echo "transactions without GTIDs are still running" >&2; exit 1; }; sleep 1; done
    fi
    q "SET PERSIST gtid_mode = $m"
  done
fi
[ "$(q "SELECT COUNT(*) FROM information_schema.plugins WHERE plugin_name = 'rpl_semi_sync_source'")" = 1 ] ||
  q "INSTALL PLUGIN rpl_semi_sync_source SONAME 'semisync_source.so'"
[ "$(q "SELECT COUNT(*) FROM information_schema.plugins WHERE plugin_name = 'rpl_semi_sync_replica'")" = 1 ] ||
  q "INSTALL PLUGIN rpl_semi_sync_replica SONAME 'semisync_replica.so'"
if [ "$(q "SELECT COUNT(*) FROM mysql.user WHERE user = '` + components.SQLStandbyUser + `'")" = 0 ]; then
  q "CREATE USER '` + components.SQLStandbyUser + `'@'%' IDENTIFIED BY '$1'; GRANT REPLICATION SLAVE, REPLICATION CLIENT, SELECT, RELOAD, SHOW VIEW, TRIGGER, EVENT, PROCESS ON *.* TO '` + components.SQLStandbyUser + `'@'%'"
fi
`

// mysqlStandbySetup fills a new standby from its primary, once: the
// primary's databases with its GTIDs as purged, as a read replica's setup
// does, and its accounts, copied with the binary log off so they add no
// GTID of the standby's own; then it replicates semi-synchronously.
const mysqlStandbySetup = `if [ -n "$(mysql $MYSQL_LOCAL -N -e 'SHOW REPLICA STATUS')" ]; then exit 0; fi
src() { MYSQL_PWD="$CLOUDSQL_STANDBY_PASSWORD" "$@" -h "$CLOUDSQL_PRIMARY_HOST" -u ` + components.SQLStandbyUser + `; }
dbs=$(src mysql -N -e "SELECT schema_name FROM information_schema.schemata WHERE schema_name NOT IN ('mysql','sys','information_schema','performance_schema')")
mysql $MYSQL_LOCAL -e 'RESET BINARY LOGS AND GTIDS' 2>/dev/null || mysql $MYSQL_LOCAL -e 'RESET MASTER' # MySQL 8.0's name
if [ -n "$dbs" ]; then
  src mysqldump --single-transaction --set-gtid-purged=ON --routines --triggers --events --databases $dbs | mysql $MYSQL_LOCAL
else
  mysql $MYSQL_LOCAL -e "SET GLOBAL gtid_purged = '$(src mysql -N -e 'SELECT @@GLOBAL.gtid_executed')'"
fi
users=$(src mysql -N -e "SELECT CONCAT(QUOTE(user), '@', QUOTE(host)) FROM mysql.user WHERE user NOT IN ('root', 'mysql.sys', 'mysql.session', 'mysql.infoschema')")
{
  echo 'SET SESSION sql_log_bin = 0;'
  for u in $users; do
    src mysql -N --init-command='SET SESSION print_identified_with_as_hex = ON' -e "SHOW CREATE USER $u" | sed 's/^CREATE USER /CREATE USER IF NOT EXISTS /; s/$/;/'
    src mysql -N -e "SHOW GRANTS FOR $u" | sed 's/$/;/'
  done
} | mysql $MYSQL_LOCAL
q() { mysql $MYSQL_LOCAL -N -e "$1"; }
[ "$(q "SELECT COUNT(*) FROM information_schema.plugins WHERE plugin_name = 'rpl_semi_sync_replica'")" = 1 ] ||
  q "INSTALL PLUGIN rpl_semi_sync_replica SONAME 'semisync_replica.so'"
[ "$(q "SELECT COUNT(*) FROM information_schema.plugins WHERE plugin_name = 'rpl_semi_sync_source'")" = 1 ] ||
  q "INSTALL PLUGIN rpl_semi_sync_source SONAME 'semisync_source.so'"
q "SET PERSIST rpl_semi_sync_source_enabled = OFF; SET PERSIST rpl_semi_sync_replica_enabled = ON"
q "CHANGE REPLICATION SOURCE TO SOURCE_HOST = '$CLOUDSQL_PRIMARY_HOST', SOURCE_PORT = 3306, SOURCE_USER = '` +
	components.SQLStandbyUser + `', SOURCE_PASSWORD = '$CLOUDSQL_STANDBY_PASSWORD', SOURCE_AUTO_POSITION = 1, SOURCE_SSL = 1, ` +
	`SOURCE_CONNECT_RETRY = 1; START REPLICA; SET PERSIST super_read_only = ON"
`

// mysqlSemiSync turns on lossless semi-synchronous replication on the
// primary: a commit is acknowledged by the standby after the primary's
// binary log is synced and before the commit is visible (AFTER_SYNC), and
// with no standby connected a commit waits (wait_no_replica) for the
// longest timeout MySQL takes, 4294967295 ms, about 49.7 days, before it
// would fall back to asynchronous replication.
const mysqlSemiSync = `mysql $MYSQL_LOCAL -e "SET PERSIST rpl_semi_sync_source_wait_point = 'AFTER_SYNC'; ` +
	`SET PERSIST rpl_semi_sync_source_wait_for_replica_count = 1; SET PERSIST rpl_semi_sync_source_wait_no_replica = ON; ` +
	`SET PERSIST rpl_semi_sync_source_timeout = 4294967295; SET PERSIST rpl_semi_sync_replica_enabled = OFF; ` +
	`SET PERSIST rpl_semi_sync_source_enabled = ON"
`

// mysqlPromoteStandby makes the standby a primary once it has applied
// every transaction it received, which includes every one its primary
// committed (AFTER_SYNC).
const mysqlPromoteStandby = `q() { mysql $MYSQL_LOCAL -N -e "$1"; }
q 'STOP REPLICA IO_THREAD'
set=$(q "SELECT RECEIVED_TRANSACTION_SET FROM performance_schema.replication_connection_status" | tr -d '\n')
[ "$(q "SELECT WAIT_FOR_EXECUTED_GTID_SET('$set', 120)")" = 0 ] || { echo "the standby did not apply $set" >&2; exit 1; }
q "STOP REPLICA; RESET REPLICA ALL; SET PERSIST rpl_semi_sync_replica_enabled = OFF; SET PERSIST super_read_only = OFF; SET PERSIST read_only = OFF"
`

// mysqlStandby is standby for a MySQL instance.
func (p *sqlProvisioner) mysqlStandby(ctx context.Context, spec sqladmin.Spec) error {
	primary := nodeName(spec.Name, spec.PrimaryNode())
	sb := nodeName(spec.Name, 1-spec.PrimaryNode())
	if !spec.Regional() {
		if spec.HAEpochs == [2]int{} {
			return nil
		}
		if out, err := p.mysqlScript(ctx, primary, `[ "$(mysql $MYSQL_LOCAL -N -e "SELECT COUNT(*) FROM information_schema.plugins `+
			`WHERE plugin_name = 'rpl_semi_sync_source'")" = 0 ] || mysql $MYSQL_LOCAL -e 'SET PERSIST rpl_semi_sync_source_enabled = OFF'`); err != nil {
			return fmt.Errorf("turn semi-synchronous replication off on %s: %w %s", primary, err, out)
		}
		obj := []string{"deployment/" + sb, "persistentvolumeclaim/" + sb + "-data"}
		if spec.PrimaryNode() == 0 {
			obj = append(obj, "service/"+sb)
		}
		_, err := p.inst.Kubectl(ctx, append(append([]string{"-n", p.inst.Namespace, "delete"}, obj...), "--ignore-not-found", "--wait=true")...)
		return err
	}
	if out, err := p.mysqlScript(ctx, primary, "set -- '"+spec.HAPassword+"'\n"+mysqlStandbySource); err != nil {
		return fmt.Errorf("prepare %s as the standby's source: %w %s", primary, err, out)
	}
	b := p.nodeBackend(spec, 1-spec.PrimaryNode())
	if err := p.inst.Apply(ctx, b.Manifest(p.inst.Namespace, p.inst.Instance)); err != nil {
		return err
	}
	if err := p.rolloutDeployment(ctx, sb); err != nil {
		return err
	}
	if out, err := p.mysqlScript(ctx, sb, mysqlStandbySetup); err != nil {
		return fmt.Errorf("start the standby %s: %w %s", sb, err, out)
	}
	if out, err := p.mysqlScript(ctx, primary, mysqlSemiSync); err != nil {
		return fmt.Errorf("turn semi-synchronous replication on on %s: %w %s", primary, err, out)
	}
	// The standby negotiates semi-synchronous replication when it
	// connects: it reconnects, and the primary is done once it counts it.
	if out, err := p.mysqlScript(ctx, sb, `mysql $MYSQL_LOCAL -e 'STOP REPLICA IO_THREAD; START REPLICA IO_THREAD'`); err != nil {
		return fmt.Errorf("reconnect the standby %s: %w %s", sb, err, out)
	}
	for {
		out, err := p.mysqlScript(ctx, primary, `mysql $MYSQL_LOCAL -N -e "SELECT VARIABLE_VALUE FROM performance_schema.global_status `+
			`WHERE VARIABLE_NAME IN ('Rpl_semi_sync_source_status', 'Rpl_semi_sync_source_clients') ORDER BY VARIABLE_NAME"`)
		if f := strings.Fields(out); err == nil && len(f) == 2 && f[0] != "0" && f[1] == "ON" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the standby of %s is not semi-synchronous (%q): %w", spec.Name, out, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}
