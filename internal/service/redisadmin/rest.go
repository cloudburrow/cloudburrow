package redisadmin

import (
	"net/http"

	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The JSON API shares the gRPC port, as redis.googleapis.com serves both:
// the google.api.http bindings of google.cloud.redis.v1, read from the
// generated descriptors, transcoded to the same Server gRPC calls. A bound
// method the server does not implement answers 501 UNIMPLEMENTED.

// Transcoder returns the JSON API's configuration, with nothing registered.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.cloud.redis.v1"}}
}

// NewRESTHandler returns the JSON API for what register adds.
func NewRESTHandler(register func(grpc.ServiceRegistrar)) http.Handler {
	t := Transcoder()
	register(t)
	return t
}
