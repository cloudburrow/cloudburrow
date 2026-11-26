package dataflow

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"

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
	return gunzipRequests(t)
}

// maxInflatedBody bounds a gzip request body once inflated.
const maxInflatedBody = 64 << 20

// gunzipRequests inflates a request body sent with Content-Encoding:
// gzip, as Google's front ends accept it: the Java SDK's DataflowRunner
// (google-api-client) gzips every request body it sends.
func gunzipRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || !strings.EqualFold(strings.TrimSpace(r.Header.Get("Content-Encoding")), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, `{"error":{"code":400,"message":"the request body is not valid gzip","status":"INVALID_ARGUMENT"}}`, http.StatusBadRequest)
			return
		}
		b, err := io.ReadAll(io.LimitReader(zr, maxInflatedBody+1))
		_ = zr.Close()
		if err != nil || len(b) > maxInflatedBody {
			http.Error(w, `{"error":{"code":400,"message":"the request body is not valid gzip or is too large","status":"INVALID_ARGUMENT"}}`, http.StatusBadRequest)
			return
		}
		r2 := r.Clone(r.Context())
		r2.Header.Del("Content-Encoding")
		r2.Body = io.NopCloser(strings.NewReader(string(b)))
		r2.ContentLength = int64(len(b))
		next.ServeHTTP(w, r2)
	})
}
