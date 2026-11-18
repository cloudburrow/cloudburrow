package mql

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func arr(t *testing.T, ext string) bson.A {
	t.Helper()
	var w struct{ A bson.A }
	if err := bson.UnmarshalExtJSON([]byte(`{"a": `+ext+`}`), false, &w); err != nil {
		t.Fatalf("%s: %v", ext, err)
	}
	return w.A
}

func TestLookupPipelineAndDocuments(t *testing.T) {
	orders := []bson.D{doc(t, `{"_id": 1, "item": "a", "qty": 5}`), doc(t, `{"_id": 2, "item": "b", "qty": 1}`)}
	stock := []bson.D{doc(t, `{"_id": "s1", "sku": "a", "inStock": 10}`), doc(t, `{"_id": "s2", "sku": "a", "inStock": 2}`), doc(t, `{"_id": "s3", "sku": "b", "inStock": 7}`)}
	env := NewEnv(func(c string) ([]bson.D, error) { return stock, nil })
	out, err := runPipeline(env, orders, arr(t, `[{"$lookup": {"from": "stock", "let": {"it": "$item", "q": "$qty"},
		"pipeline": [{"$match": {"$expr": {"$and": [{"$eq": ["$sku", "$$it"]}, {"$gte": ["$inStock", "$$q"]}]}}}, {"$project": {"_id": 1}}],
		"as": "fits"}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if got := lookup(out[0], "fits").(bson.A); len(got) != 1 || !valuesEqual(got[0], doc(t, `{"_id": "s1"}`)) {
		t.Errorf("order 1 fits %v, want s1", got)
	}
	if got := lookup(out[1], "fits").(bson.A); len(got) != 1 || !valuesEqual(got[0], doc(t, `{"_id": "s3"}`)) {
		t.Errorf("order 2 fits %v, want s3", got)
	}
	// localField and foreignField with a pipeline (MongoDB 5.0).
	out, err = runPipeline(env, orders[:1], arr(t, `[{"$lookup": {"from": "stock", "localField": "item", "foreignField": "sku",
		"pipeline": [{"$count": "n"}], "as": "c"}}]`))
	if err != nil || !valuesEqual(lookup(out[0], "c"), arr(t, `[{"n": 2}]`)) {
		t.Errorf("a pipeline after localField = %v, %v", out, err)
	}
	// $documents, collection-less.
	docs, rest, err := documentsInput(arr(t, `[{"$documents": [{"x": 1}, {"x": 2}, {"x": 3}]}, {"$match": {"x": {"$gte": 2}}}]`))
	if err != nil {
		t.Fatal(err)
	}
	out, err = runPipeline(env, docs, rest)
	if err != nil || len(out) != 2 {
		t.Errorf("$documents then $match = %v, %v", out, err)
	}
	if _, _, err := documentsInput(arr(t, `[{"$match": {}}]`)); err == nil {
		t.Error("a collection-less pipeline without $documents was accepted")
	}
	// A $lookup without from, over $documents.
	out, err = runPipeline(env, orders[:1], arr(t, `[{"$lookup": {"let": {"q": "$qty"}, "pipeline": [{"$documents": [{"k": 1}]}, {"$set": {"q": "$$q"}}], "as": "d"}}]`))
	if err != nil || !valuesEqual(lookup(out[0], "d"), arr(t, `[{"k": 1, "q": 5}]`)) {
		t.Errorf("$lookup over $documents = %v, %v", out, err)
	}
}
