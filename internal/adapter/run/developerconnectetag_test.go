package run

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// developer_connect.proto's etag rule (#1595): no etag, or the current
// one, proceeds; any other is ABORTED.
func TestDeveloperConnectEtag(t *testing.T) {
	for _, tc := range []struct {
		sent, current string
		want          codes.Code
	}{
		{"", "", codes.OK},
		{"", "e1", codes.OK},
		{"e1", "e1", codes.OK},
		{"stale", "", codes.Aborted},
		{"stale", "e1", codes.Aborted},
	} {
		if got := status.Code(dcEtagMatches("connection", "projects/p/locations/l/connections/c", tc.sent, tc.current)); got != tc.want {
			t.Errorf("dcEtagMatches(%q, %q) = %v; want %v", tc.sent, tc.current, got, tc.want)
		}
	}
}
