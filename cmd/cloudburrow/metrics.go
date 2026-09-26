package main

import (
	"net/http"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/metrics"
)

// metricsHandler serves the request counters in the Prometheus text format.
// It is mounted on the control port only, which is loopback-only whatever
// the bind address, like the admin API beside it.
func metricsHandler(reg *metrics.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = reg.Write(w)
	})
}

// unmeasuredServices are the enabled services whose calls CloudBurrow never
// sees: everything but the ones it serves itself.
func unmeasuredServices(cfg config.Config) []string {
	var out []string
	for _, s := range cfg.EnabledServices() {
		switch s {
		case config.ServiceTasks, config.ServiceRun, config.ServiceSecrets, config.ServiceKMS:
		case config.ServiceStorage:
			// The storage server reports every request it serves (#513).
		default:
			out = append(out, string(s))
		}
	}
	return out
}
