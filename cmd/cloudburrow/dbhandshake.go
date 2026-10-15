package main

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// Readiness of the Cloud SQL tunnels is a login, not a TCP connect (#1262).
//
// After stop and up, MySQL's tunnel carried a connection, `up` printed
// "ready", and the first client's handshake ended in "unexpected EOF": the
// stream behind the tunnel did not reach a serving mysqld yet. A database
// is ready for its user when its user can log in to its database through
// the address `up` prints, so that is what each tunnel checks before it
// counts as up, and again whenever it is re-established.

// databaseHandshake is the handshake for a service's tunnel, or nil for a
// service with no database protocol.
func databaseHandshake(s config.Service, mysqlCreds components.MySQLCredentials) netfwd.Handshake {
	switch s {
	case config.ServiceCloudSQLMySQL:
		return func(ctx context.Context, addr string) error { return mysqlHandshake(ctx, addr, mysqlCreds) }
	case config.ServiceCloudSQL:
		return postgresHandshake
	}
	return nil
}

// mysqlHandshake logs in as the application's user to its database and
// runs SELECT 1, through go-sql-driver/mysql, the driver the docs name.
func mysqlHandshake(ctx context.Context, addr string, creds components.MySQLCredentials) error {
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.Net, cfg.Addr, cfg.DBName = components.CloudSQLUser, creds.Password, "tcp", addr, components.CloudSQLDatabase
	// The driver logs a failed handshake's "unexpected EOF" itself; the
	// error returned says the same, once, in the tunnel's own line.
	cfg.Logger = mysql.Logger(discardLogger{})
	if dl, ok := ctx.Deadline(); ok {
		// The driver's reads and writes are not bound by ctx once the
		// connection is made: a stream that hangs mid-handshake would
		// outlive it.
		d := time.Until(dl)
		cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = d, d, d
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return err
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	var one int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("MySQL login as %s through %s: %w", components.CloudSQLUser, addr, err)
	}
	return nil
}

// postgresHandshake connects as the application's user to its database,
// with the trust authentication the server runs, and runs SELECT 1.
func postgresHandshake(ctx context.Context, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("postgres://%s@%s:%s/%s?sslmode=disable",
		components.CloudSQLUser, host, port, components.CloudSQLDatabase)
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return fmt.Errorf("PostgreSQL login as %s through %s: %w", components.CloudSQLUser, addr, err)
	}
	defer conn.Close(context.Background())
	var one int
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("PostgreSQL SELECT 1 through %s: %w", addr, err)
	}
	return nil
}

type discardLogger struct{}

func (discardLogger) Print(...any) {}
