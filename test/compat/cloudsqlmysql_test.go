//go:build compat

package compat

import (
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// EnvMySQL is the host address of Cloud SQL for MySQL, and EnvMySQLPassword
// the instance's generated password, as `cloudburrow env` exports them.
const (
	EnvMySQL         = "CLOUDBURROW_TEST_MYSQL"
	EnvMySQLPassword = "CLOUDBURROW_TEST_MYSQL_PASSWORD"
	envMySQLExpect   = "CLOUDBURROW_TEST_MYSQL_EXPECT"
)

func mysqlDB(t *testing.T, h *Harness) *sql.DB {
	t.Helper()
	addr := h.Endpoint(EnvMySQL)
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.Net, cfg.Addr, cfg.DBName = "cloudburrow", os.Getenv(EnvMySQLPassword), "tcp", addr, "cloudburrow"
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestCloudSQLMySQLDataPlane.
//
// `--services cloudsql-mysql` (#297) with go-sql-driver/mysql from the host,
// the mysql client from a pod through the Service name, and `reset`
// emptying it. A real MySQL 8.4; the Cloud SQL Admin API is not served.
func TestCloudSQLMySQLDataPlane(t *testing.T) {
	h := New(t)
	db := mysqlDB(t, h)
	ctx := h.Context()

	var version string
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		t.Fatalf("connect from the host: %v", err)
	}
	if !strings.HasPrefix(version, "8.4.") {
		t.Errorf("VERSION() = %q, want 8.4.x", version)
	}
	for _, q := range []string{
		"CREATE TABLE widgets (id INT PRIMARY KEY, name VARCHAR(40))",
		"INSERT INTO widgets VALUES (1, 'sprocket'), (2, 'gear')",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// From a pod, by Service name, with the image's own client.
	if cli := os.Getenv(EnvCLI); cli != "" {
		dir := instanceDirFrom(t, strings.Fields(os.Getenv(EnvCLIArgs)))
		out := runPod(t, ctx, filepath.Join(dir, "kubeconfig"), "mysql-client", imageConst(t, "CloudSQLMySQLImage"),
			[]string{"MYSQL_PWD=" + os.Getenv(EnvMySQLPassword)},
			"mysql", "-h", "cloudsql-mysql.cloudburrow.svc.cluster.local", "-u", "cloudburrow",
			"-N", "-e", "SELECT COUNT(*) FROM cloudburrow.widgets")
		if out != "2" {
			t.Errorf("in-cluster SELECT via cloudsql-mysql.cloudburrow.svc.cluster.local = %q, want 2", out)
		}
	} else {
		t.Logf("%s is not set; the in-cluster address was not exercised", EnvCLI)
	}

	// reset removes it.
	if code, body := adminReset(t, h.Endpoint(EnvControl), "service=cloudsql-mysql"); code != http.StatusOK {
		t.Fatalf("reset = %d: %s", code, body)
	}
	var n int
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM widgets").Scan(&n)
	var me *mysql.MySQLError
	if !errors.As(err, &me) || me.Number != 1146 {
		t.Errorf("after reset, SELECT from widgets = %d, %v; want error 1146 (no such table)", n, err)
	}

	// Left for TestCloudSQLMySQLAcrossRestart, which CI runs after stop/up.
	for _, q := range []string{
		"CREATE TABLE restart_probe (v VARCHAR(40))",
		"INSERT INTO restart_probe VALUES ('written-before-stop')",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// TestCloudSQLMySQLAcrossRestart measures durability, as the Memorystore
// probe does: after stop/up in persistent mode the row is there; in
// ephemeral mode the server starts empty.
func TestCloudSQLMySQLAcrossRestart(t *testing.T) {
	expect := os.Getenv(envMySQLExpect)
	if expect == "" {
		t.Skipf("%s is not set: this runs only after CI restarts the instance", envMySQLExpect)
	}
	h := New(t)
	db := mysqlDB(t, h)
	var v string
	err := db.QueryRowContext(h.Context(), "SELECT v FROM restart_probe").Scan(&v)
	switch expect {
	case "present":
		if err != nil || v != "written-before-stop" {
			t.Fatalf("persistent mode after stop/up: %q, %v; want the row written before stop", v, err)
		}
	case "absent":
		var me *mysql.MySQLError
		if !errors.As(err, &me) || me.Number != 1146 {
			t.Fatalf("ephemeral mode after stop/up: %q, %v; want no table (1146)", v, err)
		}
	default:
		t.Fatalf("%s must be present or absent, not %q", envMySQLExpect, expect)
	}
}
