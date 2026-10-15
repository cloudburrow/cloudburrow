package storagev2pb_test

import (
	"testing"

	_ "cloud.google.com/go/storage"

	"github.com/cloudburrow/cloudburrow/internal/service/storage/storagev2pb"
)

// TestLinksBesideClient checks the copy registers under its own names, so a
// binary that also links the Go storage client (the cloudburrow CLI's
// console) does not panic at init.
func TestLinksBesideClient(t *testing.T) {
	if got := (&storagev2pb.Bucket{}).ProtoReflect().Descriptor().FullName(); got != "cloudburrow.storage.v2.Bucket" {
		t.Fatalf("Bucket is %s", got)
	}
}
