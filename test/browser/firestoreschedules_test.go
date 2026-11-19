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

// TestFirestoreBackupSchedulesTabThroughTheBrowser (#1461, #1845), in the
// emulators shard. The Disaster recovery tab is Google's: Edit, with
// Weekly checked, Friday and 2 weeks' retention, makes the schedule the
// official admin client's ListBackupSchedules returns, and the tab
// describes it above its Backups table. Edit again opens with those values;
// changing the retention to 3 weeks is read back by GetBackupSchedule, and
// unchecking Weekly removes the schedule. Clone, in a database row's menu
// on the Databases list, makes the clone GetDatabase reads.
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
	edit := func() {
		t.Helper()
		p.navigate("/firestore-databases/(default)" + q + "&tab=schedules")
		p.waitFor(`document.querySelector("#section-actions-schedules") !== null`)
		p.clickText("#section-actions-schedules button", "Edit")
		p.waitFor(`document.querySelector(".modal.is-open #f-weekly") !== null`)
	}

	edit()
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #f-weekly`)))
	p.waitFor(`!document.querySelector(".modal.is-open #f-weeklyDay").closest(".form-row").hidden`)
	p.setField(".modal.is-open #f-weeklyDay", "FRIDAY")
	p.setField(".modal.is-open #f-weeklyRetention", "2")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	resp, err := c.ListBackupSchedules(ctx, &adminpb.ListBackupSchedulesRequest{Parent: db})
	if err != nil || len(resp.GetBackupSchedules()) != 1 {
		t.Fatalf("ListBackupSchedules after Edit = %v, %v; want one schedule", resp, err)
	}
	s := resp.GetBackupSchedules()[0]
	t.Cleanup(func() {
		_ = c.DeleteBackupSchedule(context.Background(), &adminpb.DeleteBackupScheduleRequest{Name: s.GetName()})
	})
	if s.GetWeeklyRecurrence().GetDay() != dayofweek.DayOfWeek_FRIDAY || s.GetRetention().AsDuration() != 14*24*time.Hour {
		t.Errorf("the schedule made is %v; want weekly on Friday, retained 2 weeks", s)
	}
	p.waitFor(`document.querySelector("#detail-panel").textContent.includes("every Friday")`)

	// Edit again: the form holds the schedule; 3 weeks.
	edit()
	var form struct {
		Weekly, Daily bool
		Weeks         string
	}
	p.eval(`({ Weekly: document.querySelector(".modal.is-open #f-weekly").checked, Daily: document.querySelector(".modal.is-open #f-daily").checked,
		Weeks: document.querySelector(".modal.is-open #f-weeklyRetention").value })`, &form)
	if !form.Weekly || form.Daily || form.Weeks != "2" {
		t.Errorf("Edit opens with %+v; want Weekly checked, 2 weeks, Daily unchecked", form)
	}
	p.setField(".modal.is-open #f-weeklyRetention", "3")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	got, err := c.GetBackupSchedule(ctx, &adminpb.GetBackupScheduleRequest{Name: s.GetName()})
	if err != nil || got.GetRetention().AsDuration() != 21*24*time.Hour {
		t.Errorf("after Edit, GetBackupSchedule = %v, %v; want 3 weeks", got, err)
	}

	// Weekly unchecked: the schedule goes.
	edit()
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #f-weekly`)))
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if resp, err := c.ListBackupSchedules(ctx, &adminpb.ListBackupSchedulesRequest{Parent: db}); err != nil || len(resp.GetBackupSchedules()) != 0 {
		t.Errorf("after unchecking Weekly, ListBackupSchedules = %v, %v; want none", resp, err)
	}

	// Clone, from the Databases list's row menu.
	p.navigate("/firestore-databases" + q)
	p.cellMenu("(default)", "Clone")
	p.waitFor(`document.querySelector(".modal.is-open #f-databaseId") !== null && document.querySelector(".modal.is-open #f-cloneFrom") !== null`)
	var label string
	p.eval(`document.querySelector("label[for=f-databaseId]").textContent`, &label)
	if !strings.HasPrefix(label, "Give the clone an ID") {
		t.Errorf("Clone's ID field is labelled %q", label)
	}
	p.setField(".modal.is-open #f-databaseId", "row-clone")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if _, err := c.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: "projects/" + project + "/databases/row-clone"}); err != nil {
		t.Errorf("after Clone from the row, GetDatabase: %v", err)
	}
}
