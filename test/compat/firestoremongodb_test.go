//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// EnvFirestoreMongoDB is Firestore with MongoDB compatibility's wire
// protocol (#1703), a second port of the Firestore front, and
// EnvFirestoreMongoDBCA the file of the CA its TLS certificate is of
// (#1738).
const (
	EnvFirestoreMongoDB   = "CLOUDBURROW_TEST_FIRESTORE_MONGODB"
	EnvFirestoreMongoDBCA = "CLOUDBURROW_TEST_FIRESTORE_MONGODB_CA"
)

// mongoEndpoint is the endpoint and its CA file, asked for together.
func mongoEndpoint(h *Harness) (endpoint, ca string) {
	endpoint = h.Endpoint(EnvFirestoreMongoDB)
	ca = os.Getenv(EnvFirestoreMongoDBCA)
	if ca == "" {
		h.t.Skipf("%s is not set: the endpoint serves TLS only", EnvFirestoreMongoDBCA)
	}
	return endpoint, ca
}

// mongoURI is the connection string Google documents for SCRAM
// ("Authenticate and connect to a database"): TLS, trusting the instance's
// CA (tlsCAFile), as Google's endpoint serves TLS only.
func mongoURI(user, password, endpoint, database, ca string) string {
	return fmt.Sprintf("mongodb://%s:%s@%s/%s?loadBalanced=true&authMechanism=SCRAM-SHA-256&tls=true&tlsCAFile=%s&retryWrites=false",
		url.PathEscape(user), url.PathEscape(password), endpoint, database, url.QueryEscape(ca))
}

