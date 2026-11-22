//go:build compat

package compat

import (
	"context"
	"fmt"
	"testing"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// TestFirestoreMongoDBDatabaseRoles (#2113): roles on one database, as
// Google grants them. Firestore has no IAM policy of a database's own:
// "Authenticate and connect to a database" grants a role "for this
// specific database" as a project binding with the condition
// resource.name == "projects/PROJECT/databases/DATABASE" (docs read
// 2026-11-21: https://docs.cloud.google.com/firestore/mongodb-compatibility/docs/connect),
// here through the official Resource Manager client. On the MongoDB wire
// connection, through the official MongoDB Go driver: an access token's
// service account with roles/datastore.user on database A and
// roles/datastore.viewer on database B writes A, reads B and is refused a
// write to B Unauthorized (13), and is refused a read of database C, which
// grants it nothing; a user (SCRAM user credentials) of B with
// roles/datastore.user on B writes B, and with the role on A instead is
// refused on its own database.
func TestFirestoreMongoDBDatabaseRoles(t *testing.T) {
	h := New(t)
	shardEndpoint(h, envFirestoreResourceManager, EnvResourceManager)
	ep, ca := mongoEndpoint(h)
	h.Endpoint(EnvFirestore)
	project := iamProject(t, h, "fm-dbroles-")
	p := newIAMPrincipal(t, h, project, "fm-dbrole")
	ctx := h.Context()
	admin := firestoreAdmin(t, h)
	suffix := time.Now().UnixNano() % 1e7
	db := func(letter string) string {
		id := fmt.Sprintf("roles-%s-%d", letter, suffix)
		op, err := admin.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: "projects/" + project, DatabaseId: id,
			Database: &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_FIRESTORE_NATIVE, DatabaseEdition: adminpb.Database_ENTERPRISE}})
		if err != nil {
			t.Fatalf("CreateDatabase %s: %v", id, err)
		}
		d, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("CreateDatabase %s: %v", id, err)
		}
		t.Cleanup(func() {
			if op, err := admin.DeleteDatabase(context.Background(), &adminpb.DeleteDatabaseRequest{Name: d.Name}); err == nil {
				_, _ = op.Wait(context.Background())
			}
		})
		return id
	}
	a, b, c := db("a"), db("b"), db("c")
	on := func(id string) string { return `resource.name == "projects/` + project + `/databases/` + id + `"` }
	member := "serviceAccount:" + p.email
	conditionalRole(t, h, project, member, "roles/datastore.user")(on(a))
	conditionalRole(t, h, project, member, "roles/datastore.viewer")(on(b))

	items := func(id string) *mongo.Collection {
		return mongoConnect(t, mongoTokenURI(p.token, ep, id, ca)).Database(id).Collection("items")
	}
	n := 0
	insert := func(coll *mongo.Collection) func() error {
		return func() error {
			n++
			_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: fmt.Sprintf("d%d", n)}})
			return err
		}
	}
	find := func(coll *mongo.Collection) func() error {
		return func() error {
			err := coll.FindOne(ctx, bson.D{}).Err()
			if err == mongo.ErrNoDocuments {
				return nil
			}
			return err
		}
	}
	waitMongo(t, insert(items(a)), true)
	waitMongo(t, find(items(b)), true)
	if err := insert(items(b))(); mongoCode(err) != 13 {
		t.Errorf("a write to B, where the account is a viewer = %v: want Unauthorized (13)", err)
	}
	if err := find(items(c))(); mongoCode(err) != 13 {
		t.Errorf("a read of C, which grants the account nothing = %v: want Unauthorized (13)", err)
	}

	// A user of database B: user credentials, granted on B, then on A.
	uc, err := admin.CreateUserCreds(ctx, &adminpb.CreateUserCredsRequest{Parent: "projects/" + project + "/databases/" + b, UserCredsId: "b-user", UserCreds: &adminpb.UserCreds{}})
	if err != nil {
		t.Fatalf("CreateUserCreds: %v", err)
	}
	grant := conditionalRole(t, h, project, userCredsMember(t, h, "projects/"+project+"/databases/"+b, "b-user"), "roles/datastore.user")
	userItems := mongoConnect(t, mongoURI("b-user", uc.SecurePassword, ep, b, ca)).Database(b).Collection("items")
	grant(on(b))
	waitMongo(t, insert(userItems), true)
	grant(on(a))
	waitMongo(t, insert(userItems), false)
}
