//go:build compat

package compat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cloud.google.com/go/firestore"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// mongoDatabase is an Enterprise edition database of its own, id, with
// user credentials, and the official driver connected to it as Google
// documents.
func mongoDatabase(t *testing.T, h *Harness, id string) (*mongo.Database, string) {
	t.Helper()
	ep := h.Endpoint(EnvFirestoreMongoDB)
	h.Endpoint(EnvFirestore)
	admin := firestoreAdmin(t, h)
	dbName := enterpriseDatabase(t, h, id)
	uc, err := admin.CreateUserCreds(h.Context(), &adminpb.CreateUserCredsRequest{Parent: dbName, UserCredsId: "app-user", UserCreds: &adminpb.UserCreds{}})
	if err != nil {
		t.Fatalf("CreateUserCreds: %v", err)
	}
	return mongoConnect(t, mongoURI("app-user", uc.SecurePassword, ep, id)).Database(id), dbName
}

func hasLabel(err error, label string) bool {
	var le interface{ HasErrorLabel(string) bool }
	return errors.As(err, &le) && le.HasErrorLabel(label)
}

// TestFirestoreMongoDBTransactions (#1738): sessions and multi-document
// transactions through the official MongoDB driver, as Google documents
// them (optimistic concurrency with snapshot isolation): a transaction's
// writes are invisible until it commits and then all visible, to the
// driver and to the official Firestore client alike; an aborted one
// writes nothing; its reads see its snapshot, not a later write, and a
// write another client made since fails its commit with WriteConflict and
// the TransientTransactionError label, which WithTransaction retries; a
// command MongoDB refuses in a transaction is refused.
func TestFirestoreMongoDBTransactions(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	db, _ := mongoDatabase(t, h, "mongo-txn")
	accounts := db.Collection("accounts")
	if _, err := accounts.InsertMany(ctx, []any{bson.D{{Key: "_id", Value: "alice"}, {Key: "balance", Value: int32(100)}},
		bson.D{{Key: "_id", Value: "bob"}, {Key: "balance", Value: int32(0)}}}); err != nil {
		t.Fatalf("InsertMany: %v", err)
	}
	balance := func(id string) int32 {
		t.Helper()
		var d bson.M
		if err := accounts.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&d); err != nil {
			t.Fatalf("FindOne(%s): %v", id, err)
		}
		return d["balance"].(int32)
	}

	sess, err := db.Client().StartSession()
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.EndSession(ctx)
	sctx := mongo.NewSessionContext(ctx, sess)

	// A transfer: invisible before the commit, all of it after.
	if err := sess.StartTransaction(); err != nil {
		t.Fatalf("StartTransaction: %v", err)
	}
	if _, err := accounts.UpdateOne(sctx, bson.D{{Key: "_id", Value: "alice"}}, bson.D{{Key: "$inc", Value: bson.D{{Key: "balance", Value: int32(-30)}}}}); err != nil {
		t.Fatalf("UpdateOne(alice) in the transaction: %v", err)
	}
	if _, err := accounts.UpdateOne(sctx, bson.D{{Key: "_id", Value: "bob"}}, bson.D{{Key: "$inc", Value: bson.D{{Key: "balance", Value: int32(30)}}}}); err != nil {
		t.Fatalf("UpdateOne(bob) in the transaction: %v", err)
	}
	if _, err := accounts.InsertOne(sctx, bson.D{{Key: "_id", Value: "log-1"}, {Key: "amount", Value: int32(30)}}); err != nil {
		t.Fatalf("InsertOne in the transaction: %v", err)
	}
	var inTxn bson.M
	if err := accounts.FindOne(sctx, bson.D{{Key: "_id", Value: "alice"}}).Decode(&inTxn); err != nil || inTxn["balance"] != int32(70) {
		t.Errorf("the transaction reading its own write = %v, %v, want 70", inTxn, err)
	}
	if b := balance("alice"); b != 100 {
		t.Errorf("outside the transaction alice's balance is %d before the commit, want 100", b)
	}
	if err := accounts.FindOne(ctx, bson.D{{Key: "_id", Value: "log-1"}}).Err(); !errors.Is(err, mongo.ErrNoDocuments) {
		t.Errorf("outside the transaction its insert is visible before the commit: %v", err)
	}
	if err := sess.CommitTransaction(ctx); err != nil {
		t.Fatalf("CommitTransaction: %v", err)
	}
	if a, b := balance("alice"), balance("bob"); a != 70 || b != 30 {
		t.Errorf("after the commit alice and bob have %d and %d, want 70 and 30", a, b)
	}
	t.Setenv("FIRESTORE_EMULATOR_HOST", h.Endpoint(EnvFirestore))
	fs, err := firestore.NewClientWithDatabase(ctx, h.Project(), "mongo-txn")
	if err != nil {
		t.Fatalf("NewClientWithDatabase: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	if snap, err := fs.Collection("accounts").Doc("log-1").Get(ctx); err != nil || snap.Data()["amount"] != firestore.BSONInt32(30) {
		t.Errorf("the Firestore client reading the committed insert: %v, %v", snap, err)
	}

	// An aborted transaction writes nothing.
	if err := sess.StartTransaction(); err != nil {
		t.Fatalf("StartTransaction: %v", err)
	}
	if _, err := accounts.InsertOne(sctx, bson.D{{Key: "_id", Value: "ghost"}}); err != nil {
		t.Fatalf("InsertOne in the transaction: %v", err)
	}
	if err := sess.AbortTransaction(ctx); err != nil {
		t.Fatalf("AbortTransaction: %v", err)
	}
	if err := accounts.FindOne(ctx, bson.D{{Key: "_id", Value: "ghost"}}).Err(); !errors.Is(err, mongo.ErrNoDocuments) {
		t.Errorf("an aborted transaction's insert is visible: %v", err)
	}

	// Snapshot isolation: a later write is not seen, and fails the commit.
	if err := sess.StartTransaction(); err != nil {
		t.Fatalf("StartTransaction: %v", err)
	}
	if b := func() int32 {
		var d bson.M
		_ = accounts.FindOne(sctx, bson.D{{Key: "_id", Value: "alice"}}).Decode(&d)
		v, _ := d["balance"].(int32)
		return v
	}(); b != 70 {
		t.Errorf("the transaction read alice's balance as %d, want 70", b)
	}
	if _, err := accounts.UpdateOne(ctx, bson.D{{Key: "_id", Value: "alice"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "balance", Value: int32(5)}}}}); err != nil {
		t.Fatalf("UpdateOne(alice) outside the transaction: %v", err)
	}
	var again bson.M
	if err := accounts.FindOne(sctx, bson.D{{Key: "_id", Value: "alice"}}).Decode(&again); err != nil || again["balance"] != int32(70) {
		t.Errorf("the transaction read alice again as %v, %v; want its snapshot's 70", again, err)
	}
	if _, err := accounts.UpdateOne(sctx, bson.D{{Key: "_id", Value: "alice"}}, bson.D{{Key: "$inc", Value: bson.D{{Key: "balance", Value: int32(1)}}}}); err != nil {
		t.Fatalf("UpdateOne(alice) in the transaction: %v", err)
	}
	err = sess.CommitTransaction(ctx)
	if mongoCode(err) != 112 || !hasLabel(err, "TransientTransactionError") {
		t.Errorf("committing over a conflicting write: %v, want WriteConflict (112) with TransientTransactionError", err)
	}
	if b := balance("alice"); b != 5 {
		t.Errorf("alice's balance is %d after the failed commit, want the other write's 5", b)
	}

	// WithTransaction runs a conflicting transaction again.
	attempts := 0
	_, err = sess.WithTransaction(ctx, func(sctx context.Context) (any, error) {
		attempts++
		var d bson.M
		if err := accounts.FindOne(sctx, bson.D{{Key: "_id", Value: "bob"}}).Decode(&d); err != nil {
			return nil, err
		}
		if attempts == 1 {
			if _, err := accounts.UpdateOne(ctx, bson.D{{Key: "_id", Value: "bob"}}, bson.D{{Key: "$inc", Value: bson.D{{Key: "balance", Value: int32(1)}}}}); err != nil {
				return nil, err
			}
		}
		return accounts.UpdateOne(sctx, bson.D{{Key: "_id", Value: "bob"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "balance", Value: d["balance"].(int32) * 2}}}})
	})
	if err != nil || attempts != 2 {
		t.Errorf("WithTransaction = %v after %d attempts, want success after 2", err, attempts)
	}
	if b := balance("bob"); b != 62 {
		t.Errorf("bob's balance is %d, want (30+1)*2", b)
	}

	// A command MongoDB refuses in a transaction, and a duplicate key,
	// abort it.
	if err := sess.StartTransaction(); err != nil {
		t.Fatalf("StartTransaction: %v", err)
	}
	if err := db.RunCommand(sctx, bson.D{{Key: "count", Value: "accounts"}}).Err(); mongoCode(err) != 263 {
		t.Errorf("count in a transaction: %v, want OperationNotSupportedInTransaction (263)", err)
	}
	_ = sess.AbortTransaction(ctx)
	if err := sess.StartTransaction(); err != nil {
		t.Fatalf("StartTransaction: %v", err)
	}
	if _, err := accounts.InsertOne(sctx, bson.D{{Key: "_id", Value: "alice"}}); !mongo.IsDuplicateKeyError(err) {
		t.Errorf("a duplicate _id in a transaction: %v", err)
	}
	if err := sess.CommitTransaction(ctx); mongoCode(err) != 251 {
		t.Errorf("committing a transaction a duplicate key aborted: %v, want NoSuchTransaction (251)", err)
	}
}

