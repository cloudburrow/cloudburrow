package mql

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func doc(t *testing.T, ext string) bson.D {
	t.Helper()
	var d bson.D
	if err := bson.UnmarshalExtJSON([]byte(ext), false, &d); err != nil {
		t.Fatalf("%s: %v", ext, err)
	}
	return d
}

func TestFirestoreValuesRoundTrip(t *testing.T) {
	in := bson.D{{Key: "_id", Value: bson.NewObjectID()}, {Key: "i", Value: int32(5)}, {Key: "l", Value: int64(6)}, {Key: "f", Value: 1.5},
		{Key: "s", Value: "x"}, {Key: "b", Value: true}, {Key: "n", Value: nil}, {Key: "a", Value: bson.A{int32(1), "two"}},
		{Key: "o", Value: bson.D{{Key: "k", Value: "v"}}}, {Key: "bin", Value: bson.Binary{Subtype: 4, Data: []byte{1}}},
		{Key: "re", Value: bson.Regex{Pattern: "a", Options: "i"}}, {Key: "ts", Value: bson.Timestamp{T: 3, I: 1}},
		{Key: "mn", Value: bson.MinKey{}}, {Key: "mx", Value: bson.MaxKey{}}, {Key: "dt", Value: bson.DateTime(1000)}}
	fields, err := toFirestoreDoc(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["i"].GetMapValue().GetFields()["__int__"]; !ok {
		t.Errorf("an int32 is %v, want an __int__ map", fields["i"])
	}
	for _, e := range in[1:] {
		got := fromFirestore(fields[e.Key])
		if !valuesEqual(got, e.Value) {
			t.Errorf("%s: %v round-tripped as %v", e.Key, e.Value, got)
		}
	}
	if _, err := toFirestoreDoc(bson.D{{Key: "__x__", Value: 1}}); err == nil {
		t.Error("a field named __x__ was accepted")
	}
	if _, err := toFirestoreDoc(bson.D{{Key: "j", Value: bson.JavaScript("1")}}); err == nil {
		t.Error("a JavaScript value was accepted")
	}
	if _, err := toFirestoreDoc(bson.D{{Key: "r", Value: bson.Regex{Pattern: "a", Options: "mi"}}}); err == nil {
		t.Error("regular expression options out of order were accepted")
	}
}

func TestDocIDs(t *testing.T) {
	oid := bson.NewObjectID()
	for _, id := range []any{"plain", "~tilde", "a/b", "", oid, int32(5), int64(5), 2.5, bson.D{{Key: "a", Value: int32(1)}}, true} {
		d, err := docID(id)
		if err != nil {
			t.Fatalf("docID(%v): %v", id, err)
		}
		if got := idOf(d); !valuesEqual(got, id) || typeRank(got) != typeRank(id) {
			t.Errorf("docID(%v) = %q, which names %v", id, d, got)
		}
	}
	if d, _ := docID("plain"); d != "plain" {
		t.Errorf("a string _id's document ID = %q", d)
	}
	if _, err := docID(bson.A{1}); err == nil {
		t.Error("an array _id was accepted")
	}
	if _, err := docID("__x__"); err == nil {
		t.Error("an _id matching __.*__ was accepted")
	}
}

func TestMatches(t *testing.T) {
	d := doc(t, `{"a": 5, "s": "Hello", "arr": [1, 2, 3], "sub": [{"x": 1}, {"x": 2}], "n": null}`)
	for _, c := range []struct {
		q    string
		want bool
	}{
		{`{"a": 5}`, true},
		{`{"a": {"$gt": 4, "$lt": 6}}`, true},
		{`{"a": {"$gt": "4"}}`, false},
		{`{"arr": 2}`, true},
		{`{"arr": {"$size": 3}}`, true},
		{`{"arr": {"$all": [1, 3]}}`, true},
		{`{"sub.x": 2}`, true},
		{`{"sub": {"$elemMatch": {"x": {"$gte": 2}}}}`, true},
		{`{"missing": null}`, true},
		{`{"n": {"$exists": true}}`, true},
		{`{"missing": {"$exists": true}}`, false},
		{`{"s": {"$regex": "^hel", "$options": "i"}}`, true},
		{`{"$or": [{"a": 1}, {"a": 5}]}`, true},
		{`{"$nor": [{"a": 5}]}`, false},
		{`{"a": {"$in": [1, 5]}}`, true},
		{`{"a": {"$nin": [1, 5]}}`, false},
		{`{"a": {"$not": {"$gt": 4}}}`, false},
		{`{"a": {"$type": "int"}}`, true},
		{`{"a": {"$mod": [2, 1]}}`, true},
		{`{"$expr": {"$gt": ["$a", 4]}}`, true},
	} {
		got, err := matches(d, doc(t, c.q))
		if err != nil || got != c.want {
			t.Errorf("%s = %v, %v; want %v", c.q, got, err, c.want)
		}
	}
	if _, err := matches(d, doc(t, `{"$where": "1"}`)); err == nil {
		t.Error("$where was accepted")
	}
}

func TestUpdates(t *testing.T) {
	d := doc(t, `{"_id": "x", "n": 1, "tags": ["a"], "o": {"k": 1}}`)
	got, err := applyUpdate(d, doc(t, `{"$inc": {"n": 2}, "$push": {"tags": {"$each": ["b", "c"], "$slice": 2}}, "$set": {"o.j": 2}, "$unset": {"o.k": ""}, "$rename": {"n": "m"}}`), false)
	if err != nil {
		t.Fatal(err)
	}
	want := doc(t, `{"_id": "x", "tags": ["a", "b"], "o": {"j": 2}, "m": 3}`)
	if !valuesEqual(sortKeys(got), sortKeys(want)) {
		t.Errorf("update = %v, want %v", got, want)
	}
	if _, err := applyUpdate(d, doc(t, `{"$set": {"_id": "y"}}`), false); err == nil {
		t.Error("changing _id was accepted")
	}
	got, err = applyUpdate(upsertBase(doc(t, `{"k": 1, "z": {"$gt": 1}}`)), doc(t, `{"$setOnInsert": {"new": true}}`), true)
	if err != nil || len(got) != 2 {
		t.Errorf("an upsert's document = %v, %v", got, err)
	}
}

func sortKeys(d bson.D) bson.D {
	out := bson.D{}
	for _, k := range []string{"_id", "m", "o", "tags"} {
		if v, ok := lookupE(d, k); ok {
			out = append(out, bson.E{Key: k, Value: v})
		}
	}
	return out
}

func TestPipeline(t *testing.T) {
	docs := []bson.D{
		doc(t, `{"_id": 1, "g": "a", "v": 2, "items": ["x", "y"]}`),
		doc(t, `{"_id": 2, "g": "b", "v": 3, "items": []}`),
		doc(t, `{"_id": 3, "g": "a", "v": 5, "items": ["z"]}`),
	}
	var p bson.A
	if err := bson.UnmarshalExtJSON([]byte(`[{"$unwind": "$items"}, {"$group": {"_id": "$g", "n": {"$sum": 1}, "total": {"$sum": "$v"}, "items": {"$push": "$items"}}},
		{"$sort": {"_id": 1}}, {"$project": {"n": 1, "double": {"$multiply": ["$total", 2]}}}]`), false, &p); err != nil {
		t.Fatal(err)
	}
	out, err := runPipeline(nil, docs, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || lookup(out[0], "_id") != "a" || lookup(out[0], "n") != int32(3) || !valuesEqual(lookup(out[0], "double"), int32(18)) {
		t.Errorf("pipeline = %v", out)
	}
	if _, err := runPipeline(nil, docs, bson.A{bson.D{{Key: "$out", Value: "x"}}}); err == nil {
		t.Error("$out was accepted")
	}
	env := &aggEnv{load: func(string) ([]bson.D, error) { return []bson.D{doc(t, `{"_id": "a", "name": "Group A"}`)}, nil }}
	out, err = runPipeline(env, docs[:1], bson.A{doc(t, `{"$lookup": {"from": "groups", "localField": "g", "foreignField": "_id", "as": "group"}}`),
		doc(t, `{"$count": "c"}`)})
	if err != nil || len(out) != 1 || lookup(out[0], "c") != int32(1) {
		t.Errorf("$lookup then $count = %v, %v", out, err)
	}
	long := make(bson.A, mongoMaxStages+1)
	for i := range long {
		long[i] = bson.D{{Key: "$skip", Value: 0}}
	}
	if _, err := runPipeline(nil, docs, long); err == nil {
		t.Error("a pipeline of 251 stages was accepted")
	}
}
