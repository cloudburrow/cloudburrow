//go:build compat

package compat

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestCloudSQLFromTheExportedPGVariables.
//
// `cloudburrow env` exports PGHOST, PGPORT, PGUSER, PGDATABASE and PGSSLMODE
// for Cloud SQL for PostgreSQL (#584), the variables libpq and every driver
// modelled on it read. A client given those and nothing else — pgx with an
// empty connection string, so every setting comes from the environment —
// connects and runs a query. So does one given CLOUDBURROW_CLOUDSQL_URL.
func TestCloudSQLFromTheExportedPGVariables(t *testing.T) {
	h := New(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	out, err := exec.Command(cli, append([]string{"env", "--format", "json"}, strings.Fields(os.Getenv(EnvCLIArgs))...)...).Output()
	if err != nil {
		t.Fatalf("cloudburrow env --format json: %v", err)
	}
	var exported map[string]string
	if err := json.Unmarshal(out, &exported); err != nil {
		t.Fatalf("env --format json: %v\n%s", err, out)
	}
	if _, ok := exported["CLOUDBURROW_CLOUDSQL_URL"]; !ok {
		t.Skip("the instance does not have cloudsql enabled")
	}
	for _, name := range []string{"PGHOST", "PGPORT", "PGUSER", "PGDATABASE", "PGSSLMODE"} {
		if exported[name] == "" {
			t.Fatalf("env exports CLOUDBURROW_CLOUDSQL_URL but not %s:\n%s", name, out)
		}
	}
	addr := net.JoinHostPort(exported["PGHOST"], exported["PGPORT"])
	h.requireLocal(addr)
	h.requireReachable(addr)

	// Only what env exported: every PG* variable this process inherited is
	// cleared first, so nothing but env's output can supply a setting.
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "PG") {
			t.Setenv(name, "")
			os.Unsetenv(name)
		}
	}
	for name, v := range exported {
		if strings.HasPrefix(name, "PG") {
			t.Setenv(name, v)
		}
	}

	ctx := h.Context()
	for _, dsn := range []string{"", exported["CLOUDBURROW_CLOUDSQL_URL"]} {
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("pgx.Connect(%q) with env's PG* variables: %v", dsn, err)
		}
		var one int
		err = conn.QueryRow(ctx, "SELECT 1").Scan(&one)
		_ = conn.Close(ctx)
		if err != nil || one != 1 {
			t.Errorf("SELECT 1 via %q = %d, %v; want 1", dsn, one, err)
		}
	}
}
