//go:build browser

package browser

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/type/dayofweek"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestFirestoreBackupSchedulesTabThroughTheBrowser (#1461), in the
// emulators shard. On a database's page, Create backup schedule, weekly on
// Friday with 14 days' retention, makes the schedule the official admin
// client's ListBackupSchedules returns; the Backup schedules tab lists it,
// with its note that the emulator takes no backups. Edit retention on its
// row, to 21 days, is read back by GetBackupSchedule, and Delete backup
// schedule, confirmed by its ID, removes it.
func TestFirestoreBackupSchedulesTabThroughTheBrowser(t *testing.T) {
	needService(t, "firestore")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envFirestore)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through the Admin API itself", envFirestore)
	}
	ctx := context.Background()
	c, err := admin.NewFirestoreAdminClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + project
	db := "projects/" + project + "/databases/(default)"

	p.navigate("/firestore-databases/(default)" + q)
	p.clickText("#view .page-actions button", "Create backup schedule")
	p.waitFor(`document.querySelector(".modal.is-open #f-retentionDays") !== null`)
	p.setField(".modal.is-open #f-recurrence", "weekly")
	p.setField(".modal.is-open #f-day", "FRIDAY")
	p.setField(".modal.is-open #f-retentionDays", "14")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	resp, err := c.ListBackupSchedules(ctx, &adminpb.ListBackupSchedulesRequest{Parent: db})
	if err != nil || len(resp.GetBackupSchedules()) != 1 {
		t.Fatalf("ListBackupSchedules after Create backup schedule = %v, %v; want one schedule", resp, err)
	}
	s := resp.GetBackupSchedules()[0]
	t.Cleanup(func() {
		_ = c.DeleteBackupSchedule(context.Background(), &adminpb.DeleteBackupScheduleRequest{Name: s.GetName()})
	})
	if s.GetWeeklyRecurrence().GetDay() != dayofweek.DayOfWeek_FRIDAY || s.GetRetention().AsDuration() != 14*24*time.Hour {
		t.Errorf("the schedule made is %v; want weekly on Friday, retained 14 days", s)
	}
	id := s.GetName()[strings.LastIndex(s.GetName(), "/")+1:]

	// The Backup schedules tab lists it.
	p.navigate("/firestore-databases/(default)" + q)
	p.waitFor(`document.querySelector("#tab-schedules") !== null`)
	p.run(chromedp.Click(`#tab-schedules`, chromedp.ByQuery))
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.offsetParent && r.textContent.includes("Weekly"))`)
	var tab string
	p.eval(`[...document.querySelectorAll("#view [role=tabpanel]")].filter((e) => !e.hidden).map((e) => e.textContent).join(" ")`, &tab)
	if !strings.Contains(tab, id) || !strings.Contains(tab, "336h0m0s") || !strings.Contains(tab, "take no backups") {
		t.Errorf("the Backup schedules tab reads %q; want schedule %s, 336h retention and the note that no backups are taken", tab, id)
	}

	// Edit retention on the row.
	p.cellMenu(id, "Edit retention")
	p.waitFor(`document.querySelector(".modal.is-open #f-retentionDays") !== null`)
	var days string
	p.eval(`document.querySelector(".modal.is-open #f-retentionDays").value`, &days)
	if days != "14" {
		t.Errorf("Edit retention opens with %q days; want 14", days)
	}
	p.setField(".modal.is-open #f-retentionDays", "21")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	got, err := c.GetBackupSchedule(ctx, &adminpb.GetBackupScheduleRequest{Name: s.GetName()})
	if err != nil || got.GetRetention().AsDuration() != 21*24*time.Hour {
		t.Errorf("after Edit retention, GetBackupSchedule = %v, %v; want 21 days", got, err)
	}

	// Delete backup schedule, confirmed by its ID.
	p.navigate("/firestore-databases/(default)" + q)
	p.waitFor(`document.querySelector("#tab-schedules") !== null`)
	p.run(chromedp.Click(`#tab-schedules`, chromedp.ByQuery))
	p.cellMenu(id, "Delete backup schedule")
	p.confirmByName(id)
	p.waitFor(`document.querySelector(".modal") === null`)
	if resp, err := c.ListBackupSchedules(ctx, &adminpb.ListBackupSchedulesRequest{Parent: db}); err != nil || len(resp.GetBackupSchedules()) != 0 {
		t.Errorf("after Delete backup schedule, ListBackupSchedules = %v, %v; want none", resp, err)
	}
}
