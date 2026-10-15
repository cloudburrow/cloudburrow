package netfwd

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
)

// A guarded tunnel tells its observer about each request it carried, gRPC
// and REST, with the method, the code it answered and the resource the
// routing headers name (#1267); the bytes still pass through unread.
func TestGuardObservesEachRequest(t *testing.T) {
	t.Parallel()
	emu := startFakeEmulator(t)
	var mu sync.Mutex
	var calls []Call
	g, err := startObservedGuard("127.0.0.1:0", emu.addr, t.Logf, func(c Call) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, c)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.close)
	addr := g.ln.Addr().String()
	cc := grpcConn(t, addr)
	ctx := metadata.AppendToOutgoingContext(context.Background(),
		"x-goog-request-params", "database=projects%2Fp%2Fdatabases%2F%28default%29",
		"traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	if _, err := pubsubpb.NewPublisherClient(cc).GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: "projects/p/topics/t"}); err != nil {
		t.Fatal(err)
	}
	_, _ = pubsubpb.NewPublisherClient(cc).GetTopic(context.Background(), &pubsubpb.GetTopicRequest{Topic: "projects/p/topics/missing"})
	resp, err := http.Get("http://" + addr + "/v1/projects/p/topics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("observed %+v, want 3 calls", calls)
	}
	ok, missing, rest := calls[0], calls[1], calls[2]
	if !ok.GRPC || ok.Method != "/google.pubsub.v1.Publisher/GetTopic" || ok.Code != int(codes.OK) ||
		ok.Resource != "projects/p/databases/(default)" || ok.Duration <= 0 ||
		ok.TraceParent != "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" {
		t.Errorf("GetTopic observed as %+v", ok)
	}
	// NotFound is a Trailers-Only answer: grpc-status in the headers.
	if missing.Code != int(codes.NotFound) {
		t.Errorf("a missing topic observed as %+v, want NotFound", missing)
	}
	if rest.GRPC || rest.Method != "GET /v1/projects/p/topics" || rest.Code != http.StatusOK || rest.Resource != "projects/p/topics" {
		t.Errorf("REST GET observed as %+v", rest)
	}
}

func TestResourceOf(t *testing.T) {
	for _, tc := range []struct {
		h    http.Header
		want string
	}{
		{http.Header{"Google-Cloud-Resource-Prefix": {"projects/p/databases/(default)"}}, "projects/p/databases/(default)"},
		{http.Header{"X-Goog-Request-Params": {"project_id=p"}}, "projects/p"},
		{http.Header{"X-Goog-Request-Params": {"database_id=x"}}, ""},
		{http.Header{}, ""},
	} {
		if got := resourceOf(tc.h); got != tc.want {
			t.Errorf("resourceOf(%v) = %q, want %q", tc.h, got, tc.want)
		}
	}
}
