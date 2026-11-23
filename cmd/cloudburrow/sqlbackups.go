package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// sqlDumper takes the Admin API's backups (#1330) with the tools
// `cloudburrow snapshot` uses for Cloud SQL: pg_dump and pg_restore, or
// mysqldump and mysql, each run inside the server's pod.
type sqlDumper struct {
	cfg config.Config
	// exec makes the command runner of a deployment; a test replaces it.
	exec func(deployment string) func(ctx context.Context, stdin io.Reader, stdout io.Writer, argv ...string) error
}

func newSQLDumper(cfg config.Config) *sqlDumper {
	return &sqlDumper{cfg: cfg, exec: func(d string) func(context.Context, io.Reader, io.Writer, ...string) error {
		return podExec(cfg, d)
	}}
}

// sqlBackupsDir is where the backups' dumps are kept.
func sqlBackupsDir(cfg config.Config) string {
	return filepath.Join(cfg.InstanceDir(), "cloudsql-backups")
}

func (d *sqlDumper) deployment(t sqladmin.DumpTarget) string {
	switch {
	case t.Created:
		return components.SQLInstancePrefix + sqlServerOf(t.Instance)
	case t.MySQL:
		return string(config.ServiceCloudSQLMySQL)
	}
	return string(config.ServiceCloudSQL)
}

type archiver interface {
	Export(ctx context.Context, w admin.EntryWriter) error
	Import(ctx context.Context, r admin.EntryReader) error
}

func (d *sqlDumper) archiver(t sqladmin.DumpTarget) archiver {
	exec := d.exec(d.deployment(t))
	if t.MySQL {
		return &mysqlArchiver{exec: exec, bare: t.Created}
	}
	p := &postgresSnapshotter{exec: exec}
	if t.Created {
		p.superuser, p.bare = "postgres", true
	}
	return p
}

func (d *sqlDumper) Dump(ctx context.Context, t sqladmin.DumpTarget, dir string) error {
	return d.archiver(t).Export(ctx, dirEntries(dir))
}

func (d *sqlDumper) Load(ctx context.Context, t sqladmin.DumpTarget, dir string) error {
	return d.archiver(t).Import(ctx, dirEntries(dir))
}

// dirEntries is an archive kept as files in a directory.
type dirEntries string

func (d dirEntries) path(name string) (string, error) {
	if strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("bad entry name %q", name)
	}
	return filepath.Join(string(d), filepath.FromSlash(name)), nil
}

