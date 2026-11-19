//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TestDatastoreNotEqualInNotInAndOr (#1888): the !=, IN and NOT_IN
// property filters and OR composite filters Google serves
// (docs/concepts/queries#filters), which the emulator refuses, through
// the official Go client and GQL, in the default database and a named
// one: the merged results in key order and in a property's order (an
// array at its smallest value satisfying the filter), offset and limit,
// cursors that resume, COUNT, SUM and AVG, a query in a transaction, and
// Google's limits.
//
// covers: google.datastore.v1.Datastore/RunQuery
// covers: google.datastore.v1.Datastore/RunAggregationQuery
func TestDatastoreNotEqualInNotInAndOr(t *testing.T) {
	h := New(t)
	ep := h.Endpoint(EnvDatastore)
	h.Endpoint(EnvFirestore)
	ctx := h.Context()
	project := h.Project()
	db := uniqueDB("ops")
	if _, err := createDB(t, h, db, &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_DATASTORE_MODE}); err != nil {
		t.Fatalf("CreateDatabase %s: %v", db, err)
	}
	t.Setenv("DATASTORE_EMULATOR_HOST", ep)
	conn, err := grpc.NewClient(ep, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	raw := datastorepb.NewDatastoreClient(conn)

	for _, database := range []string{"", db} {
		t.Run("database="+database, func(t *testing.T) {
			c, err := datastore.NewClientWithDatabase(ctx, project, database)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			kind := fmt.Sprintf("Ops1888x%d", time.Now().UnixNano())
			parent := datastore.NameKey("Ops1888Parent", "p", nil)
			var keys []*datastore.Key
			var ents []any
			for i := int64(1); i <= 7; i++ {
				props := datastore.PropertyList{{Name: "J", Value: 10 - i}}
				switch {
				case i == 6:
					props = append(props, datastore.Property{Name: "I", Value: []any{int64(2), int64(9)}})
				case i < 6:
					props = append(props, datastore.Property{Name: "I", Value: i})
				} // 7 has no I
				keys = append(keys, datastore.IDKey(kind, i, parent))
				ents = append(ents, &props)
			}
			if _, err := c.PutMulti(ctx, keys, ents); err != nil {
				t.Fatalf("PutMulti: %v", err)
			}
			ids := func(q *datastore.Query) string {
				t.Helper()
				ks, err := c.GetAll(ctx, q.KeysOnly(), nil)
				if err != nil {
					t.Fatalf("GetAll: %v", err)
				}
				var out []string
				for _, k := range ks {
					out = append(out, fmt.Sprint(k.ID))
				}
				return strings.Join(out, ",")
			}
			q := func() *datastore.Query { return datastore.NewQuery(kind) }
			eq := func(v int64) datastore.PropertyFilter {
				return datastore.PropertyFilter{FieldName: "I", Operator: "=", Value: v}
			}
			for _, tc := range []struct {
				name string
				q    *datastore.Query
				want string
			}{
				{"!=", q().FilterField("I", "!=", 2), "1,3,4,5,6"},
				{"!= ordered", q().FilterField("I", "!=", 3).Order("-I"), "6,5,4,2,1"},
				{"in", q().FilterField("I", "in", []any{4, 2, 5}), "2,4,5,6"},
				{"in ordered", q().FilterField("I", "in", []any{4, 2, 5}).Order("I"), "2,6,4,5"},
				{"not-in", q().FilterField("I", "not-in", []any{2, 9, 4}), "1,3,5"},
				{"or", q().FilterEntity(datastore.OrFilter{Filters: []datastore.EntityFilter{eq(1),
					datastore.PropertyFilter{FieldName: "J", Operator: "<", Value: 5}}}), "1,6,7"},
				{"and of or", q().FilterEntity(datastore.AndFilter{Filters: []datastore.EntityFilter{
					datastore.PropertyFilter{FieldName: "J", Operator: ">", Value: 5},
					datastore.OrFilter{Filters: []datastore.EntityFilter{eq(1), eq(3), eq(4)}}}}), "1,3,4"},
				{"offset and limit", q().FilterField("I", "!=", 2).Order("I").Offset(1).Limit(2), "3,4"},
			} {
				if got := ids(tc.q); got != tc.want {
					t.Errorf("%s = %s, want %s", tc.name, got, tc.want)
				}
			}

			// Full entities, and pages resumed at the iterator's cursor.
			it := c.Run(ctx, q().FilterField("I", "!=", 2).Order("I").Limit(2))
			var seen []string
			for {
				var pl datastore.PropertyList
				k, err := it.Next(&pl)
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatalf("Next: %v", err)
				}
				if len(pl) == 0 {
					t.Errorf("%v: no properties", k)
				}
				seen = append(seen, fmt.Sprint(k.ID))
			}
			cur, err := it.Cursor()
			if err != nil {
				t.Fatalf("Cursor: %v", err)
			}
			if got := ids(q().FilterField("I", "!=", 2).Order("I").Start(cur)); strings.Join(seen, ",") != "1,3" || got != "4,5,6" {
				t.Errorf("pages = %v then %s, want 1,3 then 4,5,6", seen, got)
			}

			// COUNT, SUM and AVG over them.
			ar, err := c.RunAggregationQuery(ctx, q().FilterField("I", "in", []any{1, 3}).NewAggregationQuery().
				WithCount("n").WithSum("J", "s").WithAvg("J", "a"))
			if err != nil {
				t.Fatalf("RunAggregationQuery: %v", err)
			}
			if fmt.Sprint(ar["n"], " ", ar["s"], " ", ar["a"]) != "integer_value:2 integer_value:16 double_value:8" {
				t.Errorf("aggregation over in = %v", ar)
			}

			// In a transaction (an ancestor query).
			tx, err := c.NewTransaction(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := c.GetAll(ctx, q().Ancestor(parent).FilterField("I", "in", []any{1, 5}).KeysOnly().Transaction(tx), nil); err != nil || len(got) != 2 {
				t.Errorf("in a transaction: %v, %v", got, err)
			}
			_ = tx.Rollback()

			// GQL: !=, IN ARRAY, NOT IN, OR, a positional binding, and an aggregation.
			md := metadata.Pairs("x-goog-request-params", "project_id="+project+"&database_id="+database)
			gctx := metadata.NewOutgoingContext(ctx, md)
			gql := func(s string, bind ...*datastorepb.Value) string {
				t.Helper()
				g := &datastorepb.GqlQuery{QueryString: s, AllowLiterals: true}
				for _, v := range bind {
					g.PositionalBindings = append(g.PositionalBindings, &datastorepb.GqlQueryParameter{ParameterType: &datastorepb.GqlQueryParameter_Value{Value: v}})
				}
				resp, err := raw.RunQuery(gctx, &datastorepb.RunQueryRequest{ProjectId: project, DatabaseId: database,
					QueryType: &datastorepb.RunQueryRequest_GqlQuery{GqlQuery: g}})
				if err != nil {
					t.Fatalf("%s: %v", s, err)
				}
				if resp.GetQuery() == nil {
					t.Errorf("%s: no parsed query", s)
				}
				var out []string
				for _, r := range resp.GetBatch().GetEntityResults() {
					p := r.GetEntity().GetKey().GetPath()
					out = append(out, fmt.Sprint(p[len(p)-1].GetId()))
				}
				return strings.Join(out, ",")
			}
			arr := func(vs ...int64) *datastorepb.Value {
				a := &datastorepb.ArrayValue{}
				for _, v := range vs {
					a.Values = append(a.Values, &datastorepb.Value{ValueType: &datastorepb.Value_IntegerValue{IntegerValue: v}})
				}
				return &datastorepb.Value{ValueType: &datastorepb.Value_ArrayValue{ArrayValue: a}}
			}
			for s, want := range map[string]string{
				"SELECT * FROM " + kind + " WHERE I != 2":                                    "1,3,4,5,6",
				"SELECT * FROM " + kind + " WHERE I IN ARRAY(4, 2, 5) ORDER BY I":            "2,6,4,5",
				"SELECT * FROM " + kind + " WHERE I NOT IN ARRAY(2, 9, 4)":                   "1,3,5",
				"SELECT * FROM " + kind + " WHERE I = 1 OR J < 5":                            "1,6,7",
				"SELECT __key__ FROM " + kind + " WHERE J > 5 AND (I = 1 OR 3 = I OR I = 4)": "1,3,4",
				"SELECT * FROM " + kind + " WHERE I != 2 ORDER BY I LIMIT 2 OFFSET 1":        "3,4",
			} {
				if got := gql(s); got != want {
					t.Errorf("%s = %s, want %s", s, got, want)
				}
			}
			if got := gql("SELECT * FROM "+kind+" WHERE I IN @1 AND J > @2", arr(1, 3, 5), &datastorepb.Value{ValueType: &datastorepb.Value_IntegerValue{IntegerValue: 6}}); got != "1,3" {
				t.Errorf("IN @1 AND J > @2 = %s, want 1,3", got)
			}
			agg, err := raw.RunAggregationQuery(gctx, &datastorepb.RunAggregationQueryRequest{ProjectId: project, DatabaseId: database,
				QueryType: &datastorepb.RunAggregationQueryRequest_GqlQuery{GqlQuery: &datastorepb.GqlQuery{AllowLiterals: true,
					QueryString: "AGGREGATE COUNT(*) AS n, SUM(J) AS s OVER (SELECT * FROM " + kind + " WHERE I NOT IN ARRAY(1, 2))"}}})
			if err != nil {
				t.Fatalf("GQL aggregation: %v", err)
			}
			if p := agg.GetBatch().GetAggregationResults()[0].GetAggregateProperties(); p["n"].GetIntegerValue() != 4 || p["s"].GetIntegerValue() != 22 {
				t.Errorf("GQL aggregation over NOT IN = %v", p)
			}

			// Over REST, as JSON.
			body := fmt.Sprintf(`{"databaseId": %q, "query": {"kind": [{"name": %q}], "filter": {"propertyFilter": {"property": {"name": "I"}, "op": "IN", "value": {"arrayValue": {"values": [{"integerValue": "1"}, {"integerValue": "4"}]}}}}}}`, database, kind)
			hc := &http.Client{Timeout: 30 * time.Second}
			t.Cleanup(hc.CloseIdleConnections)
			hr, err := hc.Post("http://"+ep+"/v1/projects/"+project+":runQuery", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatalf("REST runQuery: %v", err)
			}
			var rest struct {
				Batch struct {
					EntityResults []struct {
						Entity struct {
							Key struct {
								Path []struct {
									ID string `json:"id"`
								} `json:"path"`
							} `json:"key"`
						} `json:"entity"`
					} `json:"entityResults"`
				} `json:"batch"`
			}
			err = json.NewDecoder(hr.Body).Decode(&rest)
			_ = hr.Body.Close()
			var restIDs []string
			for _, r := range rest.Batch.EntityResults {
				p := r.Entity.Key.Path
				restIDs = append(restIDs, p[len(p)-1].ID)
			}
			if err != nil || hr.StatusCode != http.StatusOK || strings.Join(restIDs, ",") != "1,4" {
				t.Errorf("REST runQuery with IN: %d %v %v, want 1,4", hr.StatusCode, restIDs, err)
			}

			// Google's limits.
			var thirtyOne []any
			for i := 0; i < 31; i++ {
				thirtyOne = append(thirtyOne, i)
			}
			for name, bad := range map[string]*datastore.Query{
				"in of 31":                       q().FilterField("I", "in", thirtyOne),
				"not-in of 11":                   q().FilterField("I", "not-in", thirtyOne[:11]),
				"two !=":                         q().FilterField("I", "!=", 1).FilterField("J", "!=", 1),
				"!= and not-in":                  q().FilterField("I", "!=", 1).FilterField("I", "not-in", []any{2}),
				"!= ordered by another property": q().FilterField("I", "!=", 1).Order("J"),
			} {
				_, err := c.GetAll(ctx, bad.KeysOnly(), nil)
				if status.Code(err) != codes.InvalidArgument {
					t.Errorf("%s: %v, want InvalidArgument", name, err)
				}
			}
			if got := ids(q().FilterField("I", "in", thirtyOne[:30])); got != "1,2,3,4,5,6" {
				t.Errorf("in of 30 = %s", got)
			}
		})
	}
}
