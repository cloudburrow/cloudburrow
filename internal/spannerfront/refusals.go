package spannerfront

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/grpchttp"
)

// The admin methods neither the emulator nor the front serves are refused
// by the front UNIMPLEMENTED with the reason (#1461): the emulator refuses
// them with an empty message (measured: TestSpannerAdminRefusals). The
// backups, schedules, configurations, roles and operations it refused are
// served by the front since #1498, and AddSplitPoints and
// FetchCacheUpdate since #1523 (splitpoints.go, cacheupdate.go).
const databaseService = "/google.spanner.admin.database.v1.DatabaseAdmin/"

var refusedMethods = map[string]string{
	// Google documents it as "an internal API called by Spanner Graph
	// jobs. You should never need to call this API directly", and each
	// of its fields as "Internal field, do not use directly"
	// (google/spanner/admin/database/v1/spanner_database_admin.proto):
	// there is no documented behaviour to serve.
	databaseService + "InternalUpdateGraphOperation": "it is an internal API called by Spanner Graph jobs, which Google documents no behaviour of (\"You should never need to call this API directly\")",
}

// refusedREST are the same methods' REST bindings.
var refusedREST = []struct {
	method string
	path   *regexp.Regexp
	name   string
}{}

var restReasons = map[string]string{}

// Refusals answers the refused admin methods, over gRPC and REST.
type Refusals struct{ grpc *grpc.Server }

// NewRefusals returns the refusals.
func NewRefusals() *Refusals { return newRefusals() }

// newRefusals is NewRefusals with extra gRPC server options (tests). The
// server is served through ServeHTTP: grpchttp's, so Stop waits for every
// call's handler (#1804).
func newRefusals(opts ...grpc.ServerOption) *Refusals {
	return &Refusals{grpc: grpchttp.NewServer(append([]grpc.ServerOption{grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		m, _ := grpc.MethodFromServerStream(stream)
		return refusal(m[strings.LastIndex(m, "/")+1:], refusedMethods[m])
	})}, opts...)...)}
}

// Stop stops the gRPC server, once the HTTP server the refusals are served
// by has shut down, waiting for every call's handler to return, until ctx
// ends (#1804).
func (f *Refusals) Stop(ctx context.Context) error { return grpchttp.Stop(ctx, f.grpc) }

func refusal(method, why string) error {
	return status.Errorf(codes.Unimplemented, "%s is not served here: %s", method, why)
}

func restRefusal(r *http.Request) (string, bool) {
	for _, b := range refusedREST {
		if (b.method == "" || b.method == r.Method) && b.path.MatchString(r.URL.Path) {
			return b.name, true
		}
	}
	return "", false
}

// Claims reports whether r is a refused method.
func (f *Refusals) Claims(r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		_, ok := refusedMethods[r.URL.Path]
		return ok
	}
	_, ok := restRefusal(r)
	return ok
}

// ServeHTTP refuses a call Claims claimed.
func (f *Refusals) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		f.grpc.ServeHTTP(w, r)
		return
	}
	name, _ := restRefusal(r)
	writeError(w, refusal(name, restReasons[name]))
}
