//go:build compat

package compat

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// TestBigQueryRoutinesListGetDelete (#1177), official Go client:
// Dataset.Routines (routines.list), Routine.Metadata (routines.get) and
// Routine.Delete (routines.delete) for a function CREATE FUNCTION made, a
// table function CREATE TABLE FUNCTION made, and a function routines.insert
// made. Each is listed; each one's type, language, arguments, return type
// and body read back as made; a routine that does not exist is 404; once
// deleted, a routine is 404, gone from the list, and its function "not
// found" in a query; DROP FUNCTION and routines.delete agree.
//
// covers: bigquery.routines.list, bigquery.routines.get, bigquery.routines.delete
func TestBigQueryRoutinesListGetDelete(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	d := ds.DatasetID

	if err := bqRun(ctx, c, "CREATE FUNCTION "+d+".plus(x INT64, label STRING) RETURNS STRING AS (CONCAT(label, CAST(x + 1 AS STRING)))", false); err != nil {
		t.Fatalf("CREATE FUNCTION: %v", err)
	}
	if err := bqRun(ctx, c, "CREATE TABLE FUNCTION "+d+".rows(n INT64) RETURNS TABLE<y INT64> AS SELECT n * 2 AS y", false); err != nil {
		t.Fatalf("CREATE TABLE FUNCTION: %v", err)
	}
	if err := ds.Routine("viarest").Create(ctx, &bigquery.RoutineMetadata{
		Type: "SCALAR_FUNCTION", Language: "SQL", Body: "x * 10",
		Arguments:  []*bigquery.RoutineArgument{{Name: "x", DataType: &bigquery.StandardSQLDataType{TypeKind: "INT64"}}},
		ReturnType: &bigquery.StandardSQLDataType{TypeKind: "INT64"},
	}); err != nil {
		t.Fatalf("routines.insert: %v", err)
	}

	if got := routineIDs(t, ctx, ds); !slices.Equal(got, []string{"plus", "rows", "viarest"}) {
		t.Fatalf("routines.list = %v, want [plus rows viarest]", got)
	}

	md, err := ds.Routine("plus").Metadata(ctx)
	if err != nil {
		t.Fatalf("routines.get of plus: %v", err)
	}
	if md.Type != "SCALAR_FUNCTION" || md.Language != "SQL" || md.Body != "CONCAT(label, CAST(x + 1 AS STRING))" ||
		len(md.Arguments) != 2 || md.Arguments[0].Name != "x" || md.Arguments[0].DataType == nil || md.Arguments[0].DataType.TypeKind != "INT64" ||
		md.Arguments[1].Name != "label" || md.Arguments[1].DataType.TypeKind != "STRING" ||
		md.ReturnType == nil || md.ReturnType.TypeKind != "STRING" || md.CreationTime.IsZero() {
		t.Errorf("routines.get of plus = %+v", md)
	}
	md, err = ds.Routine("rows").Metadata(ctx)
	if err != nil {
		t.Fatalf("routines.get of rows: %v", err)
	}
	if md.Type != "TABLE_VALUED_FUNCTION" || md.Body != "SELECT n * 2 AS y" || len(md.Arguments) != 1 ||
		md.ReturnTableType == nil || len(md.ReturnTableType.Columns) != 1 || md.ReturnTableType.Columns[0].Name != "y" {
		t.Errorf("routines.get of rows = %+v", md)
	}
	md, err = ds.Routine("viarest").Metadata(ctx)
	if err != nil {
		t.Fatalf("routines.get of viarest: %v", err)
	}
	if md.Type != "SCALAR_FUNCTION" || md.Body != "x * 10" || len(md.Arguments) != 1 || md.ReturnType == nil || md.ReturnType.TypeKind != "INT64" {
		t.Errorf("routines.get of viarest = %+v", md)
	}
	notFound := func(what string, err error) {
		t.Helper()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
			t.Errorf("%s = %v, want 404", what, err)
		}
	}
	_, err = ds.Routine("nope").Metadata(ctx)
	notFound("routines.get of a routine that does not exist", err)
	notFound("routines.delete of a routine that does not exist", ds.Routine("nope").Delete(ctx))

	// routines.delete of each kind; DROP FUNCTION agrees.
	for _, id := range []string{"plus", "rows"} {
		if err := ds.Routine(id).Delete(ctx); err != nil {
			t.Fatalf("routines.delete of %s: %v", id, err)
		}
		_, err := ds.Routine(id).Metadata(ctx)
		notFound("routines.get after routines.delete of "+id, err)
	}
	if err := bqRun(ctx, c, "SELECT "+d+".plus(1, 'a')", false); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("a call of a deleted function = %v, want not found", err)
	}
	if err := bqRun(ctx, c, "DROP FUNCTION "+d+".plus", false); err == nil {
		t.Errorf("DROP FUNCTION of the function routines.delete dropped succeeded")
	}
	if err := bqRun(ctx, c, "DROP FUNCTION "+d+".viarest", false); err != nil {
		t.Fatalf("DROP FUNCTION: %v", err)
	}
	notFound("routines.delete after DROP FUNCTION", ds.Routine("viarest").Delete(ctx))
	if got := routineIDs(t, ctx, ds); len(got) != 0 {
		t.Errorf("after every routine is gone, routines.list = %v", got)
	}
}

