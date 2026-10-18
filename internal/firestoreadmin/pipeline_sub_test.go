package firestoreadmin_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestPipelineSubqueries (#1613): array and scalar subqueries, the
// subcollection stage, storage_size and reference_slice, through the
// official client's pipeline API, each result computed by hand.
func TestPipelineSubqueries(t *testing.T) {
	c, _ := pipelineFixture(t)
	ctx := context.Background()
	one := func(what string, p *firestore.Pipeline) map[string]any {
		t.Helper()
		rs, err := p.Execute(ctx).Results().GetAll()
		if err != nil || len(rs) != 1 {
			t.Fatalf("%s = %d results, %v; want 1", what, len(rs), err)
		}
		return rs[0].Data()
	}
	want := func(what string, got, want any) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v, want %#v", what, got, want)
		}
	}
	// shelves/s1 holds no fields, only the subcollection books, with
	// Delta (rating 1) in it: a literal names it.
	shelf := func() *firestore.Pipeline {
		return c.Pipeline().Literals([]map[string]any{{"__name__": c.Doc("shelves/s1"), "k": "s1"}})
	}
	got := one("subcollection", shelf().AddFields(firestore.Selectables(
		firestore.Subcollection("books").Select(firestore.Fields("title")).ToArrayExpression().As("titles"),
		firestore.Subcollection("books").Aggregate(firestore.Accumulators(firestore.Average("rating").As("avg"))).ToScalarExpression().As("avg"),
		firestore.Subcollection("books").Aggregate(firestore.Accumulators(firestore.CountAll().As("n"), firestore.Sum("rating").As("sum"))).ToScalarExpression().As("both"),
		firestore.Subcollection("none").Select(firestore.Fields("title")).ToArrayExpression().As("empty"),
		firestore.Subcollection("none").Select(firestore.Fields("title")).ToScalarExpression().As("null"),
	)))
	want("array subquery", got["titles"], []any{map[string]any{"title": "Delta"}})
	want("scalar subquery of one field", got["avg"], 1.0)
	want("scalar subquery of two fields", got["both"], map[string]any{"n": int64(1), "sum": int64(1)})
	want("array subquery of nothing", got["empty"], []any{})
	want("scalar subquery of nothing", got["null"], nil)

	// The manual form: let binds the shelf's name, and a collection group
	// filtered on its parent reads the same.
	got = one("let and collection_group", shelf().
		Define(firestore.AliasedExpressions(firestore.FieldOf("__name__").As("shelf"))).
		AddFields(firestore.Selectables(c.Pipeline().CollectionGroup("books").
			Where(firestore.Equal(firestore.GetParent(firestore.FieldOf("__name__")), firestore.Variable("shelf"))).
			Select(firestore.Fields("title")).ToScalarExpression().As("title"))))
	want("manual join", got["title"], "Delta")

	// A scalar subquery of more than one result is an error.
	_, err := shelf().AddFields(firestore.Selectables(
		c.Pipeline().Collection("books").Select(firestore.Fields("title")).ToScalarExpression().As("x"))).
		Execute(ctx).Results().GetAll()
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "3 results") {
		t.Errorf("scalar subquery of 3 = %v, want INVALID_ARGUMENT", err)
	}

	// storage_size: "Alpha" 5+1, an integer 8, ["x","y"] 2+2, books/a's
	// name 6+2+16, and the document's fields title (6+6), genre (6+3),
	// rating (7+8), tags (5+4), +32.
	got = one("storage_size", c.Pipeline().Collection("books").Where(firestore.Equal(firestore.FieldOf("title"), "Alpha")).
		Select(firestore.Fields(
			firestore.StorageSize("title").As("title"),
			firestore.StorageSize("rating").As("rating"),
			firestore.StorageSize("tags").As("tags"),
			firestore.StorageSize("__name__").As("name"),
			firestore.StorageSize(firestore.CurrentDocument()).As("doc"),
		)))
	want("storage_size", got, map[string]any{"title": int64(6), "rating": int64(8), "tags": int64(4), "name": int64(24),
		"doc": int64(12 + 9 + 15 + 9 + 32)})

	// reference_slice of shelves/s1/books/d.
	got = one("reference_slice", c.Pipeline().CollectionGroup("books").Where(firestore.Equal(firestore.FieldOf("title"), "Delta")).
		Select(firestore.Fields(
			firestore.ReferenceSlice("__name__", 1, 1).As("tail"),
			firestore.ReferenceSlice("__name__", 0, 1).As("head"),
			firestore.ReferenceSliceToEnd("__name__", -1).As("last"),
		)))
	ref := func(k string) string {
		r, _ := got[k].(*firestore.DocumentRef)
		if r == nil {
			return ""
		}
		return r.Path
	}
	root := "projects/p/databases/entdb/documents/"
	want("reference_slice tail", ref("tail"), root+"books/d")
	want("reference_slice head", ref("head"), root+"shelves/s1")
	want("reference_slice to end", ref("last"), root+"books/d")
}
