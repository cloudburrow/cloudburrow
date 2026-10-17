package firestoreadmin

// Backups, restores and clones (#1500).
//
// A backup is a copy of the database's documents, read from the emulator
// and kept in the front's memory, as the rest of the admin state is. As on
// Google, backups are taken by backup schedules alone (there is no
// CreateBackup): a schedule takes its first backup when it is created, and
// one each period after (a day for a daily schedule, a week for a weekly
// one); a backup expires, and is deleted, when its schedule's retention has
// passed. RestoreDatabase creates a database from a backup's documents.
//
// CloneDatabase copies a database at a point in time. The emulator keeps no
// earlier versions of its documents, so a clone is served for a
// snapshot_time in the current minute alone, and copies the documents as
// they are when it runs; an earlier snapshot_time is refused.

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	backupNameRe     = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/backups/([^/]+)$`)
	backupLocationRe = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)$`)
)

type backup struct {
	meta *adminpb.Backup
	docs []*firestorepb.Document
}

// takeBackup backs the database up for the schedule named schedule.
func (a *Admin) takeBackup(ctx context.Context, dbName, schedule string) error {
	a.mu.Lock()
	db, err := a.database(dbName)
	if err != nil {
		a.mu.Unlock()
		return err
	}
	s := a.schedules[schedule]
	if s == nil {
		a.mu.Unlock()
		return status.Errorf(codes.NotFound, "Backup schedule %s not found.", schedule)
	}
	project := dbNameRe.FindStringSubmatch(db.Name)[1]
	loc, uid, retention := db.LocationId, db.Uid, s.Retention.AsDuration()
	a.mu.Unlock()
	snap := a.now()
	docs, err := a.readAll(ctx, dbName)
	if err != nil {
		return err
	}
	b := &adminpb.Backup{Name: "projects/" + project + "/locations/" + loc + "/backups/" + uuid(),
		Database: dbName, DatabaseUid: uid, SnapshotTime: timestamppb.New(snap),
		ExpireTime: timestamppb.New(snap.Add(retention)), State: adminpb.Backup_READY,
		Stats: &adminpb.Backup_Stats{SizeBytes: docBytes(docs), DocumentCount: int64(len(docs))}}
	a.mu.Lock()
	defer a.mu.Unlock()
	for n := range a.indexes {
		if strings.HasPrefix(n, dbName+"/") {
			b.Stats.IndexCount++
		}
	}
	a.backups[b.Name] = &backup{meta: b, docs: docs}
	a.lastBackup[schedule] = snap
	return nil
}

// runSchedules takes the backups that are due and deletes the expired ones.
func (a *Admin) runSchedules(ctx context.Context) {
	now := a.now()
	type due struct{ db, schedule string }
	var todo []due
	a.mu.Lock()
	for n, b := range a.backups {
		if !b.meta.ExpireTime.AsTime().After(now) {
			delete(a.backups, n)
		}
	}
	for n, s := range a.schedules {
		period := 24 * time.Hour
		if !daily(s) {
			period = week
		}
		if last, ok := a.lastBackup[n]; !ok || now.Sub(last) >= period {
			todo = append(todo, due{scheduleNameRe.FindStringSubmatch(n)[1], n})
		}
	}
	a.mu.Unlock()
	for _, d := range todo {
		_ = a.takeBackup(ctx, d.db, d.schedule)
	}
}

// scheduleLoop runs the schedules each minute, for as long as ctx lives.
func (a *Admin) scheduleLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.runSchedules(ctx)
		}
	}
}

// Start runs the backup schedules in the background until ctx is done.
func (a *Admin) Start(ctx context.Context) { go a.scheduleLoop(ctx) }

// CreateBackupSchedule stores the schedule and takes its first backup.
func (a *Admin) CreateBackupSchedule(ctx context.Context, req *adminpb.CreateBackupScheduleRequest) (*adminpb.BackupSchedule, error) {
	s, err := a.createSchedule(req)
	if err != nil {
		return nil, err
	}
	// Google takes a schedule's backups later, in the background: one that
	// fails here is retried by the schedule loop.
	_ = a.takeBackup(ctx, req.GetParent(), s.Name)
	return s, nil
}

