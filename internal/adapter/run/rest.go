package run

import (
	"net/http"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The Cloud Run v2 JSON API shares the adapter's gRPC port (#591), as
// run.googleapis.com serves both. Its routes are Google's own: the
// google.api.http bindings of every google.cloud.run.v2 service, read from
// the generated descriptors, plus the Operations mixin run_v2.yaml binds.
// The shared transcoder calls the same servers gRPC does, so REST has no
// logic of its own. Services, Revisions, Jobs, Executions and Operations are
// registered; a bound path of Builds, Instances, Tasks or WorkerPools, or an
// Operations method the adapter does not implement, answers 501
// UNIMPLEMENTED, and a path Google does not bind answers 404.

// mixins are the bindings run_v2.yaml adds (googleapis
// run_v2.yaml@5b03e5ec:29-39), which no runpb descriptor carries. It lists
// google.cloud.location.Locations among its APIs but binds no path for it.
var mixins = []*annotations.HttpRule{
	{Selector: "google.longrunning.Operations.DeleteOperation",
		Pattern: &annotations.HttpRule_Delete{Delete: "/v2/{name=projects/*/locations/*/operations/*}"}},
	{Selector: "google.longrunning.Operations.GetOperation",
		Pattern: &annotations.HttpRule_Get{Get: "/v2/{name=projects/*/locations/*/operations/*}"}},
	{Selector: "google.longrunning.Operations.ListOperations",
		Pattern: &annotations.HttpRule_Get{Get: "/v2/{name=projects/*/locations/*}/operations"}},
	{Selector: "google.longrunning.Operations.WaitOperation",
		Pattern: &annotations.HttpRule_Post{Post: "/v2/{name=projects/*/locations/*/operations/*}:wait"}, Body: "*"},
}

// Transcoder returns the Cloud Run v2 JSON API's configuration, with nothing
// registered: register the same servers with it as with the gRPC server.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{
		Packages: []string{"google.cloud.run.v2"},
		Mixins:   mixins,
	}
}

// Routes returns every bound route: the proto bindings, then the mixins.
func Routes() []rest.Route { return Transcoder().Routes() }

// NewRESTHandler returns the JSON API for the servers register adds, which
// is called with the transcoder as it is with the gRPC server, so both
// transports serve the same set.
func NewRESTHandler(register func(grpc.ServiceRegistrar)) http.Handler {
	t := Transcoder()
	register(t)
	return t
}
