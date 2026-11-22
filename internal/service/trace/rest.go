package trace

import (
	"net/http"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Transcoder is the JSON API's configuration (the google.api.http
// bindings of google.devtools.cloudtrace.v1 and v2), with nothing
// registered.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.devtools.cloudtrace.v1", "google.devtools.cloudtrace.v2"}}
}

// Routes is every bound route.
func Routes() []rest.Route { return Transcoder().Routes() }

// NewRESTHandler is the JSON API over s, on the gRPC port as
// cloudtrace.googleapis.com serves both, and OTLP/HTTP's POST /v1/traces,
// as telemetry.googleapis.com serves it.
func NewRESTHandler(s *Server) http.Handler {
	t := Transcoder()
	s.RegisterCloudTrace(t)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.OTLPHandler(w, r) {
			return
		}
		t.ServeHTTP(w, r)
	})
}
