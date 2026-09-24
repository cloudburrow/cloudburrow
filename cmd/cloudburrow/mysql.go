package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-sql-driver/mysql"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// Cloud SQL for MySQL (#297): the instance's generated credentials, and the
// reset that empties the server.

const mysqlCredentialsFile = "cloudsql-mysql.json"

func mysqlCredentialsPath(cfg config.Config) string {
	return filepath.Join(cfg.InstanceDir(), mysqlCredentialsFile)
}

// loadOrCreateMySQLCredentials returns the instance's MySQL passwords,
// generating them on first use.
//
// Generated once and kept, not regenerated: MySQL writes the passwords into
// its data directory when it initialises it, so a new one on the next start
// would lock the application out of a persistent instance's own data.
func loadOrCreateMySQLCredentials(cfg config.Config) (components.MySQLCredentials, error) {
	path := mysqlCredentialsPath(cfg)
	var c components.MySQLCredentials
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &c); err != nil || c.Password == "" || c.RootPassword == "" {
			return c, fmt.Errorf("%s is unreadable; delete the instance or restore the file: %v", path, err)
		}
		return c, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	c = components.MySQLCredentials{Password: randomPassword(), RootPassword: randomPassword()}
	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err != nil {
		return c, err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return c, err
	}
	return c, nil
}

// randomPassword is 24 characters a shell, a URL and a DSN all take
// unquoted.
func randomPassword() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// mysqlSystemDatabases are MySQL's own, which a reset never touches.
var mysqlSystemDatabases = map[string]bool{
	"mysql": true, "information_schema": true, "performance_schema": true, "sys": true,
}

// mysqlResetter empties the server: every database the application made is
// dropped, and the default one recreated empty, so the application's user
// finds what a fresh instance has.
type mysqlResetter struct {
	tunnel *netfwd.Forwarder
	creds  components.MySQLCredentials
}

func (m *mysqlResetter) Name() string { return string(config.ServiceCloudSQLMySQL) }

func (m *mysqlResetter) Reset(ctx context.Context) error {
	if m.tunnel == nil || m.tunnel.HostAddr() == "" {
		return errors.New("the MySQL tunnel is not running")
	}
	dsn := (&mysql.Config{
		User: "root", Passwd: m.creds.RootPassword, Net: "tcp", Addr: m.tunnel.HostAddr(),
		AllowNativePasswords: true,
	}).FormatDSN()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return fmt.Errorf("list databases: %w", err)
	}
	var drop []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		if !mysqlSystemDatabases[name] {
			drop = append(drop, name)
		}
	}
	rows.Close()
	for _, name := range drop {
		if _, err := db.ExecContext(ctx, "DROP DATABASE "+quoteMySQLIdent(name)); err != nil {
			return fmt.Errorf("drop %s: %w", name, err)
		}
	}
	// The user's grant is on the database's name, so it applies to the new
	// one as it did to the old.
	if _, err := db.ExecContext(ctx, "CREATE DATABASE "+quoteMySQLIdent(components.CloudSQLDatabase)); err != nil {
		return fmt.Errorf("recreate %s: %w", components.CloudSQLDatabase, err)
	}
	return nil
}

func quoteMySQLIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}
