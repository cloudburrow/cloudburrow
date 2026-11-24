package filestore

import (
	"context"
	"path/filepath"
	"testing"

	"cloud.google.com/go/filestore/apiv1/filestorepb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func wait(t *testing.T, s *Server, op *longrunningpb.Operation, err error) *longrunningpb.Operation {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	s.WaitDone(context.Background(), op.GetName())
	done, err := s.Operation(op.GetName())
	if err != nil {
		t.Fatal(err)
	}
	if done.GetError() != nil {
		t.Fatalf("%s: %v", op.GetName(), done.GetError())
	}
	return done
}

func wantCode(t *testing.T, what string, err error, c codes.Code) {
	t.Helper()
	if status.Code(err) != c {
		t.Errorf("%s: %v, want %v", what, err, c)
	}
}

// TestSnapshotRevertBackupRestoreAndPersistence: the data operations reach
// the backend in order, and the records survive a restart.
func TestSnapshotRevertBackupRestoreAndPersistence(t *testing.T) {
	ctx := context.Background()
	be := &MemoryBackend{}
	state := filepath.Join(t.TempDir(), "filestore.json")
	s, err := NewServer(state, Options{Backend: be})
	if err != nil {
		t.Fatal(err)
	}
	zone := "projects/p/locations/us-central1-a"
	in := &filestorepb.Instance{Tier: filestorepb.Instance_ZONAL, FileShares: []*filestorepb.FileShareConfig{{Name: "vol1", CapacityGb: 1024}},
		Networks: []*filestorepb.NetworkConfig{{Network: "default"}}}
	op, err := s.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: zone, InstanceId: "a", Instance: in})
	wait(t, s, op, err)
	name := zone + "/instances/a"
	key := s.ServerKey(name)
	if key == "" || !be.Running(key) {
		t.Fatalf("no server for %s", name)
	}
	be.Write(key, "vol1", "one")
	op, err = s.CreateSnapshot(ctx, &filestorepb.CreateSnapshotRequest{Parent: name, SnapshotId: "s1", Snapshot: &filestorepb.Snapshot{}})
	wait(t, s, op, err)
	be.Write(key, "vol1", "two")
	op, err = s.CreateBackup(ctx, &filestorepb.CreateBackupRequest{Parent: "projects/p/locations/us-central1", BackupId: "b1",
		Backup: &filestorepb.Backup{SourceInstance: name, SourceFileShare: "vol1"}})
	wait(t, s, op, err)
	op, err = s.RevertInstance(ctx, &filestorepb.RevertInstanceRequest{Name: name, TargetSnapshotId: "s1"})
	wait(t, s, op, err)
	if got := be.Read(key, "vol1"); got != "one" {
		t.Fatalf("after revert: %q", got)
	}
	in2 := &filestorepb.Instance{Tier: filestorepb.Instance_ZONAL, Networks: []*filestorepb.NetworkConfig{{Network: "default"}},
		FileShares: []*filestorepb.FileShareConfig{{Name: "vol1", CapacityGb: 1024,
			Source: &filestorepb.FileShareConfig_SourceBackup{SourceBackup: "projects/p/locations/us-central1/backups/b1"}}}}
	op, err = s.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: zone, InstanceId: "b", Instance: in2})
	wait(t, s, op, err)
	if got := be.Read(s.ServerKey(zone+"/instances/b"), "vol1"); got != "two" {
		t.Fatalf("restored instance: %q", got)
	}
	// Shrinking a Zonal instance within its band is allowed; leaving it is not.
	_, err = s.UpdateInstance(ctx, &filestorepb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"file_shares"}},
		Instance: &filestorepb.Instance{Name: name, FileShares: []*filestorepb.FileShareConfig{{Name: "vol1", CapacityGb: 10240}}}})
	wantCode(t, "a resize out of the band", err, codes.InvalidArgument)
	_, err = s.DeleteInstance(ctx, &filestorepb.DeleteInstanceRequest{Name: name})
	wantCode(t, "deleting an instance with snapshots", err, codes.FailedPrecondition)
	s.Close()

	s2, err := NewServer(state, Options{Backend: be})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got, err := s2.GetSnapshot(ctx, &filestorepb.GetSnapshotRequest{Name: name + "/snapshots/s1"}); err != nil || got.GetState() != filestorepb.Snapshot_READY {
		t.Fatalf("snapshot after restart: %v, %v", got, err)
	}
	if got, err := s2.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: name}); err != nil || len(got.GetNetworks()[0].GetIpAddresses()) != 1 {
		t.Fatalf("instance after restart: %v, %v", got, err)
	}
	op, err = s2.DeleteInstance(ctx, &filestorepb.DeleteInstanceRequest{Name: name, Force: true})
	wait(t, s2, op, err)
	if be.Running(key) {
		t.Error("the deleted instance's server still runs")
	}
}

// TestTierRules: the documented capacity and protocol rules.
func TestTierRules(t *testing.T) {
	for _, c := range []struct {
		tier filestorepb.Instance_Tier
		gb   int64
		ok   bool
	}{
		{filestorepb.Instance_BASIC_HDD, 1024, true}, {filestorepb.Instance_BASIC_HDD, 1023, false}, {filestorepb.Instance_BASIC_HDD, 65434, true},
		{filestorepb.Instance_BASIC_HDD, 65435, false}, {filestorepb.Instance_BASIC_SSD, 2559, false}, {filestorepb.Instance_BASIC_SSD, 2560, true},
		{filestorepb.Instance_ZONAL, 1280, true}, {filestorepb.Instance_ZONAL, 1300, false}, {filestorepb.Instance_ZONAL, 12800, true},
		{filestorepb.Instance_ZONAL, 10000, false}, {filestorepb.Instance_REGIONAL, 100, true}, {filestorepb.Instance_REGIONAL, 99, false},
		{filestorepb.Instance_ENTERPRISE, 1024, true}, {filestorepb.Instance_ENTERPRISE, 10496, false},
	} {
		err := checkCapacity(c.tier, c.gb)
		if (err == nil) != c.ok {
			t.Errorf("%v %d GiB: %v, want ok=%v", c.tier, c.gb, err, c.ok)
		}
	}
}
