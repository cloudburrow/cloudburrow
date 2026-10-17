package spannerfront

import (
	"net/http"
	"regexp"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The admin methods neither the emulator nor the front serves are refused
// by the front UNIMPLEMENTED with the reason (#1461): the emulator refuses
// them with an empty message (measured: TestSpannerAdminRefusals). The
// backups, schedules, configurations, roles and operations it refused are
// served by the front since #1498.
const databaseService = "/google.spanner.admin.database.v1.DatabaseAdmin/"

var refusedMethods = map[string]string{
	databaseService + "AddSplitPoints":               "the emulator does not split databases, so split points have nothing to act on",
	databaseService + "InternalUpdateGraphOperation": "it is an internal method of Google's own services",
	// The data API's location-aware routing stream, which the emulator
	// answers UNIMPLEMENTED with an empty message on the first receive.
	"/google.spanner.v1.Spanner/FetchCacheUpdate": "location-aware routing has nowhere to route: the emulator is one server, with no splits or replicas to send a client to",
}

// refusedREST are the same methods' REST bindings.
var refusedREST = []struct {
	method string
	path   *regexp.Regexp
	name   string
}{
	{http.MethodPost, regexp.MustCompile(`^/v1/projects/[^/]+/instances/[^/]+/databases/[^/]+:addSplitPoints$`), "AddSplitPoints"},
}

var restReasons = map[string]string{
	"AddSplitPoints": refusedMethods[databaseService+"AddSplitPoints"],
}

// Refusals answers the refused admin methods, over gRPC and REST.
type Refusals struct{ grpc *grpc.Server }

// NewRefusals returns the refusals.
func NewRefusals() *Refusals {
	return &Refusals{grpc: grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		m, _ := grpc.MethodFromServerStream(stream)
		return refusal(m[strings.LastIndex(m, "/")+1:], refusedMethods[m])
	}))}
}

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
