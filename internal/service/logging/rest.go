package logging

import (
	"net/http"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The Cloud Logging JSON API shares the gRPC port (#591), as
// logging.googleapis.com serves both. Its routes are Google's own: the
// google.api.http bindings of every google.logging.v2 service, read from the
// generated descriptors. The shared transcoder calls the same Server gRPC
// does, so REST has no logic of its own. LoggingServiceV2, MetricsServiceV2
// and ConfigServiceV2 are registered (#1226); the ConfigServiceV2 methods
// Admin does not implement (the Async bucket calls, links, settings) answer 501
// UNIMPLEMENTED, as TailLogEntries does: the transcoder serves unary calls,
// and the tail, a bidirectional stream, is served over gRPC only (#1215). A path Google does not bind answers 404.
// Of logging_v2.yaml's mixins, Operations' project bindings are (#1394), for
// CopyLogEntries' operations; Locations is not bound.

// Transcoder returns the Cloud Logging JSON API's configuration, with nothing
// registered: register the same server with it as with gRPC.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.logging.v2"}, Mixins: operationsMixins}
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

// operationsMixins are logging_v2.yaml's Operations bindings for projects.
var operationsMixins = []*annotations.HttpRule{
	{Selector: "google.longrunning.Operations.GetOperation",
		Pattern: &annotations.HttpRule_Get{Get: "/v2/{name=projects/*/locations/*/operations/*}"}},
	{Selector: "google.longrunning.Operations.ListOperations",
		Pattern: &annotations.HttpRule_Get{Get: "/v2/{name=projects/*/locations/*}/operations"}},
	{Selector: "google.longrunning.Operations.CancelOperation",
		Pattern: &annotations.HttpRule_Post{Post: "/v2/{name=projects/*/locations/*/operations/*}:cancel"}, Body: "*"},
}
