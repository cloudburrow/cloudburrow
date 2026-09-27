package logging

import (
	"net/http"

	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The Cloud Logging JSON API shares the gRPC port (#591), as
// logging.googleapis.com serves both. Its routes are Google's own: the
// google.api.http bindings of every google.logging.v2 service, read from the
// generated descriptors. The shared transcoder calls the same Server gRPC
// does, so REST has no logic of its own. LoggingServiceV2 is registered;
// ConfigServiceV2 (sinks, exclusions, buckets, views, links, settings) and
// MetricsServiceV2 are not, so their bound paths answer 501 UNIMPLEMENTED, as
// TailLogEntries, a stream, does. A path Google does not bind answers 404.
// logging_v2.yaml's Locations and Operations mixins are not bound.

// Transcoder returns the Cloud Logging JSON API's configuration, with nothing
// registered: register the same server with it as with gRPC.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.logging.v2"}}
}

// Routes returns every bound route.
func Routes() []rest.Route { return Transcoder().Routes() }

// NewRESTHandler returns the JSON API for what register adds, which is called
// with the transcoder as it is with the gRPC server, so both transports serve
// the same set.
func NewRESTHandler(register func(grpc.ServiceRegistrar)) http.Handler {
	t := Transcoder()
	register(t)
	return t
}
