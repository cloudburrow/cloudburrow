package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	bqv2 "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/option"
)

// fakeCatalog serves datasets.list, tables.list, tables.get and
// routines.list of project p: dataset sales with tables orders (id, total,
// buyer STRUCT<name, city>) and my-view, and dataset hr with table staff.
func fakeCatalog(t *testing.T) *bqv2.Service {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/bigquery/v2")
		var out any
		switch {
		case p == "/projects/p/datasets":
			out = map[string]any{"datasets": []any{
				map[string]any{"datasetReference": map[string]any{"projectId": "p", "datasetId": "sales"}},
				map[string]any{"datasetReference": map[string]any{"projectId": "p", "datasetId": "hr"}}}}
		case p == "/projects/p/datasets/sales/tables":
			out = map[string]any{"tables": []any{
				map[string]any{"type": "TABLE", "tableReference": map[string]any{"projectId": "p", "datasetId": "sales", "tableId": "orders"}},
				map[string]any{"type": "VIEW", "tableReference": map[string]any{"projectId": "p", "datasetId": "sales", "tableId": "my-view"}}}}
		case p == "/projects/p/datasets/hr/tables":
			out = map[string]any{"tables": []any{
				map[string]any{"type": "TABLE", "tableReference": map[string]any{"projectId": "p", "datasetId": "hr", "tableId": "staff"}}}}
		case p == "/projects/p/datasets/sales/tables/orders":
			out = map[string]any{"schema": map[string]any{"fields": []any{
				map[string]any{"name": "id", "type": "INTEGER"}, map[string]any{"name": "total", "type": "NUMERIC"},
				map[string]any{"name": "buyer", "type": "RECORD", "fields": []any{
					map[string]any{"name": "name", "type": "STRING"}, map[string]any{"name": "city", "type": "STRING"}}}}}}
		case p == "/projects/p/datasets/hr/tables/staff":
			out = map[string]any{"schema": map[string]any{"fields": []any{map[string]any{"name": "email", "type": "STRING"}}}}
		case p == "/projects/p/datasets/sales/routines":
			out = map[string]any{"routines": []any{map[string]any{"routineType": "SCALAR_FUNCTION",
				"routineReference": map[string]any{"projectId": "p", "datasetId": "sales", "routineId": "net"}}}}
		default:
			http.Error(w, `{"error":{"code":404,"message":"not found"}}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	svc, err := bqv2.NewService(context.Background(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// TestStudioParserCompletion (#2094): Tools > Parser-based auto-completion's
// suggestions where the GoogleSQL grammar puts a table path, a column, a
// STRUCT field and a routine, and none inside a string or a comment.
func TestStudioParserCompletion(t *testing.T) {
	svc := fakeCatalog(t)
	p := bigqueryProvider{project: "p"}
	complete := func(before, after string) *bqCompleteAnswer {
		t.Helper()
		body, _ := json.Marshal(bqCompleteRequest{Before: before, After: after})
		ans, err := p.studioComplete(context.Background(), svc, body)
		if err != nil {
			t.Fatal(err)
		}
		return ans
	}
	labels := func(a *bqCompleteAnswer, kind string) []string {
		var out []string
		for _, it := range a.Items {
			if kind == "" || it.Kind == kind {
				out = append(out, it.Label)
			}
		}
		return out
	}
	insertOf := func(a *bqCompleteAnswer, label string) string {
		for _, it := range a.Items {
			if it.Label == label {
				return it.Insert
			}
		}
		return "<none>"
	}
	has := func(list []string, want ...string) bool {
		for _, w := range want {
			found := false
			for _, l := range list {
				found = found || l == w
			}
			if !found {
				return false
			}
		}
		return true
	}

	a := complete("SELECT * FROM ", "")
	if got := labels(a, ""); !has(got, "p", "sales", "hr") || has(got, "SELECT") {
		t.Errorf("after FROM: %v, want the project and its datasets only", got)
	}
	if insertOf(a, "sales") != "sales." || insertOf(a, "p") != "p." {
		t.Errorf("after FROM inserts %q, %q", insertOf(a, "sales"), insertOf(a, "p"))
	}
	a = complete("SELECT * FROM sa", "")
	if got := labels(a, ""); len(got) != 1 || got[0] != "sales" || a.Replace != "sa" {
		t.Errorf("FROM sa: %v replace %q", got, a.Replace)
	}
	a = complete("SELECT * FROM sales.", "")
	if got := labels(a, "Table"); !has(got, "orders", "my-view") || insertOf(a, "my-view") != "`my-view`" {
		t.Errorf("FROM sales.: %v, my-view inserts %q", got, insertOf(a, "my-view"))
	}
	a = complete("SELECT * FROM `p.sales.o", "")
	if got := labels(a, ""); len(got) != 1 || insertOf(a, "orders") != "`p.sales.orders`" || a.Replace != "`p.sales.o" {
		t.Errorf("FROM `p.sales.o: %v %q replace %q", got, insertOf(a, "orders"), a.Replace)
	}
	a = complete("SELECT * FROM `p.", "`")
	if !has(labels(a, "Dataset"), "sales", "hr") || insertOf(a, "hr") != "`p.hr." {
		t.Errorf("FROM `p.: %v", a.Items)
	}
	a = complete("SELECT * FROM sales.orders o JOIN ", "")
	if !has(labels(a, ""), "sales") {
		t.Errorf("after JOIN: %v", labels(a, ""))
	}

	a = complete("SELECT ", " FROM sales.orders AS o, hr.staff")
	if got := labels(a, "Column"); !has(got, "id", "total", "buyer", "email") {
		t.Errorf("SELECT list: columns %v", got)
	}
	if !has(labels(a, "Function"), "COUNT") || !has(labels(a, "Keyword"), "DISTINCT") || !has(labels(a, "Alias"), "o") {
		t.Errorf("SELECT list: %v", labels(a, ""))
	}
	if a.Items[0].Kind != "Column" {
		t.Errorf("columns come first: %v", a.Items[0])
	}
	a = complete("SELECT o.", " FROM sales.orders o")
	if got := labels(a, ""); len(got) != 3 || !has(got, "id", "total", "buyer") {
		t.Errorf("o.: %v", got)
	}
	a = complete("SELECT o.buyer.c", " FROM sales.orders o")
	if got := labels(a, ""); len(got) != 1 || got[0] != "city" || a.Replace != "c" {
		t.Errorf("o.buyer.c: %v", got)
	}
	a = complete("SELECT staff.em", " FROM hr.staff")
	if got := labels(a, ""); len(got) != 1 || got[0] != "email" {
		t.Errorf("staff.em: %v", got)
	}
	a = complete("SELECT * FROM sales.orders WHERE to", "")
	if got := labels(a, ""); !has(got, "total", "TO", "TO_JSON") || labels(a, "")[0] != "total" {
		t.Errorf("WHERE to: %v", got)
	}
	a = complete("SELECT sales.", " FROM sales.orders")
	if got := labels(a, "Function"); !has(got, "net") {
		t.Errorf("sales. routines: %v", labels(a, ""))
	}
	for _, before := range []string{"SELECT 'FROM ", "SELECT 1 -- FROM ", "SELECT /* FROM "} {
		if a := complete(before, ""); len(a.Items) != 0 {
			t.Errorf("%q: %v, want nothing inside a string or comment", before, labels(a, ""))
		}
	}
	a = complete("SELECT 1; SELECT * FROM hr.staff WHERE ", "")
	if got := labels(a, "Column"); len(got) != 1 || got[0] != "email" {
		t.Errorf("second statement: columns %v, want its own table's", got)
	}
	a = complete("WITH recent AS (SELECT 1) SELECT * FROM ", "")
	if !has(labels(a, ""), "recent") {
		t.Errorf("WITH: %v", labels(a, ""))
	}
}
