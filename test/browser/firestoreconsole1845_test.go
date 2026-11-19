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

// TestFirestorePagesMatchGooglesConsoleThroughTheBrowser (#1845), in the
// emulators shard:
//   - Firestore's menu is Google's: Databases first, then Data, Query
//     builder, Indexes, Import/Export, Disaster recovery, Time-to-live,
//     Security rules, Usage, Insights and Key Visualizer, the last four
//     saying they are not available locally;
//   - Time-to-live in the menu opens (default)'s Time-to-live tab, whose
//     Create Policy makes the policy GetField reads, offset and all;
//   - Create a Firestore database is a page: Datastore mode and a Cloud KMS
//     key are offered (#1880, #1881), and a Region choice makes the database
//     GetDatabase reads in that region;
//   - the Query builder runs a WHERE clause against the database's
//     documents, written with the official client, and COUNT counts them.
func TestFirestorePagesMatchGooglesConsoleThroughTheBrowser(t *testing.T) {
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

	// The menu, in Google's order.
	p.navigate("/firestore-databases" + q)
	p.waitFor(`document.querySelectorAll("#product-nav-pages a").length >= 11`)
	var menu []string
	p.eval(`[...document.querySelectorAll("#product-nav-pages a")].map((a) => a.textContent.trim())`, &menu)
	want := "Databases,Data,Query builder,Indexes,Import/Export,Disaster recovery,Time-to-live,Security rules,Usage,Insights,Key Visualizer"
	if got := strings.Join(menu, ","); got != want {
		t.Errorf("the Firestore menu reads %s, want %s", got, want)
	}
	p.navigate("/firestore-rules" + q)
	p.waitFor(`document.querySelector("#view").textContent.includes("Not available locally")`)

	// Time-to-live, from the menu, opens the database's tab.
	p.clickText("#product-nav-pages a", "Time-to-live")
	p.waitFor(`location.pathname === "/firestore-databases/(default)" && new URLSearchParams(location.search).get("tab") === "ttl" &&
		document.querySelector("#section-actions-ttl button") !== null`)
	var current string
	p.eval(`(document.querySelector('#product-nav-pages a[aria-current="page"]') || {}).textContent || ""`, &current)
	if current != "Time-to-live" {
		t.Errorf("the menu marks %q current on the Time-to-live tab", current)
	}
	p.clickText("#section-actions-ttl button", "Create Policy")
	p.waitFor(`document.querySelector(".modal.is-open #f-offsetUnit") !== null`)
	p.setField(".modal.is-open #f-collectionGroup", "sessions")
	p.setField(".modal.is-open #f-field", "expireAt")
	p.setField(".modal.is-open #f-offset", "2")
	p.setField(".modal.is-open #f-offsetUnit", "hours")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	f, err := c.GetField(ctx, &adminpb.GetFieldRequest{Name: "projects/" + project + "/databases/(default)/collectionGroups/sessions/fields/expireAt"})
	if err != nil || f.GetTtlConfig() == nil || f.GetTtlConfig().GetExpirationOffset().AsDuration() != 2*time.Hour {
		t.Errorf("after Create Policy, GetField = %v, %v; want a TTL policy with a 2h offset", f, err)
	}
	p.waitFor(`document.querySelector("#detail-panel").textContent.includes("expireAt")`)

	// Create a Firestore database, as a page.
	p.navigate("/firestore-databases/create" + q)
	p.waitFor(`document.querySelector("#view #f-mode-1") !== null`)
	var page struct {
		Title                    string
		DatastoreOff, KMSOff, On bool
	}
	p.eval(`({ Title: document.querySelector("#view h1").textContent.trim(),
		DatastoreOff: document.querySelector("#f-mode-1").disabled, KMSOff: document.querySelector("#f-encryption-1").disabled,
		On: document.querySelector("#f-mode-0").checked })`, &page)
	// Datastore mode (#1880) and a Cloud KMS key (#1881) are offered.
	if page.Title != "Create a Firestore database" || page.DatastoreOff || page.KMSOff || !page.On {
		t.Errorf("the create page = %+v; want Google's title, Native mode chosen, Datastore mode and Cloud KMS key offered", page)
	}
	dbID := "parity-" + strings.ToLower(strings.ReplaceAll(project, "_", "-"))
	if len(dbID) > 60 {
		dbID = dbID[:60]
	}
	dbID = strings.TrimRight(dbID, "-")
	p.setField("#view #f-database", dbID)
	p.run(chromedp.Click(chromedp.CSS(`#view #f-locationType-0`)))
	p.waitFor(`!document.querySelector("#view #f-region").closest(".form-row").hidden`)
	p.setField("#view #f-region", "europe-west3")
	p.clickText(`#view form button[type="submit"]`, "Create Database")
	deadline := time.Now().Add(30 * time.Second)
	var db *adminpb.Database
	for time.Now().Before(deadline) {
		if db, err = c.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: "projects/" + project + "/databases/" + dbID}); err == nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if err != nil || db.GetLocationId() != "europe-west3" || db.GetType() != adminpb.Database_FIRESTORE_NATIVE {
		t.Fatalf("after Create Database, GetDatabase = %v, %v; want a Native database in europe-west3", db, err)
	}

	// The Query builder, against that database.
	t.Setenv("FIRESTORE_EMULATOR_HOST", addr)
	fs, err := firestore.NewClientWithDatabase(ctx, project, dbID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	for id, pop := range map[string]int{"sf": 870, "la": 3900, "ny": 8400} {
		if _, err := fs.Collection("cities").Doc(id).Set(ctx, map[string]any{"pop": pop}); err != nil {
			t.Fatal(err)
		}
	}
	p.navigate("/firestore-databases/" + dbID + q + "&tab=query")
	p.waitFor(`document.querySelector("#detail-panel .query-form #f-collection") !== null`)
	p.setField("#detail-panel #f-collection", "cities")
	p.clickText("#detail-panel .group-add", "Add WHERE clause")
	p.waitFor(`document.querySelector('#detail-panel [aria-label="Field 1"]') !== null`)
	p.setField(`#detail-panel [aria-label="Field 1"]`, "pop")
	p.setField(`#detail-panel [aria-label="Operator 1"]`, ">")
	p.setField(`#detail-panel [aria-label="Value type 1"]`, "number")
	p.setField(`#detail-panel [aria-label="Value 1"]`, "1000")
	p.clickText(`#detail-panel .query-form button[type="submit"]`, "Run")
	p.waitFor(`document.querySelector("#detail-panel .query-results table") !== null ||
		!document.querySelector("#detail-panel .query-pane .form-error").hidden`)
	var rows string
	p.eval(`(document.querySelector("#detail-panel .query-results") || {}).textContent || document.querySelector("#detail-panel .query-pane .form-error").textContent`, &rows)
	if !strings.Contains(rows, "la") || !strings.Contains(rows, "ny") || strings.Contains(rows, "cities/sf") {
		t.Errorf("the Query builder answers %q; want la and ny, not sf", rows)
	}
	p.setField("#detail-panel #f-aggregation", "count")
	p.clickText(`#detail-panel .query-form button[type="submit"]`, "Run")
	p.waitFor(`document.querySelector("#detail-panel .query-results").textContent.includes("COUNT")`)
	p.eval(`document.querySelector("#detail-panel .query-results").textContent`, &rows)
	if !strings.Contains(rows, "COUNT2") && !strings.Contains(rows, "COUNT 2") {
		t.Errorf("COUNT answers %q, want 2", rows)
	}
}
