//go:build compat

package compat

// Cloud SQL for SQL Server (#1916). Microsoft's SQL Server image starts
// only once its End-User Licensing Agreement is accepted, and the user
// who provisions SQL Server accepts it: nobody, these tests included,
// accepts it on their behalf. So the tests that start SQL Server run only
// when the person running them has opted in, by exporting
// CLOUDBURROW_ACCEPT_MSSQL_EULA=Y both for `cloudburrow up` (which then
// records them as accepting it) and for `go test`; otherwise they skip,
// and the refusal test runs instead (docs/cloudsql.md#cloud-sql-for-sql-server).

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	mssql "github.com/microsoft/go-mssqldb"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// EnvAcceptMSSQLEULA is the user's own opt-in: Y accepts Microsoft's SQL
// Server licence, as `up` reads it too.
const EnvAcceptMSSQLEULA = "CLOUDBURROW_ACCEPT_MSSQL_EULA"

func mssqlOptedIn() bool { return os.Getenv(EnvAcceptMSSQLEULA) == "Y" }

// wantEULARefusal checks a SQL Server create was refused for want of the
// user's acceptance, saying how to accept.
func wantEULARefusal(t *testing.T, what string, err error, out string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: created with no acceptance of Microsoft's licence", what)
		return
	}
	msg := err.Error() + " " + out
	for _, want := range []string{"--accept-mssql-eula", "CLOUDBURROW_ACCEPT_MSSQL_EULA=Y", "never accepts it on your behalf"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%s: refusal %q does not say %q", what, msg, want)
		}
	}
}

// TestCloudSQLServerRefusedWithoutEULA (#1916): with no acceptance of
// Microsoft's licence, instances.insert of every SQL Server version is
// FAILED_PRECONDITION (412) saying how to accept, and no instance is made.
func TestCloudSQLServerRefusedWithoutEULA(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	if mssqlOptedIn() {
		t.Skip(EnvAcceptMSSQLEULA + "=Y: this instance was started accepting Microsoft's licence, so the refusal is not what it answers; " +
			"TestCloudSQLServerInstance runs instead, and the unit tests check the refusal")
	}
	project := h.Project()
	for _, v := range []string{"SQLSERVER_2017_EXPRESS", "SQLSERVER_2019_WEB", "SQLSERVER_2022_STANDARD", "SQLSERVER_2025_ENTERPRISE"} {
		name := fmt.Sprintf("ms-%s-%d", strings.ToLower(strings.ReplaceAll(v[10:], "_", "-")), time.Now().UnixNano()%1e6)
		_, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: v, RootPassword: "Str0ng!Passw0rd",
			Settings: &sqladmin.Settings{Tier: "db-custom-2-7680"}}).Do()
		wantHTTP(t, "insert "+v, err, 412)
		wantEULARefusal(t, "insert "+v, err, "")
		_, err = svc.Instances.Get(project, name).Do()
		wantHTTP(t, "get of the refused "+v, err, 404)
	}
}

// TestCloudSQLServerInstance (#1916): only for a person who accepted
// Microsoft's licence. A SQLSERVER_2022_EXPRESS instance is created,
// becomes RUNNABLE, and serves its sqlserver user, databases, users and a
// database flag through the official Go client, and a connection through
// the official Go connector as sqlserver.
func TestCloudSQLServerInstance(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	if !mssqlOptedIn() {
		t.Skip("starts SQL Server, whose licence only you can accept: export " + EnvAcceptMSSQLEULA +
			"=Y for `cloudburrow up` and for this test to run it")
	}
	project := h.Project()
	name := fmt.Sprintf("ms22-%d", time.Now().UnixNano()%1e6)
	const pw = "Str0ng!Passw0rd"
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "SQLSERVER_2022_EXPRESS", RootPassword: pw,
		Settings: &sqladmin.Settings{Tier: "db-custom-2-7680",
			DatabaseFlags: []*sqladmin.DatabaseFlags{{Name: "max degree of parallelism", Value: "2"}}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if op, err := svc.Instances.Delete(project, name).Do(); err == nil {
			waitSQLOp(t, svc, project, op)
		}
	}()
	// The image's first pull is about 460 MB.
	deadline := time.Now().Add(15 * time.Minute)
	for {
		in, err := svc.Instances.Get(project, name).Do()
		if err != nil {
			t.Fatal(err)
		}
		if in.State == "RUNNABLE" {
			break
		}
		if in.State == "FAILED" || time.Now().After(deadline) {
			t.Fatalf("instance %s is %s", name, in.State)
		}
		time.Sleep(5 * time.Second)
	}
	op, err = svc.Databases.Insert(project, name, &sqladmin.Database{Name: "orders"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	dbs, err := svc.Databases.List(project, name).Do()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range dbs.Items {
		found = found || d.Name == "orders"
	}
	if !found {
		t.Errorf("databases.list lacks orders: %+v", dbs.Items)
	}
	op, err = svc.Users.Insert(project, name, &sqladmin.User{Name: "app", Password: "An0ther!Passw0rd"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	users, err := svc.Users.List(project, name).Do()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, u := range users.Items {
		names = append(names, u.Name)
	}
	if got := strings.Join(names, ","); !strings.Contains(got, "sqlserver") || !strings.Contains(got, "app") || strings.Contains(got, ",sa,") {
		t.Errorf("users.list = %s", got)
	}

	// The official connector, then a query as sqlserver.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d, err := cloudsqlconn.NewDialer(ctx, connectorOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	in, err := svc.Instances.Get(project, name).Do()
	if err != nil {
		t.Fatal(err)
	}
	u := &url.URL{Scheme: "sqlserver", User: url.UserPassword("sqlserver", pw), Host: "localhost",
		RawQuery: url.Values{"database": {"orders"}, "encrypt": {"disable"}}.Encode()}
	c, err := mssql.NewConnector(u.String())
	if err != nil {
		t.Fatal(err)
	}
	c.Dialer = connDialer{d: d, name: in.ConnectionName}
	db := sql.OpenDB(c)
	defer db.Close()
	var maxdop int
	if err := db.QueryRowContext(ctx, "SELECT CAST(value_in_use AS int) FROM sys.configurations WHERE name = 'max degree of parallelism'").Scan(&maxdop); err != nil {
		t.Fatalf("query through the connector: %v", err)
	}
	if maxdop != 2 {
		t.Errorf("max degree of parallelism = %d, want 2 (the database flag)", maxdop)
	}
}

// connDialer dials every connection of the driver through the connector.
type connDialer struct {
	d    *cloudsqlconn.Dialer
	name string
}

func (c connDialer) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	return c.d.Dial(ctx, c.name)
}