// TestFirestoreMongoDBPositional (#1738): the positional update operators
// ($, $[], $[<identifier>]) with arrayFilters, and the positional
// projection, through the official driver.
func TestFirestoreMongoDBPositional(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	db, _ := mongoDatabase(t, h, "mongo-pos")
	students := db.Collection("students")
	if _, err := students.InsertOne(ctx, bson.D{{Key: "_id", Value: "s1"}, {Key: "grades", Value: bson.A{int32(80), int32(85), int32(90)}},
		{Key: "items", Value: bson.A{bson.D{{Key: "n", Value: "a"}, {Key: "q", Value: int32(1)}}, bson.D{{Key: "n", Value: "b"}, {Key: "q", Value: int32(5)}}}}}); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}
	get := func() bson.M {
		var d bson.M
		if err := students.FindOne(ctx, bson.D{{Key: "_id", Value: "s1"}}).Decode(&d); err != nil {
			t.Fatalf("FindOne: %v", err)
		}
		return d
	}
	if _, err := students.UpdateOne(ctx, bson.D{{Key: "_id", Value: "s1"}, {Key: "grades", Value: int32(85)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "grades.$", Value: int32(86)}}}}); err != nil {
		t.Fatalf("UpdateOne($): %v", err)
	}
	if g := get()["grades"].(bson.A); g[1] != int32(86) {
		t.Errorf("after grades.$ = 86, grades = %v", g)
	}
	if _, err := students.UpdateOne(ctx, bson.D{{Key: "_id", Value: "s1"}}, bson.D{{Key: "$inc", Value: bson.D{{Key: "grades.$[]", Value: int32(1)}}}}); err != nil {
		t.Fatalf("UpdateOne($[]): %v", err)
	}
	if g := get()["grades"].(bson.A); g[0] != int32(81) || g[2] != int32(91) {
		t.Errorf("after grades.$[] += 1, grades = %v", g)
	}
	if _, err := students.UpdateOne(ctx, bson.D{{Key: "_id", Value: "s1"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "grades.$[g]", Value: int32(100)}}}},
		options.UpdateOne().SetArrayFilters([]any{bson.D{{Key: "g", Value: bson.D{{Key: "$gte", Value: 87}}}}})); err != nil {
		t.Fatalf("UpdateOne($[g]): %v", err)
	}
	if g := get()["grades"].(bson.A); g[0] != int32(81) || g[1] != int32(100) || g[2] != int32(100) {
		t.Errorf("after grades.$[g >= 87] = 100, grades = %v", g)
	}
	var after bson.M
	if err := students.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: "s1"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "items.$[it].q", Value: int32(0)}}}},
		options.FindOneAndUpdate().SetArrayFilters([]any{bson.D{{Key: "it.n", Value: "b"}}}).SetReturnDocument(options.After)).Decode(&after); err != nil {
		t.Fatalf("FindOneAndUpdate($[it]): %v", err)
	}
	if it := after["items"].(bson.A); it[1].(bson.D)[1].Value != int32(0) {
		t.Errorf("after items.$[it.n == b].q = 0, items = %v", it)
	}
	if _, err := students.UpdateOne(ctx, bson.D{{Key: "_id", Value: "s1"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "grades.$[x]", Value: 1}}}}); mongoCode(err) != 2 {
		t.Errorf("$[x] without an array filter: %v, want BadValue (2)", err)
	}
	var proj bson.M
	if err := students.FindOne(ctx, bson.D{{Key: "items.n", Value: "a"}}, options.FindOne().SetProjection(bson.D{{Key: "items.$", Value: 1}})).Decode(&proj); err != nil {
		t.Fatalf("FindOne with items.$: %v", err)
	}
	if it, _ := proj["items"].(bson.A); len(it) != 1 || it[0].(bson.D)[0].Value != "a" {
		t.Errorf("the positional projection = %v, want items a alone", proj)
	}
}

