//go:build compat

package compat

import (
	"context"
	"fmt"
	"testing"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	"go.mongodb.org/mongo-driver/v2/bson"
	"google.golang.org/genproto/googleapis/type/expr"
)

// conditionalRole sets member's only binding of role on project to one
// under expression (none when it is empty), through the official Resource
// Manager client with a version 3 read-modify-write, and drops it when the
// test ends.
func conditionalRole(t *testing.T, h *Harness, project, member, role string) func(expression string) {
	t.Helper()
	ctx := h.Context()
	rm, err := resourcemanager.NewProjectsClient(ctx, rmOptionsAt(h.Endpoint(EnvResourceManager))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rm.Close() })
	set := func(expression string) {
		t.Helper()
		pol, err := rm.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + project,
			Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
		if err != nil {
			t.Fatalf("GetIamPolicy: %v", err)
		}
		var bindings []*iampb.Binding
		for _, b := range pol.GetBindings() {
			if b.GetRole() == role && len(b.GetMembers()) == 1 && b.GetMembers()[0] == member {
				continue
			}
			bindings = append(bindings, b)
		}
		if expression != "" {
			bindings = append(bindings, &iampb.Binding{Role: role, Members: []string{member},
				Condition: &expr.Expr{Title: "database", Expression: expression}})
		}
		if _, err := rm.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: "projects/" + project,
			Policy: &iampb.Policy{Version: 3, Etag: pol.GetEtag(), Bindings: bindings}}); err != nil {
			t.Fatalf("SetIamPolicy with %q: %v", expression, err)
		}
	}
	t.Cleanup(func() { set("") })
	return set
}

// TestFirestoreMongoDBIAMConditions (#1742): the SCRAM user credentials'
// role is granted as Google's "Authenticate and connect to a database"
// grants it, with --condition='expression=resource.name ==
// "projects/PROJECT_NAME/databases/DATABASE_ID"', through the official
// Resource Manager client: with the condition naming the user's database
// the user writes; naming another database, it is refused Unauthorized
// (13); a request.time condition in the past grants nothing and one in
// the future grants the role; resource.type and resource.service are
// Firestore's documented values.
func TestFirestoreMongoDBIAMConditions(t *testing.T) {
	h := New(t)
	shardEndpoint(h, envFirestoreResourceManager, EnvResourceManager)
	ep, ca := mongoEndpoint(h)
	h.Endpoint(EnvFirestore)
	h.Endpoint(EnvResourceManager)
	ctx := h.Context()
	project := iamProject(t, h, "fm-cond-")
	admin := firestoreAdmin(t, h)
	id := fmt.Sprintf("mongo-cond-%d", time.Now().UnixNano()%1e8)
	op, err := admin.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: "projects/" + project, DatabaseId: id,
		Database: &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_FIRESTORE_NATIVE, DatabaseEdition: adminpb.Database_ENTERPRISE}})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	pdb, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	t.Cleanup(func() {
		if op, err := admin.DeleteDatabase(context.Background(), &adminpb.DeleteDatabaseRequest{Name: pdb.Name}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	uc, err := admin.CreateUserCreds(ctx, &adminpb.CreateUserCredsRequest{Parent: pdb.Name, UserCredsId: "app-user", UserCreds: &adminpb.UserCreds{}})
	if err != nil {
		t.Fatalf("CreateUserCreds: %v", err)
	}
	items := mongoConnect(t, mongoURI("app-user", uc.SecurePassword, ep, id, ca)).Database(id).Collection("items")
	n := 0
	insert := func() error {
		n++
		_, err := items.InsertOne(ctx, bson.D{{Key: "_id", Value: fmt.Sprintf("d%d", n)}})
		return err
	}
	grant := conditionalRole(t, h, project, userCredsMember(t, h, pdb.Name, "app-user"), "roles/datastore.user")

	grant(`resource.name == "projects/` + project + `/databases/` + id + `"`)
	waitMongo(t, insert, true)
	grant(`resource.name == "projects/` + project + `/databases/another"`)
	waitMongo(t, insert, false)
	grant(`resource.type == "firestore.googleapis.com/Database" && resource.service == "firestore.googleapis.com" && resource.name.endsWith("/` + id + `")`)
	waitMongo(t, insert, true)
	grant(`request.time < timestamp("2001-01-01T00:00:00Z")`)
	waitMongo(t, insert, false)
	grant(`request.time < timestamp("2099-01-01T00:00:00Z")`)
	waitMongo(t, insert, true)
}
