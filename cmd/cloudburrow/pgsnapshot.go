package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
)

// postgresSnapshotter captures Cloud SQL's PostgreSQL with its own tools
// (#311): pg_dump on export and pg_restore on import, each run inside the
// server's pod through the instance's kubeconfig.
//
// Inside the pod rather than over the host tunnel, so the host needs no
// PostgreSQL client of a matching version, and nothing is exposed beyond
// what `up` already exposes. The archive holds only what pg_dump writes: no
// kubeconfig, no cluster credential, and no role, since pg_dump of a
// database leaves roles out and the server has only the one it was
// initialised with.
type postgresSnapshotter struct {
	// exec runs a command in the server's container, with stdin when it is
	// not nil. A test replaces it.
	exec func(ctx context.Context, stdin io.Reader, stdout io.Writer, argv ...string) error
	// superuser is the role the tools run as, components.CloudSQLUser when
	// empty: a created instance's is postgres (#1330).
	superuser string
	// bare is a created instance's server, which has no default database
	// for Import to recreate.
	bare bool
}

func newPostgresSnapshotter(cfg config.Config) *postgresSnapshotter {
	return &postgresSnapshotter{exec: podExec(cfg, string(config.ServiceCloudSQL))}
}

// podExec runs a command in deployment's container through the instance's
// kubeconfig. A created Cloud SQL instance's command runs in its primary's
// server, the one its Service selects, which a high-availability
// instance's failover changes (#1408).
func podExec(cfg config.Config, deployment string) func(ctx context.Context, stdin io.Reader, stdout io.Writer, argv ...string) error {
	return execIn(cfg, deployment, strings.HasPrefix(deployment, components.SQLInstancePrefix))
}

// nodeExec runs a command in deployment's own container: one server of a
// high-availability instance, whichever its role.
func nodeExec(cfg config.Config, deployment string) func(ctx context.Context, stdin io.Reader, stdout io.Writer, argv ...string) error {
	return execIn(cfg, deployment, false)
}

func execIn(cfg config.Config, deployment string, primary bool) func(ctx context.Context, stdin io.Reader, stdout io.Writer, argv ...string) error {
	kube := kubeRunner(cfg.KubeconfigPath(), cfg.Cluster.Namespace)
	return func(ctx context.Context, stdin io.Reader, stdout io.Writer, argv ...string) error {
		deployment := deployment
		if primary {
			if app, err := kube.Get(ctx, "service", deployment, "jsonpath={.spec.selector.app}"); err == nil &&
				strings.TrimSpace(app) != "" {
				deployment = strings.TrimSpace(app)
			}
		}
		args := []string{"exec"}
		if stdin != nil {
			args = append(args, "-i")
		}
		args = append(append(args, "deploy/"+deployment, "--"), argv...)
		var stderr bytes.Buffer
		if err := kube.Pipe(ctx, stdin, stdout, &stderr, args...); err != nil {
			return fmt.Errorf("%s: %w: %s", argv[0], err, strings.TrimSpace(tail(stderr.String(), 2000)))
		}
		return nil
	}
}

// maintenance is the database psql connects to, which no application
// uses. A created instance's applications use postgres, which Cloud SQL
// gives them, so its backups include postgres and template1 is the one.
func (p *postgresSnapshotter) maintenance() string {
	if p.bare {
		return "template1"
	}
	return "postgres"
}

