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

// Disaster recovery as Google's page (#1845, round 2;
// https://docs.cloud.google.com/firestore/native/docs/backups, read
// 2026-11-19): the page "describes backup schedules and lists available
// backups", and Edit holds Daily (with its retention period) and Weekly
// (with a backup day and its retention period) checkboxes, then Save. A
// database has at most one daily and one weekly schedule, so the form's two
// checkboxes are the whole of its schedules: Save creates, updates or
// deletes them through the Admin API.

const editDisasterRecovery = "editdisasterrecovery"

// firestoreDaySymbols are Google's day choices, as gcloud names them.
var firestoreDayLabels = map[string]string{"MONDAY": "Monday", "TUESDAY": "Tuesday", "WEDNESDAY": "Wednesday",
	"THURSDAY": "Thursday", "FRIDAY": "Friday", "SATURDAY": "Saturday", "SUNDAY": "Sunday"}

// drSchedules are db's daily and weekly schedules, either nil.
func drSchedules(ctx context.Context, c *admin.FirestoreAdminClient, db string) (daily, weekly *adminpb.BackupSchedule, err error) {
	resp, err := c.ListBackupSchedules(ctx, &adminpb.ListBackupSchedulesRequest{Parent: db})
	if err != nil {
		return nil, nil, err
	}
	for _, s := range resp.GetBackupSchedules() {
		switch {
		case s.GetDailyRecurrence() != nil && daily == nil:
			daily = s
		case s.GetWeeklyRecurrence() != nil && weekly == nil:
			weekly = s
		}
	}
	return daily, weekly, nil
}

func retentionDaysOf(s *adminpb.BackupSchedule, def int) int {
	if s == nil {
		return def
	}
	return int(s.GetRetention().AsDuration() / (24 * time.Hour))
}

// disasterRecoveryEdit is the tab's Edit, its values the database's
// schedules as they are.
func disasterRecoveryEdit(daily, weekly *adminpb.BackupSchedule) console.Action {
	day := "SUNDAY"
	if weekly != nil {
		day = weekly.GetWeeklyRecurrence().GetDay().String()
	}
	weeks := retentionDaysOf(weekly, 14*7) / 7
	if weeks < 1 {
		weeks = 1
	}
	return console.Action{ID: editDisasterRecovery, Label: "Edit", Fields: []console.Field{
		{Name: "daily", Label: "Daily", Type: "checkbox", Default: strconv.FormatBool(daily != nil),
			Help: "A backup of the database each day."},
		{Name: "dailyRetention", Label: "Daily retention period (days)", Type: "text", Pattern: `^[0-9]{1,2}$`,
			Default: strconv.Itoa(retentionDaysOf(daily, 7)), ShowWhen: "daily",
			Help: "How long each daily backup is kept, in days."},
		{Name: "weekly", Label: "Weekly", Type: "checkbox", Default: strconv.FormatBool(weekly != nil),
			Help: "A backup of the database each week, on the day chosen."},
		{Name: "weeklyDay", Label: "Backup day", Type: "select", Options: weekdays, OptionLabels: firestoreDayLabels,
			Default: day, ShowWhen: "weekly"},
		{Name: "weeklyRetention", Label: "Weekly retention period (weeks)", Type: "text", Pattern: `^[0-9]{1,2}$`,
			Default: strconv.Itoa(weeks), ShowWhen: "weekly",
			Help: "How long each weekly backup is kept: up to 14 weeks."},
	}}
}

// disasterRecoveryNote describes the schedules above the Backups table.
func disasterRecoveryNote(daily, weekly *adminpb.BackupSchedule) string {
	d := "Daily backups: off."
	if daily != nil {
		d = fmt.Sprintf("Daily backups: on, each kept %d days.", retentionDaysOf(daily, 0))
	}
	w := "Weekly backups: off."
	if weekly != nil {
		w = fmt.Sprintf("Weekly backups: on, every %s, each kept %d weeks.",
			firestoreDayLabels[weekly.GetWeeklyRecurrence().GetDay().String()], retentionDaysOf(weekly, 0)/7)
	}
	return d + " " + w + " A schedule takes its first backup when it is created, then one each day or week."
}

