package storage

import (
	"context"
	"testing"

	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/grpchttp/grpchttptest"
)

// TestStopWaitsForCallEnd: Stop, after the HTTP server's Shutdown, returns
// only once every gRPC call has ended for the server's stats handlers
// (#1804). gRPC through ServeHTTP ends a call on a goroutine of its own,
// after the answer is written, so Shutdown alone can return first, and
// storage-server would close the stores under it.
func TestStopWaitsForCallEnd(t *testing.T) {
	ctx := context.Background()
	var h grpchttptest.SlowEnd
	s, err := NewServer(Options{grpcOptions: []grpc.ServerOption{h.Option()}})
	if err != nil {
		t.Fatal(err)
	}
	hs, addr, err := grpchttptest.Serve(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := grpchttptest.Call(ctx, addr, "/google.storage.v2.Storage/GetBucket"); err != nil {
		t.Fatal(err)
	}
	_ = hs.Shutdown(ctx)
	_ = s.Stop(ctx)
	if !h.Ended() {
		t.Fatalf("Stop returned before the call's stats End (%d calls)", h.Calls())
	}
}
