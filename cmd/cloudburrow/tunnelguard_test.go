package main

import (
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// TestWhichTunnelsAreGuarded pins the Host check on the upstream tunnels
// (#725, ADR-0004) to the ports that serve HTTP/1.1, which a browser can
// send. A gRPC-only or database port has nothing a browser can reach, and
// storage's own server checks Host itself.
func TestWhichTunnelsAreGuarded(t *testing.T) {
	cfg := config.Default()
	cfg.Services = config.KnownServices()

	want := map[string]bool{
		"pubsub":           true, // REST and gRPC on 8085
		"firestore":        true, // REST and gRPC on 8080
		"datastore":        true, // REST and gRPC on 8081
		"bigquery":         true, // REST on 9050
		"bigquery-storage": false,
		"spanner":          false,
		"bigtable":         false,
		"storage":          false,
		"cloudsql":         false,
		"cloudsql-mysql":   false,
		"memorystore":      false,
	}
	seen := map[string]bool{}
	for _, s := range cfg.EnabledServices() {
		for _, tg := range forwardTargets(cfg, s) {
			name := tg.Label
			if name == "" {
				name = tg.Name
			}
			seen[name] = true
			w, known := want[name]
			if !known {
				t.Errorf("tunnel %s is not in this table: decide whether it carries HTTP a browser can send, and add it", name)
				continue
			}
			if tg.Guarded != w {
				t.Errorf("tunnel %s: Guarded = %v, want %v", name, tg.Guarded, w)
			}
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("no tunnel %s with every service enabled", name)
		}
	}
}
