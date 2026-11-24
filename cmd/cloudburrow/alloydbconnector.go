package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/service/alloydb"
)

// The AlloyDB connectors' listener address, kept in the instance's state
// directory for `env`, a separate process (#2191), as Cloud SQL's is
// (#1571).
type alloyDBConnectorState struct {
	Address string `json:"address"`
}

func alloyDBConnectorStatePath(cfg config.Config) string {
	return filepath.Join(cfg.InstanceDir(), "alloydb-connector.json")
}

// recordAlloyDBConnector records addr when it is not port 5433, and
// removes the record when it is (or there is no listener).
func recordAlloyDBConnector(cfg config.Config, addr string) {
	path := alloyDBConnectorStatePath(cfg)
	if _, port, err := net.SplitHostPort(addr); err != nil || port == strconv.Itoa(alloydb.ConnectorPort) {
		_ = os.Remove(path)
		return
	}
	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err != nil {
		return
	}
	b, _ := json.MarshalIndent(alloyDBConnectorState{Address: addr}, "", "  ")
	_ = os.WriteFile(path, append(b, '\n'), 0o600)
}

// recordedAlloyDBConnector is the moved listener's address, or "".
func recordedAlloyDBConnector(cfg config.Config) string {
	b, err := os.ReadFile(alloyDBConnectorStatePath(cfg))
	if err != nil {
		return ""
	}
	var s alloyDBConnectorState
	if json.Unmarshal(b, &s) != nil {
		return ""
	}
	return s.Address
}
