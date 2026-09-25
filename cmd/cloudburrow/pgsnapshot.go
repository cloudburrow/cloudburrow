package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
}

func newPostgresSnapshotter(cfg config.Config) *postgresSnapshotter {
	kubeconfig, namespace := cfg.KubeconfigPath(), cfg.Cluster.Namespace
	return &postgresSnapshotter{exec: func(ctx context.Context, stdin io.Reader, stdout io.Writer, argv ...string) error {
		args := []string{"--kubeconfig", kubeconfig, "-n", namespace, "exec"}
		if stdin != nil {
			args = append(args, "-i")
		}
		args = append(append(args, "deploy/"+string(config.ServiceCloudSQL), "--"), argv...)
		cmd := exec.CommandContext(ctx, "kubectl", args...)
		var stderr bytes.Buffer
		cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w: %s", argv[0], err, strings.TrimSpace(tail(stderr.String(), 2000)))
		}
		return nil
	}}
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
	err := p.exec(ctx, nil, &out, "psql", "-U", components.CloudSQLUser, "-d", "postgres",
		"-v", "ON_ERROR_STOP=1", "-AtX", "-c", statement)
	return out.String(), err
}

// databases are the application's: every database but the templates and
// the maintenance one.
func (p *postgresSnapshotter) databases(ctx context.Context) ([]string, error) {
	out, err := p.psql(ctx, "SELECT datname FROM pg_database WHERE NOT datistemplate AND datname <> 'postgres' ORDER BY datname")
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
	return nil
}

func (p *postgresSnapshotter) exportOne(ctx context.Context, w admin.EntryWriter, i int, name string) error {
	f, err := os.CreateTemp("", "cloudburrow-pgdump-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := p.exec(ctx, nil, f, "pg_dump", "-U", components.CloudSQLUser, "-d", name, "-Fc", "--no-owner"); err != nil {
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
		if name == "" || name == "postgres" || strings.HasPrefix(name, "template") {
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
			return fmt.Errorf("drop %s: %w", name, err)
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
	if !restored {
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
	return p.exec(ctx, f, io.Discard, "pg_restore", "-U", components.CloudSQLUser, "-d", name,
		"--no-owner", "--exit-on-error")
}
