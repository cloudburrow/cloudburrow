//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"cloud.google.com/go/firestore/apiv1/firestorepb"
)

// TestConsoleFirestoreCountOnly.
//
// The query builder's Count only (#1198), sent through the console API
// against the emulator and checked against the official client's own COUNT
// aggregation over the same query. A collection holds ages 10 and 20 as
// integers, 30.5 as a double, "twenty" as a string and a document with no age;
// its subcollection under one document holds two more. Count only answers
// one row, count(*), and its count is the client's: age >= 20 counts the
// integer and the double and not the string, which Firestore compares by
// type; age == "twenty" counts the string; no filter counts all five; the
// subcollection counts its two; and a filter nothing matches counts 0.
// Limit and Order by, which a count does not apply, change nothing: a
// Limit of 1 still counts all five, and an Order by on age does not drop
// the document without one.
//
// covers: google.firestore.v1.Firestore/RunAggregationQuery
func TestConsoleFirestoreCountOnly(t *testing.T) {
	h := New(t)
	t.Setenv("FIRESTORE_EMULATOR_HOST", h.Endpoint(EnvFirestore))
	addr := consoleAddr(t, h)
	ctx := h.Context()
	project := h.Project()

	c, err := firestore.NewClient(ctx, project)
	if err != nil {
		t.Fatalf("firestore.NewClient: %v", err)
	}
	defer c.Close()
	coll := fmt.Sprintf("console-count-%d", time.Now().UnixNano())
	docs := map[string]map[string]any{
		coll + "/p1":        {"age": int64(10)},
		coll + "/p2":        {"age": int64(20)},
		coll + "/p3":        {"age": 30.5},
		coll + "/p4":        {"age": "twenty"},
		coll + "/p5":        {"name": "no age"},
		coll + "/p1/pets/a": {"age": int64(3)},
		coll + "/p1/pets/b": {"age": int64(4)},
	}
	for path, data := range docs {
		ref := c.Doc(path)
		if _, err := ref.Set(ctx, data); err != nil {
			t.Fatalf("Set %s: %v", path, err)
		}
		t.Cleanup(func() { _, _ = ref.Delete(ctx) })
	}

	consoleCount := func(path string, values map[string]string) string {
		t.Helper()
		values["count"] = "true"
		body, _ := json.Marshal(map[string]any{"Path": []string{path}, "Values": values})
		code, out := consoleDo(t, addr, http.MethodPost, "/api/query/firestore?project="+project, string(body))
		if code != http.StatusOK {
			t.Fatalf("Count only on %s %v = %d: %s", path, values, code, out)
		}
		var res struct {
			Listing struct {
				Columns []string
				Items   []struct {
					Name   string
					Fields map[string]string
				}
			}
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("decode the count: %v: %s", err, out)
		}
		if len(res.Listing.Items) != 1 || res.Listing.Items[0].Name != "count(*)" {
			t.Fatalf("Count only on %s %v answered %+v, want one row count(*)", path, values, res.Listing.Items)
		}
		return res.Listing.Items[0].Fields["Count"]
	}
	clientCount := func(q firestore.Query) string {
		t.Helper()
		res, err := q.NewAggregationQuery().WithCount("n").Get(ctx)
		if err != nil {
			t.Fatalf("the client's count: %v", err)
		}
		v, ok := res["n"].(*firestorepb.Value)
		if !ok {
			t.Fatalf("the client's count is %#v", res["n"])
		}
		return fmt.Sprint(v.GetIntegerValue())
	}

	all := c.Collection(coll).Query
	for _, tc := range []struct {
		name   string
		path   string
		values map[string]string
		query  firestore.Query
		want   string
	}{
		{"age >= 20", coll, map[string]string{"field": "age", "op": ">=", "value": "20"}, all.Where("age", ">=", 20), "2"},
		{`age == "twenty"`, coll, map[string]string{"field": "age", "op": "==", "value": "twenty"}, all.Where("age", "==", "twenty"), "1"},
		{"no filter", coll, map[string]string{}, all, "5"},
		{"no filter, Limit 1 and Order by age", coll, map[string]string{"limit": "1", "orderBy": "age"}, all, "5"},
		{"nothing matches", coll, map[string]string{"field": "age", "op": ">", "value": "1000"}, all.Where("age", ">", 1000), "0"},
		{"a subcollection", coll + "/p1/pets", map[string]string{}, c.Collection(coll + "/p1/pets").Query, "2"},
	} {
		got, client := consoleCount(tc.path, tc.values), clientCount(tc.query)
		if got != client || got != tc.want {
			t.Errorf("%s: the console counts %s and the client %s, want %s", tc.name, got, client, tc.want)
		}
	}
}
