package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// Upgrade is a created instance's major version upgrade (#1381): a dump
// of the old server, taken in its pod, kept on the host while the server
// is replaced by one of the new version's image on a fresh data
// directory, and loaded into it.
//
// PostgreSQL's dump is pg_dumpall's: every role with its password hash,
// and every database with its owners and privileges, which is what
// pg_upgrade carries over too. MySQL's is the accounts (SHOW CREATE USER,
// with the authentication string, and SHOW GRANTS) and mysqldump of every
// database but the system schemas, which the new server initialises as
// its own version's: the logical upgrade MySQL documents, which keeps no
// table of the old mysql schema.
//
// If the new server cannot be started or loaded, the old version is
// started again from the same dump, so the instance keeps its data.
func (p *sqlProvisioner) Upgrade(ctx context.Context, from, to sqladmin.Spec) error {
	dir, err := os.MkdirTemp(p.cfg.InstanceDir(), "cloudsql-upgrade-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	dump := dir + "/dump.sql"
	if err := p.dumpAll(ctx, from, dump); err != nil {
		return fmt.Errorf("dump %s before its upgrade: %w", from.Name, err)
	}
	if err := p.Remove(ctx, from.Name); err != nil {
		return err
	}
	if err := p.startAndLoad(ctx, to, dump); err != nil {
		p.logf("upgrade of Cloud SQL instance %s to %s failed (%v); starting %s again", from.Name, to.DatabaseVersion, err, from.DatabaseVersion)
		_ = p.Remove(ctx, to.Name)
		if rerr := p.startAndLoad(ctx, from, dump); rerr != nil {
			return fmt.Errorf("upgrade to %s: %w; and restoring %s failed: %v", to.DatabaseVersion, err, from.DatabaseVersion, rerr)
		}
		return fmt.Errorf("upgrade to %s: %w", to.DatabaseVersion, err)
	}
	return nil
}

// mysqlAccounts prints the statements that recreate every account but
// root, the image's own and the replication user, with its grants.
const mysqlAccounts = mysqlRootEnv + `set -e
for u in $(mysql $MYSQL_LOCAL -N -B -e "SELECT CONCAT(QUOTE(user), '@', QUOTE(host)) FROM mysql.user WHERE user NOT LIKE 'mysql.%' AND user NOT IN ('root', '` +
	components.SQLReplicaUser + `')"); do
  mysql $MYSQL_LOCAL -N -B -r -e "SET SESSION print_identified_with_as_hex = ON; SHOW CREATE USER $u" | sed 's/$/;/'
  mysql $MYSQL_LOCAL -N -B -r -e "SHOW GRANTS FOR $u" | sed 's/$/;/'
done
dbs=$(mysql $MYSQL_LOCAL -N -B -e "SELECT schema_name FROM information_schema.schemata WHERE schema_name NOT IN ('mysql','sys','information_schema','performance_schema')")
if [ -n "$dbs" ]; then
  mysqldump $MYSQL_LOCAL --single-transaction --set-gtid-purged=OFF --routines --triggers --events --databases $dbs
fi
`

func (p *sqlProvisioner) dumpAll(ctx context.Context, spec sqladmin.Spec, path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	exec := podExec(p.cfg, components.SQLInstancePrefix+spec.Name)
	if spec.MySQL() {
		err = exec(ctx, nil, f, "sh", "-c", mysqlAccounts)
	} else {
		err = exec(ctx, nil, f, "pg_dumpall", "-U", p.superuser(spec))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (p *sqlProvisioner) superuser(spec sqladmin.Spec) string {
	if spec.MasterUp {
		return components.CloudSQLUser
	}
	return "postgres"
}

// startAndLoad starts spec's server and loads the dump at path into it.
func (p *sqlProvisioner) startAndLoad(ctx context.Context, spec sqladmin.Spec, path string) error {
	if err := p.Start(ctx, spec); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	exec := podExec(p.cfg, components.SQLInstancePrefix+spec.Name)
	if spec.MySQL() {
		return exec(ctx, f, io.Discard, "sh", "-c", mysqlRootEnv+`exec mysql $MYSQL_LOCAL`)
	}
	// The new server's own postgres role and database exist already, so
	// pg_dumpall's CREATE ROLE postgres fails, which psql reports and goes
	// on from, as pg_dumpall's documentation says to expect; ALTER ROLE
	// then gives the role its old attributes.
	return exec(ctx, f, io.Discard, "psql", "-X", "-q", "-U", "postgres", "-d", "postgres", "-v", "ON_ERROR_STOP=0")
}
