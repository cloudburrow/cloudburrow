package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
)

// TestConsoleBigtableSQLUnpackAndTypedValues (#1704) runs, in the table's
// query form, an UNPACK query of the time series, a STRUCT, a DATE and an
// array of floats, and checks each reads as the console writes it.
func TestConsoleBigtableSQLUnpackAndTypedValues(t *testing.T) {
	srv, addr := bigtablePickerConsole(t)
	ctx := context.Background()
	iac, err := bigtable.NewInstanceAdminClient(ctx, "demo", localOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iac.Close() })
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: "series-inst", DisplayName: "Time series",
		ClusterId: "series-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatal(err)
	}
	admin, err := bigtable.NewAdminClient(ctx, "demo", "series-inst", localOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	client, err := bigtable.NewClient(ctx, "demo", "series-inst", localOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := admin.CreateTable(ctx, "report"); err != nil {
		t.Fatal(err)
	}
	if err := admin.CreateColumnFamily(ctx, "report", "report"); err != nil {
		t.Fatal(err)
	}
	m := bigtable.NewMutation()
	m.Set("report", "sales", bigtable.Time(time.Date(2025, 4, 1, 22, 0, 0, 0, time.UTC)), []byte("12"))
	m.Set("report", "sales", bigtable.Time(time.Date(2025, 4, 1, 20, 0, 0, 0, time.UTC)), []byte("10"))
	if err := client.Open("report").Apply(ctx, "Dept1", m); err != nil {
		t.Fatal(err)
	}
	query := func(sql string) string {
		t.Helper()
		code, out := pickerDo(t, srv, http.MethodPost, "/api/query/bigtable?project=demo&instance=series-inst",
			map[string]any{"Path": []string{"report"}, "Values": map[string]string{"sql": sql}})
		if code != http.StatusOK {
			t.Fatalf("%s = %d: %s", sql, code, out)
		}
		return out
	}
	out := query("SELECT _key, _timestamp, report FROM UNPACK((SELECT _key, report FROM report(with_history => true)))")
	for _, want := range []string{"2025-04-01T22:00:00Z", "2025-04-01T20:00:00Z", "sales: 12", "sales: 10"} {
		if !strings.Contains(out, want) {
			t.Errorf("UNPACK in the query form lacks %q: %s", want, out)
		}
	}
	out = query("SELECT _key, STRUCT(1 AS a, 'abc' AS b) AS s, DATE(2016, 12, 25) AS d, TO_VECTOR32(b'\\x3f\\xc0\\x00\\x00') AS v, SOUNDEX('Ashcraft') AS x FROM report")
	for _, want := range []string{"(1, abc)", "2016-12-25", "[1.5]", "A261"} {
		if !strings.Contains(out, want) {
			t.Errorf("typed values in the query form lack %q: %s", want, out)
		}
	}
}