func mongoConnect(t *testing.T, uri string) *mongo.Client {
	t.Helper()
	c, err := mongo.Connect(options.Client().ApplyURI(uri).SetTimeout(30 * time.Second))
	if err != nil {
		t.Fatalf("mongo.Connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Disconnect(t.Context()) })
	return c
}

func mongoCode(err error) int32 {
	var ce mongo.CommandError
	if errors.As(err, &ce) {
		return ce.Code
	}
	var we mongo.WriteException
	if errors.As(err, &we) && len(we.WriteErrors) > 0 {
		return int32(we.WriteErrors[0].Code)
	}
	var bwe mongo.BulkWriteException
	if errors.As(err, &bwe) && len(bwe.WriteErrors) > 0 {
		return int32(bwe.WriteErrors[0].Code)
	}
	return 0
}

// TestFirestoreMongoDB (#1703): the official MongoDB Go driver connects to
// an Enterprise edition database with the connection string Google
// documents, authenticates by SCRAM-SHA-256 with the user credentials the
// Admin API created, and inserts, finds (filter, sort, limit, projection,
// a cursor over several batches), counts, aggregates, updates, upserts
// and deletes; a unique index refuses a duplicate. The documents are the
// database's Firestore documents: the official Firestore client reads
// what the driver wrote, BSON types mapped as Google documents, and the
// driver reads what the Firestore client wrote. A wrong password, disabled
// credentials and an unauthenticated connection are refused, and so is a
// command Google does not support.
func TestFirestoreMongoDB(t *testing.T) {
	h := New(t)
	ep, ca := mongoEndpoint(h)
	h.Endpoint(EnvFirestore)
	h.Endpoint(EnvResourceManager)
	ctx := h.Context()
	admin := firestoreAdmin(t, h)
	dbName := enterpriseDatabase(t, h, "mongo-ent")
	uc, err := admin.CreateUserCreds(ctx, &adminpb.CreateUserCredsRequest{Parent: dbName, UserCredsId: "app-user", UserCreds: &adminpb.UserCreds{}})
	if err != nil {
		t.Fatalf("CreateUserCreds: %v", err)
	}

	client := mongoConnect(t, mongoURI("app-user", uc.SecurePassword, ep, "mongo-ent", ca))
	if err := client.Ping(ctx, nil); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	// A new user credential has no permissions until a role is granted.
	grantUserCreds(t, h, dbName, "app-user", "roles/datastore.user", client.Database("mongo-ent"))
	books := client.Database("mongo-ent").Collection("books")
	oid := bson.NewObjectID()
	published := time.Date(1965, 8, 1, 0, 0, 0, 0, time.UTC)
	docs := []any{
		bson.D{{Key: "_id", Value: "dune"}, {Key: "title", Value: "Dune"}, {Key: "year", Value: int32(1965)}, {Key: "pages", Value: int64(412)},
			{Key: "rating", Value: 4.5}, {Key: "genre", Value: "sf"}, {Key: "tags", Value: bson.A{"desert", "spice"}},
			{Key: "published", Value: bson.NewDateTimeFromTime(published)}, {Key: "isbn", Value: "1"}},
		bson.D{{Key: "_id", Value: "neuromancer"}, {Key: "title", Value: "Neuromancer"}, {Key: "year", Value: int32(1984)}, {Key: "genre", Value: "sf"}, {Key: "pages", Value: int64(271)}, {Key: "isbn", Value: "2"}},
		bson.D{{Key: "_id", Value: "emma"}, {Key: "title", Value: "Emma"}, {Key: "year", Value: int32(1815)}, {Key: "genre", Value: "classic"}, {Key: "pages", Value: int64(474)}, {Key: "isbn", Value: "3"}},
		bson.D{{Key: "_id", Value: oid}, {Key: "title", Value: "Hyperion"}, {Key: "year", Value: int32(1989)}, {Key: "genre", Value: "sf"}, {Key: "pages", Value: int64(482)}, {Key: "isbn", Value: "4"}},
		bson.D{{Key: "title", Value: "Middlemarch"}, {Key: "year", Value: int32(1871)}, {Key: "genre", Value: "classic"}, {Key: "pages", Value: int64(880)}, {Key: "isbn", Value: "5"}},
	}
	res, err := books.InsertMany(ctx, docs)
	if err != nil || len(res.InsertedIDs) != 5 {
		t.Fatalf("InsertMany = %v, %v", res, err)
	}
	if _, err := books.InsertOne(ctx, bson.D{{Key: "_id", Value: "dune"}}); !mongo.IsDuplicateKeyError(err) {
		t.Errorf("inserting _id dune again: %v, want a duplicate key error", err)
	}

	// A query with a filter, a sort and a limit.
	cur, err := books.Find(ctx, bson.D{{Key: "genre", Value: "sf"}, {Key: "year", Value: bson.D{{Key: "$gte", Value: 1960}}}},
		options.Find().SetSort(bson.D{{Key: "year", Value: -1}}).SetLimit(2).SetProjection(bson.D{{Key: "title", Value: 1}, {Key: "_id", Value: 0}}))
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	var got []bson.D
	if err := cur.All(ctx, &got); err != nil {
		t.Fatalf("Find's cursor: %v", err)
	}
	if len(got) != 2 || got[0][0].Value != "Hyperion" || got[1][0].Value != "Neuromancer" || len(got[0]) != 1 {
		t.Errorf("Find(sf since 1960, newest first, 2) = %v, want Hyperion and Neuromancer's titles alone", got)
	}

	// A cursor over several batches: getMore, then killCursors.
	cur, err = books.Find(ctx, bson.D{}, options.Find().SetBatchSize(2).SetSort(bson.D{{Key: "year", Value: 1}}))
	if err != nil {
		t.Fatalf("Find with batchSize 2: %v", err)
	}
	var years []int32
	for cur.Next(ctx) {
		years = append(years, cur.Current.Lookup("year").Int32())
	}
	if err := cur.Err(); err != nil || len(years) != 5 || years[0] != 1815 || years[4] != 1989 {
		t.Errorf("iterating 5 documents 2 at a time = %v, %v", years, err)
	}
	cur, err = books.Find(ctx, bson.D{}, options.Find().SetBatchSize(1))
	if err != nil || !cur.Next(ctx) {
		t.Fatalf("Find with batchSize 1: %v", err)
	}
	if err := cur.Close(ctx); err != nil {
		t.Errorf("closing a cursor (killCursors): %v", err)
	}

	var dune bson.M
	if err := books.FindOne(ctx, bson.D{{Key: "_id", Value: "dune"}}).Decode(&dune); err != nil {
		t.Fatalf("FindOne(dune): %v", err)
	}
	if dune["year"] != int32(1965) || dune["pages"] != int64(412) || dune["rating"] != 4.5 ||
		dune["published"] != bson.NewDateTimeFromTime(published) {
		t.Errorf("FindOne(dune) = %v: the BSON types did not round-trip", dune)
	}
	var hyperion bson.M
	if err := books.FindOne(ctx, bson.D{{Key: "_id", Value: oid}}).Decode(&hyperion); err != nil || hyperion["title"] != "Hyperion" {
		t.Errorf("FindOne(ObjectId) = %v, %v", hyperion, err)
	}

	n, err := books.CountDocuments(ctx, bson.D{{Key: "genre", Value: "classic"}})
	if err != nil || n != 2 {
		t.Errorf("CountDocuments(classic) = %d, %v, want 2", n, err)
	}
	if n, err := books.EstimatedDocumentCount(ctx); err != nil || n != 5 {
		t.Errorf("EstimatedDocumentCount = %d, %v, want 5", n, err)
	}
	var gs []string
	if err := books.Distinct(ctx, "genre", bson.D{}).Decode(&gs); err != nil || len(gs) != 2 {
		t.Errorf("Distinct(genre) = %v, %v", gs, err)
	}

	// An aggregate: pages by genre.
	agg, err := books.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "pages", Value: bson.D{{Key: "$gt", Value: 0}}}}}},
		{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$genre"}, {Key: "pages", Value: bson.D{{Key: "$sum", Value: "$pages"}}}, {Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}}}}},
		{{Key: "$sort", Value: bson.D{{Key: "_id", Value: 1}}}},
	})
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	var groups []bson.M
	if err := agg.All(ctx, &groups); err != nil {
		t.Fatalf("Aggregate's cursor: %v", err)
	}
	if len(groups) != 2 || groups[0]["_id"] != "classic" || groups[0]["pages"] != int64(1354) || groups[1]["n"] != int32(3) {
		t.Errorf("Aggregate = %v, want classic 1354 pages, sf 3 books", groups)
	}

	// Updates and an upsert.
	ur, err := books.UpdateOne(ctx, bson.D{{Key: "_id", Value: "dune"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "rating", Value: 5.0}}},
		{Key: "$inc", Value: bson.D{{Key: "pages", Value: int64(1)}}}, {Key: "$push", Value: bson.D{{Key: "tags", Value: "worms"}}}})
	if err != nil || ur.MatchedCount != 1 || ur.ModifiedCount != 1 {
		t.Errorf("UpdateOne(dune) = %+v, %v", ur, err)
	}
	ur, err = books.UpdateMany(ctx, bson.D{{Key: "genre", Value: "classic"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "era", Value: "19th"}}}})
	if err != nil || ur.MatchedCount != 2 || ur.ModifiedCount != 2 {
		t.Errorf("UpdateMany(classic) = %+v, %v", ur, err)
	}
	ur, err = books.UpdateOne(ctx, bson.D{{Key: "_id", Value: "solaris"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "year", Value: int32(1961)}}}},
		options.UpdateOne().SetUpsert(true))
	if err != nil || ur.UpsertedCount != 1 || ur.UpsertedID != "solaris" {
		t.Errorf("an upsert = %+v, %v", ur, err)
	}
	var after bson.M
	if err := books.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: "emma"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "read", Value: true}}}},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&after); err != nil || after["read"] != true {
		t.Errorf("FindOneAndUpdate(emma) = %v, %v", after, err)
	}

	// A unique index refuses a duplicate key.
	name, err := books.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "isbn", Value: 1}}, Options: options.Index().SetUnique(true)})
	if err != nil || name != "isbn_1" {
		t.Fatalf("CreateOne(isbn unique) = %q, %v", name, err)
	}
	if _, err := books.InsertOne(ctx, bson.D{{Key: "title", Value: "Copy"}, {Key: "isbn", Value: "1"}}); !mongo.IsDuplicateKeyError(err) {
		t.Errorf("inserting isbn 1 again: %v, want a duplicate key error", err)
	}
	specs, err := books.Indexes().ListSpecifications(ctx)
	if err != nil || len(specs) != 1 || specs[0].Name != "isbn_1" || specs[0].Unique == nil || !*specs[0].Unique {
		t.Errorf("ListSpecifications = %v, %v", specs, err)
	}
	idx, err := admin.ListIndexes(ctx, &adminpb.ListIndexesRequest{Parent: dbName + "/collectionGroups/books"}).Next()
	if err != nil || idx.ApiScope != adminpb.Index_MONGODB_COMPATIBLE_API || !idx.Unique {
		t.Errorf("the Admin API's index = %v, %v: want the MongoDB index", idx, err)
	}

	// The Firestore client sees the same documents.
	t.Setenv("FIRESTORE_EMULATOR_HOST", h.Endpoint(EnvFirestore))
	fs, err := firestore.NewClientWithDatabase(ctx, h.Project(), "mongo-ent")
	if err != nil {
		t.Fatalf("NewClientWithDatabase: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	snap, err := fs.Collection("books").Doc("dune").Get(ctx)
	if err != nil {
		t.Fatalf("the Firestore client reading dune: %v", err)
	}
	d := snap.Data()
	if d["year"] != firestore.BSONInt32(1965) || d["pages"] != int64(413) || d["rating"] != 5.0 || d["title"] != "Dune" {
		t.Errorf("the Firestore client read dune as %v", d)
	}
	if _, err := fs.Collection("books").Doc("~o:" + oid.Hex()).Get(ctx); err != nil {
		t.Errorf("the Firestore client reading the ObjectId document: %v", err)
	}
	if _, err := fs.Collection("books").Doc("solaris-2").Set(ctx, map[string]any{"title": "Solaris 2", "year": firestore.BSONInt32(2000), "pages": 10}); err != nil {
		t.Fatalf("the Firestore client writing: %v", err)
	}
	var s2 bson.M
	if err := books.FindOne(ctx, bson.D{{Key: "year", Value: 2000}}).Decode(&s2); err != nil || s2["_id"] != "solaris-2" || s2["pages"] != int64(10) {
		t.Errorf("the driver reading the Firestore client's document = %v, %v", s2, err)
	}

	// Deletes.
	dr, err := books.DeleteOne(ctx, bson.D{{Key: "_id", Value: "solaris"}})
	if err != nil || dr.DeletedCount != 1 {
		t.Errorf("DeleteOne = %+v, %v", dr, err)
	}
	dr, err = books.DeleteMany(ctx, bson.D{{Key: "genre", Value: "classic"}})
	if err != nil || dr.DeletedCount != 2 {
		t.Errorf("DeleteMany(classic) = %+v, %v", dr, err)
	}
	if _, err := fs.Collection("books").Doc("emma").Get(ctx); err == nil {
		t.Errorf("the Firestore client still reads emma after the driver deleted it")
	}

	// A command Google does not list is refused.
	if err := client.Database("mongo-ent").RunCommand(ctx, bson.D{{Key: "collMod", Value: "books"}}).Err(); mongoCode(err) != 59 {
		t.Errorf("collMod: %v, want CommandNotFound (59)", err)
	}
	if _, err := books.Aggregate(ctx, mongo.Pipeline{{{Key: "$out", Value: "other"}}}); err == nil {
		t.Errorf("an aggregate with $out succeeded; Google does not support $out")
	}

	// TLS only, as Google requires: a cleartext client is refused, and so
	// is one that does not trust the instance's CA.
	clear := mongoConnect(t, strings.Replace(mongoURI("app-user", uc.SecurePassword, ep, "mongo-ent", ca), "tls=true&tlsCAFile="+url.QueryEscape(ca), "tls=false", 1))
	if err := clear.Ping(ctx, nil); err == nil {
		t.Errorf("a cleartext client reached the endpoint; Google's serves TLS only")
	}
	untrusted := mongoConnect(t, strings.Replace(mongoURI("app-user", uc.SecurePassword, ep, "mongo-ent", ca), "&tlsCAFile="+url.QueryEscape(ca), "", 1))
	if err := untrusted.Ping(ctx, nil); err == nil {
		t.Errorf("a client that does not trust the instance's CA connected")
	}

	// Authentication failures.
	if err := mongoConnect(t, mongoURI("app-user", "wrong-password", ep, "mongo-ent", ca)).Ping(ctx, nil); err == nil {
		t.Errorf("a wrong password authenticated")
	}
	if err := mongoConnect(t, mongoURI("nobody", uc.SecurePassword, ep, "mongo-ent", ca)).Ping(ctx, nil); err == nil {
		t.Errorf("an unknown user authenticated")
	}
	anon := mongoConnect(t, "mongodb://"+ep+"/mongo-ent?loadBalanced=true&tls=true&tlsCAFile="+url.QueryEscape(ca)+"&retryWrites=false")
	if err := anon.Database("mongo-ent").Collection("books").FindOne(ctx, bson.D{}).Err(); mongoCode(err) != 13 {
		t.Errorf("an unauthenticated find: %v, want Unauthorized (13)", err)
	}
	if _, err := admin.DisableUserCreds(ctx, &adminpb.DisableUserCredsRequest{Name: uc.Name}); err != nil {
		t.Fatalf("DisableUserCreds: %v", err)
	}
	if err := mongoConnect(t, mongoURI("app-user", uc.SecurePassword, ep, "mongo-ent", ca)).Ping(ctx, nil); err == nil {
		t.Errorf("disabled user credentials authenticated")
	}
	if _, err := admin.EnableUserCreds(ctx, &adminpb.EnableUserCredsRequest{Name: uc.Name}); err != nil {
		t.Fatalf("EnableUserCreds: %v", err)
	}
	reset, err := admin.ResetUserPassword(ctx, &adminpb.ResetUserPasswordRequest{Name: uc.Name})
	if err != nil {
		t.Fatalf("ResetUserPassword: %v", err)
	}
	if err := mongoConnect(t, mongoURI("app-user", uc.SecurePassword, ep, "mongo-ent", ca)).Ping(ctx, nil); err == nil {
		t.Errorf("the password before ResetUserPassword still authenticates")
	}
	if err := mongoConnect(t, mongoURI("app-user", reset.SecurePassword, ep, "mongo-ent", ca)).Ping(ctx, nil); err != nil {
		t.Errorf("the password ResetUserPassword returned: %v", err)
	}
}
