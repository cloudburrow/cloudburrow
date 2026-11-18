package vertexai

import (
	"context"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/grpchttp/grpchttptest"
)

// TestStopWaitsForCallEnd: Stop returns only once every PredictionService
// call has ended for the server's stats handlers (#1804). gRPC through
// ServeHTTP ends a call on a goroutine of its own, after the answer is
// written.
func TestStopWaitsForCallEnd(t *testing.T) {
	ctx := context.Background()
	var h grpchttptest.SlowEnd
	s := NewServerOn("127.0.0.1:0", nil)
	s.lis.options = append(s.lis.options, h.Option())
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := grpchttptest.Call(ctx, s.Addr(), "/google.cloud.aiplatform.v1.PredictionService/Predict"); err != nil {
		t.Fatal(err)
	}
	_ = s.Stop(ctx)
	if !h.Ended() {
		t.Fatalf("Stop returned before the call's stats End (%d calls)", h.Calls())
	}
}
