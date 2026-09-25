//go:build compat

package compat

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// EnvCloudSQL is the host address of Cloud SQL's PostgreSQL.
const EnvCloudSQL = "CLOUDBURROW_TEST_CLOUDSQL"

func pgConnect(t *testing.T, h *Harness, database string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(h.Context(), fmt.Sprintf("postgres://cloudburrow@%s/%s?sslmode=disable", h.Endpoint(EnvCloudSQL), database))
	if err != nil {
		t.Fatalf("connect to %s: %v", database, err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func pgRows(t *testing.T, h *Harness, c *pgx.Conn) []string {
	t.Helper()
	rows, err := c.Query(h.Context(), "SELECT id::text || '=' || name FROM snap_widgets ORDER BY id")
	if err != nil {
		t.Fatalf("read rows: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestStateRestoresCloudSQL (#311): a table with rows is saved with
// `cloudburrow state save`, then changed and a database added, and `state
// load` brings back exactly the saved rows and removes the database made
// since. `reset` leaves PostgreSQL alone, so the changes stand in for one:
// the load has to replace them, not merely find the data still there. The
// archive holds no kubeconfig.
func TestStateRestoresCloudSQL(t *testing.T) {
	h := New(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	flags := strings.Fields(os.Getenv(EnvCLIArgs))
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(cli, append(args, flags...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("cloudburrow %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	c := pgConnect(t, h, "cloudburrow")
	for _, stmt := range []string{
		"DROP TABLE IF EXISTS snap_widgets",
		"CREATE TABLE snap_widgets (id int PRIMARY KEY, name text NOT NULL)",
		"INSERT INTO snap_widgets VALUES (1, 'first'), (2, 'second'), (3, 'ünïcode ✓')",
	} {
		if _, err := c.Exec(h.Context(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	want := pgRows(t, h, c)
	_ = c.Close(h.Context())

	file := filepath.Join(t.TempDir(), "state.tar.gz")
	if saved := run("state", "save", file); !strings.Contains(saved, "captured:     cloudsql") {
		t.Fatalf("state save does not capture cloudsql:\n%s", saved)
	}
	kubeconfig := filepath.Join(instanceDirFrom(t, flags), "kubeconfig")
	assertArchiveOmits(t, file, kubeconfig)

	// Changes after the snapshot, all of which the load must undo.
	c = pgConnect(t, h, "cloudburrow")
	for _, stmt := range []string{"DELETE FROM snap_widgets WHERE id = 2", "INSERT INTO snap_widgets VALUES (4, 'later')"} {
		if _, err := c.Exec(h.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.Close(h.Context())
	admin := pgConnect(t, h, "postgres")
	if _, err := admin.Exec(h.Context(), "DROP DATABASE IF EXISTS snap_later WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(h.Context(), "CREATE DATABASE snap_later"); err != nil {
		t.Fatal(err)
	}

	if loaded := run("state", "load", file); !strings.Contains(loaded, "cloudsql") {
		t.Errorf("state load output does not name cloudsql:\n%s", loaded)
	}
	if got := pgRows(t, h, pgConnect(t, h, "cloudburrow")); !reflect.DeepEqual(got, want) {
		t.Errorf("rows after the load %v, want %v", got, want)
	}
	var n int
	if err := admin.QueryRow(h.Context(), "SELECT count(*) FROM pg_database WHERE datname = 'snap_later'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("a database created after the snapshot survived the load")
	}
}

// assertArchiveOmits fails if any entry holds the kubeconfig's contents or
// its path.
func assertArchiveOmits(t *testing.T, archive, kubeconfig string) {
	t.Helper()
	kc, err := os.ReadFile(kubeconfig)
	if err != nil {
		t.Fatalf("read the instance kubeconfig: %v", err)
	}
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		if strings.Contains(string(b), strings.TrimSpace(string(kc))) || strings.Contains(string(b), kubeconfig) ||
			strings.Contains(string(b), "client-key-data") {
			t.Errorf("archive entry %s holds the kubeconfig or a cluster credential", hdr.Name)
		}
	}
}
