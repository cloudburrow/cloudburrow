package analyticshub

import (
	"context"
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

// NewRESTHandler returns the JSON API for what register adds; each call
// passes through check, in order, as its gRPC call does (IAM enforcement,
// #1989).
func NewRESTHandler(register func(grpc.ServiceRegistrar), check ...grpc.UnaryServerInterceptor) http.Handler {
	t := Transcoder()
	if len(check) > 0 {
		t.Interceptor = chain(check)
	}
	register(t)
	return t
}

// chain runs is in order, the first outermost.
func chain(is []grpc.UnaryServerInterceptor) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		next := handler
		for i := len(is) - 1; i >= 0; i-- {
			in, h := is[i], next
			next = func(ctx context.Context, req any) (any, error) { return in(ctx, req, info, h) }
		}
		return next(ctx, req)
	}
}
