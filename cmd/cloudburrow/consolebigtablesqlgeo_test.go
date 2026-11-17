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
// distance and an S2 cell, and a GEOGRAPHY column, which Bigtable's result
// types cannot carry, refused with the reason.
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
	if code, out := query("SELECT " + g + " AS g FROM places"); code == http.StatusOK || !strings.Contains(out, "ST_ASTEXT") {
		t.Errorf("a GEOGRAPHY column = %d: %s, want the refusal naming ST_ASTEXT", code, out)
	}
}
