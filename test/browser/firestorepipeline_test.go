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
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestFirestorePipelineAndUserCredsThroughThePage (#1591), in the
// emulators shard. An Enterprise edition database is made with the
// official admin client and holds three cities written with the official
// client. On its page, Run pipeline over the cities collection where pop >
// 100, sorted by pop descending, lists the two larger cities in that
// order; Create user credentials shows the generated password once, and
// the User credentials tab lists the user, which ListUserCreds reads; Disable
// from its row's menu leaves it DISABLED, as GetUserCreds reads it.
func TestFirestorePipelineAndUserCredsThroughThePage(t *testing.T) {
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
	dbID := "pipes"
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
	for id, pop := range map[string]int{"sf": 800, "la": 3900, "tiny": 50} {
		if _, err := client.Collection("cities").Doc(id).Set(ctx, map[string]any{"name": id, "pop": pop}); err != nil {
			t.Fatalf("write cities/%s: %v", id, err)
		}
	}

	p := open(t)
	q := "?project=" + project
	p.navigate("/firestore-databases/" + dbID + q)
	rows := p.pageAction("Run pipeline", map[string]string{"source": "collection", "path": "cities", "field": "pop", "op": ">",
		"value": "100", "orderBy": "pop", "direction": "descending", "limit": "10"}, "")
	la, sf := strings.Index(rows, "cities/la"), strings.Index(rows, "cities/sf")
	if la < 0 || sf < 0 || la > sf || strings.Contains(rows, "cities/tiny") {
		t.Errorf("Run pipeline where pop > 100 by pop descending answers %q; want cities/la then cities/sf, not cities/tiny", rows)
	}

	rows = p.pageAction("Create user credentials", map[string]string{"userCredsId": "app-user"}, "")
	uc, err := ac.GetUserCreds(ctx, &adminpb.GetUserCredsRequest{Name: db + "/userCreds/app-user"})
	if err != nil || uc.GetState() != adminpb.UserCreds_ENABLED {
		t.Fatalf("after Create user credentials, GetUserCreds = %v, %v", uc, err)
	}
	if !strings.Contains(rows, "app-user") || len(strings.TrimSpace(strings.ReplaceAll(rows, "app-user", ""))) < 8 {
		t.Errorf("Create user credentials answers %q; want the user and its generated password", rows)
	}

	p.navigate("/firestore-databases/" + dbID + q)
	p.waitFor(`document.querySelector("#tab-usercreds") !== null`)
	p.clickText("#tab-usercreds", "User credentials")
	p.cellMenu("app-user", "Disable")
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(250 * time.Millisecond) {
		uc, err = ac.GetUserCreds(ctx, &adminpb.GetUserCredsRequest{Name: db + "/userCreds/app-user"})
		if err == nil && uc.GetState() == adminpb.UserCreds_DISABLED {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after Disable, GetUserCreds = %v, %v; want DISABLED", uc, err)
		}
	}
}
