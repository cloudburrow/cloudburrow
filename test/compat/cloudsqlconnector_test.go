//go:build compat

package compat

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	cloudsqlmysql "cloud.google.com/go/cloudsqlconn/mysql/mysql"
	"cloud.google.com/go/cloudsqlconn/postgres/pgxv5"
	"golang.org/x/oauth2"
)

// EnvCloudSQLConnector is the Cloud SQL connectors' TLS listener (#1364).
const EnvCloudSQLConnector = "CLOUDBURROW_TEST_CLOUDSQL_CONNECTOR"

// connectorOptions point the official connector at CloudBurrow's Admin API.
// The token is never checked; the connector insists on having one.
//
// An instance started with --cloudsql-connector on a port other than 3307
// (#1364; a second instance on macOS, where 127.0.0.1 is the only loopback
// address without root) needs what an application beside it needs: the
// connector's own WithDialFunc, redirecting its dial of the reported
// address at 3307 to the listener. The connector still dials the host
// connect.get reported, at 3307, and the redirect checks it.
func connectorOptions(h *Harness) []cloudsqlconn.Option {
	opts := []cloudsqlconn.Option{
		cloudsqlconn.WithAdminAPIEndpoint("http://" + h.Endpoint(EnvSQLAdmin) + "/"),
		cloudsqlconn.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{
			AccessToken: "cloudburrow", Expiry: time.Now().Add(time.Hour)})),
	}
	listener := os.Getenv(EnvCloudSQLConnector)
	host, port, err := net.SplitHostPort(listener)
	if err != nil || port == "3307" {
		return opts
	}
	return append(opts, cloudsqlconn.WithDialFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
		if want := net.JoinHostPort(host, "3307"); addr != want {
			return nil, fmt.Errorf("the connector dialed %s, want %s (connect.get's address at 3307)", addr, want)
		}
		return (&net.Dialer{}).DialContext(ctx, network, listener)
	}))
}

// TestCloudSQLConnectorDialsBothServers (#1330): the official Cloud SQL Go
// connector (cloud.google.com/go/cloudsqlconn), through its own pgx v5 and
// MySQL drivers and nothing else, reaches both servers by connection name
// and runs SELECT 1. Under it: connectSettings for the server CA and
// address, generateEphemeralCert for a client certificate signed for the
// connector's key, and TLS 1.3 to port 3307 with that certificate.
// covers: sql.connect.get, sql.connect.generateEphemeral
func TestCloudSQLConnectorDialsBothServers(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" && os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	project := h.Project()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// connect.get through the generated client: the CA and address the
	// connector will use.
	svc := sqlAdminService(t, h)
	inst := "postgres"
	if os.Getenv(EnvCloudSQL) == "" {
		inst = "mysql"
	}
	cs, err := svc.Connect.Get(project, inst).Do()
	if err != nil {
		t.Fatal(err)
	}
	if cs.ServerCaCert == nil || cs.ServerCaCert.Cert == "" || cs.Region != "us-central1" || cs.BackendType != "SECOND_GEN" ||
		len(cs.IpAddresses) != 1 || cs.IpAddresses[0].Type != "PRIMARY" || cs.DnsName == "" {
		t.Errorf("connectSettings: %+v", cs)
	}
	// The address is the connector listener's host (#1364).
	if host, _, err := net.SplitHostPort(os.Getenv(EnvCloudSQLConnector)); err == nil && len(cs.IpAddresses) == 1 &&
		host != "0.0.0.0" && cs.IpAddresses[0].IpAddress != host {
		t.Errorf("connectSettings reports %s, want the connector listener's host %s", cs.IpAddresses[0].IpAddress, host)
	}

	selectOne := func(t *testing.T, db *sql.DB) {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&n); err != nil || n != 1 {
			t.Fatalf("SELECT 1 through the connector: %d, %v", n, err)
		}
	}
	if os.Getenv(EnvCloudSQL) != "" {
		t.Run("postgres", func(t *testing.T) {
			h := h.For(t)
			cleanup, err := pgxv5.RegisterDriver("cloudsql-postgres", connectorOptions(h)...)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			db, err := sql.Open("cloudsql-postgres", fmt.Sprintf(
				"host=%s:us-central1:postgres user=cloudburrow dbname=cloudburrow sslmode=disable", project))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			selectOne(t, db)
		})
	}
	if os.Getenv(EnvMySQL) != "" {
		t.Run("mysql", func(t *testing.T) {
			h := h.For(t)
			cleanup, err := cloudsqlmysql.RegisterDriver("cloudsql-mysql", connectorOptions(h)...)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			db, err := sql.Open("cloudsql-mysql", fmt.Sprintf("cloudburrow:%s@cloudsql-mysql(%s:us-central1:mysql)/cloudburrow",
				os.Getenv(EnvMySQLPassword), project))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			selectOne(t, db)
		})
	}
}
