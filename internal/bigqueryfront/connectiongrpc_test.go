package bigqueryfront

import (
	"net/http"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestConnectionStatus (#1766): a REST error of the Connection API is the
// gRPC status of its HTTP code, its message kept; a stale policy's 409 is
// ABORTED.
func TestConnectionStatus(t *testing.T) {
	for code, want := range map[int]codes.Code{http.StatusBadRequest: codes.InvalidArgument, http.StatusNotFound: codes.NotFound,
		http.StatusConflict: codes.AlreadyExists, http.StatusNotImplemented: codes.Unimplemented, 418: codes.Unknown} {
		err := connectionStatus(code, []byte(`{"error":{"code":1,"message":"why"}}`))
		if status.Code(err) != want || status.Convert(err).Message() != "why" {
			t.Errorf("%d: %v; want %v", code, err, want)
		}
	}
	if err := connectionStatus(409, []byte(`{"error":{"message":"The policy's etag does not match"}}`)); status.Code(err) != codes.Aborted {
		t.Errorf("stale etag: %v", err)
	}
}
