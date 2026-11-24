package dataflow

import (
	"net/http"

	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Register serves the API's six services on g.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	dataflowpb.RegisterJobsV1Beta3Server(g, s)
	dataflowpb.RegisterMessagesV1Beta3Server(g, s)
	dataflowpb.RegisterMetricsV1Beta3Server(g, s)
	dataflowpb.RegisterSnapshotsV1Beta3Server(g, s)
	dataflowpb.RegisterTemplatesServiceServer(g, s)
	dataflowpb.RegisterFlexTemplatesServiceServer(g, s)
}

// Transcoder is the JSON API's configuration, with nothing registered.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.dataflow.v1beta3"}}
}

// NewRESTHandler is the JSON API over s, by the protos' google.api.http
// bindings (the v1b3 REST paths).
func NewRESTHandler(s *Server) http.Handler {
	t := Transcoder()
	s.Register(t)
	return t
}
