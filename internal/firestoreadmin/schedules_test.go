package firestoreadmin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/type/dayofweek"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func TestBackupSchedulesOverGRPC(t *testing.T) {
	addr, _ := front(t)
	ctx := context.Background()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	c, err := admin.NewFirestoreAdminClient(ctx, option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	db := "projects/p/databases/(default)"
	day := 24 * time.Hour
	d, err := c.CreateBackupSchedule(ctx, &adminpb.CreateBackupScheduleRequest{Parent: db, BackupSchedule: &adminpb.BackupSchedule{
		Retention: durationpb.New(7 * day), Recurrence: &adminpb.BackupSchedule_DailyRecurrence{DailyRecurrence: &adminpb.DailyRecurrence{}}}})
	if err != nil || !strings.HasPrefix(d.Name, db+"/backupSchedules/") || d.CreateTime == nil {
		t.Fatalf("create daily = %v, %v", d, err)
	}
	if _, err := c.CreateBackupSchedule(ctx, &adminpb.CreateBackupScheduleRequest{Parent: db, BackupSchedule: &adminpb.BackupSchedule{
		Retention: durationpb.New(day), Recurrence: &adminpb.BackupSchedule_DailyRecurrence{DailyRecurrence: &adminpb.DailyRecurrence{}}}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second daily schedule: %v, want AlreadyExists", err)
	}
	for name, s := range map[string]*adminpb.BackupSchedule{
		"daily over 7 days": {Retention: durationpb.New(8 * day), Recurrence: &adminpb.BackupSchedule_DailyRecurrence{DailyRecurrence: &adminpb.DailyRecurrence{}}},
		"weekly no day":     {Retention: durationpb.New(day), Recurrence: &adminpb.BackupSchedule_WeeklyRecurrence{WeeklyRecurrence: &adminpb.WeeklyRecurrence{}}},
		"no recurrence":     {Retention: durationpb.New(day)},
		"no retention":      {Recurrence: &adminpb.BackupSchedule_DailyRecurrence{DailyRecurrence: &adminpb.DailyRecurrence{}}},
	} {
		if _, err := c.CreateBackupSchedule(ctx, &adminpb.CreateBackupScheduleRequest{Parent: db, BackupSchedule: s}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", name, err)
		}
	}
	w, err := c.CreateBackupSchedule(ctx, &adminpb.CreateBackupScheduleRequest{Parent: db, BackupSchedule: &adminpb.BackupSchedule{
		Retention: durationpb.New(14 * 7 * day), Recurrence: &adminpb.BackupSchedule_WeeklyRecurrence{WeeklyRecurrence: &adminpb.WeeklyRecurrence{Day: dayofweek.DayOfWeek_MONDAY}}}})
	if err != nil {
		t.Fatalf("create weekly: %v", err)
	}
	list, err := c.ListBackupSchedules(ctx, &adminpb.ListBackupSchedulesRequest{Parent: db})
	if err != nil || len(list.BackupSchedules) != 2 {
		t.Fatalf("list = %v, %v", list, err)
	}
	u, err := c.UpdateBackupSchedule(ctx, &adminpb.UpdateBackupScheduleRequest{BackupSchedule: &adminpb.BackupSchedule{Name: d.Name, Retention: durationpb.New(3 * day)},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"retention"}}})
	if err != nil || u.Retention.AsDuration() != 3*day || u.GetDailyRecurrence() == nil {
		t.Fatalf("update = %v, %v", u, err)
	}
	if _, err := c.UpdateBackupSchedule(ctx, &adminpb.UpdateBackupScheduleRequest{BackupSchedule: &adminpb.BackupSchedule{Name: w.Name,
		Recurrence: &adminpb.BackupSchedule_DailyRecurrence{DailyRecurrence: &adminpb.DailyRecurrence{}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"daily_recurrence"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("changing the recurrence: %v, want InvalidArgument", err)
	}
	if err := c.DeleteBackupSchedule(ctx, &adminpb.DeleteBackupScheduleRequest{Name: d.Name}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetBackupSchedule(ctx, &adminpb.GetBackupScheduleRequest{Name: d.Name}); status.Code(err) != codes.NotFound {
		t.Errorf("get after delete: %v, want NotFound", err)
	}
	if _, err := c.ListBackupSchedules(ctx, &adminpb.ListBackupSchedulesRequest{Parent: "projects/p/databases/nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("list of a missing database: %v, want NotFound", err)
	}
	if _, err := c.ListBackups(ctx, &adminpb.ListBackupsRequest{Parent: "projects/p/locations/nam5"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("ListBackups: %v, want Unimplemented", err)
	}
}

func TestBackupSchedulesOverREST(t *testing.T) {
	addr, _ := front(t)
	base := "http://" + addr + "/v1/projects/p/databases/(default)/backupSchedules"
	do := func(method, url, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	code, s := do("POST", base, `{"retention":"8467200s","weeklyRecurrence":{"day":"SUNDAY"}}`)
	if code != 200 || s["weeklyRecurrence"].(map[string]any)["day"] != "SUNDAY" {
		t.Fatalf("create = %d %v", code, s)
	}
	name := s["name"].(string)
	code, s = do("PATCH", "http://"+addr+"/v1/"+name+"?updateMask=retention", `{"retention":"604800s"}`)
	if code != 200 || s["retention"] != "604800s" {
		t.Fatalf("patch = %d %v", code, s)
	}
	code, s = do("GET", base, "")
	if code != 200 || len(s["backupSchedules"].([]any)) != 1 {
		t.Fatalf("list = %d %v", code, s)
	}
	if code, s = do("DELETE", "http://"+addr+"/v1/"+name, ""); code != 200 {
		t.Fatalf("delete = %d %v", code, s)
	}
	if code, _ = do("GET", "http://"+addr+"/v1/"+name, ""); code != 404 {
		t.Errorf("get after delete = %d, want 404", code)
	}
	if code, _ = do("GET", "http://"+addr+"/v1/projects/p/locations/nam5/backups", ""); code != 501 {
		t.Errorf("list backups = %d, want 501", code)
	}
}
