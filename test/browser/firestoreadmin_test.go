//go:build browser

package browser

import (
	"context"
	"os"
	"strings"
	"testing"

	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestFirestoreDatabaseEditAndIndexFormsThroughTheBrowser (#1343). On a
// project's (default) database page, Edit database opens with Delete
// protection unchecked; checked and saved, the official admin client's
// GetDatabase reads it on. Create index, with two fields and collection
// group scope, makes the index ListIndexes returns; Delete index from its
// row's menu on the Indexes tab, confirmed by its ID, removes it.
func TestFirestoreDatabaseEditAndIndexFormsThroughTheBrowser(t *testing.T) {
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
	p.clickText("#view .page-actions button", "Edit database")
	p.waitFor(`document.querySelector(".modal #f-deleteProtection") !== null`)
	var checked bool
	p.eval(`document.querySelector(".modal #f-deleteProtection").checked`, &checked)
	if checked {
		t.Error("Edit database opens with Delete protection checked on a new database")
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal #f-deleteProtection`)))
	p.run(chromedp.Click(chromedp.CSS(`.modal button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	got, err := c.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: db})
	if err != nil {
		t.Fatalf("GetDatabase: %v", err)
	}
	if got.DeleteProtectionState != adminpb.Database_DELETE_PROTECTION_ENABLED {
		t.Errorf("after Edit database, GetDatabase reads delete protection %v, want enabled", got.DeleteProtectionState)
	}

	p.navigate("/firestore-databases/(default)" + q)
	p.clickText("#view .page-actions button", "Create index")
	p.waitFor(`document.querySelector(".modal #f-fields") !== null`)
	p.setField(".modal #f-collectionGroup", "orders")
	p.setField(".modal #f-fields", "city ascending\ntotal descending")
	p.setField(".modal #f-queryScope", "COLLECTION_GROUP")
	p.run(chromedp.Click(chromedp.CSS(`.modal button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	idx, err := c.ListIndexes(ctx, &adminpb.ListIndexesRequest{Parent: db + "/collectionGroups/orders"}).Next()
	if err != nil {
		t.Fatalf("ListIndexes after Create index: %v", err)
	}
	if idx.QueryScope != adminpb.Index_COLLECTION_GROUP || len(idx.Fields) < 2 || idx.Fields[1].FieldPath != "total" ||
		idx.Fields[1].GetOrder() != adminpb.Index_IndexField_DESCENDING {
		t.Errorf("ListIndexes returns %v", idx)
	}

	id := idx.Name[strings.LastIndex(idx.Name, "/")+1:]
	p.navigate("/firestore-databases/(default)" + q)
	p.cellMenu(id, "Delete index")
	p.confirmByName(id)
	p.waitFor(`document.querySelector(".modal") === null`)
	if _, err := c.ListIndexes(ctx, &adminpb.ListIndexesRequest{Parent: db + "/collectionGroups/orders"}).Next(); err != iterator.Done {
		t.Errorf("after Delete index, ListIndexes: %v, want no index", err)
	}
}