func (d dirEntries) Add(name string, size int64, r io.Reader) error {
	p, err := d.path(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != size {
		err = fmt.Errorf("%s: wrote %d bytes, declared %d", name, n, size)
	}
	return err
}

func (d dirEntries) Open(name string) (io.ReadCloser, error) {
	p, err := d.path(name)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

func (d dirEntries) List(prefix string) []string {
	var out []string
	_ = filepath.WalkDir(string(d), func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(string(d), p)
		if rel = filepath.ToSlash(rel); strings.HasPrefix(rel, prefix) {
			out = append(out, rel)
		}
		return nil
	})
	return out
}

// mysqlArchiver dumps each application database of a MySQL server with
// mysqldump, as the server's administrative account (mysqlRootEnv), whose
// password the server's container holds in MYSQL_ROOT_PASSWORD: it never
// passes through the host's command line.
type mysqlArchiver struct {
	exec func(ctx context.Context, stdin io.Reader, stdout io.Writer, argv ...string) error
	// bare is a created instance's server, which has no default database
	// for Import to recreate.
	bare bool
}

// mysqlSystem are MySQL's own databases, which a backup leaves out: the
// users and grants in mysql stay the target's.
var mysqlSystem = map[string]bool{"mysql": true, "information_schema": true, "performance_schema": true, "sys": true}

func (m *mysqlArchiver) sh(ctx context.Context, stdin io.Reader, stdout io.Writer, tool string, args ...string) error {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	script := mysqlRootEnv + `exec ` + tool + " $MYSQL_LOCAL " + strings.Join(quoted, " ")
	return m.exec(ctx, stdin, stdout, "sh", "-c", script)
}

// mysqlRootEnv starts every script run in a MySQL server's container: the
// clients sign in as sqladmin.MySQLAdminUser (#1993), or as root on a
// server that has none (`up`'s, or one made before #1993 until its next
// start), over TCP to 127.0.0.1:3306 ($MYSQL_LOCAL), not over
// the unix socket, and wait until the server answers there. On CI's runners
// the server's classic socket, /var/run/mysqld/mysqld.sock, was missing
// while the server served TCP (only mysqlx.sock was there), so a backup or a
// replica's setup failed with ERROR 2002; TCP is what the pod's readiness
// probe checks. The flags are explicit because not every client reads
// MYSQL_HOST (mysqladmin pinged over the socket with it set). The
// administrative account is at 127.0.0.1, and the image's root is
// root@'%', which a TCP sign-in from 127.0.0.1 is too, unless the user
// deleted it. mysqladmin ping succeeds once the server runs, even when it
// refuses the login.
const mysqlRootEnv = `export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:-}"; MYSQL_LOCAL="-u` + sqladmin.MySQLAdminUser + ` -h127.0.0.1 -P3306 --protocol=TCP"; ` +
	`i=0; until mysqladmin $MYSQL_LOCAL --connect-timeout=2 ping >/dev/null 2>&1; do ` +
	`i=$((i+1)); if [ "$i" -ge 120 ]; then ` +
	`echo "mysqld did not answer on 127.0.0.1:3306 in 120s" >&2; exit 1; fi; sleep 1; done
mysql $MYSQL_LOCAL -N -e 'SELECT 1' >/dev/null 2>&1 || MYSQL_LOCAL="-uroot -h127.0.0.1 -P3306 --protocol=TCP"
`

func (m *mysqlArchiver) query(ctx context.Context, stmt string) (string, error) {
	var out bytes.Buffer
	err := m.sh(ctx, nil, &out, "mysql", "-N", "-B", "-e", stmt)
	return out.String(), err
}

func (m *mysqlArchiver) databases(ctx context.Context) ([]string, error) {
	out, err := m.query(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" && !mysqlSystem[line] {
			names = append(names, line)
		}
	}
	return names, nil
}

func (m *mysqlArchiver) Export(ctx context.Context, w admin.EntryWriter) error {
	names, err := m.databases(ctx)
	if err != nil {
		return err
	}
	b, err := json.Marshal(names)
	if err != nil {
		return err
	}
	if err := w.Add(pgDatabasesEntry, int64(len(b)), bytes.NewReader(b)); err != nil {
		return err
	}
	if m.bare {
		if err := m.exportAccounts(ctx, w); err != nil {
			return err
		}
	}
	for i, name := range names {
		f, err := os.CreateTemp("", "cloudburrow-mysqldump-*")
		if err != nil {
			return err
		}
		err = m.sh(ctx, nil, f, "mysqldump", "--single-transaction", "--routines", "--triggers", "--events",
			"--set-gtid-purged=OFF", "--no-tablespaces", name)
		var size int64
		if err == nil {
			size, err = f.Seek(0, io.SeekCurrent)
		}
		if err == nil {
			_, err = f.Seek(0, io.SeekStart)
		}
		if err == nil {
			err = w.Add(fmt.Sprintf("databases/%03d.sql", i), size, f)
		}
		_ = f.Close()
		_ = os.Remove(f.Name())
		if err != nil {
			return fmt.Errorf("dump %s: %w", name, err)
		}
	}
	return nil
}

// mysqlAccountsEntry holds a created instance's accounts (#1993): Cloud
// SQL's restore "takes the source instance's settings, databases, and
// users" (docs.cloud.google.com/sql/docs/mysql/backup-recovery/restore),
// so a restore, a clone or a restore to a new instance gives the target
// the source's users, a deleted 'root'@'%' included, and its IAM users.
const mysqlAccountsEntry = "mysql/accounts.sql"

