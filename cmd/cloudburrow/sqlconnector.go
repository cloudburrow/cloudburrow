package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// The Cloud SQL connector listener's address, kept in the instance's state
// directory (#1571). `env` is a separate process: with only its own flags,
// `env --name x` for an instance started with --cloudsql-connector left out
// CLOUDBURROW_CLOUDSQL_CONNECTOR_ENDPOINT unless the flag was passed again,
// and a dialer that redirects 3307 then dialed the default, which is often
// another instance's.
type cloudSQLConnectorState struct {
	Address string `json:"address"`
}

func cloudSQLConnectorStatePath(cfg config.Config) string {
	return filepath.Join(cfg.InstanceDir(), "cloudsql-connector.json")
}

// recordCloudSQLConnector writes the address `up` binds when
// --cloudsql-connector moved it, and removes the record when it did not, so
// an instance restarted at the default no longer reports the old address.
func recordCloudSQLConnector(cfg config.Config) error {
	path := cloudSQLConnectorStatePath(cfg)
	if cfg.CloudSQLConnector == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(cloudSQLConnectorState{Address: cfg.CloudSQLConnector}, "", "  ")
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// withRecordedCloudSQLConnector is cfg with the connector address the
// instance recorded, unless this process was given one itself.
func withRecordedCloudSQLConnector(cfg config.Config) config.Config {
	if cfg.CloudSQLConnector != "" {
		return cfg
	}
	b, err := os.ReadFile(cloudSQLConnectorStatePath(cfg))
	if err != nil {
		return cfg
	}
	var s cloudSQLConnectorState
	if json.Unmarshal(b, &s) == nil && s.Address != "" {
		cfg.CloudSQLConnector = s.Address
	}
	return cfg
}
