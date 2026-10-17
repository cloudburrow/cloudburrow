//go:build compat

package compat

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/genproto/googleapis/type/latlng"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestFirestorePipelinesMore (#1523): the pipeline stages and functions
// served since #1500, through the official Go client's pipeline API, each
// result checked against a value computed by hand: let and variables,
// literals, the regex, array, lambda, timestamp and geographic functions,
// geographic search, the update and delete stages (written through the
// emulator), pipelines in a transaction and at a read time. Full-text
// search stays refused UNIMPLEMENTED, saying why.
//
// covers: google.firestore.v1.Firestore/ExecutePipeline
func TestFirestorePipelinesMore(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvFirestore)
	ctx := h.Context()
	enterpriseDatabase(t, h, "piped-more")
	t.Setenv("FIRESTORE_EMULATOR_HOST", h.Endpoint(EnvFirestore))
	c, err := firestore.NewClientWithDatabase(ctx, h.Project(), "piped-more")
	if err != nil {
		t.Fatalf("NewClientWithDatabase: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	paris := &latlng.LatLng{Latitude: 48.8566, Longitude: 2.3522}
	london := &latlng.LatLng{Latitude: 51.5074, Longitude: -0.1278}
	items := c.Collection("items")
	beforeCreate := time.Now()
	time.Sleep(20 * time.Millisecond)
	if _, err := items.Doc("i1").Set(ctx, map[string]any{"name": "Widget", "price": 100, "stock": 20,
		"email": "john.doe@x.io", "zip": "12345", "a": []int{1, 2, 3},
		"ts": time.Date(2024, 3, 15, 13, 45, 30, 250e6, time.UTC), "loc": paris}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := items.Doc("i2").Set(ctx, map[string]any{"name": "Gadget", "price": 50, "stock": 5,
		"a": []int{3, 1, 3}, "ts": time.Date(2024, 12, 30, 23, 0, 0, 0, time.UTC), "loc": london}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	afterCreate := time.Now()

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
	widget := func() *firestore.Pipeline {
		return c.Pipeline().Collection("items").Where(firestore.Equal(firestore.FieldOf("name"), "Widget"))
	}
	gadget := func() *firestore.Pipeline {
		return c.Pipeline().Collection("items").Where(firestore.Equal(firestore.FieldOf("name"), "Gadget"))
	}

	// let: price*0.9 is 90 and 45, both under 100; stock+10 is 30 and 15.
	rs, err := c.Pipeline().Collection("items").
		Define(firestore.AliasedExpressions(
			firestore.Multiply("price", 0.9).As("discounted"),
			firestore.Add("stock", 10).As("newStock"))).
		Where(firestore.LessThan(firestore.Variable("discounted"), 100)).
		Select(firestore.Fields("name", firestore.Variable("newStock").As("newStock"))).
		Sort(firestore.Orders(firestore.Ascending(firestore.FieldOf("name")))).
		Execute(ctx).Results().GetAll()
	if err != nil || len(rs) != 2 {
		t.Fatalf("let = %d results, %v", len(rs), err)
	}
	want("let, Gadget", rs[0].Data(), map[string]any{"name": "Gadget", "newStock": int64(15)})
	want("let, Widget", rs[1].Data(), map[string]any{"name": "Widget", "newStock": int64(30)})

	// literals: ages 10, 30, 40; over 20 are bob and alice.
	rs, err = c.Pipeline().Literals([]map[string]any{{"name": "joe", "age": 10}, {"name": "bob", "age": 30}, {"name": "alice", "age": 40}}).
		Where(firestore.GreaterThan(firestore.FieldOf("age"), 20)).
		Sort(firestore.Orders(firestore.Ascending(firestore.FieldOf("age")))).
		Execute(ctx).Results().GetAll()
	if err != nil || len(rs) != 2 || rs[0].Data()["name"] != "bob" || rs[1].Data()["name"] != "alice" {
		t.Errorf("literals where age > 20 = %v, %v; want bob, alice", rs, err)
	}

	// Regex, array and lambda functions on Widget (a = [1,2,3]) and
	// Gadget (a = [3,1,3]).
	got := one("regex and arrays", widget().Select(firestore.Fields(
		firestore.RegexFind("email", "[a-z]+").As("find"),
		firestore.RegexFindAll("zip", "[0-9]").As("all"),
		firestore.ArrayFirstN("a", 2).As("first"),
		firestore.ArrayLastN("a", 2).As("last"),
		firestore.ArrayFilter("a", "x", firestore.GreaterThan(firestore.Variable("x"), 1)).As("filter"),
		firestore.ArrayTransformWithIndex("a", "x", "i", firestore.Add(firestore.Variable("x"), firestore.Variable("i"))).As("transform"))))
	want("regex_find", got["find"], "john")
	want("regex_find_all", got["all"], []any{"1", "2", "3", "4", "5"})
	want("array_first_n", got["first"], []any{int64(1), int64(2)})
	want("array_last_n", got["last"], []any{int64(2), int64(3)})
	want("array_filter x > 1", got["filter"], []any{int64(2), int64(3)})
	want("array_transform x + i", got["transform"], []any{int64(1), int64(3), int64(5)})
	got = one("arrays on Gadget", gadget().Select(firestore.Fields(
		firestore.ArrayIndexOfAll("a", 3).As("idx"),
		firestore.ArrayMaximumN("a", 2).As("max2"))))
	want("array_index_of_all 3", got["idx"], []any{int64(0), int64(2)})
	want("maximum_n 2", got["max2"], []any{int64(3), int64(3)})

	// Timestamps: Widget's is Friday 2024-03-15 13:45:30.25 UTC, 09:45
	// in New York (EDT); Gadget's is Monday 2024-12-30 23:00 UTC, in ISO
	// week 1 of 2025, 290 whole days later.
	got = one("timestamps", widget().Select(firestore.Fields(
		firestore.TimestampTruncate("ts", "day").As("day"),
		firestore.TimestampTruncate("ts", "month").As("month"),
		firestore.TimestampTruncateWithTimezone("ts", "day", "America/New_York").As("dayNY"),
		firestore.TimestampExtract("ts", "hour").As("hour"),
		firestore.TimestampExtract("ts", "dayofweek").As("dow"))))
	wantTime := func(what string, got any, w time.Time) {
		t.Helper()
		if g, ok := got.(time.Time); !ok || !g.Equal(w) {
			t.Errorf("%s = %v, want %v", what, got, w)
		}
	}
	wantTime("timestamp_trunc day", got["day"], time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC))
	wantTime("timestamp_trunc month", got["month"], time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC))
	wantTime("timestamp_trunc day in New York", got["dayNY"], time.Date(2024, 3, 15, 4, 0, 0, 0, time.UTC))
	want("timestamp_extract hour", got["hour"], int64(13))
	want("timestamp_extract dayofweek", got["dow"], int64(6))
	got = one("timestamps on Gadget", gadget().Select(firestore.Fields(
		firestore.TimestampExtract("ts", "isoweek").As("isoweek"),
		firestore.TimestampExtract("ts", "isoyear").As("isoyear"),
		firestore.TimestampDiff("ts", firestore.ConstantOf(time.Date(2024, 3, 15, 13, 45, 30, 250e6, time.UTC)), "day").As("days"))))
	want("timestamp_extract isoweek", got["isoweek"], int64(1))
	want("timestamp_extract isoyear", got["isoyear"], int64(2025))
	want("timestamp_diff days", got["days"], int64(290))

	// Geography: Paris to London is 343556.6 m on a sphere of S2's
	// radius, 6371010 m (haversine, computed by hand).
	got = one("geo_distance", gadget().Select(firestore.Fields(firestore.GeoDistance("loc", paris).As("d"))))
	if d, ok := got["d"].(float64); !ok || math.Abs(d-343556.6) > 1 {
		t.Errorf("geo_distance Paris-London = %v, want 343556.6 m", got["d"])
	}
	rs, err = c.Pipeline().Collection("items").
		Search(firestore.WithSearchQuery(firestore.LessThanOrEqual(firestore.GeoDistance("loc", paris), 1000))).
		Execute(ctx).Results().GetAll()
	if err != nil || len(rs) != 1 || rs[0].Data()["name"] != "Widget" {
		t.Errorf("search within 1 km of Paris = %v, %v; want Widget", rs, err)
	}
	rs, err = c.Pipeline().Collection("items").
		Search(firestore.WithSearchQuery(firestore.LessThanOrEqual(firestore.GeoDistance("loc", london), 400000)),
			firestore.WithSearchSort(firestore.Descending(firestore.GeoDistance("loc", london)))).
		Execute(ctx).Results().GetAll()
	if err != nil || len(rs) != 2 || rs[0].Data()["name"] != "Widget" || rs[1].Data()["name"] != "Gadget" {
		t.Errorf("search within 400 km of London, farthest first = %v, %v; want Widget, Gadget", rs, err)
	}
	_, err = c.Pipeline().Collection("items").Search(firestore.WithSearchQuery("widget")).Execute(ctx).Results().GetAll()
	if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "full-text search") {
		t.Errorf("full-text search = %v, want UNIMPLEMENTED saying why", err)
	}

	// A transaction: the pipeline reads in it, and the client commits it.
	var inTx int
	err = c.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		rs, err := tx.Execute(c.Pipeline().Collection("items")).Results().GetAll()
		if err != nil {
			return err
		}
		inTx = len(rs)
		return tx.Set(items.Doc("count"), map[string]any{"name": "count", "n": len(rs)})
	})
	if err != nil || inTx != 2 {
		t.Fatalf("a pipeline in a transaction = %d results, %v; want 2", inTx, err)
	}
	if s, err := items.Doc("count").Get(ctx); err != nil || s.Data()["n"] != int64(2) {
		t.Errorf("the transaction's write = %v, %v; want n 2", s, err)
	}
	err = c.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		_, err := tx.Execute(c.Pipeline().Collection("items").Delete()).Results().GetAll()
		return err
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("a delete stage in a transaction = %v, want INVALID_ARGUMENT", err)
	}

	// update: Widget (price 100) gets status Active, its other fields
	// kept; then Widget without its zip replaces the stored one.
	got = one("update", c.Pipeline().Collection("items").Where(firestore.GreaterThanOrEqual(firestore.FieldOf("price"), 100)).
		Update(firestore.WithUpdateTransformations(firestore.ConstantOf("Active").As("status"))))
	want("update's result", got, map[string]any{"documents_modified": int64(1)})
	s, err := items.Doc("i1").Get(ctx)
	if err != nil || s.Data()["status"] != "Active" || s.Data()["name"] != "Widget" || s.Data()["zip"] != "12345" {
		t.Errorf("i1 after the update = %v, %v; want status Active, name and zip kept", s.Data(), err)
	}
	afterUpdate := time.Now()
	time.Sleep(20 * time.Millisecond)
	one("update without zip", widget().RemoveFields(firestore.Fields("zip")).Update())
	if s, err := items.Doc("i1").Get(ctx); err != nil || s.Data()["zip"] != nil || s.Data()["status"] != "Active" {
		t.Errorf("i1 after the update without zip = %v, %v; want no zip", s.Data(), err)
	}
	_, err = c.Pipeline().Literals([]map[string]any{{"__name__": items.Doc("ghost"), "name": "Ghost"}}).Update().Execute(ctx).Results().GetAll()
	if status.Code(err) != codes.NotFound {
		t.Errorf("updating a document that does not exist = %v, want NOT_FOUND", err)
	}
	_, err = c.Pipeline().Collection("items").Delete().Limit(1).Execute(ctx).Results().GetAll()
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("a delete stage before another stage = %v, want INVALID_ARGUMENT", err)
	}

	// delete: Gadget goes; Widget and the count stay.
	got = one("delete", gadget().Delete())
	want("delete's result", got, map[string]any{"documents_modified": int64(1)})
	if _, err := items.Doc("i2").Get(ctx); status.Code(err) != codes.NotFound {
		t.Errorf("i2 after the delete = %v, want NOT_FOUND", err)
	}

	// read_time: the emulator keeps versions. Before the documents were
	// created there were none; after the first update, Gadget was there
	// and Widget still had its zip.
	rs, err = c.Pipeline().Collection("items").WithReadOptions(firestore.ReadTime(beforeCreate)).Execute(ctx).Results().GetAll()
	if err != nil || len(rs) != 0 {
		t.Errorf("at a read time before the writes = %d results, %v; want 0", len(rs), err)
	}
	rs, err = c.Pipeline().Collection("items").WithReadOptions(firestore.ReadTime(afterCreate)).
		Sort(firestore.Orders(firestore.Ascending(firestore.FieldOf("name")))).Execute(ctx).Results().GetAll()
	if err != nil || len(rs) != 2 || rs[0].Data()["name"] != "Gadget" {
		t.Errorf("at a read time after the creates = %v, %v; want Gadget, Widget", rs, err)
	}
	got = one("Widget at a read time", widget().WithReadOptions(firestore.ReadTime(afterUpdate)))
	if got["zip"] != "12345" || got["status"] != "Active" {
		t.Errorf("Widget after the first update = %v, want zip 12345, status Active", got)
	}
}