// TestFirestoreMongoDBLookupPipeline (#1738): $lookup with let and a
// pipeline, and a collection-less aggregation ($documents).
func TestFirestoreMongoDBLookupPipeline(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	db, _ := mongoDatabase(t, h, "mongo-lookup")
	if _, err := db.Collection("orders").InsertMany(ctx, []any{bson.D{{Key: "_id", Value: "o1"}, {Key: "item", Value: "a"}, {Key: "qty", Value: int32(5)}},
		bson.D{{Key: "_id", Value: "o2"}, {Key: "item", Value: "b"}, {Key: "qty", Value: int32(1)}}}); err != nil {
		t.Fatalf("InsertMany(orders): %v", err)
	}
	if _, err := db.Collection("stock").InsertMany(ctx, []any{bson.D{{Key: "_id", Value: "s1"}, {Key: "sku", Value: "a"}, {Key: "inStock", Value: int32(10)}},
		bson.D{{Key: "_id", Value: "s2"}, {Key: "sku", Value: "a"}, {Key: "inStock", Value: int32(2)}}, bson.D{{Key: "_id", Value: "s3"}, {Key: "sku", Value: "b"}, {Key: "inStock", Value: int32(7)}}}); err != nil {
		t.Fatalf("InsertMany(stock): %v", err)
	}
	cur, err := db.Collection("orders").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "stock"}, {Key: "let", Value: bson.D{{Key: "it", Value: "$item"}, {Key: "q", Value: "$qty"}}},
			{Key: "pipeline", Value: bson.A{
				bson.D{{Key: "$match", Value: bson.D{{Key: "$expr", Value: bson.D{{Key: "$and", Value: bson.A{
					bson.D{{Key: "$eq", Value: bson.A{"$sku", "$$it"}}}, bson.D{{Key: "$gte", Value: bson.A{"$inStock", "$$q"}}}}}}}}}},
				bson.D{{Key: "$project", Value: bson.D{{Key: "_id", Value: 1}}}}}},
			{Key: "as", Value: "fits"}}}},
		{{Key: "$sort", Value: bson.D{{Key: "_id", Value: 1}}}},
	})
	if err != nil {
		t.Fatalf("Aggregate($lookup with a pipeline): %v", err)
	}
	var got []bson.M
	if err := cur.All(ctx, &got); err != nil {
		t.Fatalf("the cursor: %v", err)
	}
	if len(got) != 2 || len(got[0]["fits"].(bson.A)) != 1 || got[0]["fits"].(bson.A)[0].(bson.D)[0].Value != "s1" ||
		got[1]["fits"].(bson.A)[0].(bson.D)[0].Value != "s3" {
		t.Errorf("$lookup with let and a pipeline = %v, want o1 fits s1, o2 fits s3", got)
	}

	cur, err = db.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$documents", Value: bson.A{bson.D{{Key: "x", Value: int32(1)}}, bson.D{{Key: "x", Value: int32(2)}}, bson.D{{Key: "x", Value: int32(3)}}}}},
		{{Key: "$match", Value: bson.D{{Key: "x", Value: bson.D{{Key: "$gte", Value: 2}}}}}},
		{{Key: "$group", Value: bson.D{{Key: "_id", Value: nil}, {Key: "sum", Value: bson.D{{Key: "$sum", Value: "$x"}}}}}},
	})
	if err != nil {
		t.Fatalf("a collection-less Aggregate($documents): %v", err)
	}
	got = nil
	if err := cur.All(ctx, &got); err != nil || len(got) != 1 || got[0]["sum"] != int32(5) {
		t.Errorf("$documents, $match, $group = %v, %v, want a sum of 5", got, err)
	}
	if _, err := db.Aggregate(ctx, mongo.Pipeline{{{Key: "$match", Value: bson.D{}}}}); mongoCode(err) != 73 {
		t.Errorf("a collection-less aggregation without $documents: %v, want InvalidNamespace (73)", err)
	}
}

