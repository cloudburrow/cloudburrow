//go:build browser

package browser

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestDatastoreStudioQueryByGQLIndexesAndImportExportThroughTheBrowser
// (#1846), in the emulators shard. In a Datastore mode database chosen in
// the picker, a kind's query tab switches to Query by GQL and Run query
// lists that database's matching entity; the Datastore page offers Export,
// Import and Indexes, and Indexes opens the database's Indexes page.
func TestDatastoreStudioQueryByGQLIndexesAndImportExportThroughTheBrowser(t *testing.T) {
	needService(t, "datastore")
	fsAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envFirestore)), "http://")
	dsAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envDatastore)), "http://")
	if fsAddr == "" || dsAddr == "" {
		t.Skipf("%s and %s are needed: the database is made through the Firestore Admin API and written through Datastore", envFirestore, envDatastore)
	}
	ctx := context.Background()
	ac, err := admin.NewFirestoreAdminClient(ctx, option.WithEndpoint(fsAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	project := uniqueProject(t)
	dbID := fmt.Sprintf("studio-%d", time.Now().UnixNano()%1e8)
	op, err := ac.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: "projects/" + project, DatabaseId: dbID,
		Database: &adminpb.Database{Type: adminpb.Database_DATASTORE_MODE, LocationId: "nam5"}})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	t.Setenv("DATASTORE_EMULATOR_HOST", dsAddr)
	dc, err := datastore.NewClientWithDatabase(ctx, project, dbID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	type task struct{ Hours int }
	if _, err := dc.PutMulti(ctx, []*datastore.Key{datastore.NameKey("Task", "short", nil), datastore.NameKey("Task", "long", nil)},
		[]*task{{1}, {8}}); err != nil {
		t.Fatal(err)
	}
	q := "?project=" + project + "&database=" + dbID

	p := open(t)
	p.navigate("/datastore/Task" + q)
	p.waitFor(`document.querySelector("#tab-query") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-query`)))
	p.waitFor(`document.querySelector(".query-form #f-queryBy") !== null`)
	p.setField(".query-form #f-queryBy", "Query by GQL")
	p.waitFor(`document.querySelector(".query-form #f-gql") && document.querySelector(".query-form #f-gql").offsetParent !== null`)
	p.setField(".query-form #f-gql", "SELECT * FROM Task WHERE Hours > 4")
	p.run(chromedp.Click(chromedp.CSS(`.query-form button[type="submit"]`)))
	p.waitFor(`document.querySelectorAll(".query-results tbody tr").length === 1`)
	var row string
	p.eval(`document.querySelector(".query-results tbody tr").textContent`, &row)
	if !strings.Contains(row, "name=long") {
		t.Errorf("Query by GQL Hours > 4 lists %q, want name=long", row)
	}

	p.navigate("/datastore" + q)
	for _, label := range []string{"Export", "Import", "Indexes"} {
		p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view a, #view button")].some((b) => b.textContent.trim() === %q)`, label))
	}
	p.clickText("#view a, #view button", "Indexes")
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent.includes("Indexes")`)
	p.waitFor(fmt.Sprintf(`new URLSearchParams(location.search).get("database") === %q`, dbID))
}
