package firestoreadmin

import (
	"context"
	"regexp"
	"sort"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Backup schedules (#1461) are stored and returned as Google returns them,
// with Google's limits: a database has at most one daily and one weekly
// schedule, a daily one keeps its backups up to 7 days and a weekly one up
// to 14 weeks. They take no backups: the emulator cannot snapshot a
// database, so backups stay refused (noBackups).

var scheduleNameRe = regexp.MustCompile(`^(projects/[^/]+/databases/[^/]+)/backupSchedules/([^/]+)$`)

const (
	maxDailyRetention  = 7 * 24 * time.Hour
	maxWeeklyRetention = 14 * week
)

// checkSchedule validates s's recurrence and retention.
func checkSchedule(s *adminpb.BackupSchedule) error {
	ret := s.GetRetention()
	if ret == nil {
		return status.Error(codes.InvalidArgument, "retention is required.")
	}
	if err := ret.CheckValid(); err != nil || ret.AsDuration() <= 0 {
		return status.Error(codes.InvalidArgument, "retention must be a positive duration.")
	}
	switch r := s.GetRecurrence().(type) {
	case *adminpb.BackupSchedule_DailyRecurrence:
		if ret.AsDuration() > maxDailyRetention {
			return status.Error(codes.InvalidArgument, "retention of a daily backup schedule must be at most 7 days.")
		}
	case *adminpb.BackupSchedule_WeeklyRecurrence:
		if r.WeeklyRecurrence.GetDay() == 0 {
			return status.Error(codes.InvalidArgument, "weekly_recurrence.day is required.")
		}
		if ret.AsDuration() > maxWeeklyRetention {
			return status.Error(codes.InvalidArgument, "retention of a weekly backup schedule must be at most 14 weeks.")
		}
	default:
		return status.Error(codes.InvalidArgument, "recurrence is required: daily_recurrence or weekly_recurrence.")
	}
	return nil
}

func daily(s *adminpb.BackupSchedule) bool { return s.GetDailyRecurrence() != nil }

// CreateBackupSchedule stores a schedule for an existing database.
func (a *Admin) CreateBackupSchedule(_ context.Context, req *adminpb.CreateBackupScheduleRequest) (*adminpb.BackupSchedule, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	db, err := a.database(req.GetParent())
	if err != nil {
		return nil, err
	}
	in := req.GetBackupSchedule()
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "backup_schedule is required.")
	}
	if err := checkSchedule(in); err != nil {
		return nil, err
	}
	for _, s := range a.schedules {
		if scheduleNameRe.FindStringSubmatch(s.Name)[1] == db.Name && daily(s) == daily(in) {
			kind := "weekly"
			if daily(in) {
				kind = "daily"
			}
			return nil, status.Errorf(codes.AlreadyExists, "The database %s already has a %s backup schedule: a database has at most one daily and one weekly backup schedule.", db.Name, kind)
		}
	}
	now := timestamppb.New(a.now())
	s := &adminpb.BackupSchedule{Name: db.Name + "/backupSchedules/" + uuid(), CreateTime: now, UpdateTime: now,
		Retention: in.Retention, Recurrence: in.Recurrence}
	a.schedules[s.Name] = s
	return proto.Clone(s).(*adminpb.BackupSchedule), nil
}

// schedule is the schedule named name. a.mu is held.
func (a *Admin) schedule(name string) (*adminpb.BackupSchedule, error) {
	m := scheduleNameRe.FindStringSubmatch(name)
	if m == nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid backup schedule name %q: want projects/{project}/databases/{database}/backupSchedules/{backup_schedule}", name)
	}
	if _, err := a.database(m[1]); err != nil {
		return nil, err
	}
	s, ok := a.schedules[name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "Backup schedule %s not found.", name)
	}
	return s, nil
}

func (a *Admin) GetBackupSchedule(_ context.Context, req *adminpb.GetBackupScheduleRequest) (*adminpb.BackupSchedule, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, err := a.schedule(req.GetName())
	if err != nil {
		return nil, err
	}
	return proto.Clone(s).(*adminpb.BackupSchedule), nil
}

func (a *Admin) ListBackupSchedules(_ context.Context, req *adminpb.ListBackupSchedulesRequest) (*adminpb.ListBackupSchedulesResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	db, err := a.database(req.GetParent())
	if err != nil {
		return nil, err
	}
	resp := &adminpb.ListBackupSchedulesResponse{}
	for _, s := range a.schedules {
		if scheduleNameRe.FindStringSubmatch(s.Name)[1] == db.Name {
			resp.BackupSchedules = append(resp.BackupSchedules, proto.Clone(s).(*adminpb.BackupSchedule))
		}
	}
	sort.Slice(resp.BackupSchedules, func(i, j int) bool {
		x, y := resp.BackupSchedules[i], resp.BackupSchedules[j]
		if !x.CreateTime.AsTime().Equal(y.CreateTime.AsTime()) {
			return x.CreateTime.AsTime().Before(y.CreateTime.AsTime())
		}
		return x.Name < y.Name
	})
	return resp, nil
}

// UpdateBackupSchedule changes a schedule's retention, the one field Google
// lets an update change: its recurrence is fixed at creation.
func (a *Admin) UpdateBackupSchedule(_ context.Context, req *adminpb.UpdateBackupScheduleRequest) (*adminpb.BackupSchedule, error) {
	in := req.GetBackupSchedule()
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "backup_schedule is required.")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, err := a.schedule(in.GetName())
	if err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"retention"}
		if in.Recurrence != nil {
			paths = append(paths, "recurrence")
		}
	}
	next := proto.Clone(s).(*adminpb.BackupSchedule)
	for _, p := range paths {
		switch snake(p) {
		case "retention":
			next.Retention = in.Retention
		case "daily_recurrence", "weekly_recurrence", "recurrence":
			if in.Recurrence != nil && !proto.Equal(&adminpb.BackupSchedule{Recurrence: in.Recurrence}, &adminpb.BackupSchedule{Recurrence: s.Recurrence}) {
				return nil, status.Error(codes.InvalidArgument, "a backup schedule's recurrence cannot be changed: delete the schedule and create one with the new recurrence.")
			}
		default:
			return nil, status.Errorf(codes.InvalidArgument, "update_mask path %q cannot be updated: only retention can", p)
		}
	}
	if err := checkSchedule(next); err != nil {
		return nil, err
	}
	next.UpdateTime = timestamppb.New(a.now())
	a.schedules[s.Name] = next
	return proto.Clone(next).(*adminpb.BackupSchedule), nil
}

func (a *Admin) DeleteBackupSchedule(_ context.Context, req *adminpb.DeleteBackupScheduleRequest) (*emptypb.Empty, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, err := a.schedule(req.GetName())
	if err != nil {
		return nil, err
	}
	delete(a.schedules, s.Name)
	return &emptypb.Empty{}, nil
}
