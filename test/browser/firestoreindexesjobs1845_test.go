//go:build browser

package browser

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"cloud.google.com/go/firestore"
	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestFirestoreIndexesAndImportExportMatchGooglesThroughTheBrowser (#1845,
// round 2), in the emulators shard:
//   - Indexes is one tab holding Google's Manual and Automatic tabs; Create
//     Index and Create vector index are on Manual and not beside the
//     title, and Create vector index makes the index ListIndexes reads,
//     which the local Firestore's FindNearest then answers;
//   - Export's Destination has Browse, which picks a bucket of this
//     instance's Cloud Storage; the export runs and is a row of the
//     Import/Export tab's jobs table; Export from an earlier point in time
//     is offered, its snapshot time asked for once it is checked (#1942,
//     TestFirestorePointInTimeRecoveryThroughTheBrowser).
func TestFirestoreIndexesAndImportExportMatchGooglesThroughTheBrowser(t *testing.T) {
	needService(t, "firestore")
	needService(t, "storage")
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
	q := "?project=" + project
	p := open(t)

	p.navigate("/firestore-databases/(default)" + q + "&tab=indexes")
	p.waitFor(`document.querySelector("#subtab-indexes") !== null && document.querySelector("#section-actions-indexes") !== null`)
	var tabs struct {
		Top, Sub, Title string
		Hidden          bool
	}
	p.eval(`({ Top: document.querySelector("#tab-indexes").textContent, Hidden: document.querySelector("#tab-fields").hidden,
		Sub: [...document.querySelectorAll(".sub-tabs .tab")].map((b) => b.textContent).join(","),
		Title: document.querySelector(".page-header-actions").textContent })`, &tabs)
	if tabs.Top != "Indexes" || !tabs.Hidden || tabs.Sub != "Manual,Automatic" {
		t.Errorf("the Indexes tab = %+v; want one Indexes tab holding Manual and Automatic", tabs)
	}
	if strings.Contains(tabs.Title, "Create Index") || strings.Contains(tabs.Title, "Add Exemption") {
		t.Errorf("the title line holds the tabs' actions as well: %q", tabs.Title)
	}
	p.clickText("#section-actions-indexes button", "Create vector index")
	p.waitFor(`document.querySelector(".modal.is-open #f-dimension") !== null`)
	p.setField(".modal.is-open #f-collectionGroup", "docs")
	p.setField(".modal.is-open #f-field", "embedding")
	p.setField(".modal.is-open #f-dimension", "3")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null && document.querySelector("#detail-panel").textContent.includes("Vector (3)")`)
	found := false
	it := ac.ListIndexes(ctx, &adminpb.ListIndexesRequest{Parent: "projects/" + project + "/databases/(default)/collectionGroups/docs"})
	for {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range x.GetFields() {
			found = found || (f.GetFieldPath() == "embedding" && f.GetVectorConfig().GetDimension() == 3)
		}
	}
	if !found {
		t.Error("after Create vector index, ListIndexes has no 3-dimension vector index on docs.embedding")
	}
	t.Setenv("FIRESTORE_EMULATOR_HOST", addr)
	fs, err := firestore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	for id, v := range map[string]firestore.Vector64{"a": {1, 0, 0}, "b": {0, 1, 0}} {
		if _, err := fs.Collection("docs").Doc(id).Set(ctx, map[string]any{"embedding": v}); err != nil {
			t.Fatal(err)
		}
	}
	near, err := fs.Collection("docs").FindNearest("embedding", firestore.Vector64{0.9, 0.1, 0}, 1, firestore.DistanceMeasureEuclidean, nil).Documents(ctx).GetAll()
	if err != nil || len(near) != 1 || near[0].Ref.ID != "a" {
		t.Errorf("FindNearest = %v, %v; want a", near, err)
	}
	p.run(chromedp.Click(chromedp.CSS(`#subtab-fields`)))
	p.waitFor(`new URLSearchParams(location.search).get("tab") === "fields" && document.querySelector("#section-actions-fields") !== null`)

	// Import/Export: Browse, the export, its row.
	bucket := strings.ToLower(strings.ReplaceAll(project, "_", "-")) + "-exports"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/storage"+q, `{"name":"`+bucket+`"}`); code != http.StatusOK {
		t.Fatalf("creating the bucket = %d: %s", code, body)
	}
	p.navigate("/firestore-databases/(default)" + q + "&tab=importexport")
	p.waitFor(`document.querySelector("#section-actions-importexport") !== null`)
	p.clickText("#section-actions-importexport button", "Export")
	p.waitFor(`document.querySelector(".modal.is-open #browse-outputUriPrefix") !== null`)
	var snapshot struct{ Disabled, TimeHidden bool }
	p.eval(`({ Disabled: document.querySelector("#f-snapshot").disabled,
		TimeHidden: document.querySelector(".modal.is-open #f-snapshotTime").closest(".form-row").hidden })`, &snapshot)
	if snapshot.Disabled || !snapshot.TimeHidden {
		t.Errorf("Export from an earlier point in time = %+v; want it offered, its snapshot time shown only once it is checked (#1942)", snapshot)
	}
	p.run(chromedp.Click(chromedp.CSS(`#browse-outputUriPrefix`)))
	p.clickText("#browse-list .fs-row-link", bucket)
	// Google's Select folder (#2097): the bucket chosen, then Select.
	p.run(chromedp.Click(chromedp.CSS(`#browse-select`)))
	p.waitFor(`document.querySelector("#f-outputUriPrefix").value === "gs://` + bucket + `/" && document.querySelector("#browse-title") === null`)
	p.setField("#f-outputUriPrefix", "gs://"+bucket+"/run1")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null && document.querySelector("#detail-panel tbody") !== null &&
		document.querySelector("#detail-panel tbody").textContent.includes("gs://` + bucket + `/run1")`)
	var row string
	p.eval(`document.querySelector("#detail-panel tbody").textContent`, &row)
	if !strings.Contains(row, "Export") || !strings.Contains(row, "Successful") {
		t.Errorf("the jobs table's row reads %q; want an Export, Successful", row)
	}
}
