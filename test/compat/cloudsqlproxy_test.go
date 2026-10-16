//go:build compat

package compat

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// EnvCloudSQLProxy names Google's Cloud SQL Auth Proxy binary, the release
// `make cloud-sql-proxy` fetches and checks against its pinned SHA-256
// (#1421).
const EnvCloudSQLProxy = "CLOUDBURROW_TEST_CLOUD_SQL_PROXY"

// cloudSQLProxyVersion is the release the Makefile pins, the one the Cloud
// Run adapter's sidecar image is.
const cloudSQLProxyVersion = "2.19.0"

// freeLocalPort is a loopback port nothing listens on now.
func freeLocalPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// syncBuffer is the proxy's output, written by its process and read by the
// test.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startCloudSQLProxy runs the proxy binary for the connection names, each
// at its port on 127.0.0.1, against the instance's Admin API, with a static
// token and Automatic IAM Authentication off, and stops it at the end of
// the test. It returns once the proxy reports it is ready.
func startCloudSQLProxy(t *testing.T, h *Harness, bin string, ports map[string]int) {
	t.Helper()
	args := []string{"--sqladmin-api-endpoint", "http://" + h.Endpoint(EnvSQLAdmin) + "/",
		"--token", "cloudburrow", "--auto-iam-authn=false", "--address", "127.0.0.1", "--exit-zero-on-sigterm"}
	for conn, port := range ports {
		args = append(args, fmt.Sprintf("%s?port=%d", conn, port))
	}
	cmd := exec.Command(bin, args...)
	// No HTTPS request may leave the machine: the Admin API is plain HTTP on
	// loopback, and the connector is TLS straight to it, not through a proxy.
	cmd.Env = noGoogleEgress()
	out := &syncBuffer{}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = io.MultiWriter(out, pw), io.MultiWriter(out, pw)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(pr)
		closed := false
		for sc.Scan() {
			if !closed && strings.Contains(sc.Text(), "ready for new connections") {
				close(ready)
				closed = true
			}
		}
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); _ = pw.Close() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the proxy exited with %v after SIGTERM:\n%s", err, lastLines(out.String(), 20))
			}
		case <-time.After(30 * time.Second):
			_ = cmd.Process.Kill()
			t.Errorf("the proxy did not stop on SIGTERM:\n%s", lastLines(out.String(), 20))
		}
	})
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("the proxy exited (%v):\n%s", err, lastLines(out.String(), 20))
	case <-time.After(time.Minute):
		t.Fatalf("the proxy did not become ready:\n%s", lastLines(out.String(), 20))
	}
	t.Logf("cloud-sql-proxy %s", strings.Join(args, " "))
}

