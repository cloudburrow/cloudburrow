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
// UNIMPLEMENTED. The transcoder serves unary calls; the tail, a
// bidirectional stream, is served over gRPC (#1215) and, as a streamed JSON
// array, at its JSON binding by tailjson.go (#1410). A path Google does not bind answers 404.
// Of logging_v2.yaml's mixins, Operations' bindings are, for CopyLogEntries'
// operations: projects' (#1394), and organizations', folders' and billing
// accounts' (#1411). Locations is not bound.

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
//
// POST /v2/entries:tail is served beside the transcoder (tailjson.go).
func NewRESTHandler(register func(grpc.ServiceRegistrar)) http.Handler {
	t := Transcoder()
	rec := &tailRecorder{next: t}
	register(rec)
	if rec.srv == nil {
		return t
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == tailJSONPath {
			rec.srv.serveTailJSON(w, r)
			return
		}
		t.ServeHTTP(w, r)
	})
}

// operationsMixins are logging_v2.yaml's Operations bindings, one set per
// parent collection.
var operationsMixins = func() []*annotations.HttpRule {
	var out []*annotations.HttpRule
	for _, coll := range []string{"projects", "organizations", "folders", "billingAccounts"} {
		out = append(out,
			&annotations.HttpRule{Selector: "google.longrunning.Operations.GetOperation",
				Pattern: &annotations.HttpRule_Get{Get: "/v2/{name=" + coll + "/*/locations/*/operations/*}"}},
			&annotations.HttpRule{Selector: "google.longrunning.Operations.ListOperations",
				Pattern: &annotations.HttpRule_Get{Get: "/v2/{name=" + coll + "/*/locations/*}/operations"}},
			&annotations.HttpRule{Selector: "google.longrunning.Operations.CancelOperation",
				Pattern: &annotations.HttpRule_Post{Post: "/v2/{name=" + coll + "/*/locations/*/operations/*}:cancel"}, Body: "*"})
	}
	return out
}()
