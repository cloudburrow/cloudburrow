package spannerfront

import (
	"context"
	"net/http"
	"testing"

	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/grpchttp/grpchttptest"
)

// TestStopWaitsForCallEnd: the instance front's and the refusals' Stop,
// after the call front's HTTP server has shut down, return only once every
// call they answered has ended for their stats handlers (#1804). gRPC
// through ServeHTTP ends a call on a goroutine of its own, after the answer
// is written. No emulator answers at the address the front is given, so
// its call fails, and ends.
func TestStopWaitsForCallEnd(t *testing.T) {
	type front interface {
		http.Handler
		Stop(context.Context) error
	}
	for name, c := range map[string]struct {
		make   func(t *testing.T, opt grpc.ServerOption) front
		method string
	}{
		"instances": {func(t *testing.T, o grpc.ServerOption) front {
			s, err := newWith("127.0.0.1:1", o)
			if err != nil {
				t.Fatal(err)
			}
			return s
		}, "/google.spanner.admin.instance.v1.InstanceAdmin/GetInstance"},
		"refusals": {func(_ *testing.T, o grpc.ServerOption) front { return newRefusals(o) },
			databaseService + "InternalUpdateGraphOperation"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var h grpchttptest.SlowEnd
			f := c.make(t, h.Option())
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
