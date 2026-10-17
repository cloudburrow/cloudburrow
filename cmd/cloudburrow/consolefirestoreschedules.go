package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/genproto/googleapis/type/dayofweek"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// A database's backup schedules (#1461): its Backup schedules tab, Create
// backup schedule on its page, and Edit retention and Delete backup
// schedule on a schedule's row, through the official Firestore Admin
// client. The schedules take the backups of the Backups tab
// (consolefirestorebackups.go).

// firestoreSchedulesSegment is the second segment of a schedule's action path.
const firestoreSchedulesSegment = "backupSchedules"

var weekdays = []string{"MONDAY", "TUESDAY", "WEDNESDAY", "THURSDAY", "FRIDAY", "SATURDAY", "SUNDAY"}

func createScheduleFields() []console.Field {
	return []console.Field{
		{Name: "recurrence", Label: "Recurrence", Type: "select", Options: []string{"daily", "weekly"}, Default: "daily",
			Help: "A database has at most one daily and one weekly schedule."},
		{Name: "day", Label: "Day of the week (weekly)", Type: "select", Options: weekdays, Default: "MONDAY"},
		retentionField("7"),
	}
}

func retentionField(days string) console.Field {
	return console.Field{Name: "retentionDays", Label: "Retention (days)", Type: "text", Required: true, Pattern: `^[0-9]+$`, Default: days,
		Help: "Up to 7 days for a daily schedule and 98 days (14 weeks) for a weekly one."}
}

func scheduleActions(s *adminpb.BackupSchedule) []console.Action {
	days := strconv.Itoa(int(s.GetRetention().AsDuration() / (24 * time.Hour)))
	return []console.Action{
		{ID: "editschedule", Label: "Edit retention", Fields: []console.Field{retentionField(days)}},
		{ID: "deleteschedule", Label: "Delete backup schedule", Destructive: true},
	}
}

func retention(values map[string]string) (*durationpb.Duration, error) {
	n, err := strconv.Atoi(strings.TrimSpace(values["retentionDays"]))
	if err != nil || n <= 0 {
		return nil, fmt.Errorf("retention %q is not a positive number of days", values["retentionDays"])
	}
	return durationpb.New(time.Duration(n) * 24 * time.Hour), nil
}

// schedules is a database's Backup schedules tab.
func (p firestoreDatabasesProvider) schedules(ctx context.Context, c *admin.FirestoreAdminClient, db, id string) console.Listing {
	list := console.Listing{Columns: []string{"Recurrence", "Retention", "Created"}, NameColumn: "Schedule ID", Noun: "backup schedules"}
	resp, err := c.ListBackupSchedules(ctx, &adminpb.ListBackupSchedulesRequest{Parent: db})
	if err != nil {
		list.Unavailable = "listing backup schedules: " + err.Error()
		return list
	}
	for _, s := range resp.GetBackupSchedules() {
		sid := s.GetName()[strings.LastIndex(s.GetName(), "/")+1:]
		rec := "Daily"
		if w := s.GetWeeklyRecurrence(); w != nil {
			rec = "Weekly, " + enumWord(w.GetDay(), "")
		}
		list.Items = append(list.Items, console.Resource{Name: sid, Fields: map[string]string{
			"Recurrence": rec,
			"Retention":  s.GetRetention().AsDuration().String(),
			"Created":    s.GetCreateTime().AsTime().Format("2006-01-02 15:04:05 MST"),
		}, Actions: scheduleActions(s), ActsOn: []string{id, firestoreSchedulesSegment, sid}})
	}
	list.Total = len(list.Items)
	list.Note = "Backup schedules are stored with Google's limits. A schedule takes its first backup when it is created, then one each day or week: see the Backups tab."
	return list
}

// actSchedule performs a backup schedule action on db. ok is false for an
// action that is not one.
func actSchedule(ctx context.Context, c *admin.FirestoreAdminClient, db string, path []string, action string, values map[string]string) (bool, error) {
	switch {
	case action == "createschedule" && len(path) == 1:
		ret, err := retention(values)
		if err != nil {
			return true, err
		}
		s := &adminpb.BackupSchedule{Retention: ret}
		switch values["recurrence"] {
		case "daily":
			s.Recurrence = &adminpb.BackupSchedule_DailyRecurrence{DailyRecurrence: &adminpb.DailyRecurrence{}}
		case "weekly":
			day, ok := dayofweek.DayOfWeek_value[values["day"]]
			if !ok || day == 0 {
				return true, fmt.Errorf("day %q is not a day of the week", values["day"])
			}
			s.Recurrence = &adminpb.BackupSchedule_WeeklyRecurrence{WeeklyRecurrence: &adminpb.WeeklyRecurrence{Day: dayofweek.DayOfWeek(day)}}
		default:
			return true, fmt.Errorf("recurrence %q is not daily or weekly", values["recurrence"])
		}
		_, err = c.CreateBackupSchedule(ctx, &adminpb.CreateBackupScheduleRequest{Parent: db, BackupSchedule: s})
		return true, err
	case len(path) == 3 && path[1] == firestoreSchedulesSegment && action == "editschedule":
		ret, err := retention(values)
		if err != nil {
			return true, err
		}
		_, err = c.UpdateBackupSchedule(ctx, &adminpb.UpdateBackupScheduleRequest{
			BackupSchedule: &adminpb.BackupSchedule{Name: db + "/backupSchedules/" + path[2], Retention: ret},
			UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"retention"}}})
		return true, err
	case len(path) == 3 && path[1] == firestoreSchedulesSegment && action == "deleteschedule":
		return true, c.DeleteBackupSchedule(ctx, &adminpb.DeleteBackupScheduleRequest{Name: db + "/backupSchedules/" + path[2]})
	}
	return false, nil
}
