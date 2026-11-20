package spannerfront

import (
	"fmt"
	"testing"
	"time"

	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// An incremental schedule's backups form chains of a full backup and up
// to 13 incremental ones; deleting the newest, or changing the schedule,
// starts a new chain; older backups free nothing (#1949).
func TestIncrementalChains(t *testing.T) {
	s, err := newWith("127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	const sched = "projects/p/instances/i/databases/d/backupSchedules/inc"
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s.schedules[sched] = &schedule{pb: &databasepb.BackupSchedule{Name: sched, UpdateTime: timestamppb.New(start),
		BackupTypeSpec: &databasepb.BackupSchedule_IncrementalBackupSpec{IncrementalBackupSpec: &databasepb.IncrementalBackupSpec{}}}}
	take := func(i int) *databasepb.Backup {
		at := start.Add(time.Duration(i+1) * 4 * time.Hour)
		b := &databasepb.Backup{Name: fmt.Sprintf("projects/p/instances/i/backups/b%02d", i), CreateTime: timestamppb.New(at),
			VersionTime: timestamppb.New(at), SizeBytes: int64(100 + 10*i), ExclusiveSizeBytes: int64(100 + 10*i), BackupSchedules: []string{sched}}
		s.backups[b.Name] = &backup{pb: b}
		s.chainBackup(b, []string{sched})
		return b
	}
	var chain []*databasepb.Backup
	for i := 0; i < 14; i++ {
		chain = append(chain, take(i))
	}
	id := chain[0].GetIncrementalBackupChainId()
	for i, b := range chain {
		if b.GetIncrementalBackupChainId() != id || !b.GetOldestVersionTime().AsTime().Equal(chain[0].GetVersionTime().AsTime()) {
			t.Fatalf("backup %d = %v, want chain %s from the full backup", i, b, id)
		}
		if want := int64(0); i < 13 && b.GetFreeableSizeBytes() != want {
			t.Errorf("backup %d frees %d, want 0: newer backups need it", i, b.GetFreeableSizeBytes())
		}
	}
	if chain[13].GetExclusiveSizeBytes() != 10 || chain[13].GetFreeableSizeBytes() != 10 {
		t.Errorf("the newest incremental = %v, want 10 bytes its own", chain[13])
	}
	next := take(14)
	if next.GetIncrementalBackupChainId() == id || next.GetFreeableSizeBytes() != next.GetSizeBytes() {
		t.Errorf("the 15th backup = %v, want a new chain's full backup", next)
	}
	// Deleting the newest backup ends its chain.
	after := take(15)
	if after.GetIncrementalBackupChainId() != next.GetIncrementalBackupChainId() {
		t.Fatalf("the 16th backup = %v, want it in the second chain", after)
	}
	s.mu.Lock()
	delete(s.backups, after.Name)
	s.unchainBackupLocked(after)
	s.mu.Unlock()
	if next.GetFreeableSizeBytes() != next.GetExclusiveSizeBytes() {
		t.Errorf("the chain's newest left = %v, want it to free its own bytes", next)
	}
	third := take(16)
	if third.GetIncrementalBackupChainId() == next.GetIncrementalBackupChainId() {
		t.Errorf("after deleting the newest, %v continues the chain", third)
	}
	// A modified schedule starts a new chain.
	s.schedules[sched].pb.UpdateTime = timestamppb.New(start.Add(1000 * time.Hour))
	if fourth := take(17); fourth.GetIncrementalBackupChainId() == third.GetIncrementalBackupChainId() {
		t.Errorf("after the schedule changed, %v continues the chain", fourth)
	}
}
