package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
)

// PostgreSQL's generated credentials (#2208): the password of the
// application's user on the server `up` runs (cloudburrow), and the
// management role's on every PostgreSQL server of the instance
// (cloudsqladmin, alloydbadmin). Kept owner-only in the instance's
// directory, generated once: a server's data directory keeps them.
const postgresCredentialsFile = "cloudsql-postgres.json"

func postgresCredentialsPath(cfg config.Config) string {
	return filepath.Join(cfg.InstanceDir(), postgresCredentialsFile)
}

var pgCredsMu sync.Mutex

// loadOrCreatePostgresCredentials returns the instance's PostgreSQL
// passwords, generating them on first use.
func loadOrCreatePostgresCredentials(cfg config.Config) (components.PostgresCredentials, error) {
	pgCredsMu.Lock()
	defer pgCredsMu.Unlock()
	path := postgresCredentialsPath(cfg)
	var c components.PostgresCredentials
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &c); err != nil || c.Password == "" || c.AdminPassword == "" {
			return c, fmt.Errorf("%s is unreadable; delete the instance or restore the file: %v", path, err)
		}
		return c, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	// Passwords not kept are none: every call would make others.
	if cfg.StateDir == "" {
		return c, errors.New("the instance has no directory to keep its PostgreSQL passwords in")
	}
	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err != nil {
		return c, err
	}
	n := components.PostgresCredentials{Password: randomPassword(), AdminPassword: randomPassword()}
	b, _ := json.MarshalIndent(n, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return c, err
	}
	return n, nil
}

// postgresAdminPassword is the management role's password, "" when the
// credentials cannot be read (the servers then refuse CloudBurrow too,
// and say so in the error).
func postgresAdminPassword(cfg config.Config) string {
	c, _ := loadOrCreatePostgresCredentials(cfg)
	return c.AdminPassword
}
