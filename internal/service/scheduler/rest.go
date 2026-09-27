package scheduler

import (
	"net/http"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The Cloud Scheduler JSON API shares the gRPC port (#591), as
// cloudscheduler.googleapis.com serves both. Its routes are Google's own:
// the google.api.http bindings of google.cloud.scheduler.v1, read from the
// generated descriptors, plus the Locations bindings cloudscheduler_v1.yaml
// adds (gcloud's generated client has the same two paths). The shared
// transcoder calls the same GRPCServer gRPC does, so REST has no logic of its
// own: a bound method the server does not implement, the Locations mixin
// among them, answers 501 UNIMPLEMENTED, and a path Google does not bind
// answers 404.

// mixins are the Locations bindings, which no schedulerpb descriptor carries.
var mixins = []*annotations.HttpRule{
	{Selector: "google.cloud.location.Locations.GetLocation",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*}"}},
	{Selector: "google.cloud.location.Locations.ListLocations",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*}/locations"}},
}

// Transcoder returns the Cloud Scheduler JSON API's configuration, with
// nothing registered: register the same server with it as with gRPC.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.cloud.scheduler.v1"}, Mixins: mixins}
}

// Routes returns every bound route: the proto bindings, then the mixins.
func Routes() []rest.Route { return Transcoder().Routes() }

// NewRESTHandler returns the JSON API for what register adds, which is called
// with the transcoder as it is with the gRPC server, so both transports serve
// the same set.
func NewRESTHandler(register func(grpc.ServiceRegistrar)) http.Handler {
	t := Transcoder()
	register(t)
	return t
}