// mysqlUserAccounts is the mysql.user condition of the accounts a backup
// carries: neither MySQL's own nor the system accounts users.list hides.
const mysqlUserAccounts = `user NOT LIKE 'mysql.%' AND user NOT IN ('` + sqladmin.MySQLAdminUser + `', '` +
	components.SQLReplicaUser + `', '` + components.SQLStandbyUser + `') AND NOT (user = 'root' AND host IN ('localhost', '127.0.0.1', '::1'))`

// mysqlExportAccounts prints the statements that recreate the accounts,
// their grants and the IAM users' table, in forms a replica of the target
// replays whatever accounts it has. PROXY grants are left out: only the
// image's root@localhost may grant them. MySQL 5.6 has no SHOW CREATE
// USER: its accounts are mysql56Accounts' (#1524).
const mysqlExportAccounts = mysqlRootEnv + `set -e
acct() { mysql $MYSQL_LOCAL "$@"; }
` + mysqlAccountStatements

// mysqlAccountStatements is mysqlExportAccounts' body, which reads the
// accounts through acct, a shell function running the mysql client
// against the server whose accounts are printed: the server itself for a
// backup, a replica's primary for mysqlReplicaSetup. On MySQL 5.6 it ends
// with exit 0, so a caller that goes on runs it in a subshell.
const mysqlAccountStatements = `case "$(acct -N -B -e 'SELECT VERSION()')" in 5.6*)
` + mysql56Accounts + `  exit 0 ;;
esac
for u in $(acct -N -B -e "SELECT CONCAT(QUOTE(user), '@', QUOTE(host)) FROM mysql.user WHERE ` + mysqlUserAccounts + `"); do
  { acct -N -B -r -e "SET SESSION print_identified_with_as_hex = ON; SHOW CREATE USER $u" 2>/dev/null ||
    acct -N -B -r -e "SHOW CREATE USER $u"; } | sed 's/^CREATE USER /CREATE USER IF NOT EXISTS /; s/$/;/'
  acct -N -B -r -e "SHOW GRANTS FOR $u" | grep -v '^GRANT PROXY ON ' | sed 's/$/;/'
done
if [ "$(acct -N -B -e "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'mysql' AND table_name = 'cloudburrow_iam_users'")" = 1 ]; then
  echo "CREATE TABLE IF NOT EXISTS mysql.cloudburrow_iam_users (user VARCHAR(32) NOT NULL PRIMARY KEY, type VARCHAR(40) NOT NULL, email VARCHAR(255) NOT NULL) ENGINE=InnoDB;"
  acct -N -B -r -e "SELECT CONCAT('REPLACE INTO mysql.cloudburrow_iam_users (user, type, email) VALUES (', QUOTE(user), ', ', QUOTE(type), ', ', QUOTE(email), ');') FROM mysql.cloudburrow_iam_users"
fi
`

// mysql56Accounts prints, on MySQL 5.6, which has no SHOW CREATE USER
// (#1524), a CREATE USER for each account mysqlUserAccounts selects, with
// its password hash (IDENTIFIED BY PASSWORD, which MySQL 5.6 and 5.7 both
// read), and its grants without that clause, which 5.6's SHOW GRANTS
// repeats.
const mysql56Accounts = `  acct -N -B -r -e "SELECT CONCAT('CREATE USER ', QUOTE(user), '@', QUOTE(host), IF(password = '', '', ` +
	`CONCAT(' IDENTIFIED BY PASSWORD ', QUOTE(password))), ';') FROM mysql.user WHERE ` + mysqlUserAccounts + `"
  for u in $(acct -N -B -e "SELECT CONCAT(QUOTE(user), '@', QUOTE(host)) FROM mysql.user WHERE ` + mysqlUserAccounts + `"); do
    acct -N -B -r -e "SHOW GRANTS FOR $u" | grep -v '^GRANT PROXY ON ' | sed -E "s/ IDENTIFIED BY PASSWORD '[^']*'//" | sed 's/$/;/'
  done
`

