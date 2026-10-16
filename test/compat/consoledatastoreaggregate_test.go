//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
)

// TestConsoleDatastoreAggregate.
//
// The Datastore query builder's Aggregate (#1405), sent through the console
// API and checked against the official client's own aggregation over the
// same query. A kind holds hours 1, 2 and 4.5 and a string: COUNT counts
// all four, or the two done ones with the filter; SUM and AVG add up the
// numbers and skip the string; Limit 1 changes nothing.
//
// covers: google.datastore.v1.Datastore/RunAggregationQuery
func TestConsoleDatastoreAggregate(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := datastoreClient(t, h, h.Project())
	ctx := h.Context()
	project := h.Project()
	const kind = "Agg1405"
	rows := []datastore.PropertyList{
		{{Name: "hours", Value: int64(1)}, {Name: "done", Value: true}},
		{{Name: "hours", Value: int64(2)}, {Name: "done", Value: false}},
		{{Name: "hours", Value: 4.5}, {Name: "done", Value: true}},
		{{Name: "hours", Value: "many"}, {Name: "done", Value: false}},
	}
	keys := make([]*datastore.Key, len(rows))
	for i := range keys {
		keys[i] = datastore.NameKey(kind, fmt.Sprintf("t%d", i), nil)
	}
	if _, err := c.PutMulti(ctx, keys, rows); err != nil {
		t.Fatalf("PutMulti: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteMulti(ctx, keys) })

	console := func(values map[string]string) (string, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": []string{kind}, "Values": values})
		var res struct{ Listing datastoreRows }
		consoleJSON(t, addr, http.MethodPost, "/api/query/datastore?project="+url.QueryEscape(project), string(body), &res)
		if len(res.Listing.Items) != 1 {
			t.Fatalf("Aggregate %v answered %+v, want one row", values, res.Listing.Items)
		}
		return res.Listing.Items[0].Name, res.Listing.Items[0].Fields["Value"]
	}
	client := func(aq *datastore.AggregationQuery, alias string) string {
		t.Helper()
		res, err := c.RunAggregationQuery(ctx, aq)
		if err != nil {
			t.Fatalf("the client's aggregation: %v", err)
		}
		switch v := res[alias].(*datastorepb.Value).GetValueType().(type) {
		case *datastorepb.Value_IntegerValue:
			return fmt.Sprint(v.IntegerValue)
		case *datastorepb.Value_DoubleValue:
			return fmt.Sprint(v.DoubleValue)
		}
		return fmt.Sprint(res[alias])
	}
	all := datastore.NewQuery(kind)
	for _, tc := range []struct {
		values map[string]string
		query  *datastore.AggregationQuery
		name   string
		want   string
	}{
		{map[string]string{"aggregate": "COUNT"}, all.NewAggregationQuery().WithCount("count"), "COUNT(*)", "4"},
		{map[string]string{"aggregate": "COUNT", "limit": "1"}, all.NewAggregationQuery().WithCount("count"), "COUNT(*)", "4"},
		{map[string]string{"aggregate": "COUNT", "property": "done", "op": "=", "value": "true"},
			all.FilterField("done", "=", true).NewAggregationQuery().WithCount("count"), "COUNT(*)", "2"},
		{map[string]string{"aggregate": "SUM", "aggregateProperty": "hours"}, all.NewAggregationQuery().WithSum("hours", "sum"), "SUM(hours)", "7.5"},
		{map[string]string{"aggregate": "AVG", "aggregateProperty": "hours"}, all.NewAggregationQuery().WithAvg("hours", "avg"), "AVG(hours)", "2.5"},
	} {
		name, got := console(tc.values)
		alias := map[string]string{"COUNT(*)": "count", "SUM(hours)": "sum", "AVG(hours)": "avg"}[tc.name]
		if want := client(tc.query, alias); name != tc.name || got != want || got != tc.want {
			t.Errorf("%v: the console answers %s = %s and the client %s, want %s", tc.values, name, got, want, tc.want)
		}
	}
}
