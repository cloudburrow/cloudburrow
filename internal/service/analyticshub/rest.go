package analyticshub

import (
	"net/http"

	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The JSON API shares the gRPC port, as analyticshub.googleapis.com serves
// both. Its routes are the google.api.http bindings of
// google.cloud.bigquery.analyticshub.v1, read from the generated
// descriptors; its discovery document (revision 20261011) publishes no
// Locations or Operations methods, so none are added.

// Transcoder returns the JSON API's configuration, with nothing registered.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.cloud.bigquery.analyticshub.v1"}, SnakeCaseMasks: true}
}

// Routes returns every bound route.
func Routes() []rest.Route { return Transcoder().Routes() }

// NewRESTHandler returns the JSON API for what register adds.
func NewRESTHandler(register func(grpc.ServiceRegistrar)) http.Handler {
	t := Transcoder()
	register(t)
	return t
}