// TestFirestoreMongoDBExplain (#1738): the explain command, of find,
// aggregate, count and update, in the verbosities Google lists.
func TestFirestoreMongoDBExplain(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	db, _ := mongoDatabase(t, h, "mongo-explain")
	orders := db.Collection("orders")
	for i := 0; i < 5; i++ {
		if _, err := orders.InsertOne(ctx, bson.D{{Key: "user_id", Value: int32(i % 2)}, {Key: "n", Value: int32(i)}}); err != nil {
			t.Fatalf("InsertOne: %v", err)
		}
	}
	var res bson.M
	err := db.RunCommand(ctx, bson.D{{Key: "explain", Value: bson.D{{Key: "find", Value: "orders"}, {Key: "filter", Value: bson.D{{Key: "user_id", Value: 1}}},
		{Key: "sort", Value: bson.D{{Key: "n", Value: 1}}}}}, {Key: "verbosity", Value: "executionStats"}}).Decode(&res)
	if err != nil {
		t.Fatalf("explain(find, executionStats): %v", err)
	}
	stats, _ := res["executionStats"].(bson.D)
	if n := lookupD(stats, "nReturned"); n != int64(2) || lookupD(stats, "totalDocsExamined") != int64(5) {
		t.Errorf("executionStats = %v, want 2 returned of 5 examined", stats)
	}
	if txt, _ := res["explainText"].(string); !strings.Contains(txt, "TableScan") || !strings.Contains(txt, "MajorSort") || !strings.Contains(txt, "results returned: 2") {
		t.Errorf("explainText = %q, want Google's tree with a TableScan and a MajorSort", txt)
	}
	res = nil
	if err := db.RunCommand(ctx, bson.D{{Key: "explain", Value: bson.D{{Key: "count", Value: "orders"}}}, {Key: "verbosity", Value: "queryPlanner"}}).Decode(&res); err != nil {
		t.Fatalf("explain(count, queryPlanner): %v", err)
	}
	if _, ok := res["executionStats"]; ok || res["queryPlanner"] == nil {
		t.Errorf("queryPlanner verbosity = %v, want the plan without executionStats", res)
	}
	if err := db.RunCommand(ctx, bson.D{{Key: "explain", Value: bson.D{{Key: "update", Value: "orders"},
		{Key: "updates", Value: bson.A{bson.D{{Key: "q", Value: bson.D{}}, {Key: "u", Value: bson.D{{Key: "$set", Value: bson.D{{Key: "x", Value: 1}}}}}, {Key: "multi", Value: true}}}}}},
		{Key: "verbosity", Value: "executionStats"}}).Err(); err != nil {
		t.Fatalf("explain(update): %v", err)
	}
	if n, err := orders.CountDocuments(ctx, bson.D{{Key: "x", Value: 1}}); err != nil || n != 0 {
		t.Errorf("an explained update wrote %d documents, %v", n, err)
	}
	if err := db.RunCommand(ctx, bson.D{{Key: "explain", Value: bson.D{{Key: "aggregate", Value: "orders"},
		{Key: "pipeline", Value: bson.A{bson.D{{Key: "$match", Value: bson.D{{Key: "user_id", Value: 0}}}}}}, {Key: "cursor", Value: bson.D{}}}}}).Err(); err != nil {
		t.Errorf("explain(aggregate): %v", err)
	}
	if err := db.RunCommand(ctx, bson.D{{Key: "aggregate", Value: "orders"}, {Key: "pipeline", Value: bson.A{}}, {Key: "explain", Value: true}}).Err(); mongoCode(err) != 115 {
		t.Errorf("aggregate with explain: true: %v, want CommandNotSupported (115), as Google refuses it", err)
	}
}

