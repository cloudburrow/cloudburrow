package redisadmin

import (
	"net/http"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The JSON API shares the gRPC port, as redis.googleapis.com serves both:
// the google.api.http bindings of google.cloud.redis.v1, read from the
// generated descriptors, transcoded to the same Server gRPC calls. A bound
// method the server does not implement answers 501 UNIMPLEMENTED.

// mixins are the Operations bindings redis_v1.yaml adds, which no redispb
// descriptor carries, and the Locations mixin (#1386).
var mixins = []*annotations.HttpRule{
	{Selector: "google.cloud.location.Locations.GetLocation",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*}"}},
	{Selector: "google.cloud.location.Locations.ListLocations",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*}/locations"}},
	{Selector: "google.longrunning.Operations.CancelOperation",
		Pattern: &annotations.HttpRule_Post{Post: "/v1/{name=projects/*/locations/*/operations/*}:cancel"}},
	{Selector: "google.longrunning.Operations.DeleteOperation",
		Pattern: &annotations.HttpRule_Delete{Delete: "/v1/{name=projects/*/locations/*/operations/*}"}},
	{Selector: "google.longrunning.Operations.GetOperation",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*/operations/*}"}},
	{Selector: "google.longrunning.Operations.ListOperations",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*}/operations"}},
}

// Transcoder returns the JSON API's configuration, with nothing registered.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.cloud.redis.v1"}, Mixins: mixins, SnakeCaseMasks: true}
}

// NewRESTHandler returns the JSON API for what register adds.
func NewRESTHandler(register func(grpc.ServiceRegistrar)) http.Handler {
	t := Transcoder()
	register(t)
	return t
}
