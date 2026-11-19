//go:build browser

package browser

import (
	"context"
	"errors"
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

// TestDatastoreDatabasePickerWritesTheChosenDatabaseThroughTheBrowser
// (#1883), in the emulators shard. The Datastore page's database picker
// lists (default) and the project's Datastore mode databases; chosen, it
// travels as ?database=, and Create entity writes that database, which the
// official client opened on it reads back, while (default) is untouched.
func TestDatastoreDatabasePickerWritesTheChosenDatabaseThroughTheBrowser(t *testing.T) {
	needService(t, "datastore")
	fsAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envFirestore)), "http://")
	dsAddr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envDatastore)), "http://")
	if fsAddr == "" || dsAddr == "" {
		t.Skipf("%s and %s are needed: the database is made through the Firestore Admin API and read back through Datastore", envFirestore, envDatastore)
	}
	ctx := context.Background()
	ac, err := admin.NewFirestoreAdminClient(ctx, option.WithEndpoint(fsAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	project := uniqueProject(t)
	dbID := fmt.Sprintf("picked-%d", time.Now().UnixNano()%1e8)
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
	def, err := datastore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = def.Close() })

	p := open(t)
	p.navigate("/datastore?project=" + project)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#scope-database option")].map((o) => o.value).join(",") === %q`,
		"(default),"+dbID))
	p.eval(fmt.Sprintf(`(() => { const s = document.querySelector("#scope-database"); s.value = %q;
		s.dispatchEvent(new Event("change")); return true; })()`, dbID), nil)
	p.waitFor(fmt.Sprintf(`new URLSearchParams(location.search).get("database") === %q`, dbID))

	p.clickText(`#view button`, "Create entity")
	p.waitFor(`document.querySelector("#f-kind") !== null`)
	p.run(chromedp.SendKeys(chromedp.CSS(`#f-kind`), "Picked"))
	p.run(chromedp.SendKeys(chromedp.CSS(`#f-key`), "p1"))
	p.run(chromedp.Click(chromedp.CSS(`form button[type="submit"]`)))
	key := datastore.NameKey("Picked", "p1", nil)
	var got datastore.PropertyList
	deadline := time.Now().Add(10 * time.Second)
	for err = dc.Get(ctx, key, &got); err != nil && time.Now().Before(deadline); err = dc.Get(ctx, key, &got) {
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Create entity with %s chosen: the database reads %v", dbID, err)
	}
	if err := def.Get(ctx, key, &got); !errors.Is(err, datastore.ErrNoSuchEntity) {
		t.Errorf("(default) holds the entity created in %s: %v", dbID, err)
	}
}
