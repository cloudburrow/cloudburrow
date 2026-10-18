//go:build compat

package compat

import (
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestFirestorePipelineSubqueries (#1613): the subcollection stage and the
// array and scalar subqueries over it, storage_size and reference_slice,
// through the official Go client's pipeline API (Subcollection,
// ToArrayExpression, ToScalarExpression, StorageSize, ReferenceSlice).
// The data and results are Google's own examples where it gives them: the
// subcollection stage's restaurants and reviews, the storage size page's
// users/jeff/tasks/my_task_id (147 bytes) and the reference functions'
// a/1/b/2/c/3; the rest are computed by hand.
//
// covers: google.firestore.v1.Firestore/ExecutePipeline
func TestFirestorePipelineSubqueries(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvFirestore)
	ctx := h.Context()
	enterpriseDatabase(t, h, "piped-sub")
	t.Setenv("FIRESTORE_EMULATOR_HOST", h.Endpoint(EnvFirestore))
	c, err := firestore.NewClientWithDatabase(ctx, h.Project(), "piped-sub")
	if err != nil {
		t.Fatalf("NewClientWithDatabase: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	set := func(path string, data map[string]any) {
		t.Helper()
		if _, err := c.Doc(path).Set(ctx, data); err != nil {
			t.Fatalf("Set %s: %v", path, err)
		}
	}
	// The subcollection stage's sample documents (Google's give each
	// restaurant's reviews the one ID "1"; they are numbered here).
	set("restaurants/pizza-place", map[string]any{"name": "Pizza Place"})
	set("restaurants/urban-bite", map[string]any{"name": "Urban Bite"})
	set("restaurants/nacho-house", map[string]any{"name": "Nacho House"})
	set("restaurants/pizza-place/reviews/1", map[string]any{"rating": 5})
	set("restaurants/pizza-place/reviews/2", map[string]any{"rating": 2})
	set("restaurants/urban-bite/reviews/1", map[string]any{"rating": 3})
	set("restaurants/urban-bite/reviews/2", map[string]any{"rating": 4})
	set("restaurants/urban-bite/reviews/3", map[string]any{"rating": 5})

	want := func(what string, got, want any) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v, want %#v", what, got, want)
		}
	}
	// byName runs p and returns each result's field, by its name field.
	byName := func(what string, p *firestore.Pipeline, field string) map[string]any {
		t.Helper()
		rs, err := p.Execute(ctx).Results().GetAll()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		out := map[string]any{}
		for _, r := range rs {
			d := r.Data()
			out[d["name"].(string)] = d[field]
		}
		return out
	}

	// Google's query: review_count and avg_rating, as one map.
	got := byName("review_summary", c.Pipeline().CollectionGroup("restaurants").
		AddFields(firestore.Selectables(firestore.Subcollection("reviews").
			Aggregate(firestore.Accumulators(firestore.CountAll().As("review_count"), firestore.Average("rating").As("avg_rating"))).
			ToScalarExpression().As("review_summary"))), "review_summary")
	want("review_summary", got, map[string]any{
		"Pizza Place": map[string]any{"review_count": int64(2), "avg_rating": 3.5},
		"Urban Bite":  map[string]any{"review_count": int64(3), "avg_rating": 4.0},
		"Nacho House": map[string]any{"review_count": int64(0), "avg_rating": nil},
	})

	// Google's first example: one field, elevated to the value; and the
	// manual equivalent it gives, let and a collection group filtered on
	// the parent of __name__, reads the same.
	avg := map[string]any{"Pizza Place": 3.5, "Urban Bite": 4.0, "Nacho House": nil}
	got = byName("avg_rating", c.Pipeline().Collection("restaurants").
		AddFields(firestore.Selectables(firestore.Subcollection("reviews").
			Aggregate(firestore.Accumulators(firestore.Average("rating").As("avg_rating"))).
			ToScalarExpression().As("avg_rating"))), "avg_rating")
	want("avg_rating", got, avg)
	got = byName("manual avg_rating", c.Pipeline().Collection("restaurants").
		Define(firestore.AliasedExpressions(firestore.FieldOf("__name__").As("restaurant_name"))).
		AddFields(firestore.Selectables(c.Pipeline().CollectionGroup("reviews").
			Where(firestore.Equal(firestore.GetParent(firestore.FieldOf("__name__")), firestore.Variable("restaurant_name"))).
			Aggregate(firestore.Accumulators(firestore.Average("rating").As("avg_rating"))).
			ToScalarExpression().As("avg_rating"))), "avg_rating")
	want("manual avg_rating", got, avg)

	// An array subquery: the reviews, sorted, as maps; none is an empty
	// array, never null.
	got = byName("ratings", c.Pipeline().Collection("restaurants").
		AddFields(firestore.Selectables(firestore.Subcollection("reviews").
			Sort(firestore.Orders(firestore.Ascending(firestore.FieldOf("rating")))).
			Select(firestore.Fields("rating")).ToArrayExpression().As("ratings"))), "ratings")
	r := func(n int64) map[string]any { return map[string]any{"rating": n} }
	want("ratings", got, map[string]any{
		"Pizza Place": []any{r(2), r(5)},
		"Urban Bite":  []any{r(3), r(4), r(5)},
		"Nacho House": []any{},
	})

	// A subquery in where: the anti-join, restaurants with no review.
	rs, err := c.Pipeline().Collection("restaurants").
		Where(firestore.Equal(firestore.Subcollection("reviews").
			Aggregate(firestore.Accumulators(firestore.CountAll().As("n"))).ToScalarExpression(), 0)).
		Execute(ctx).Results().GetAll()
	if err != nil || len(rs) != 1 || rs[0].Data()["name"] != "Nacho House" {
		t.Errorf("anti-join = %d results, %v; want Nacho House", len(rs), err)
	}

	// A scalar subquery of more than one result is an error; the
	// subcollection stage outside a subquery is refused.
	_, err = c.Pipeline().Collection("restaurants").
		AddFields(firestore.Selectables(firestore.Subcollection("reviews").Select(firestore.Fields("rating")).ToScalarExpression().As("x"))).
		Execute(ctx).Results().GetAll()
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "results") {
		t.Errorf("scalar subquery of many = %v, want INVALID_ARGUMENT", err)
	}
	_, err = c.Pipeline().Collection("restaurants").RawStage("subcollection", []any{"reviews"}).Execute(ctx).Results().GetAll()
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "within a subquery") {
		t.Errorf("subcollection outside a subquery = %v, want INVALID_ARGUMENT", err)
	}

	// storage_size, on the storage size page's document: its name is
	// 6+5+6+11+16 = 44 bytes; the values "Personal" 9, false 1, 1 (an
	// integer) 8 and "Learn Cloud Firestore" 22; with the field names
	// (5, 5, 9, 12) the fields are 71 bytes, and the document 44+71+32 =
	// 147 bytes, its name and its fields as a map (calculated "the same
	// way as document size": 71+32).
	set("users/jeff/tasks/my_task_id", map[string]any{"type": "Personal", "done": false, "priority": 1, "description": "Learn Cloud Firestore"})
	rs, err = c.Pipeline().Documents([]*firestore.DocumentRef{c.Doc("users/jeff/tasks/my_task_id")}).
		Select(firestore.Fields(
			firestore.StorageSize("__name__").As("name"),
			firestore.StorageSize("type").As("type"),
			firestore.StorageSize("done").As("done"),
			firestore.StorageSize("priority").As("priority"),
			firestore.StorageSize("description").As("description"),
			firestore.StorageSize("absent").As("absent"),
			firestore.Add(firestore.StorageSize("__name__"), firestore.StorageSize(firestore.CurrentDocument())).As("document"),
		)).Execute(ctx).Results().GetAll()
	if err != nil || len(rs) != 1 {
		t.Fatalf("storage_size = %d results, %v", len(rs), err)
	}
	want("storage_size", rs[0].Data(), map[string]any{"name": int64(44), "type": int64(9), "done": int64(1),
		"priority": int64(8), "description": int64(22), "document": int64(147)})

	// reference_slice, on the reference functions' a/1/b/2/c/3.
	set("a/1/b/2/c/3", map[string]any{"x": 1})
	rs, err = c.Pipeline().Documents([]*firestore.DocumentRef{c.Doc("a/1/b/2/c/3")}).
		Select(firestore.Fields(
			firestore.ReferenceSlice("__name__", 1, 2).As("one_two"),
			firestore.ReferenceSlice("__name__", 0, 2).As("zero_two"),
			firestore.ReferenceSlice("__name__", -1, 1).As("last"),
			firestore.ReferenceSliceToEnd("__name__", 2).As("to_end"),
		)).Execute(ctx).Results().GetAll()
	if err != nil || len(rs) != 1 {
		t.Fatalf("reference_slice = %d results, %v", len(rs), err)
	}
	root := "projects/" + h.Project() + "/databases/piped-sub/documents/"
	for k, w := range map[string]string{"one_two": "b/2/c/3", "zero_two": "a/1/b/2", "last": "c/3", "to_end": "c/3"} {
		ref, _ := rs[0].Data()[k].(*firestore.DocumentRef)
		if ref == nil || ref.Path != root+w {
			t.Errorf("reference_slice %s = %v, want %s", k, rs[0].Data()[k], w)
		}
	}
}
