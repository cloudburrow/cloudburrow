package main

import (
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// An API whose port is OS-assigned (--port-storage 0, as CI starts the
// run shard) is served once `up` has bound it: the connectors read the
// live address, not the configured 0, which left a workflow's storage
// connector answering 403 SERVICE_DISABLED (main run 44740769561).
func TestLocalAPIsReadsTheBoundPorts(t *testing.T) {
	cfg := config.Default()
	cfg.Services = []config.Service{config.ServiceStorage, config.ServicePubSub, config.ServiceWorkflows}
	cfg.Endpoints.Storage, cfg.Endpoints.PubSub = 0, 0
	if got := localAPIs(cfg, nil)("storage"); got != "" {
		t.Fatalf("an unbound OS-assigned storage port resolved to %q", got)
	}
	var live map[string]string
	local := localAPIs(cfg, func() map[string]string { return live })
	if got := local("storage"); got != "" {
		t.Fatalf("before up bound it, storage resolved to %q", got)
	}
	live = map[string]string{"storage": "127.0.0.1:41234", "pubsub": "127.0.0.1:41235"}
	if got, want := local("storage"), "http://127.0.0.1:41234"; got != want {
		t.Errorf("storage = %q, want %q", got, want)
	}
	if got, want := local("pubsub"), "http://127.0.0.1:41235"; got != want {
		t.Errorf("pubsub = %q, want %q", got, want)
	}
	if got := local("bigquery"); got != "" {
		t.Errorf("bigquery, not served, resolved to %q", got)
	}
}
