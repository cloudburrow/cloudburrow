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

// metricsServices names every enabled service for the registry. Which of them
// are measured is not decided here: a service is measured once a call
// observer counting into the registry is built for it (callEvents,
// requestEvents, storageEvents), and every other one is reported unmeasured.
func metricsServices(cfg config.Config) []string {
	enabled := cfg.EnabledServices()
	out := make([]string, 0, len(enabled))
	for _, s := range enabled {
		out = append(out, string(s))
	}
	return out
}
