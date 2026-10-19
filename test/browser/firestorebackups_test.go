//go:build browser

package browser

import (
	"context"
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
	"google.golang.org/protobuf/types/known/durationpb"
)

// TestFirestoreBulkDeleteAndBackupsThroughTheBrowser (#1500), in the
// emulators shard. On a database's page, Bulk delete documents of the
// collection group "gone", confirmed, deletes its documents and keeps the
// others; the Backups tab lists the backup a daily schedule took, Restore
// on its row makes a database with the backup's documents, and Delete
// backup, confirmed by its ID, removes it. The official clients read each
// change back.
func TestFirestoreBulkDeleteAndBackupsThroughTheBrowser(t *testing.T) {
	needService(t, "firestore")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envFirestore)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through the APIs themselves", envFirestore)
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
	t.Setenv("FIRESTORE_EMULATOR_HOST", addr)
	fs, err := firestore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	for _, path := range []string{"gone/a", "gone/b", "kept/c"} {
		if _, err := fs.Doc(path).Set(ctx, map[string]any{"p": path}); err != nil {
			t.Fatalf("Set %s: %v", path, err)
		}
	}
	count := func(fc *firestore.Client, col string) int {
		t.Helper()
		refs, err := fc.Collection(col).DocumentRefs(ctx).GetAll()
		if err != nil {
			t.Fatalf("documents of %s: %v", col, err)
		}
		n := 0
		for _, r := range refs {
			if s, err := r.Get(ctx); err == nil && s.Exists() {
				n++
			}
		}
		return n
	}

	// Bulk delete documents, of the collection group "gone".
	p.navigate("/firestore-databases/(default)" + q)
	p.clickText("#view .page-actions button", "Bulk delete documents")
	p.waitFor(`document.querySelector(".modal.is-open #f-collectionIds") !== null`)
	p.setField(".modal.is-open #f-collectionIds", "gone")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.confirmByName("(default)")
	p.waitFor(`document.querySelector(".modal") === null`)
	if g, k := count(fs, "gone"), count(fs, "kept"); g != 0 || k != 1 {
		t.Errorf("after Bulk delete of gone: %d in gone, %d in kept; want 0 and 1", g, k)
	}

	// A daily schedule takes a backup, which the Backups tab lists.
	s, err := c.CreateBackupSchedule(ctx, &adminpb.CreateBackupScheduleRequest{Parent: db, BackupSchedule: &adminpb.BackupSchedule{
		Retention: durationpb.New(24 * time.Hour), Recurrence: &adminpb.BackupSchedule_DailyRecurrence{DailyRecurrence: &adminpb.DailyRecurrence{}}}})
	if err != nil {
		t.Fatalf("CreateBackupSchedule: %v", err)
	}
	t.Cleanup(func() {
		_ = c.DeleteBackupSchedule(context.Background(), &adminpb.DeleteBackupScheduleRequest{Name: s.GetName()})
	})
	list, err := c.ListBackups(ctx, &adminpb.ListBackupsRequest{Parent: "projects/" + project + "/locations/-"})
	if err != nil || len(list.GetBackups()) != 1 {
		t.Fatalf("ListBackups = %v, %v; want one", list, err)
	}
	bname := list.GetBackups()[0].GetName()
	id := bname[strings.LastIndex(bname, "/")+1:]
	p.navigate("/firestore-databases/(default)" + q)
	p.waitFor(`document.querySelector("#tab-backups") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-backups`)))
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.offsetParent && r.textContent.includes("Ready"))`)

	// Restore on its row.
	p.cellMenu(id, "Restore")
	p.waitFor(`document.querySelector(".modal.is-open #f-databaseId") !== null`)
	p.setField(".modal.is-open #f-databaseId", "from-backup")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if _, err := c.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: "projects/" + project + "/databases/from-backup"}); err != nil {
		t.Fatalf("GetDatabase of the restored database: %v", err)
	}
	restored, err := firestore.NewClientWithDatabase(ctx, project, "from-backup")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	if n := count(restored, "kept"); n != 1 {
		t.Errorf("the restored database has %d documents in kept; want 1", n)
	}

	// Delete backup, confirmed by its ID.
	p.navigate("/firestore-databases/(default)" + q)
	p.waitFor(`document.querySelector("#tab-backups") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-backups`)))
	p.cellMenu(id, "Delete backup")
	p.confirmByName(id)
	p.waitFor(`document.querySelector(".modal") === null`)
	if l, err := c.ListBackups(ctx, &adminpb.ListBackupsRequest{Parent: "projects/" + project + "/locations/-"}); err != nil || len(l.GetBackups()) != 0 {
		t.Errorf("after Delete backup, ListBackups = %v, %v; want none", l, err)
	}
}
