package monitoring

import (
	"net/http"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Transcoder is the JSON API's configuration (the google.api.http bindings
// of google.monitoring.v3 and google.monitoring.dashboard.v1), with nothing registered.
func Transcoder() *rest.Transcoder {
	// The overlay serves the uptime check fields the protos lack (#2146).
	return &rest.Transcoder{Packages: []string{"google.monitoring.v3", "google.monitoring.dashboard.v1"}, Overlay: monitoringOverlay}
}

// Routes is every bound route.
func Routes() []rest.Route { return Transcoder().Routes() }

// NewRESTHandler is the JSON API over s, on the gRPC port as
// monitoring.googleapis.com serves both.
func NewRESTHandler(s *Server) http.Handler {
	t := Transcoder()
	s.Register(t)
	// And dashboards' opsAnalyticsQuery, which the protos lack (#2031).
	return withDashboardSQL(s, withPrometheusAPI(s, t))
}
