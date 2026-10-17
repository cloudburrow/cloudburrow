package spannerfront

import (
	"context"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"github.com/robfig/cron/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Backup schedules (#1498), kept in memory beside the backups, and run:
// every minute the front takes the backups whose cron time has come, as
// full backups (backups.go) named after the schedule and kept for its
// retention duration. Times are UTC, as Google's are. Incremental backups
// are refused: they need the Enterprise edition, which the emulator does
// not have.

const creationWindow = 4 * time.Hour

var scheduleNameRe = regexp.MustCompile(`^(projects/[^/]+/instances/[^/]+/databases/[^/]+)/backupSchedules/([^/]+)$`)

type schedule struct {
	pb   *databasepb.BackupSchedule
	cron cron.Schedule
	last time.Time // the cron time last taken, or the schedule's creation
}

// checkSchedule validates in for method, and returns its parsed cron.
func checkSchedule(method string, in *databasepb.BackupSchedule) (cron.Schedule, error) {
	text := in.GetSpec().GetCronSpec().GetText()
	if text == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: spec.cron_spec.text is required.", method)
	}
	c, err := cron.ParseStandard(text)
	if err != nil || strings.HasPrefix(strings.TrimSpace(text), "@") || strings.Contains(text, "TZ=") {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: spec.cron_spec.text %q is not a five-field crontab.", method, text)
	}
	r := in.GetRetentionDuration()
	if r == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: retention_duration is required.", method)
	}
	if d := r.AsDuration(); d < minBackupLife || d > maxBackupLife {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: retention_duration must be from 6 hours to 366 days.", method)
	}
	if in.GetIncrementalBackupSpec() != nil {
		return nil, status.Errorf(codes.Unimplemented, "%s of incremental backups is not served here: they need the Enterprise edition, which the emulator does not have", method)
	}
	if e := in.GetEncryptionConfig(); e != nil {
		if err := refuseCMEK(method, e.GetEncryptionType(), e.GetKmsKeyName()); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// normalize fills the output fields Google fills.
func (s *Instances) normalize(b *databasepb.BackupSchedule) {
	if b.GetFullBackupSpec() == nil && b.GetIncrementalBackupSpec() == nil {
		b.BackupTypeSpec = &databasepb.BackupSchedule_FullBackupSpec{FullBackupSpec: &databasepb.FullBackupSpec{}}
	}
	cs := b.GetSpec().GetCronSpec()
	cs.TimeZone = "UTC"
	cs.CreationWindow = durationpb.New(creationWindow)
	b.UpdateTime = timestamppb.New(s.now())
}

func (d databases) CreateBackupSchedule(ctx context.Context, req *databasepb.CreateBackupScheduleRequest) (*databasepb.BackupSchedule, error) {
	s := d.s
	if !backupIDRe.MatchString(req.GetBackupScheduleId()) {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid CreateBackupSchedule request: backup_schedule_id %q must match [a-z][a-z0-9_\\-]*[a-z0-9] and be 2 to 60 characters long.", req.GetBackupScheduleId())
	}
	if _, err := s.emuDB.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: req.GetParent()}); err != nil {
		return nil, err
	}
	in := proto.Clone(req.GetBackupSchedule()).(*databasepb.BackupSchedule)
	c, err := checkSchedule("CreateBackupSchedule", in)
	if err != nil {
		return nil, err
	}
	in.Name = req.GetParent() + "/backupSchedules/" + req.GetBackupScheduleId()
	s.normalize(in)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.schedules[in.Name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "Backup schedule already exists: %s", in.Name)
	}
	s.schedules[in.Name] = &schedule{pb: in, cron: c, last: s.now()}
	return proto.Clone(in).(*databasepb.BackupSchedule), nil
}

func scheduleNotFound(name string) error {
	return status.Errorf(codes.NotFound, "Backup schedule not found: %s", name)
}

func (d databases) GetBackupSchedule(_ context.Context, req *databasepb.GetBackupScheduleRequest) (*databasepb.BackupSchedule, error) {
	d.s.mu.Lock()
	defer d.s.mu.Unlock()
	sc, ok := d.s.schedules[req.GetName()]
	if !ok {
		return nil, scheduleNotFound(req.GetName())
	}
	return proto.Clone(sc.pb).(*databasepb.BackupSchedule), nil
}

