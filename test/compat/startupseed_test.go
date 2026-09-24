//go:build compat

package compat

import (
	"io"
	"os"
	"testing"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
)

// TestTheStartupSeedFileWasApplied.
//
// The compat instance is started with --seed-file test/compat/testdata/seed.json
// (#286). Its queue and bucket exist, read through the official clients. The
// file names project ci-local, the default project of the instance CI starts
// as "ci". CI also checks, in up.log, that the seed was applied before the
// ready line.
func TestTheStartupSeedFileWasApplied(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCLI) == "" {
		t.Skipf("%s is not set: this runs against the CI instance started with the seed file", EnvCLI)
	}
	tc := tasksClient(t, h)
	if _, err := tc.GetQueue(h.Context(), &taskspb.GetQueueRequest{
		Name: "projects/ci-local/locations/us-central1/queues/seeded-at-startup"}); err != nil {
		t.Errorf("the seeded queue is missing: %v", err)
	}
	r, err := storageClient(t, h).Bucket("cloudburrow-seeded-at-startup").Object("hello.txt").NewReader(h.Context())
	if err != nil {
		t.Fatalf("the seeded object is missing: %v", err)
	}
	b, _ := io.ReadAll(r)
	_ = r.Close()
	if string(b) != "seeded by up --seed-file" {
		t.Errorf("the seeded object reads %q", b)
	}
}
