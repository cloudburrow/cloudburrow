package filestore

import (
	"context"
	"path/filepath"
	"testing"

	betapb "github.com/cloudburrow/cloudburrow/internal/service/filestore/filestorev1beta1pb"
	"google.golang.org/grpc/codes"
)

// A multishare instance has no file share at create: creating one must not
// read file_shares[0] (the Backup and DR source check once did, and the
// panic ended the instance), and a Backup and DR backup of one is refused
// like a backup.
func TestMultishareCreateAndBackupDR(t *testing.T) {
	ctx := context.Background()
	s, err := NewServer(filepath.Join(t.TempDir(), "filestore.json"), Options{Backend: &MemoryBackend{}})
	if err != nil {
		t.Fatal(err)
	}
	region := "projects/p/locations/us-central1"
	op, err := s.Beta().CreateInstance(ctx, &betapb.CreateInstanceRequest{Parent: region, InstanceId: "ms",
		Instance: &betapb.Instance{Tier: betapb.Instance_ENTERPRISE, MultiShareEnabled: true, CapacityGb: 1024, MaxShareCount: 10,
			Networks: []*betapb.NetworkConfig{{Network: "default"}}}})
	wait(t, s, op, err)
	_, err = s.BackupDRBackup(ctx, region+"/instances/ms", "run1")
	wantCode(t, "Backup and DR backup of a multishare instance", err, codes.FailedPrecondition)
}