func (d databases) UpdateBackupSchedule(_ context.Context, req *databasepb.UpdateBackupScheduleRequest) (*databasepb.BackupSchedule, error) {
	s := d.s
	in := req.GetBackupSchedule()
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Invalid UpdateBackupSchedule request: update_mask must name at least one field.")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.schedules[in.GetName()]
	if !ok {
		return nil, scheduleNotFound(in.GetName())
	}
	next := proto.Clone(sc.pb).(*databasepb.BackupSchedule)
	for _, p := range paths {
		switch snake(p) {
		case "spec", "spec.cron_spec", "spec.cron_spec.text":
			next.Spec = proto.Clone(in.GetSpec()).(*databasepb.BackupScheduleSpec)
		case "retention_duration":
			next.RetentionDuration = in.GetRetentionDuration()
		case "encryption_config":
			next.EncryptionConfig = in.GetEncryptionConfig()
		case "full_backup_spec", "incremental_backup_spec", "backup_type_spec":
			next.BackupTypeSpec = in.BackupTypeSpec
		default:
			return nil, status.Errorf(codes.InvalidArgument, "Invalid UpdateBackupSchedule request: field_mask path %q is not a field of BackupSchedule that can be updated.", p)
		}
	}
	c, err := checkSchedule("UpdateBackupSchedule", next)
	if err != nil {
		return nil, err
	}
	s.normalize(next)
	sc.pb, sc.cron = next, c
	return proto.Clone(next).(*databasepb.BackupSchedule), nil
}

func (d databases) DeleteBackupSchedule(_ context.Context, req *databasepb.DeleteBackupScheduleRequest) (*emptypb.Empty, error) {
	d.s.mu.Lock()
	defer d.s.mu.Unlock()
	if _, ok := d.s.schedules[req.GetName()]; !ok {
		return nil, scheduleNotFound(req.GetName())
	}
	delete(d.s.schedules, req.GetName())
	return &emptypb.Empty{}, nil
}

func (d databases) ListBackupSchedules(ctx context.Context, req *databasepb.ListBackupSchedulesRequest) (*databasepb.ListBackupSchedulesResponse, error) {
	if _, err := d.s.emuDB.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: req.GetParent()}); err != nil {
		return nil, err
	}
	d.s.mu.Lock()
	byName := map[string]*databasepb.BackupSchedule{}
	for name, sc := range d.s.schedules {
		if strings.HasPrefix(name, req.GetParent()+"/backupSchedules/") {
			byName[name] = proto.Clone(sc.pb).(*databasepb.BackupSchedule)
		}
	}
	d.s.mu.Unlock()
	page, next, err := pageOf("schedules:"+req.GetParent(), byName, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	resp := &databasepb.ListBackupSchedulesResponse{NextPageToken: next}
	for _, n := range page {
		resp.BackupSchedules = append(resp.BackupSchedules, byName[n])
	}
	return resp, nil
}

// dropSchedules forgets the schedules of database.
func (s *Instances) dropSchedules(database string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.schedules {
		if strings.HasPrefix(name, database+"/") {
			delete(s.schedules, name)
		}
	}
}

// runSchedules takes the backups whose cron time has come by now.
func (s *Instances) runSchedules(ctx context.Context) {
	type due struct {
		name, database string
		at             time.Time
		retention      time.Duration
	}
	now := s.now()
	var todo []due
	s.mu.Lock()
	for name, sc := range s.schedules {
		next := sc.cron.Next(sc.last)
		if next.After(now) {
			continue
		}
		// One backup for the latest time passed, as a schedule that missed
		// several (the instance was stopped) takes one.
		for n := sc.cron.Next(next); !n.After(now); n = sc.cron.Next(n) {
			next = n
		}
		sc.last = next
		m := scheduleNameRe.FindStringSubmatch(name)
		todo = append(todo, due{name: name, database: m[1], at: next, retention: sc.pb.GetRetentionDuration().AsDuration()})
	}
	s.mu.Unlock()
	for _, t := range todo {
		m := databaseNameRe.FindStringSubmatch(t.database)
		id := t.name[strings.LastIndex(t.name, "/")+1:]
		if len(id) > 44 {
			id = id[:44]
		}
		backupID := strings.TrimRight(id, "-_") + "-" + t.at.UTC().Format("20060102150405")
		_, _ = s.takeBackup(ctx, m[1]+"/backups/"+backupID, t.database, nil,
			timestamppb.New(s.now().Add(t.retention)), []string{t.name})
	}
}

// schedule runs runSchedules every minute, for the front's lifetime.
func (s *Instances) scheduleLoop() {
	ctx := context.Background()
	for range time.Tick(time.Minute) {
		s.runSchedules(ctx)
	}
}
