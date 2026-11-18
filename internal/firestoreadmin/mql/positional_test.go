package mql

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestPositionalUpdates(t *testing.T) {
	d := doc(t, `{"_id": 1, "grades": [80, 85, 90], "items": [{"n": "a", "q": 1}, {"n": "b", "q": 5}, {"n": "c", "q": 9}]}`)
	for _, tc := range []struct {
		name, q, u, af, want string
	}{
		{"$ by value", `{"grades": 85}`, `{"$set": {"grades.$": 86}}`, ``, `{"_id": 1, "grades": [80, 86, 90], "items": [{"n": "a", "q": 1}, {"n": "b", "q": 5}, {"n": "c", "q": 9}]}`},
		{"$ in documents", `{"items.n": "b"}`, `{"$inc": {"items.$.q": 1}}`, ``, `{"_id": 1, "grades": [80, 85, 90], "items": [{"n": "a", "q": 1}, {"n": "b", "q": 6}, {"n": "c", "q": 9}]}`},
		{"$ with $elemMatch", `{"items": {"$elemMatch": {"q": {"$gt": 4}}}}`, `{"$set": {"items.$.hit": true}}`, ``, `{"_id": 1, "grades": [80, 85, 90], "items": [{"n": "a", "q": 1}, {"n": "b", "q": 5, "hit": true}, {"n": "c", "q": 9}]}`},
		{"$[]", `{}`, `{"$inc": {"grades.$[]": 1}}`, ``, `{"_id": 1, "grades": [81, 86, 91], "items": [{"n": "a", "q": 1}, {"n": "b", "q": 5}, {"n": "c", "q": 9}]}`},
		{"$[id]", `{}`, `{"$set": {"grades.$[g]": 100}}`, `[{"g": {"$gte": 85}}]`, `{"_id": 1, "grades": [80, 100, 100], "items": [{"n": "a", "q": 1}, {"n": "b", "q": 5}, {"n": "c", "q": 9}]}`},
		{"$[id] in documents", `{}`, `{"$set": {"items.$[it].q": 0}}`, `[{"it.q": {"$lt": 6}}]`, `{"_id": 1, "grades": [80, 85, 90], "items": [{"n": "a", "q": 0}, {"n": "b", "q": 0}, {"n": "c", "q": 9}]}`},
	} {
		uc := &UpdateContext{Query: doc(t, tc.q)}
		if tc.af != "" {
			var w struct{ A bson.A }
			if err := bson.UnmarshalExtJSON([]byte(`{"a": `+tc.af+`}`), false, &w); err != nil {
				t.Fatal(err)
			}
			uc.ArrayFilters = w.A
		}
		got, err := applyUpdateCtx(d, doc(t, tc.u), false, uc)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !valuesEqual(got, doc(t, tc.want)) {
			t.Errorf("%s: got %v, want %s", tc.name, got, tc.want)
		}
	}
	if _, err := applyUpdateCtx(d, doc(t, `{"$set": {"grades.$": 1}}`), false, &UpdateContext{Query: doc(t, `{"_id": 1}`)}); err == nil {
		t.Error("$ without a query on the array was applied")
	}
	if _, err := applyUpdateCtx(d, doc(t, `{"$set": {"grades.$[x]": 1}}`), false, &UpdateContext{}); err == nil {
		t.Error("$[x] without an array filter was applied")
	}
	if _, err := applyUpdateCtx(d, doc(t, `{"$set": {"grades.0": 1}}`), false, &UpdateContext{ArrayFilters: bson.A{doc(t, `{"x": 1}`)}}); err == nil {
		t.Error("an unused array filter was accepted")
	}
	got, err := ProjectQuery(d, doc(t, `{"items.$": 1}`), doc(t, `{"items.q": {"$gt": 4}}`))
	if err != nil || !valuesEqual(got, doc(t, `{"_id": 1, "items": [{"n": "b", "q": 5}]}`)) {
		t.Errorf("positional projection = %v, %v", got, err)
	}
}