func (p *postgresSnapshotter) user() string {
	if p.superuser != "" {
		return p.superuser
	}
	return components.CloudSQLUser
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func (p *postgresSnapshotter) Name() string { return string(config.ServiceCloudSQL) }
func (p *postgresSnapshotter) Secret() bool { return false }

// pgDatabasesEntry lists the archived databases in order. Each one's dump is
// databases/<index>.dump, named by position rather than by the database, so
// no database name, whatever it contains, becomes a path.
const pgDatabasesEntry = "databases.json"

func pgDumpEntry(i int) string { return fmt.Sprintf("databases/%03d.dump", i) }

// psql runs one statement against the maintenance database, which no
// application uses, so every application database can be dropped from it.
func (p *postgresSnapshotter) psql(ctx context.Context, statement string) (string, error) {
	var out bytes.Buffer
	err := p.exec(ctx, nil, &out, "psql", "-U", p.user(), "-d", p.maintenance(),
		"-v", "ON_ERROR_STOP=1", "-AtX", "-c", statement)
	return out.String(), err
}

// databases are the application's: every database but the templates and
// the maintenance one.
func (p *postgresSnapshotter) databases(ctx context.Context) ([]string, error) {
	q := "SELECT datname FROM pg_database WHERE NOT datistemplate AND datname <> 'postgres' ORDER BY datname"
	if p.bare {
		q = "SELECT datname FROM pg_database WHERE NOT datistemplate ORDER BY datname"
	}
	out, err := p.psql(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

func quotePGIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func quotePGLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// Export dumps each database in pg_dump's custom format. The dump is spooled
// to a temporary file because an archive entry declares its size first.
func (p *postgresSnapshotter) Export(ctx context.Context, w admin.EntryWriter) error {
	names, err := p.databases(ctx)
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
	for i, name := range names {
		if err := p.exportOne(ctx, w, i, name); err != nil {
			return fmt.Errorf("dump %s: %w", name, err)
		}
	}
	if p.bare {
		return p.exportRoles(ctx, w)
	}
	return nil
}

// pgRolesEntry is a created instance's users: pg_dumpall --roles-only, each
// role with its password hash, CloudBurrow's own roles left out. A restore
// "takes the source instance's settings, databases, and users, and sets
// them in the target instance" (Google's restore page), so the backup's
// postgres password is the target's after it (#2208), as MySQL's accounts
// are (#1993).
const pgRolesEntry = "roles.sql"

// pgInternalRole matches a line that names one of CloudBurrow's own roles,
// quoted or not: the target keeps its own.
var pgInternalRole = regexp.MustCompile(`(^|[^A-Za-z0-9_$])"?(` + regexp.QuoteMeta(components.PGAdminUser) + `|` +
	regexp.QuoteMeta(components.SQLReplicaUser) + `)"?([^A-Za-z0-9_$]|$)`)

// pgUserRoles is dump without CloudBurrow's roles' lines.
func pgUserRoles(dump []byte) []byte {
	var out bytes.Buffer
	for _, line := range strings.SplitAfter(string(dump), "\n") {
		if !pgInternalRole.MatchString(line) {
			out.WriteString(line)
		}
	}
	return out.Bytes()
}

func (p *postgresSnapshotter) exportRoles(ctx context.Context, w admin.EntryWriter) error {
	var out bytes.Buffer
	if err := p.exec(ctx, nil, &out, "pg_dumpall", "-U", p.user(), "--roles-only"); err != nil {
		return fmt.Errorf("dump the roles: %w", err)
	}
	b := pgUserRoles(out.Bytes())
	return w.Add(pgRolesEntry, int64(len(b)), bytes.NewReader(b))
}

// importRoles runs the archive's roles, when it has them (a backup taken
// before has none, and the target keeps its own). The target's postgres
// exists already, so CREATE ROLE postgres fails, which psql reports and
// goes on from, as pg_dumpall's documentation says to expect; ALTER ROLE
// then gives it the backup's attributes and password.
func (p *postgresSnapshotter) importRoles(ctx context.Context, r admin.EntryReader) error {
	f, err := r.Open(pgRolesEntry)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<20))
	_ = f.Close()
	if err != nil {
		return err
	}
	if err := p.exec(ctx, bytes.NewReader(pgUserRoles(b)), io.Discard, "psql", "-X", "-q", "-U", p.user(), "-d", p.maintenance(),
		"-v", "ON_ERROR_STOP=0"); err != nil {
		return fmt.Errorf("restore the roles: %w", err)
	}
	return nil
}

func (p *postgresSnapshotter) exportOne(ctx context.Context, w admin.EntryWriter, i int, name string) error {
	f, err := os.CreateTemp("", "cloudburrow-pgdump-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := p.exec(ctx, nil, f, "pg_dump", "-U", p.user(), "-d", name, "-Fc", "--no-owner"); err != nil {
		return err
	}
	size, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return w.Add(pgDumpEntry(i), size, f)
}

// Import replaces the server's databases with the archive's: every
// application database is dropped, each archived one created and restored,
// and the default database recreated empty if the archive lacks it, so an
// application finds at least what a fresh instance has.
func (p *postgresSnapshotter) Import(ctx context.Context, r admin.EntryReader) error {
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
		if name == "" || (name == "postgres" && !p.bare) || strings.HasPrefix(name, "template") {
			return fmt.Errorf("%s names %q, which is not an application database", pgDatabasesEntry, name)
		}
	}

	current, err := p.databases(ctx)
	if err != nil {
		return err
	}
	for _, name := range current {
		// FORCE ends the sessions an application still holds; without it
		// a connected client would make the load fail halfway.
		if _, err := p.psql(ctx, "DROP DATABASE "+quotePGIdent(name)+" WITH (FORCE)"); err != nil {
			if !strings.Contains(err.Error(), "syntax error") {
				return fmt.Errorf("drop %s: %w", name, err)
			}
			// Before PostgreSQL 13 DROP DATABASE has no FORCE (#1524): the
			// sessions are ended first, as FORCE does, then it is dropped.
			if _, err := p.psql(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = "+
				quotePGLiteral(name)+" AND pid <> pg_backend_pid()"); err != nil {
				return fmt.Errorf("end the sessions of %s: %w", name, err)
			}
			if _, err := p.psql(ctx, "DROP DATABASE "+quotePGIdent(name)); err != nil {
				return fmt.Errorf("drop %s: %w", name, err)
			}
		}
	}
	if p.bare {
		// Before the databases, so a restored object's grants find their
		// roles.
		if err := p.importRoles(ctx, r); err != nil {
			return err
		}
	}
	restored := false
	for i, name := range names {
		if _, err := p.psql(ctx, "CREATE DATABASE "+quotePGIdent(name)); err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		if err := p.importOne(ctx, r, i, name); err != nil {
			return fmt.Errorf("restore %s: %w", name, err)
		}
		restored = restored || name == components.CloudSQLDatabase
	}
	if !restored && !p.bare {
		if _, err := p.psql(ctx, "CREATE DATABASE "+quotePGIdent(components.CloudSQLDatabase)); err != nil {
			return fmt.Errorf("recreate %s: %w", components.CloudSQLDatabase, err)
		}
	}
	return nil
}

func (p *postgresSnapshotter) importOne(ctx context.Context, r admin.EntryReader, i int, name string) error {
	f, err := r.Open(pgDumpEntry(i))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("the archive lists %s but holds no dump for it", name)
		}
		return err
	}
	defer f.Close()
	return p.exec(ctx, f, io.Discard, "pg_restore", "-U", p.user(), "-d", name,
		"--no-owner", "--exit-on-error")
}
