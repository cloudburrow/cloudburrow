package pubsubfront

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/apienable"
)

// allDisabled disables every API in every project.
type allDisabled struct{}

func (allDisabled) Disabled(context.Context, string, string) (apienable.Denial, bool) {
	return apienable.Denial{Service: "pubsub.googleapis.com", Title: "Cloud Pub/Sub API", Number: 1}, true
}

type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s ctxStream) Context() context.Context { return s.ctx }

// TestStateCallsPassTheDisabledAPIGate: a project whose Pub/Sub API is
// disabled (as unlinking its billing does, #2109) made every `state save`
// fail, its archive truncated, since the save lists every known project's
// topics. CloudBurrow's own state calls (StateHeader) pass the gate, over
// gRPC and over HTTP; every other call is refused as before.
func TestStateCallsPassTheDisabledAPIGate(t *testing.T) {
	gate := skipForStateCalls(apienable.StreamInterceptor(allDisabled{}, "pubsub.googleapis.com"))
	info := &grpc.StreamServerInfo{FullMethod: "/google.pubsub.v1.Publisher/ListTopics"}
	ok := func(any, grpc.ServerStream) error { return nil }
	routed := metadata.Pairs("x-goog-request-params", "project=projects/off")

	plain := metadata.NewIncomingContext(context.Background(), routed)
	if err := gate(nil, ctxStream{ctx: plain}, info, ok); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a client's call in a disabled project = %v; want PERMISSION_DENIED", err)
	}
	state := metadata.NewIncomingContext(context.Background(), metadata.Join(routed, metadata.Pairs(StateHeader, "1")))
	if err := gate(nil, ctxStream{ctx: state}, info, ok); err != nil {
		t.Fatalf("a state call in a disabled project = %v; want it served", err)
	}

	h := apienable.Gate(allDisabled{}, "pubsub.googleapis.com",
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		apienable.Options{Skip: stateCallHTTP})
	for _, c := range []struct {
		state bool
		want  int
	}{{false, http.StatusForbidden}, {true, http.StatusOK}} {
		r := httptest.NewRequest(http.MethodGet, "/v1/projects/off/topics", nil)
		if c.state {
			r.Header.Set(StateHeader, "1")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("REST list in a disabled project, state call %v = %d; want %d", c.state, w.Code, c.want)
		}
	}
}
