//go:build browser

package browser

import (
	"context"
	"os"
	"strings"
	"testing"

	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// enterpriseDatabase makes an Enterprise edition database, Firestore with
// MongoDB compatibility's, with the official admin client.
func enterpriseDatabase(t *testing.T, project, dbID string) *admin.FirestoreAdminClient {
	t.Helper()
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
	op, err := ac.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: "projects/" + project, DatabaseId: dbID,
		Database: &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_FIRESTORE_NATIVE, DatabaseEdition: adminpb.Database_ENTERPRISE}})
	if err != nil {
		t.Fatalf("CreateDatabase (Enterprise): %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("CreateDatabase (Enterprise): %v", err)
	}
	return ac
}

// TestFirestoreMongoSecurityAndConnectThroughThePage (#1845, round 3), in
// the emulators shard: Google's Firestore with MongoDB compatibility pages
// (https://docs.cloud.google.com/firestore/mongodb-compatibility/docs/connect).
// An Enterprise database's Security tab's Add User, with a Username and the
// role Cloud Datastore User, shows the password once; GetUserCreds reads
// the user, and the tab lists it with its role. On the Data page, the
// database column's View more opens Connect using MongoDB tools with the
// base connection string, which carries no credentials.
func TestFirestoreMongoSecurityAndConnectThroughThePage(t *testing.T) {
	project := uniqueProject(t)
	dbID := "mongo-sec"
	ac := enterpriseDatabase(t, project, dbID)
	ctx := context.Background()

	p := open(t)
	q := "?project=" + project
	p.navigate("/firestore-databases/" + dbID + q)
	rows := p.pageAction("Add User", map[string]string{"userCredsId": "reader", "role": "roles/datastore.user"}, "")
	uc, err := ac.GetUserCreds(ctx, &adminpb.GetUserCredsRequest{Name: "projects/" + project + "/databases/" + dbID + "/userCreds/reader"})
	if err != nil || uc.GetState() != adminpb.UserCreds_ENABLED {
		t.Fatalf("after Add User, GetUserCreds = %v, %v", uc, err)
	}
	if !strings.Contains(rows, "reader") || !strings.Contains(rows, "Cloud Datastore User") {
		t.Errorf("Add User's dialog reads %q; want the user, its password and its role", rows)
	}

	p.navigate("/firestore-databases/" + dbID + q + "&tab=usercreds")
	p.waitFor(`[...document.querySelectorAll("#view [role=tab]")].some((t) => t.textContent.trim() === "Security")`)
	p.waitFor(rowReady("reader"))
	var row string
	p.eval(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes("reader")).textContent`, &row)
	if !strings.Contains(row, "Cloud Datastore User") {
		t.Errorf("the Security tab's row reads %q; want its role Cloud Datastore User", row)
	}

	p.navigate("/firestore" + q + "&database=" + dbID)
	p.waitFor(`document.querySelector('#fs-collections select.fs-menu') !== null`)
	p.eval(`(() => { const s = document.querySelector("#fs-collections select.fs-menu"); s.value = "connect";
		s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.waitFor(`document.querySelector(".modal.is-open #fs-connect-string") !== null`)
	var cs string
	p.eval(`document.querySelector(".modal.is-open #fs-connect-string").value`, &cs)
	if !strings.HasPrefix(cs, "mongodb://") || !strings.Contains(cs, "/"+dbID+"?loadBalanced=true&tls=true&retryWrites=false") || strings.Contains(cs, "@") {
		t.Errorf("Connect using MongoDB tools shows %q; want the base connection string, without credentials", cs)
	}
	var title string
	p.eval(`document.querySelector(".modal.is-open h2").textContent`, &title)
	if title != "Connect using MongoDB tools" {
		t.Errorf("the dialog is titled %q", title)
	}

	// (default), a Standard database, has neither.
	p.navigate("/firestore" + q)
	p.waitFor(`document.querySelector("#fs-collections") !== null`)
	var menus int
	p.eval(`document.querySelectorAll("#fs-collections select.fs-menu").length`, &menus)
	if menus != 0 {
		t.Error("a Standard database's Data panel offers Connect using MongoDB tools")
	}
}

// TestFirestoreMongoScreensMeetWCAG (#1845, round 3): the Security tab, Add
// User's dialog and Connect using MongoDB tools pass the WCAG 2.1 AA audit.
func TestFirestoreMongoScreensMeetWCAG(t *testing.T) {
	project := uniqueProject(t)
	dbID := "mongo-a11y"
	enterpriseDatabase(t, project, dbID)
	q := "?project=" + project
	auditScreens(t, []a11yPage{
		{"Firestore Security tab", "/firestore-databases/" + dbID + q + "&tab=usercreds",
			settled + ` && document.querySelector("#section-actions-usercreds button") !== null`, ""},
		{"Add User dialog", "/firestore-databases/" + dbID + q + "&tab=usercreds",
			settled + ` && document.querySelector("#section-actions-usercreds button") !== null`,
			`[...document.querySelectorAll("#section-actions-usercreds button")].find((b) => b.textContent.trim() === "Add User").click()`},
		{"Connect using MongoDB tools dialog", "/firestore" + q + "&database=" + dbID,
			`document.querySelector("#fs-collections select.fs-menu") !== null`,
			`((s) => { s.value = "connect"; s.dispatchEvent(new Event("change", { bubbles: true })); })(document.querySelector("#fs-collections select.fs-menu"))`},
	})
}
