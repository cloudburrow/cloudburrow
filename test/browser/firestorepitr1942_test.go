//go:build browser

package browser

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestFirestorePointInTimeRecoveryThroughTheBrowser (#1942), in the
// emulators shard, through Google's console flows
// (https://docs.cloud.google.com/firestore/native/docs/use-pitr,
// .../manage-databases and .../manage-data/export-import, read 2026-11-20):
//   - Disaster recovery's Edit, Enable point-in-time recovery checked and
//     saved, is the PITR GetDatabase reads, and the tab's settings say it
//     is on with 7 days' retention;
//   - Clone, in the database's row menu, Clone from a whole minute before a
//     write, makes a clone holding the document as it was at that minute;
//   - Import/Export's Export, Export from an earlier point in time at that
//     minute, writes an export that, imported with the official client,
//     holds the document as it was then;
//   - Edit with the checkbox cleared turns point-in-time recovery off.
func TestFirestorePointInTimeRecoveryThroughTheBrowser(t *testing.T) {
	needService(t, "firestore")
	needService(t, "storage")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envFirestore)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through the APIs themselves", envFirestore)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ac, err := admin.NewFirestoreAdminClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	project := uniqueProject(t)
	q := "?project=" + project
	dbName := "projects/" + project + "/databases/(default)"
	p := open(t)

	setPITR := func(on bool) {
		t.Helper()
		p.navigate("/firestore-databases/(default)" + q + "&tab=schedules")
		p.waitFor(`document.querySelector("#section-actions-schedules") !== null`)
		p.clickText("#section-actions-schedules button", "Edit")
		p.waitFor(`document.querySelector(".modal.is-open #f-pitr") !== null`)
		var checked bool
		p.eval(`document.querySelector(".modal.is-open #f-pitr").checked`, &checked)
		if checked != on {
			p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #f-pitr`)))
		}
		p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
		p.waitFor(`document.querySelector(".modal") === null`)
		db, err := ac.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: dbName})
		want := adminpb.Database_POINT_IN_TIME_RECOVERY_DISABLED
		if on {
			want = adminpb.Database_POINT_IN_TIME_RECOVERY_ENABLED
		}
		if err != nil || db.GetPointInTimeRecoveryEnablement() != want {
			t.Fatalf("after Edit with Enable point-in-time recovery %v, GetDatabase = %v, %v", on, db, err)
		}
	}
	setPITR(true)
	p.waitFor(`document.querySelector("#detail-panel").textContent.includes("Point-in-time recovery: on. Retention period: 7 days.")`)

	t.Setenv("FIRESTORE_EMULATOR_HOST", addr)
	fs, err := firestore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	sf := fs.Doc("cities/sf")
	if _, err := sf.Set(ctx, map[string]any{"v": 1}); err != nil {
		t.Fatal(err)
	}
	// The next whole minute, which the write below follows.
	minute := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	time.Sleep(time.Until(minute.Add(time.Second)))
	if _, err := sf.Set(ctx, map[string]any{"v": 2}); err != nil {
		t.Fatal(err)
	}
	valueIn := func(database string) any {
		t.Helper()
		cl, err := firestore.NewClientWithDatabase(ctx, project, database)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cl.Close() }()
		snap, err := cl.Doc("cities/sf").Get(ctx)
		if err != nil {
			t.Fatalf("cities/sf in %s: %v", database, err)
		}
		return snap.Data()["v"]
	}
	local := minute.Format("2006-01-02T15:04")

	// Clone, from the Databases list's row menu, at that minute.
	p.navigate("/firestore-databases" + q)
	p.cellMenu("(default)", "Clone")
	p.waitFor(`document.querySelector(".modal.is-open #f-cloneFrom") !== null`)
	var from struct{ Type, Label string }
	p.eval(`({ Type: document.querySelector(".modal.is-open #f-cloneFrom").type, Label: document.querySelector("label[for=f-cloneFrom]").textContent })`, &from)
	if from.Type != "datetime-local" || !strings.HasPrefix(from.Label, "Clone from") {
		t.Errorf("Clone from = %+v; want a point in time", from)
	}
	p.setField(".modal.is-open #f-databaseId", "pitr-clone")
	p.setField(".modal.is-open #f-cloneFrom", local)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitForWithin(`document.querySelector(".modal") === null`, time.Minute)
	if v := valueIn("pitr-clone"); v != int64(1) {
		t.Errorf("the clone from %s holds v = %v, want 1", local, v)
	}

	// Export from an earlier point in time, at that minute.
	bucket := strings.ToLower(strings.ReplaceAll(project, "_", "-")) + "-pitr"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/storage"+q, `{"name":"`+bucket+`"}`); code != http.StatusOK {
		t.Fatalf("creating the bucket = %d: %s", code, body)
	}
	p.navigate("/firestore-databases/(default)" + q + "&tab=importexport")
	p.waitFor(`document.querySelector("#section-actions-importexport") !== null`)
	p.clickText("#section-actions-importexport button", "Export")
	p.waitFor(`document.querySelector(".modal.is-open #f-snapshot") !== null`)
	p.setField(".modal.is-open #f-outputUriPrefix", "gs://"+bucket+"/at-minute")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #f-snapshot`)))
	p.waitFor(`!document.querySelector(".modal.is-open #f-snapshotTime").closest(".form-row").hidden`)
	p.setField(".modal.is-open #f-snapshotTime", local)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitForWithin(`document.querySelector(".modal") === null`, time.Minute)
	impName := "projects/" + project + "/databases/pitr-import"
	cop, err := ac.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: "projects/" + project, DatabaseId: "pitr-import",
		Database: &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_FIRESTORE_NATIVE}})
	if err == nil {
		_, err = cop.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("CreateDatabase pitr-import: %v", err)
	}
	iop, err := ac.ImportDocuments(ctx, &adminpb.ImportDocumentsRequest{Name: impName, InputUriPrefix: "gs://" + bucket + "/at-minute"})
	if err == nil {
		err = iop.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("importing the console's export at %s: %v", local, err)
	}
	if v := valueIn("pitr-import"); v != int64(1) {
		t.Errorf("the export from %s, imported, holds v = %v, want 1", local, v)
	}
	for _, id := range []string{"pitr-clone", "pitr-import"} {
		if op, err := ac.DeleteDatabase(ctx, &adminpb.DeleteDatabaseRequest{Name: "projects/" + project + "/databases/" + id}); err == nil {
			_, _ = op.Wait(ctx)
		}
	}

	setPITR(false)
	p.waitFor(`document.querySelector("#detail-panel").textContent.includes("Point-in-time recovery: off. Retention period: 1 hour.")`)
}
