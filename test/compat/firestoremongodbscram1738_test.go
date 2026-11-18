//go:build compat

package compat

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	"cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// userCredsMember is the IAM principal of the user credentials user of the
// database dbName (projects/{id}/databases/{db}), as Google's
// "Authenticate and connect to a database" grants roles to it:
// principal://firestore.googleapis.com/projects/PROJECT_NUMBER/name/databases/DATABASE_ID/userCreds/USERNAME,
// the project's number read through the official Resource Manager client.
func userCredsMember(t *testing.T, h *Harness, dbName, user string) string {
	t.Helper()
	project, db, _ := strings.Cut(strings.TrimPrefix(dbName, "projects/"), "/databases/")
	rm, err := resourcemanager.NewProjectsClient(h.Context(), rmOptionsAt(h.Endpoint(EnvResourceManager))...)
	if err != nil {
		t.Fatal(err)
	}
	defer rm.Close()
	p, err := rm.GetProject(h.Context(), &resourcemanagerpb.GetProjectRequest{Name: "projects/" + project})
	if status.Code(err) == codes.NotFound {
		// The test's own project, which Firestore takes unregistered: a
		// role is granted on a project Resource Manager has.
		v1 := rmV1(t, h)
		if _, err := v1.Projects.Create(&crmv1.Project{ProjectId: project, Name: "Firestore MongoDB"}).Context(h.Context()).Do(); err != nil {
			t.Fatalf("v1 projects.create %s: %v", project, err)
		}
		t.Cleanup(func() { _, _ = v1.Projects.Delete(project).Do() })
		p, err = rm.GetProject(h.Context(), &resourcemanagerpb.GetProjectRequest{Name: "projects/" + project})
	}
	if err != nil {
		t.Fatalf("GetProject %s: %v", project, err)
	}
	number := strings.TrimPrefix(p.GetName(), "projects/")
	return "principal://firestore.googleapis.com/projects/" + number + "/name/databases/" + db + "/userCreds/" + user
}

// grantUserCreds grants roles on dbName's project to its user credentials
// user, takes them back when the test ends, and waits until db (a driver
// signed in as them) may list its collections, the grant having reached the
// front.
func grantUserCreds(t *testing.T, h *Harness, dbName, user string, db *mongo.Database, roles ...string) {
	t.Helper()
	project, _, _ := strings.Cut(strings.TrimPrefix(dbName, "projects/"), "/databases/")
	change := projectRoles(t, h, project, userCredsMember(t, h, dbName, user))
	for _, role := range roles {
		change(role, true)
		t.Cleanup(func() { change(role, false) })
	}
	waitMongo(t, func() error { _, err := db.ListCollectionNames(h.Context(), bson.D{}); return err }, true)
}

// waitMongo waits up to 20 s until f succeeds (ok) or is refused
// Unauthorized (13) (!ok).
func waitMongo(t *testing.T, f func() error, ok bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := f()
		if (ok && err == nil) || (!ok && mongoCode(err) == 13) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after 20 s: %v (want success: %v)", err, ok)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestFirestoreMongoDBIAMUserCreds (#1738): a SCRAM sign-in is checked
// against its user credentials' principal's roles, as Google documents ("A
// new user credential has no permissions by default"; the role is granted
// to principal://firestore.googleapis.com/projects/PROJECT_NUMBER/name/
// databases/DATABASE_ID/userCreds/USERNAME), set through the official
// Resource Manager client: with no role the user signs in and is refused a
// read Unauthorized (13); roles/datastore.viewer reads and is refused a
// write; roles/datastore.user writes. listDatabases on a token connection
// that has not chosen its database is refused without
// datastore.databases.getMetadata, and lists the database with it.
func TestFirestoreMongoDBIAMUserCreds(t *testing.T) {
	h := New(t)
	ep, ca := mongoEndpoint(h)
	h.Endpoint(EnvFirestore)
	h.Endpoint(EnvResourceManager)
	ctx := h.Context()
	project := iamProject(t, h, "fm-scram-")
	admin := firestoreAdmin(t, h)
	id := fmt.Sprintf("mongo-scram-%d", time.Now().UnixNano()%1e8)
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
	if err := items.FindOne(ctx, bson.D{}).Err(); mongoCode(err) != 13 {
		t.Errorf("user credentials with no role: FindOne = %v, want Unauthorized (13)", err)
	}

	change := projectRoles(t, h, project, userCredsMember(t, h, pdb.Name, "app-user"))
	change("roles/datastore.viewer", true)
	waitMongo(t, func() error { return ignoreNoDocs(items.FindOne(ctx, bson.D{}).Err()) }, true)
	if _, err := items.InsertOne(ctx, bson.D{{Key: "_id", Value: "a"}}); mongoCode(err) != 13 {
		t.Errorf("roles/datastore.viewer: InsertOne = %v, want Unauthorized (13)", err)
	}
	change("roles/datastore.viewer", false)
	change("roles/datastore.user", true)
	t.Cleanup(func() { change("roles/datastore.user", false) })
	waitMongo(t, func() error { _, err := items.InsertOne(ctx, bson.D{{Key: "_id", Value: "a"}}); return err }, true)

	// listDatabases before a token connection chooses its database.
	nobody := newIAMPrincipal(t, h, project, "fm-list")
	list := func() ([]string, error) {
		return mongoConnect(t, mongoTokenURI(nobody.token, ep, id, ca)).ListDatabaseNames(ctx, bson.D{})
	}
	if _, err := list(); mongoCode(err) != 13 {
		t.Errorf("listDatabases with no role = %v, want Unauthorized (13)", err)
	}
	nobody.grant("roles/datastore.viewer")
	t.Cleanup(func() { nobody.revoke("roles/datastore.viewer") })
	waitMongo(t, func() error { _, err := list(); return err }, true)
	if names, err := list(); err != nil || !slices.Contains(names, id) {
		t.Errorf("listDatabases with roles/datastore.viewer = %v, %v: want %s among them", names, err, id)
	}
}

func ignoreNoDocs(err error) error {
	if err == mongo.ErrNoDocuments {
		return nil
	}
	return err
}