// mysqlImportAccounts drops the target's accounts, and its IAM users'
// rows, and runs the archive's statements, read on standard input. MySQL
// 5.6's DROP USER has no IF EXISTS; the accounts it drops exist.
const mysqlImportAccounts = mysqlRootEnv + `set -e
ifx='IF EXISTS '; case "$(mysql $MYSQL_LOCAL -N -B -e 'SELECT VERSION()')" in 5.6*) ifx= ;; esac
drops=$(mysql $MYSQL_LOCAL -N -B -e "SELECT CONCAT('DROP USER $ifx', QUOTE(user), '@', QUOTE(host), ';') FROM mysql.user WHERE ` + mysqlUserAccounts + `")
iam=$(mysql $MYSQL_LOCAL -N -B -e "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'mysql' AND table_name = 'cloudburrow_iam_users'")
{
  printf '%s\n' "$drops"
  if [ "$iam" = 1 ]; then echo 'DELETE FROM mysql.cloudburrow_iam_users;'; fi
  cat
} | mysql $MYSQL_LOCAL
`

func (m *mysqlArchiver) exportAccounts(ctx context.Context, w admin.EntryWriter) error {
	var out bytes.Buffer
	if err := m.exec(ctx, nil, &out, "sh", "-c", mysqlExportAccounts); err != nil {
		return fmt.Errorf("dump the accounts: %w", err)
	}
	return w.Add(mysqlAccountsEntry, int64(out.Len()), &out)
}

// Import drops every application database and loads the archive's; on a
// created instance, the archive's accounts too, when it has them (a backup
// taken before #1993 has none, and the target keeps its own).
func (m *mysqlArchiver) Import(ctx context.Context, r admin.EntryReader) error {
	f, err := r.Open(pgDatabasesEntry)
	if err != nil {
		return err
	}
	var names []string
	err = json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&names)
	_ = f.Close()
	if err != nil {
		return fmt.Errorf("%s: %w", pgDatabasesEntry, err)
	}
	for _, name := range names {
		if name == "" || mysqlSystem[name] {
			return fmt.Errorf("%s names %q, which is not an application database", pgDatabasesEntry, name)
		}
	}
	current, err := m.databases(ctx)
	if err != nil {
		return err
	}
	for _, name := range current {
		if _, err := m.query(ctx, "DROP DATABASE "+quoteMySQLIdent(name)); err != nil {
			return fmt.Errorf("drop %s: %w", name, err)
		}
	}
	restored := false
	for i, name := range names {
		if _, err := m.query(ctx, "CREATE DATABASE "+quoteMySQLIdent(name)); err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		d, err := r.Open(fmt.Sprintf("databases/%03d.sql", i))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("the archive lists %s but holds no dump for it", name)
			}
			return err
		}
		err = m.sh(ctx, d, io.Discard, "mysql", name)
		_ = d.Close()
		if err != nil {
			return fmt.Errorf("restore %s: %w", name, err)
		}
		restored = restored || name == components.CloudSQLDatabase
	}
	if m.bare {
		a, err := r.Open(mysqlAccountsEntry)
		if err == nil {
			err = m.exec(ctx, a, io.Discard, "sh", "-c", mysqlImportAccounts)
			_ = a.Close()
			if err != nil {
				return fmt.Errorf("restore the accounts: %w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if !restored && !m.bare {
		if _, err := m.query(ctx, "CREATE DATABASE "+quoteMySQLIdent(components.CloudSQLDatabase)); err != nil {
			return fmt.Errorf("recreate %s: %w", components.CloudSQLDatabase, err)
		}
	}
	return nil
}
