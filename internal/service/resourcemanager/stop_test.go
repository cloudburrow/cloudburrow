package resourcemanager

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/stats"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// slowEnd is a stats handler slow to see a call end, as a trace exporter
// can be; it notes when it has.
type slowEnd struct{ ended *atomic.Bool }

func (h slowEnd) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context   { return ctx }
func (h slowEnd) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context { return ctx }
func (h slowEnd) HandleConn(context.Context, stats.ConnStats)                       {}
func (h slowEnd) HandleRPC(_ context.Context, s stats.RPCStats) {
	if _, ok := s.(*stats.End); ok {
		time.Sleep(50 * time.Millisecond)
		h.ended.Store(true)
	}
}

// TestStopWaitsForCallEnd: Stop returns only once every call has ended for
// the server's stats handlers, where tracing ends the call's span (#1159).
// gRPC through ServeHTTP ends a call on a goroutine of its own, after the
// answer is written; main CI run 44296179658 lost a Resource Manager span
// that ended after Stop had returned.
func TestStopWaitsForCallEnd(t *testing.T) {
	ctx := context.Background()
	var ended atomic.Bool
	rm := NewServer("127.0.0.1:0", New(store.NewMemory()))
	rm.ServerOptions(grpc.StatsHandler(slowEnd{&ended}))
	if err := rm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	rc, err := resourcemanager.NewProjectsClient(ctx, option.WithEndpoint(rm.Addr()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	_, _ = rc.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/absent"})
	_ = rm.Stop(ctx)
	if !ended.Load() {
		t.Fatal("Stop returned before the call's stats End")
	}
}