// routineIDs is every routine of a dataset, from routines.list.
func routineIDs(t *testing.T, ctx context.Context, ds *bigquery.Dataset) []string {
	t.Helper()
	var ids []string
	it := ds.Routines(ctx)
	for {
		r, err := it.Next()
		if errors.Is(err, iterator.Done) {
			slices.Sort(ids)
			return ids
		}
		if err != nil {
			t.Fatalf("routines.list: %v", err)
		}
		ids = append(ids, r.RoutineID)
	}
}

// TestConsoleBigQueryRoutines (#782), in the emulators shard, through the
// official Go client and the console API. A function CREATE FUNCTION made
// and a table function routines.insert made are both in routines.list, and
// routines.get gives each one's type, arguments and body. The dataset's page
// lists them in its Routines section with their type and signature, and
// beside the dataset's table; a routine's page shows its body,
// arguments and details. Delete routine on the console drops one
// (routines.get is then 404, and the page is gone), and routines.delete
// through the client drops the other; the table is left. (A table named
// as a function of its dataset breaks the engine's catalog, #1186, so the
// table here has a name of its own.)
//
// covers: bigquery.routines.list, bigquery.routines.get, bigquery.routines.delete
func TestConsoleBigQueryRoutines(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	d := ds.DatasetID

	if err := bqRun(ctx, c, "CREATE FUNCTION "+d+".twice(x INT64) RETURNS INT64 AS (x * 2)", false); err != nil {
		t.Fatalf("CREATE FUNCTION: %v", err)
	}
	if err := ds.Routine("tf").Create(ctx, &bigquery.RoutineMetadata{
		Type: "TABLE_VALUED_FUNCTION", Language: "SQL", Body: "SELECT x + 100 AS y",
		Arguments: []*bigquery.RoutineArgument{{Name: "x", DataType: &bigquery.StandardSQLDataType{TypeKind: "INT64"}}},
	}); err != nil {
		t.Fatalf("routines.insert: %v", err)
	}
	if err := ds.Table("numbers").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "n", Type: bigquery.IntegerFieldType}}}); err != nil {
		t.Fatalf("create table: %v", err)
	}

	if got := routineIDs(t, ctx, ds); !slices.Equal(got, []string{"tf", "twice"}) {
		t.Fatalf("routines.list = %v, want [tf twice]", got)
	}
	md, err := ds.Routine("tf").Metadata(ctx)
	if err != nil {
		t.Fatalf("routines.get: %v", err)
	}
	if md.Type != "TABLE_VALUED_FUNCTION" || !strings.Contains(md.Body, "x + 100") || len(md.Arguments) != 1 || md.Arguments[0].Name != "x" {
		t.Errorf("routines.get of tf = type %q body %q arguments %d", md.Type, md.Body, len(md.Arguments))
	}
	md, err = ds.Routine("twice").Metadata(ctx)
	if err != nil {
		t.Fatalf("routines.get: %v", err)
	}
	if md.Type != "SCALAR_FUNCTION" || !strings.Contains(md.Body, "x * 2") {
		t.Errorf("routines.get of twice = type %q body %q", md.Type, md.Body)
	}

	// The dataset's page.
	var page consoleDetail
	consoleJSON(t, addr, http.MethodGet, "/api/detail/bigquery?"+url.Values{"project": {project}, "name": {d}}.Encode(), "", &page)
	routines := page.section(t, "routines")
	if routines.Unavailable != "" {
		t.Fatalf("the Routines section: %s", routines.Unavailable)
	}
	byName := map[string]map[string]string{}
	for _, it := range routines.Items {
		byName[it.Name] = it.Fields
	}
	if f := byName["tf"]; f["Type"] != "Table function" || f["Arguments"] != "x INT64" {
		t.Errorf("the tf row = %v", f)
	}
	if f := byName["twice"]; f["Type"] != "Function" || f["Arguments"] != "x INT64" {
		t.Errorf("the twice row = %v", f)
	}
	if tables := page.section(t, "tables"); len(tables.Items) != 1 || tables.Items[0].Name != "numbers" {
		t.Errorf("the Tables section = %+v, want the table numbers", tables.Items)
	}

	// A routine's page.
	routinePage := func(name string) (int, string) {
		v := url.Values{"project": {project}, "name": {d, "routines", name}}
		return consoleDo(t, addr, http.MethodGet, "/api/detail/bigquery?"+v.Encode(), "")
	}
	code, out := routinePage("twice")
	if code != http.StatusOK || !strings.Contains(out, "x * 2") || !strings.Contains(out, `"Function"`) {
		t.Errorf("the twice routine's page = %d: %s", code, out)
	}

	// Delete routine on the console, and routines.delete through the client.
	if code, out := consoleAct(t, addr, "bigquery", project, []string{d, "routines", "twice"}, "deleteroutine", nil); code != http.StatusOK {
		t.Fatalf("console Delete routine = %d: %s", code, out)
	}
	_, err = ds.Routine("twice").Metadata(ctx)
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("after Delete routine, routines.get = %v, want 404", err)
	}
	if code, out := routinePage("twice"); code != http.StatusOK || !strings.Contains(out, "cannot read the routine") {
		t.Errorf("the deleted routine's page = %d: %s", code, out)
	}
	if err := ds.Routine("tf").Delete(ctx); err != nil {
		t.Fatalf("routines.delete: %v", err)
	}
	if got := routineIDs(t, ctx, ds); len(got) != 0 {
		t.Errorf("after both deletes, routines.list = %v", got)
	}
	if _, err := ds.Table("numbers").Metadata(ctx); err != nil {
		t.Errorf("the table is gone: %v", err)
	}
}
