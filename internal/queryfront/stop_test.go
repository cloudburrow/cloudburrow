package queryfront

import (
	"context"
	"net/http"
	"testing"

	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/grpchttp/grpchttptest"
)

// TestStopWaitsForCallEnd: each front's Stop, after the call front's HTTP
// server has shut down, returns only once every call it answered has ended
// for its stats handlers (#1804). gRPC through ServeHTTP ends a call on a
// goroutine of its own, after the answer is written. No emulator answers
// at the address the fronts are given, so each call fails, and ends.
func TestStopWaitsForCallEnd(t *testing.T) {
	type front interface {
		http.Handler
		Stop(context.Context) error
	}
	for name, c := range map[string]struct {
		make   func(opt grpc.ServerOption) front
		method string
	}{
		"firestore": {func(o grpc.ServerOption) front { return newFirestoreWith("127.0.0.1:1", o) }, firestorePartition},
		"datastore": {func(o grpc.ServerOption) front { return newDatastoreWith("127.0.0.1:1", o) }, datastoreAggregation},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var h grpchttptest.SlowEnd
			f := c.make(h.Option())
			hs, addr, err := grpchttptest.Serve(f)
			if err != nil {
				t.Fatal(err)
			}
			if err := grpchttptest.Call(ctx, addr, c.method); err != nil {
				t.Fatal(err)
			}
			_ = hs.Shutdown(ctx)
			_ = f.Stop(ctx)
			if !h.Ended() {
				t.Fatalf("Stop returned before the call's stats End (%d calls)", h.Calls())
			}
		})
	}
}