func (a *Admin) backup(name string) (*backup, error) {
	if !backupNameRe.MatchString(name) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid backup name %q: want projects/{project}/locations/{location}/backups/{backup}", name)
	}
	b, ok := a.backups[name]
	if !ok || !b.meta.ExpireTime.AsTime().After(a.now()) {
		return nil, status.Errorf(codes.NotFound, "Backup %s not found.", name)
	}
	return b, nil
}

func (a *Admin) GetBackup(_ context.Context, req *adminpb.GetBackupRequest) (*adminpb.Backup, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, err := a.backup(req.GetName())
	if err != nil {
		return nil, err
	}
	return proto.Clone(b.meta).(*adminpb.Backup), nil
}

func (a *Admin) ListBackups(_ context.Context, req *adminpb.ListBackupsRequest) (*adminpb.ListBackupsResponse, error) {
	m := backupLocationRe.FindStringSubmatch(req.GetParent())
	if m == nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid parent %q: want projects/{project}/locations/{location}, or locations/- for every location", req.GetParent())
	}
	var keep func(*adminpb.Backup) bool
	switch f := strings.ReplaceAll(req.GetFilter(), " ", ""); {
	case f == "":
		keep = func(*adminpb.Backup) bool { return true }
	case strings.HasPrefix(f, "database_uid="):
		uid := strings.Trim(strings.TrimPrefix(f, "database_uid="), `"`)
		keep = func(b *adminpb.Backup) bool { return b.DatabaseUid == uid }
	default:
		return nil, status.Errorf(codes.InvalidArgument, "filter %q is not supported: the filter is database_uid=\"...\"", req.GetFilter())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	resp := &adminpb.ListBackupsResponse{}
	now := a.now()
	for _, b := range a.backups {
		bm := backupNameRe.FindStringSubmatch(b.meta.Name)
		if bm[1] == m[1] && (m[2] == "-" || bm[2] == m[2]) && b.meta.ExpireTime.AsTime().After(now) && keep(b.meta) {
			resp.Backups = append(resp.Backups, proto.Clone(b.meta).(*adminpb.Backup))
		}
	}
	sort.Slice(resp.Backups, func(i, j int) bool { return resp.Backups[i].Name < resp.Backups[j].Name })
	return resp, nil
}

func (a *Admin) DeleteBackup(_ context.Context, req *adminpb.DeleteBackupRequest) (*emptypb.Empty, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.backup(req.GetName()); err != nil {
		return nil, err
	}
	delete(a.backups, req.GetName())
	return &emptypb.Empty{}, nil
}

// newDatabase adds the database id under parent, for a restore or a clone,
// in location loc. a.mu is held.
func (a *Admin) newDatabase(parent, id, loc string, tags map[string]string) (*adminpb.Database, error) {
	m := projectRe.FindStringSubmatch(parent)
	if m == nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid parent %q: want projects/{project}", parent)
	}
	if id != DefaultDatabase && (!databaseIDRe.MatchString(id) || uuidLikeRe.MatchString(id)) {
		return nil, status.Errorf(codes.InvalidArgument, "Database ID %q is invalid: it must be 4-63 characters of lowercase letters, numbers and hyphens, start with a letter and end with a letter or number, or be \"(default)\".", id)
	}
	a.seed(m[1])
	name := parent + "/databases/" + id
	if old, ok := a.databases[name]; ok && old.DeleteTime == nil {
		return nil, status.Errorf(codes.AlreadyExists, "Database already exists. Please use another database_id")
	}
	now := a.now()
	db := &adminpb.Database{Name: name, Uid: uuid(), CreateTime: timestamppb.New(now), UpdateTime: timestamppb.New(now),
		LocationId: loc, Type: adminpb.Database_FIRESTORE_NATIVE, ConcurrencyMode: adminpb.Database_PESSIMISTIC,
		PointInTimeRecoveryEnablement: adminpb.Database_POINT_IN_TIME_RECOVERY_DISABLED,
		AppEngineIntegrationMode:      adminpb.Database_DISABLED,
		DeleteProtectionState:         adminpb.Database_DELETE_PROTECTION_DISABLED,
		DatabaseEdition:               adminpb.Database_STANDARD, Tags: tags}
	a.databases[name] = db
	return db, nil
}