func lookupD(d bson.D, key string) any {
	for _, e := range d {
		if e.Key == key {
			return e.Value
		}
	}
	return nil
}

// TestFirestoreMongoDBText (#1738): a text index, and $text in find and
// aggregate with Google's search string syntax and {$meta: "textScore"}.
func TestFirestoreMongoDBText(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	db, dbName := mongoDatabase(t, h, "mongo-text")
	cities := db.Collection("cities")
	if _, err := cities.InsertMany(ctx, []any{
		bson.D{{Key: "_id", Value: "paris"}, {Key: "country", Value: "France"}, {Key: "food", Value: "the best french bread is here"}},
		bson.D{{Key: "_id", Value: "lyon"}, {Key: "country", Value: "France"}, {Key: "food", Value: "french cheeses"}},
		bson.D{{Key: "_id", Value: "rome"}, {Key: "country", Value: "Italy"}, {Key: "food", Value: "best pizza and breads"}},
	}); err != nil {
		t.Fatalf("InsertMany: %v", err)
	}
	if err := cities.FindOne(ctx, bson.D{{Key: "$text", Value: bson.D{{Key: "$search", Value: "bread"}}}}).Err(); mongoCode(err) != 27 {
		t.Errorf("$text without a text index: %v, want IndexNotFound (27)", err)
	}
	name, err := cities.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "country", Value: "text"}, {Key: "food", Value: "text"}}})
	if err != nil || name != "country_text_food_text" {
		t.Fatalf("CreateOne(text) = %q, %v", name, err)
	}
	if _, err := cities.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "other", Value: "text"}}}); mongoCode(err) != 85 {
		t.Errorf("a second text index: %v, want IndexOptionsConflict (85)", err)
	}
	ids := func(filter any, opts ...options.Lister[options.FindOptions]) []string {
		t.Helper()
		cur, err := cities.Find(ctx, filter, opts...)
		if err != nil {
			t.Fatalf("Find(%v): %v", filter, err)
		}
		var docs []bson.M
		if err := cur.All(ctx, &docs); err != nil {
			t.Fatalf("Find's cursor: %v", err)
		}
		var out []string
		for _, d := range docs {
			out = append(out, d["_id"].(string))
		}
		return out
	}
	text := func(s string) bson.D {
		return bson.D{{Key: "$text", Value: bson.D{{Key: "$search", Value: s}}}}
	}
	sortID := options.Find().SetSort(bson.D{{Key: "_id", Value: 1}})
	for _, tc := range []struct {
		search string
		want   string
	}{
		{"french bread", "lyon paris rome"},
		{`"best french bread"`, "paris"},
		{`"best" "french" bread is`, "paris"},
		{"best bread -french", "rome"},
		{"italy", "rome"},
	} {
		if got := strings.Join(ids(text(tc.search), sortID), " "); got != tc.want {
			t.Errorf("$search %q = %s, want %s", tc.search, got, tc.want)
		}
	}
	// The score, sorted on and projected.
	cur, err := cities.Find(ctx, text("french bread"), options.Find().SetSort(bson.D{{Key: "score", Value: bson.D{{Key: "$meta", Value: "textScore"}}}}).
		SetProjection(bson.D{{Key: "score", Value: bson.D{{Key: "$meta", Value: "textScore"}}}}))
	if err != nil {
		t.Fatalf("Find sorted by textScore: %v", err)
	}
	var scored []bson.M
	if err := cur.All(ctx, &scored); err != nil || len(scored) != 3 {
		t.Fatalf("Find sorted by textScore = %v, %v", scored, err)
	}
	if scored[0]["_id"] != "paris" || scored[0]["score"].(float64) < scored[2]["score"].(float64) || scored[0]["food"] == nil {
		t.Errorf("sorted by textScore = %v, want paris first, the highest score, and every field", scored)
	}
	agg, err := cities.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: text("cheese")}},
		{{Key: "$project", Value: bson.D{{Key: "s", Value: bson.D{{Key: "$meta", Value: "textScore"}}}}}},
	})
	if err != nil {
		t.Fatalf("Aggregate($match $text): %v", err)
	}
	var aggd []bson.M
	if err := agg.All(ctx, &aggd); err != nil || len(aggd) != 1 || aggd[0]["_id"] != "lyon" || aggd[0]["s"] == nil {
		t.Errorf("$match $text then $meta = %v, %v, want lyon with a score", aggd, err)
	}
	if _, err := cities.Aggregate(ctx, mongo.Pipeline{{{Key: "$sort", Value: bson.D{{Key: "_id", Value: 1}}}}, {{Key: "$match", Value: text("cheese")}}}); err == nil {
		t.Errorf("$text in a $match that is not the first stage succeeded")
	}
	idx, err := firestoreAdmin(t, h).ListIndexes(ctx, &adminpb.ListIndexesRequest{Parent: dbName + "/collectionGroups/cities"}).Next()
	if err != nil || idx.ApiScope != adminpb.Index_MONGODB_COMPATIBLE_API || idx.Fields[0].GetSearchConfig().GetTextSpec() == nil {
		t.Errorf("the Admin API's index = %v, %v: want the text index, with a search config", idx, err)
	}
}
