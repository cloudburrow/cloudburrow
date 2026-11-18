package firestoreadmin

import (
	"context"
	"net/http"
	"testing"

	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/grpchttp/grpchttptest"
)

// TestStopWaitsForCallEnd: the Admin API's and the edition guard's Stop,
// after the call front's HTTP server has shut down, return only once every
// call they answered has ended for their stats handlers (#1804). gRPC
// through ServeHTTP ends a call on a goroutine of its own, after the answer
// is written.
func TestStopWaitsForCallEnd(t *testing.T) {
	type front interface {
		http.Handler
		Stop(context.Context) error
	}
	for name, c := range map[string]struct {
		make   func(opt grpc.ServerOption) front
		method string
	}{
		"admin": {func(o grpc.ServerOption) front { return newWith("127.0.0.1:1", o) },
			"/google.firestore.admin.v1.FirestoreAdmin/GetDatabase"},
		"edition guard": {func(o grpc.ServerOption) front { return New("127.0.0.1:1").editionGuard(o) },
			"/google.firestore.v1.Firestore/Commit"},
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