// disasterRecovery is the Disaster recovery tab: the schedules described,
// Edit, and the Backups table.
func (p firestoreDatabasesProvider) disasterRecovery(ctx context.Context, c *admin.FirestoreAdminClient, db, id string) console.Section {
	sec := console.Section{ID: "schedules", Label: "Disaster recovery", Listing: p.backups(ctx, c, db, id)}
	daily, weekly, err := drSchedules(ctx, c, db)
	if err != nil {
		sec.Unavailable = "listing backup schedules: " + err.Error()
		return sec
	}
	sec.Note = disasterRecoveryNote(daily, weekly)
	sec.Actions = []console.Action{disasterRecoveryEdit(daily, weekly)}
	sec.Listing.Noun = "backups"
	return sec
}

func positiveInt(raw, what string, max int) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 || n > max {
		return 0, fmt.Errorf("%s %q is not a whole number from 1 to %d", what, raw, max)
	}
	return n, nil
}

// actDisasterRecovery is Edit's Save: each checkbox's schedule is created,
// changed or deleted to match.
func actDisasterRecovery(ctx context.Context, c *admin.FirestoreAdminClient, db string, values map[string]string) error {
	daily, weekly, err := drSchedules(ctx, c, db)
	if err != nil {
		return err
	}
	retain := func(s *adminpb.BackupSchedule, d time.Duration) error {
		if s.GetRetention().AsDuration() == d {
			return nil
		}
		_, err := c.UpdateBackupSchedule(ctx, &adminpb.UpdateBackupScheduleRequest{
			BackupSchedule: &adminpb.BackupSchedule{Name: s.GetName(), Retention: durationpb.New(d)},
			UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"retention"}}})
		return err
	}
	create := func(s *adminpb.BackupSchedule) error {
		_, err := c.CreateBackupSchedule(ctx, &adminpb.CreateBackupScheduleRequest{Parent: db, BackupSchedule: s})
		return err
	}
	remove := func(s *adminpb.BackupSchedule) error {
		return c.DeleteBackupSchedule(ctx, &adminpb.DeleteBackupScheduleRequest{Name: s.GetName()})
	}
	if values["daily"] == "true" {
		days, err := positiveInt(values["dailyRetention"], "the daily retention period", 98)
		if err != nil {
			return err
		}
		d := time.Duration(days) * 24 * time.Hour
		if daily != nil {
			err = retain(daily, d)
		} else {
			err = create(&adminpb.BackupSchedule{Retention: durationpb.New(d),
				Recurrence: &adminpb.BackupSchedule_DailyRecurrence{DailyRecurrence: &adminpb.DailyRecurrence{}}})
		}
		if err != nil {
			return fmt.Errorf("the daily schedule: %w", err)
		}
	} else if daily != nil {
		if err := remove(daily); err != nil {
			return fmt.Errorf("deleting the daily schedule: %w", err)
		}
	}
	if values["weekly"] == "true" {
		weeks, err := positiveInt(values["weeklyRetention"], "the weekly retention period", 14)
		if err != nil {
			return err
		}
		day, ok := dayofweek.DayOfWeek_value[values["weeklyDay"]]
		if !ok || day == 0 {
			return fmt.Errorf("backup day %q is not a day of the week", values["weeklyDay"])
		}
		d := time.Duration(weeks) * 7 * 24 * time.Hour
		// A schedule's recurrence cannot be changed, so another day is a new
		// schedule in place of the old.
		if weekly != nil && weekly.GetWeeklyRecurrence().GetDay() != dayofweek.DayOfWeek(day) {
			if err := remove(weekly); err != nil {
				return fmt.Errorf("replacing the weekly schedule: %w", err)
			}
			weekly = nil
		}
		if weekly != nil {
			err = retain(weekly, d)
		} else {
			err = create(&adminpb.BackupSchedule{Retention: durationpb.New(d),
				Recurrence: &adminpb.BackupSchedule_WeeklyRecurrence{WeeklyRecurrence: &adminpb.WeeklyRecurrence{Day: dayofweek.DayOfWeek(day)}}})
		}
		if err != nil {
			return fmt.Errorf("the weekly schedule: %w", err)
		}
	} else if weekly != nil {
		if err := remove(weekly); err != nil {
			return fmt.Errorf("deleting the weekly schedule: %w", err)
		}
	}
	return nil
}
