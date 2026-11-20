package spannerfront

// Incremental backups (#1949), as https://docs.cloud.google.com/spanner/docs/backup
// documents them: an incremental backup schedule (Enterprise and
// Enterprise Plus editions) takes backups that form chains. "The first
// backup created by an incremental backup schedule is a full backup", and
// a chain holds "up to 13 incremental backups ... in addition to the
// initial full backup"; a new chain starts when the chain is full, when
// "the oldest full backup was taken 28 or more days ago", when "the newest
// backup in the chain is deleted", or when "the incremental backup
// schedule is modified". Each backup carries the chain's
// incremental_backup_chain_id and oldest_version_time, the version time of
// the chain's full backup "even if that backup has expired";
// freeable_size_bytes is the newest backup's own data, and zero for every
// older backup of the chain. "An incremental backup schedule can create a
// backup every 4 hours or more", and "You can't create an incremental
// backup on demand" (CreateBackup has no such request).
//
// Locally each backup of a chain keeps a whole copy of the database
// (backups.go), so restoring any of them needs no other ("Spanner needs
// all the older backups in the chain to restore an incremental backup":
// those Google keeps, deleted or expired, until the newer ones go), and
// deleting or expiring one never breaks another's restore. Sizes are
// reported as Google does: size_bytes the database's, exclusive and
// freeable the growth since the chain's previous backup.

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"github.com/robfig/cron/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	maxChainIncrementals = 13
	maxChainAge          = 28 * 24 * time.Hour
	minIncrementalEvery  = 4 * time.Hour
)

func cronOf(text string) (cron.Schedule, error) { return cron.ParseStandard(text) }

// checkIncremental checks an incremental schedule in, of database, which
// needs an Enterprise edition and a cron of at most one backup every 4
// hours. s.mu is held.
func (s *Instances) checkIncrementalLocked(method, database string, in *databasepb.BackupSchedule) error {
	if in.GetIncrementalBackupSpec() == nil {
		return nil
	}
	m := databaseNameRe.FindStringSubmatch(database)
	if m == nil {
		return nil
	}
	if ed := s.editionLocked(m[1]); editionRank(ed) < editionRank(instancepb.Instance_ENTERPRISE) {
		return featureNeeds(method, "incremental backups", instancepb.Instance_ENTERPRISE, ed)
	}
	c, err := cronOf(in.GetSpec().GetCronSpec().GetText())
	if err != nil {
		return nil // checkSchedule names the cron's error
	}
	// The shortest gap between the cron's times over a year of them.
	t := c.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	for i := 0; i < 3000; i++ {
		n := c.Next(t)
		if n.Sub(t) < minIncrementalEvery {
			return status.Errorf(codes.InvalidArgument, "Invalid %s request: an incremental backup schedule can create a backup every 4 hours or more: spec.cron_spec.text %q runs more often.", method, in.GetSpec().GetCronSpec().GetText())
		}
		t = n
	}
	return nil
}

func (s *Instances) checkIncremental(method, database string, in *databasepb.BackupSchedule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkIncrementalLocked(method, database, in)
}

func chainID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// brokenChains are the chains whose newest backup was deleted: the next
// backup of their schedule starts a new chain.
var brokenChains sync.Map

// chainBackup makes b, just taken by schedules, a backup of its
// incremental schedule's chain, if the schedule is incremental.
func (s *Instances) chainBackup(b *databasepb.Backup, schedules []string) {
	if len(schedules) != 1 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.schedules[schedules[0]]
	if !ok || sc.pb.GetIncrementalBackupSpec() == nil {
		return
	}
	// The schedule's newest backup of a chain, and that chain's members.
	var newest *databasepb.Backup
	for name, o := range s.backups {
		if name == b.GetName() || o.pb.GetIncrementalBackupChainId() == "" || !hasString(o.pb.GetBackupSchedules(), schedules[0]) {
			continue
		}
		if newest == nil || o.pb.GetCreateTime().AsTime().After(newest.GetCreateTime().AsTime()) {
			newest = o.pb
		}
	}
	var members []*databasepb.Backup
	if newest != nil {
		for _, o := range s.backups {
			if o.pb != b && o.pb.GetIncrementalBackupChainId() == newest.GetIncrementalBackupChainId() {
				members = append(members, o.pb)
			}
		}
	}
	fresh := newest == nil || chainBroken(newest.GetIncrementalBackupChainId()) ||
		len(members) >= maxChainIncrementals+1 ||
		b.GetVersionTime().AsTime().Sub(newest.GetOldestVersionTime().AsTime()) >= maxChainAge ||
		sc.pb.GetUpdateTime().AsTime().After(newest.GetCreateTime().AsTime())
	if fresh {
		b.IncrementalBackupChainId = chainID()
		b.OldestVersionTime = b.GetVersionTime()
		b.FreeableSizeBytes = b.GetSizeBytes()
		return
	}
	b.IncrementalBackupChainId = newest.GetIncrementalBackupChainId()
	b.OldestVersionTime = newest.GetOldestVersionTime()
	grew := b.GetSizeBytes() - newest.GetSizeBytes()
	if grew < 0 {
		grew = 0
	}
	b.ExclusiveSizeBytes, b.FreeableSizeBytes = grew, grew
	for _, o := range members {
		o.FreeableSizeBytes = 0
	}
}

// unchainBackup is DeleteBackup's of b, a chain's backup: deleting the
// chain's newest backup ends the chain, and the newest left frees its
// own data. s.mu is held.
func (s *Instances) unchainBackupLocked(b *databasepb.Backup) {
	id := b.GetIncrementalBackupChainId()
	if id == "" {
		return
	}
	var newest *databasepb.Backup
	for _, o := range s.backups {
		if o.pb.GetIncrementalBackupChainId() == id && (newest == nil || o.pb.GetCreateTime().AsTime().After(newest.GetCreateTime().AsTime())) {
			newest = o.pb
		}
	}
	if newest == nil || b.GetCreateTime().AsTime().After(newest.GetCreateTime().AsTime()) {
		brokenChains.Store(id, true)
		if newest != nil {
			newest.FreeableSizeBytes = newest.GetExclusiveSizeBytes()
		}
	}
}

func hasString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func chainBroken(id string) bool {
	_, ok := brokenChains.Load(id)
	return ok
}
