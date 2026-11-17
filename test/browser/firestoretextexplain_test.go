//go:build browser

package browser

import (
	"context"
	"os"
	"strings"
	"testing"

	"cloud.google.com/go/firestore"
	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestFirestoreTextIndexAndExplainThroughThePage (#1738), in the emulators
// shard. An Enterprise edition database is made with the official admin
// client and holds books written with the official client. On its page,
// Create index with two text fields, the MongoDB compatible API scope and
// a language makes a text index, which ListIndexes reads with its search
// config and the Indexes tab shows; Query Explain (MongoDB) of a find
// answers the explain's queryPlanner and executionStats sections.
func TestFirestoreTextIndexAndExplainThroughThePage(t *testing.T) {
	needService(t, "firestore")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envFirestore)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through the Admin API itself", envFirestore)
	}
	ctx := context.Background()
	ac, err := admin.NewFirestoreAdminClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	project := uniqueProject(t)
	dbID := "texts"
	db := "projects/" + project + "/databases/" + dbID
	op, err := ac.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: "projects/" + project, DatabaseId: dbID,
		Database: &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_FIRESTORE_NATIVE, DatabaseEdition: adminpb.Database_ENTERPRISE}})
	if err != nil {
		t.Fatalf("CreateDatabase (Enterprise): %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("CreateDatabase (Enterprise): %v", err)
	}
	t.Setenv("FIRESTORE_EMULATOR_HOST", addr)
	client, err := firestore.NewClientWithDatabase(ctx, project, dbID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	for id, year := range map[string]int{"dune": 1965, "emma": 1815} {
		if _, err := client.Collection("books").Doc(id).Set(ctx, map[string]any{"title": id, "year": year}); err != nil {
			t.Fatalf("write books/%s: %v", id, err)
		}
	}

	p := open(t)
	q := "?project=" + project
	p.navigate("/firestore-databases/" + dbID + q)
	p.pageAction("Create index", map[string]string{"collectionGroup": "books", "fields": "title text\nsummary text",
		"queryScope": "COLLECTION_GROUP", "apiScope": "MONGODB_COMPATIBLE_API", "textLanguage": "en"}, "")
	var text *adminpb.Index
	it := ac.ListIndexes(ctx, &adminpb.ListIndexesRequest{Parent: db + "/collectionGroups/books"})
	for {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListIndexes: %v", err)
		}
		if len(x.Fields) > 0 && x.Fields[0].GetSearchConfig().GetTextSpec() != nil {
			text = x
		}
	}
	if text == nil || text.ApiScope != adminpb.Index_MONGODB_COMPATIBLE_API || text.GetSearchIndexOptions().GetTextLanguage() != "en" {
		t.Fatalf("after Create index, the text index ListIndexes reads = %v", text)
	}
	p.navigate("/firestore-databases/" + dbID + q)
	p.waitFor(`document.querySelector("#tab-indexes") !== null`)
	p.clickText("#tab-indexes", "Indexes")
	p.waitFor(`[...document.querySelectorAll("td")].some((td) => td.textContent.includes("title Text"))`)
	p.waitFor(`[...document.querySelectorAll("td")].some((td) => td.textContent.includes("MongoDB compatible API, text language en"))`)

	rows := p.pageAction("Query Explain (MongoDB)", map[string]string{"collection": "books", "command": "find",
		"query": `{"year": {"$gt": 1900}}`, "verbosity": "executionStats"}, "")
	if !strings.Contains(rows, "queryPlanner") || !strings.Contains(rows, "executionStats") {
		t.Errorf("Query Explain answers %q; want its queryPlanner and executionStats sections", rows)
	}
}
