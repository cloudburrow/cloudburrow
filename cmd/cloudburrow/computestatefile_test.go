package main

import (
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/remotecompute"
)

// The BYOC placement record and the Compute Engine service's state share the
// instance directory; one name for both made the next up read Compute
// Engine's state as a placement that "names no host".
func TestComputeStateAndPlacementRecordAreDifferentFiles(t *testing.T) {
	if remotecompute.RecordFile == computeStateFile {
		t.Fatalf("remotecompute.RecordFile and the Compute Engine state file are both %q", computeStateFile)
	}
}
