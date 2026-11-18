//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// mongoTokenURI is the connection string Google documents for an access
// token ("Authenticate and connect to a database", PLAIN with the user
// access_token), with the instance's CA.
func mongoTokenURI(token, endpoint, database, ca string) string {
	return fmt.Sprintf("mongodb://access_token:%s@%s/%s?loadBalanced=true&tls=true&tlsCAFile=%s&authMechanism=PLAIN&authSource=$external&retryWrites=false",
		url.PathEscape(token), endpoint, database, url.QueryEscape(ca))
}

// TestFirestoreMongoDBIAMToken (#1738): a token sign-in takes only an
// access token CloudBurrow minted (through the official IAM Credentials
// client), and each command needs the permissions Google's IAM page lists
// for it in the principal's roles on the database's project, set through
// the official Resource Manager client: roles/datastore.user reads and
// writes, roles/datastore.viewer reads and is refused a write
// Unauthorized (13), a principal with no role is refused a read, and a
// made-up token does not sign in. MONGODB-OIDC with a callback that
// returns the access token signs in the same way. Taking a role back
// refuses the principal's next command.
func TestFirestoreMongoDBIAMToken(t *testing.T) {
	h := New(t)
	shardEndpoint(h, envFirestoreResourceManager, EnvResourceManager)
	ep, ca := mongoEndpoint(h)
	h.Endpoint(EnvFirestore)
	project := iamProject(t, h, "fm-iam-")
	user := newIAMPrincipal(t, h, project, "fm-user", "roles/datastore.user")
	viewer := newIAMPrincipal(t, h, project, "fm-view", "roles/datastore.viewer")
	nobody := newIAMPrincipal(t, h, project, "fm-none")
	ctx := h.Context()

	admin := firestoreAdmin(t, h)
	id := fmt.Sprintf("mongo-iam-%d", time.Now().UnixNano()%1e8)
	op, err := admin.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: "projects/" + project, DatabaseId: id,
		Database: &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_FIRESTORE_NATIVE, DatabaseEdition: adminpb.Database_ENTERPRISE}})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	db, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	t.Cleanup(func() {
		if op, err := admin.DeleteDatabase(context.Background(), &adminpb.DeleteDatabaseRequest{Name: db.Name}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})

	items := func(token string) *mongo.Collection {
		return mongoConnect(t, mongoTokenURI(token, ep, id, ca)).Database(id).Collection("items")
	}
	doc := bson.D{{Key: "_id", Value: "a"}, {Key: "n", Value: int32(1)}}
	if _, err := items(user.token).InsertOne(ctx, doc); err != nil {
		t.Fatalf("roles/datastore.user's InsertOne: %v", err)
	}
	if n, err := items(user.token).CountDocuments(ctx, bson.D{}); err != nil || n != 1 {
		t.Fatalf("roles/datastore.user's CountDocuments = %d, %v", n, err)
	}
	vc := items(viewer.token)
	if err := vc.FindOne(ctx, bson.D{{Key: "_id", Value: "a"}}).Err(); err != nil {
		t.Errorf("roles/datastore.viewer's FindOne: %v", err)
	}
	if _, err := vc.InsertOne(ctx, bson.D{{Key: "_id", Value: "b"}}); mongoCode(err) != 13 {
		t.Errorf("roles/datastore.viewer's InsertOne = %v: want Unauthorized (13)", err)
	}
	if err := items(nobody.token).FindOne(ctx, bson.D{}).Err(); mongoCode(err) != 13 {
		t.Errorf("a principal without a role's FindOne = %v: want Unauthorized (13)", err)
	}
	if err := items("cbl_made-up-token").FindOne(ctx, bson.D{}).Err(); err == nil || mongoCode(err) == 13 {
		t.Errorf("a made-up token's FindOne = %v: want an authentication failure", err)
	}

	// MONGODB-OIDC, the callback giving the access token.
	cb := func(context.Context, *options.OIDCArgs) (*options.OIDCCredential, error) {
		return &options.OIDCCredential{AccessToken: user.token}, nil
	}
	oc, err := mongo.Connect(options.Client().ApplyURI(fmt.Sprintf("mongodb://%s/%s?loadBalanced=true&tls=true&tlsCAFile=%s&retryWrites=false", ep, id, url.QueryEscape(ca))).
		SetAuth(options.Credential{AuthMechanism: "MONGODB-OIDC", AuthSource: "$external", OIDCMachineCallback: cb}).SetTimeout(30 * time.Second))
	if err != nil {
		t.Fatalf("mongo.Connect (MONGODB-OIDC): %v", err)
	}
	t.Cleanup(func() { _ = oc.Disconnect(context.Background()) })
	if err := oc.Database(id).Collection("items").FindOne(ctx, bson.D{}).Err(); err != nil {
		t.Errorf("MONGODB-OIDC's FindOne: %v", err)
	}

	// Taking the role back refuses the next command.
	viewer.revoke("roles/datastore.viewer")
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := vc.FindOne(ctx, bson.D{}).Err()
		if mongoCode(err) == 13 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("20 s after its role was taken back, the viewer's FindOne = %v: want Unauthorized (13)", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