func (a *Admin) RestoreDatabase(ctx context.Context, req *adminpb.RestoreDatabaseRequest) (*longrunningpb.Operation, error) {
	if req.GetEncryptionConfig().GetCustomerManagedEncryption() != nil {
		return nil, status.Error(codes.Unimplemented, "customer-managed encryption is not served here: the emulator does not encrypt with Cloud KMS keys")
	}
	start := a.now()
	a.mu.Lock()
	b, err := a.backup(req.GetBackup())
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	bm := backupNameRe.FindStringSubmatch(b.meta.Name)
	if "projects/"+bm[1] != req.GetParent() {
		a.mu.Unlock()
		return nil, status.Errorf(codes.InvalidArgument, "the backup %s is not in the project %s", b.meta.Name, req.GetParent())
	}
	db, err := a.newDatabase(req.GetParent(), req.GetDatabaseId(), bm[2], req.GetTags())
	docs := b.docs
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := a.writeAll(ctx, db.Name, docs); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.done(db.Name, &adminpb.RestoreDatabaseMetadata{StartTime: timestamppb.New(start), EndTime: timestamppb.New(a.now()),
		OperationState: adminpb.OperationState_SUCCESSFUL, Database: db.Name, Backup: b.meta.Name,
		ProgressPercentage: &adminpb.Progress{EstimatedWork: 100, CompletedWork: 100}}, a.describe(db))
}

func (a *Admin) CloneDatabase(ctx context.Context, req *adminpb.CloneDatabaseRequest) (*longrunningpb.Operation, error) {
	if req.GetEncryptionConfig().GetCustomerManagedEncryption() != nil {
		return nil, status.Error(codes.Unimplemented, "customer-managed encryption is not served here: the emulator does not encrypt with Cloud KMS keys")
	}
	snap := req.GetPitrSnapshot()
	if snap == nil || snap.GetDatabase() == "" || snap.GetSnapshotTime() == nil {
		return nil, status.Error(codes.InvalidArgument, "pitr_snapshot is required, with its database and snapshot_time.")
	}
	at := snap.GetSnapshotTime().AsTime()
	if !at.Truncate(time.Minute).Equal(at) {
		return nil, status.Error(codes.InvalidArgument, "pitr_snapshot.snapshot_time must be a whole minute.")
	}
	start := a.now()
	if at.After(start) {
		return nil, status.Error(codes.InvalidArgument, "pitr_snapshot.snapshot_time is in the future.")
	}
	if at.Before(start.Truncate(time.Minute)) {
		return nil, status.Errorf(codes.Unimplemented, "a clone at %s is not served here: the emulator keeps no earlier versions of its documents, so a clone is served for a snapshot_time in the current minute (%s) alone", at.Format(time.RFC3339), start.Truncate(time.Minute).Format(time.RFC3339))
	}
	a.mu.Lock()
	src, err := a.database(snap.GetDatabase())
	if err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if len(snap.GetDatabaseUid()) > 0 && string(snap.GetDatabaseUid()) != src.Uid {
		a.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "The database %s with that database_uid does not exist.", src.Name)
	}
	srcName := src.Name
	db, err := a.newDatabase(req.GetParent(), req.GetDatabaseId(), src.LocationId, req.GetTags())
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	docs, err := a.readAll(ctx, srcName)
	if err == nil {
		err = a.writeAll(ctx, db.Name, docs)
	}
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.done(db.Name, &adminpb.CloneDatabaseMetadata{StartTime: timestamppb.New(start), EndTime: timestamppb.New(a.now()),
		OperationState: adminpb.OperationState_SUCCESSFUL, Database: db.Name, PitrSnapshot: snap,
		ProgressPercentage: &adminpb.Progress{EstimatedWork: 100, CompletedWork: 100}}, a.describe(db))
}
