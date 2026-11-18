package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/bigtable"
)

// TestConsoleBigtableSQLGeography (#1720) runs the geography functions in
// the table's query form: a GEOGRAPHY written as WKT (ST_ASTEXT), a
// distance and an S2 cell, the point-set operations, buffers and
// ST_SIMPLIFY (#1728) with ST_UNION_AGG over the table's rows, and a
// GEOGRAPHY column, which Bigtable's result types cannot carry, refused
// with the reason.
func TestConsoleBigtableSQLGeography(t *testing.T) {
	srv, addr := bigtablePickerConsole(t)
	ctx := context.Background()
	iac, err := bigtable.NewInstanceAdminClient(ctx, "demo", localOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iac.Close() })
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: "geo-inst", DisplayName: "Geography",
		ClusterId: "geo-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatal(err)
	}
	admin, err := bigtable.NewAdminClient(ctx, "demo", "geo-inst", localOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	client, err := bigtable.NewClient(ctx, "demo", "geo-inst", localOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := admin.CreateTable(ctx, "places"); err != nil {
		t.Fatal(err)
	}
	if err := admin.CreateColumnFamily(ctx, "places", "loc"); err != nil {
		t.Fatal(err)
	}
	m := bigtable.NewMutation()
	m.Set("loc", "wkt", bigtable.Now(), []byte("POLYGON((0 0, 0 2, 2 2, 2 0, 0 0))"))
	if err := client.Open("places").Apply(ctx, "square", m); err != nil {
		t.Fatal(err)
	}
	query := func(sql string) (int, string) {
		t.Helper()
		return pickerDo(t, srv, http.MethodPost, "/api/query/bigtable?project=demo&instance=geo-inst",
			map[string]any{"Path": []string{"places"}, "Values": map[string]string{"sql": sql}})
	}
	g := "ST_GEOGFROMTEXT(CAST(loc['wkt'] AS STRING))"
	code, out := query("SELECT _key, ST_ASTEXT(" + g + ") AS wkt, ST_COVERS(" + g + ", ST_GEOGPOINT(1, 1)) AS inside, S2_CELLIDFROMPOINT(ST_GEOGPOINT(-122, 47), level => 10) AS cell FROM places")
	if code != http.StatusOK {
		t.Fatalf("query = %d: %s", code, out)
	}
	for _, want := range []string{"POLYGON((2 0, 2 2, 0 2, 0 0, 2 0))", "true", "6093613287902019584"} {
		if !strings.Contains(out, want) {
			t.Errorf("the query form lacks %q: %s", want, out)
		}
	}
	// #1728: the references' examples, and a union over the rows.
	code, out = query("SELECT ST_ASTEXT(ST_DIFFERENCE(ST_GEOGFROMTEXT('POLYGON((0 0, 10 0, 10 10, 0 0))'), ST_GEOGFROMTEXT('POLYGON((4 2, 6 2, 8 6, 4 2))'))) AS d, " +
		"ST_ASTEXT(ST_INTERSECTION(" + g + ", ST_GEOGPOINT(1, 1))) AS i, ST_NUMPOINTS(ST_BUFFER(ST_GEOGPOINT(1, 2), 50, 2)) AS b, " +
		"ST_NUMPOINTS(ST_BUFFERWITHTOLERANCE(ST_GEOGPOINT(1, 2), 100, 25)) AS bt, ST_ASTEXT(ST_SIMPLIFY(ST_GEOGFROMTEXT('LINESTRING(0 0, 0.05 0, 0.1 0, 0.15 0, 2 0)'), 1)) AS s, " +
		"ST_ASTEXT(ST_UNION(ST_GEOGFROMTEXT('LINESTRING(-122.12 47.67, -122.19 47.69)'), ST_GEOGFROMTEXT('LINESTRING(-122.12 47.67, -100.19 47.69)'))) AS u FROM places")
	if code != http.StatusOK {
		t.Fatalf("query = %d: %s", code, out)
	}
	for _, want := range []string{"POLYGON((0 0, 10 0, 10 10, 0 0), (8 6, 6 2, 4 2, 8 6))", "POINT(1 1)", "9", "6", "LINESTRING(0 0, 2 0)", "LINESTRING(-100.19 47.69, -122.12 47.67, -122.19 47.69)"} {
		if !strings.Contains(out, want) {
			t.Errorf("the query form lacks %q: %s", want, out)
		}
	}
	code, out = query("SELECT ST_ASTEXT(ST_UNION_AGG(" + g + ")) AS u FROM places")
	if code != http.StatusOK || !strings.Contains(out, "POLYGON((2 0, 2 2, 0 2, 0 0, 2 0))") {
		t.Errorf("ST_UNION_AGG = %d: %s", code, out)
	}
	if code, out := query("SELECT " + g + " AS g FROM places"); code == http.StatusOK || !strings.Contains(out, "ST_ASTEXT") {
		t.Errorf("a GEOGRAPHY column = %d: %s, want the refusal naming ST_ASTEXT", code, out)
	}
}
