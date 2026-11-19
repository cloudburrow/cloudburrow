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
)

// TestFirestoreDataPanelBrowsesEveryDatabaseThroughTheBrowser (#1845,
// round 2), in the emulators shard. Data is Google's three-column panel
// (collections | documents | fields) over a database other than (default),
// chosen with ?database=: the per-collection filter narrows the documents,
// Add field with the type picker, a field edited in place, a field's delete
// icon, Delete document fields, Add document, Start collection, Edit path,
// Panel settings and Delete collection each change what the official
// client then reads back from that database, and (default) is untouched.
func TestFirestoreDataPanelBrowsesEveryDatabaseThroughTheBrowser(t *testing.T) {
	needService(t, "firestore")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envFirestore)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through the APIs themselves", envFirestore)
	}
	ctx := context.Background()
	ac, err := admin.NewFirestoreAdminClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	project := uniqueProject(t)
	dbID := "panel-db"
	op, err := ac.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: "projects/" + project, DatabaseId: dbID,
		Database: &adminpb.Database{Type: adminpb.Database_FIRESTORE_NATIVE, LocationId: "nam5"}})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	t.Setenv("FIRESTORE_EMULATOR_HOST", addr)
	fs, err := firestore.NewClientWithDatabase(ctx, project, dbID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	def, err := firestore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = def.Close() })
	for id, pop := range map[string]int{"sf": 870, "la": 3900} {
		if _, err := fs.Collection("cities").Doc(id).Set(ctx, map[string]any{"pop": pop, "state": "CA"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fs.Collection("cities").Doc("sf").Collection("landmarks").Doc("gg").Set(ctx, map[string]any{"name": "Golden Gate"}); err != nil {
		t.Fatal(err)
	}

	p := open(t)
	q := "?project=" + project + "&database=" + dbID
	p.navigate("/firestore" + q)
	p.waitFor(`document.querySelector("#fs-panel #fs-collections") !== null &&
		document.querySelector("#fs-collections").textContent.includes("cities")`)
	var head string
	p.eval(`document.querySelector("#fs-collections .fs-column-title").textContent`, &head)
	if head != dbID {
		t.Errorf("the collections column is headed %q, want the database %s", head, dbID)
	}

	// Collections, then documents.
	p.clickText("#fs-collections .fs-row-link", "cities")
	p.waitFor(`document.querySelector("#fs-documents") !== null && document.querySelector("#fs-documents").textContent.includes("la")`)

	// The per-collection filter: pop > 1000 leaves la.
	p.run(chromedp.Click(chromedp.CSS(`#fs-filter`)))
	p.waitFor(`document.querySelector(".modal.is-open #fs-filter-field") !== null`)
	p.setField(".modal.is-open #fs-filter-field", "pop")
	p.setField(".modal.is-open #fs-filter-op", ">")
	p.setField(".modal.is-open #fs-filter-value", "1000")
	p.clickText(".modal.is-open button", "Apply")
	p.waitFor(`document.querySelector(".modal") === null && document.querySelector("#fs-filter").textContent.includes("pop")`)
	var docs []string
	p.eval(`[...document.querySelectorAll("#fs-documents .fs-row-link")].map((b) => b.textContent)`, &docs)
	if strings.Join(docs, ",") != "la" {
		t.Errorf("filtered on pop > 1000 the documents column lists %v, want [la]", docs)
	}
	p.run(chromedp.Click(chromedp.CSS(`#fs-filter`)))
	p.waitFor(`document.querySelector(".modal.is-open #fs-filter-clear") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #fs-filter-clear`)))
	p.waitFor(`document.querySelector(".modal") === null && document.querySelector("#fs-documents").textContent.includes("sf")`)

	// The document: its subcollection and fields.
	p.clickText("#fs-documents .fs-row-link", "sf")
	p.waitFor(`document.querySelector("#fs-document") !== null && document.querySelector("#fs-document").textContent.includes("landmarks") &&
		document.querySelector("#fs-document").textContent.includes("pop")`)

	// Add field, with the type picker.
	p.run(chromedp.Click(chromedp.CSS(`#fs-add-field`)))
	p.waitFor(`document.querySelector(".modal.is-open select#f-type") !== null`)
	var types []string
	p.eval(`[...document.querySelectorAll(".modal.is-open #f-type option")].map((o) => o.value)`, &types)
	if !strings.Contains(strings.Join(types, ","), "string,number,boolean,map,array,null,timestamp,geopoint,reference") {
		t.Errorf("Add field's type picker offers %v", types)
	}
	p.setField(".modal.is-open #f-field", "visited")
	p.setField(".modal.is-open #f-type", "boolean")
	p.setField(".modal.is-open #f-value", "true")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null && document.querySelector("#fs-document").textContent.includes("visited")`)
	readSF := func() map[string]any {
		t.Helper()
		snap, err := fs.Collection("cities").Doc("sf").Get(ctx)
		if err != nil {
			t.Fatalf("reading sf back: %v", err)
		}
		return snap.Data()
	}
	if v := readSF()["visited"]; v != true {
		t.Errorf("after Add field, visited = %v, want true", v)
	}

	// Edit in place: click the field, change its value.
	p.run(chromedp.Click(chromedp.CSS(`#fs-document [aria-label="Edit field pop"]`)))
	p.waitFor(`document.querySelector(".modal.is-open #f-value") !== null`)
	p.setField(".modal.is-open #f-value", "900")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null && document.querySelector("#fs-document").textContent.includes("900")`)
	if v := readSF()["pop"]; v != int64(900) {
		t.Errorf("after editing pop in place, pop = %v (%T), want 900", v, v)
	}

	// A field's delete icon.
	p.run(chromedp.Click(chromedp.CSS(`#fs-document [aria-label="Delete field state"]`)))
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	p.setField(".modal.is-open #confirm-input", "state")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null && !document.querySelector("#fs-document").textContent.includes("state:")`)
	if _, ok := readSF()["state"]; ok {
		t.Error("after the delete icon, sf still has state")
	}

	// Delete document fields, from the document's menu.
	p.setField(`#fs-document .fs-menu`, "deletefields")
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	p.setField(".modal.is-open #confirm-input", "sf")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	deadline := time.Now().Add(10 * time.Second)
	for len(readSF()) != 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if got := readSF(); len(got) != 0 {
		t.Errorf("after Delete document fields, sf holds %v", got)
	}

	// Add document, in the documents column.
	p.waitFor(`document.querySelector("#fs-add-document") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#fs-add-document`)))
	p.waitFor(`document.querySelector(".modal.is-open #f-documentId") !== null`)
	p.setField(".modal.is-open #f-documentId", "ny")
	p.setField(".modal.is-open #f-field", "pop")
	p.setField(".modal.is-open #f-type", "number")
	p.setField(".modal.is-open #f-value", "8400")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null && document.querySelector("#fs-documents").textContent.includes("ny")`)
	if _, err := fs.Collection("cities").Doc("ny").Get(ctx); err != nil {
		t.Errorf("after Add document, ny: %v", err)
	}

	// Edit path opens a subcollection's document.
	p.run(chromedp.Click(chromedp.CSS(`#fs-edit-path`)))
	p.waitFor(`document.querySelector(".modal.is-open #fs-path-input") !== null`)
	p.setField(".modal.is-open #fs-path-input", "cities/sf/landmarks/gg")
	p.clickText(".modal.is-open button", "Go")
	p.waitFor(`document.querySelector("#fs-document") !== null && document.querySelector("#fs-document").textContent.includes("Golden Gate") &&
		document.querySelector("#fs-collections .fs-column-title").textContent === "sf"`)

	// Panel settings: Google's Project panel settings dialog.
	p.run(chromedp.Click(chromedp.CSS(`#fs-panel-settings`)))
	p.waitFor(`document.querySelector(".modal.is-open #fs-show-missing") !== null`)
	var title string
	p.eval(`document.querySelector("#fs-settings-title").textContent + "|" + document.querySelector(".modal.is-open").textContent.includes("Realtime updates enabled")`, &title)
	if title != "Project panel settings|true" {
		t.Errorf("Panel settings opens %q", title)
	}
	p.clickText(".modal.is-open button", "Save")
	p.waitFor(`document.querySelector(".modal") === null`)

	// Start collection, at the database's root.
	p.navigate("/firestore" + q)
	p.waitFor(`document.querySelector("#fs-start-collection") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#fs-start-collection`)))
	p.waitFor(`document.querySelector(".modal.is-open #f-collection") !== null`)
	p.setField(".modal.is-open #f-collection", "towns")
	p.setField(".modal.is-open #f-documentId", "t1")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null && document.querySelector("#fs-collections").textContent.includes("towns")`)
	if _, err := fs.Collection("towns").Doc("t1").Get(ctx); err != nil {
		t.Errorf("after Start collection, towns/t1 in %s: %v", dbID, err)
	}
	if _, err := def.Collection("towns").Doc("t1").Get(ctx); err == nil {
		t.Errorf("Start collection in %s wrote towns/t1 into (default) too", dbID)
	}

	// Delete collection, from the collection's menu: its documents and their
	// subcollections go.
	p.navigate("/firestore" + q + "&path=cities")
	p.waitFor(`document.querySelector("#fs-documents .fs-menu") !== null`)
	p.setField(`#fs-documents .fs-menu`, "deletecollection")
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	p.setField(".modal.is-open #confirm-input", "cities")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null && document.querySelector("#fs-documents") === null`)
	left, err := fs.Collection("cities").DocumentRefs(ctx).GetAll()
	if err != nil || len(left) != 0 {
		t.Errorf("after Delete collection, cities lists %d documents (%v)", len(left), err)
	}
	if _, err := fs.Collection("cities").Doc("sf").Collection("landmarks").Doc("gg").Get(ctx); err == nil {
		t.Error("after Delete collection, cities/sf/landmarks/gg is still there")
	}
}