// TestCloudSQLAuthProxyBinary (#1421): Google's own Cloud SQL Auth Proxy
// binary, v2.19.0 as `make cloud-sql-proxy` fetches it (pinned by its
// SHA-256), pointed at the instance with --sqladmin-api-endpoint and a
// static --token, Automatic IAM Authentication off, serves each instance by
// connection name on a local port: pgx through it reads the rows a
// parallel import restored on PostgreSQL, and go-sql-driver/mysql the rows
// a parallel import restored on MySQL and those of a clone to binary log
// coordinates. Under it the proxy does what it does against Google:
// connectSettings, generateEphemeralCert for its own key, and TLS 1.3 to
// the address connectSettings reports, at port 3307.
//
// The proxy has no flag for the server port: it always dials 3307. An
// instance whose connector listener is elsewhere (--cloudsql-connector on
// another port) cannot be reached by it, and the test skips.
// covers: sql.connect.get, sql.connect.generateEphemeral, sql.instances.import, sql.instances.clone
func TestCloudSQLAuthProxyBinary(t *testing.T) {
	h := New(t)
	bin := os.Getenv(EnvCloudSQLProxy)
	if bin == "" {
		t.Skip("needs " + EnvCloudSQLProxy + ": export " + EnvCloudSQLProxy + "=$(make -s cloud-sql-proxy)")
	}
	if _, port, err := net.SplitHostPort(os.Getenv(EnvCloudSQLConnector)); err == nil && port != "3307" {
		t.Skipf("the connector listens at %s; the Auth Proxy always dials port 3307 and has no flag to change it",
			os.Getenv(EnvCloudSQLConnector))
	}
	v, err := exec.Command(bin, "--version").Output()
	if err != nil || !strings.Contains(string(v), "version "+cloudSQLProxyVersion+"+") {
		t.Fatalf("%s --version: %q, %v; want %s, the pinned release", bin, v, err, cloudSQLProxyVersion)
	}
	engines := sqlTransferEngines(t, h)
	svc := sqlAdminService(t, h)
	p := h.Project()
	bh := sqlTransferBucket(t, h, "sqlproxy")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Rows each engine's parallel import wrote, read through the proxy below.
	ports := map[string]int{}
	connName := map[string]string{}
	const want = "1=one;2=two;3=three"
	for _, e := range engines {
		e.exec(t, "DROP TABLE IF EXISTS xfer_rows", "CREATE TABLE xfer_rows (id INT PRIMARY KEY, name VARCHAR(40))",
			`INSERT INTO xfer_rows VALUES (1, 'one'), (2, 'two'), (3, 'three')`)
		t.Cleanup(func() { e.exec(t, "DROP TABLE IF EXISTS xfer_rows") })
		uri := "gs://" + bh.BucketName() + "/" + e.instance + "-par"
		x := &sqladmin.ExportContext{Uri: uri, FileType: "SQL", Databases: []string{"cloudburrow"},
			SqlExportOptions: &sqladmin.ExportContextSqlExportOptions{Parallel: true, Threads: 2}}
		if e.instance == "mysql" {
			x.SqlExportOptions.Tables = []string{"xfer_rows"}
		}
		op, err := svc.Instances.Export(p, e.instance, &sqladmin.InstancesExportRequest{ExportContext: x}).Do()
		if err != nil {
			t.Fatal(err)
		}
		if op = waitSQLOpDone(t, svc, p, op); op.Error != nil {
			t.Fatalf("parallel export of %s: %s", e.instance, op.Error.Errors[0].Message)
		}
		e.exec(t, "DROP TABLE xfer_rows")
		op, err = svc.Instances.Import(p, e.instance, &sqladmin.InstancesImportRequest{ImportContext: &sqladmin.ImportContext{
			Uri: uri, FileType: "SQL", Database: "cloudburrow",
			SqlImportOptions: &sqladmin.ImportContextSqlImportOptions{Parallel: true, Threads: 2}}}).Do()
		if err != nil {
			t.Fatal(err)
		}
		if op = waitSQLOpDone(t, svc, p, op); op.Error != nil {
			t.Fatalf("parallel import into %s: %s", e.instance, op.Error.Errors[0].Message)
		}
		in, err := svc.Instances.Get(p, e.instance).Do()
		if err != nil {
			t.Fatal(err)
		}
		connName[e.instance] = in.ConnectionName
		ports[in.ConnectionName] = freeLocalPort(t)
	}

	// A clone of a MySQL instance with binary logging, to the coordinates
	// after its first row and before its second.
	var clone string
	if os.Getenv(EnvMySQL) != "" {
		suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
		src := "px-src-" + suffix
		in := createSQLInstance(t, svc, p, &sqladmin.DatabaseInstance{Name: src, DatabaseVersion: "MYSQL_8_4", RootPassword: "Root-pass1",
			Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", BackupConfiguration: &sqladmin.BackupConfiguration{Enabled: true, BinaryLogEnabled: true}}})
		waitRecoverable(t, svc, p, src)
		db := sqlEngine{mysql: true}.root(t, h, in.ConnectionName)
		for _, q := range []string{"CREATE DATABASE app", "CREATE TABLE app.t (id INT PRIMARY KEY)", "INSERT INTO app.t VALUES (1)"} {
			q := q
			eventually(t, q, func() error { _, err := db.Exec(q); return err })
		}
		var file, doDB, ignoreDB, gtids string
		var pos int64
		if err := db.QueryRow("SHOW BINARY LOG STATUS").Scan(&file, &pos, &doDB, &ignoreDB, &gtids); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO app.t VALUES (2)"); err != nil {
			t.Fatal(err)
		}
		dest := src + "-bl"
		op, err := svc.Instances.Clone(p, src, &sqladmin.InstancesCloneRequest{CloneContext: &sqladmin.CloneContext{
			DestinationInstanceName: dest, BinLogCoordinates: &sqladmin.BinLogCoordinates{BinLogFileName: file, BinLogPosition: pos}}}).Do()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if d, err := svc.Instances.Delete(p, dest).Do(); err == nil {
				waitSQLOpDone(t, svc, p, d)
			}
		})
		if op = waitSQLOpDone(t, svc, p, op); op.Error != nil {
			t.Fatalf("clone to %s:%d: %s", file, pos, op.Error.Errors[0].Message)
		}
		got, err := svc.Instances.Get(p, dest).Do()
		if err != nil {
			t.Fatal(err)
		}
		clone = got.ConnectionName
		ports[clone] = freeLocalPort(t)
	}

	startCloudSQLProxy(t, h, bin, ports)

	if c := connName["postgres"]; c != "" {
		t.Run("pgx", func(t *testing.T) {
			var got string
			eventually(t, "read through the proxy", func() error {
				conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://cloudburrow@127.0.0.1:%d/cloudburrow?sslmode=disable", ports[c]))
				if err != nil {
					return err
				}
				defer conn.Close(context.Background())
				return conn.QueryRow(ctx, "SELECT string_agg(id::text || '=' || name, ';' ORDER BY id) FROM xfer_rows").Scan(&got)
			})
			if got != want {
				t.Errorf("PostgreSQL rows through the proxy at %s: %q, want %q", c, got, want)
			}
		})
	}
	mysqlQuery := func(t *testing.T, conn, user, password, database, q string) string {
		t.Helper()
		cfg := mysql.NewConfig()
		cfg.User, cfg.Passwd, cfg.Net, cfg.Addr, cfg.DBName = user, password, "tcp", fmt.Sprintf("127.0.0.1:%d", ports[conn]), database
		db, err := sql.Open("mysql", cfg.FormatDSN())
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var got sql.NullString
		eventually(t, "read through the proxy", func() error { return db.QueryRowContext(ctx, q).Scan(&got) })
		return got.String
	}
	if c := connName["mysql"]; c != "" {
		t.Run("mysql", func(t *testing.T) {
			got := mysqlQuery(t, c, "cloudburrow", os.Getenv(EnvMySQLPassword), "cloudburrow",
				"SELECT GROUP_CONCAT(CONCAT(id, '=', name) ORDER BY id SEPARATOR ';') FROM xfer_rows")
			if got != want {
				t.Errorf("MySQL rows through the proxy at %s: %q, want %q", c, got, want)
			}
		})
	}
	if clone != "" {
		t.Run("mysql-binlog-clone", func(t *testing.T) {
			if got := mysqlQuery(t, clone, "root", "Root-pass1", "app", "SELECT GROUP_CONCAT(id ORDER BY id) FROM t"); got != "1" {
				t.Errorf("rows in the binary log clone through the proxy at %s: %q, want 1", clone, got)
			}
		})
	}
}
