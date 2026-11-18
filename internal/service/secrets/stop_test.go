package secrets

import (
	"context"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/grpchttp/grpchttptest"
)

// TestStopWaitsForCallEnd: Stop returns only once every call has ended for
// the server's stats handlers, where tracing ends the call's span (#1804).
// gRPC through ServeHTTP ends a call on a goroutine of its own, after the
// answer is written.
func TestStopWaitsForCallEnd(t *testing.T) {
	ctx := context.Background()
	var h grpchttptest.SlowEnd
	srv := NewServer("127.0.0.1:0", newTestStore(t))
	srv.ServerOptions(h.Option())
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := grpchttptest.Call(ctx, srv.Addr(), "/google.cloud.secretmanager.v1.SecretManagerService/GetSecret"); err != nil {
		t.Fatal(err)
	}
	_ = srv.Stop(ctx)
	if !h.Ended() {
		t.Fatalf("Stop returned before the call's stats End (%d calls)", h.Calls())
	}
}
